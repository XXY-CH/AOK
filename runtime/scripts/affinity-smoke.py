#!/usr/bin/env python3
"""Prove prefix-affinity placement with a real backend.

The stories model trains at a 128-token context, so a single unrelated
turn fully evicts the KV cache. After warming a shared prefix on one
application, two further turns are queued back to back: an unrelated one
and a same-prefix one. Without affinity the creation order evicts the
prefix and the same-prefix turn misses; with affinity scheduling it runs
first and hits. Skips when llama-server or the model is unavailable.
"""
import json
import os
from pathlib import Path
import shutil
import socket
import subprocess
import tempfile
import time
import urllib.request


SHARED = "Once upon a time in a quiet village lived a curious little robot named Pip. " * 5
OTHER = "Deep in the misty mountains an old dragon guarded a silent golden bell. " * 5


def main():
    server = os.environ.get("AOK_LLAMA_SERVER") or shutil.which("llama-server")
    model = os.environ.get("AOK_LLAMA_MODEL")
    repo = Path(__file__).resolve().parents[2]
    if not model:
        candidate = repo / "kernel" / ".build" / "stories15M-q4_0.gguf"
        model = str(candidate) if candidate.exists() else None
    if not server or not model:
        print(f"AOK_AFFINITY_SMOKE=skip server={server} model={model}")
        return 0

    root = Path(__file__).resolve().parents[1]
    with tempfile.TemporaryDirectory(prefix="aok-affinity-", dir="/tmp") as directory:
        work = Path(directory)
        port = int(os.environ.get("AOK_LLAMA_PORT", "8145"))
        llama = subprocess.Popen(
            [server, "-m", model, "--port", str(port), "--ctx-size", "2048",
             "-np", "1", "-ngl", "0"], stdout=subprocess.DEVNULL, stderr=subprocess.DEVNULL)
        binary = work / "supervisor"
        subprocess.run(["go", "build", "-o", str(binary), "./cmd/aok-supervisor"], cwd=root, check=True)
        manifest = work / "manifest.yaml"
        manifest.write_text("engine: llama\ncapabilities:\n  net: false\n")
        state = work / "state"
        path = str(state / "control.sock")
        process = subprocess.Popen(
            [str(binary), "-state", str(state), "-manifest", str(manifest),
             "-llama-endpoint", f"http://127.0.0.1:{port}", "-max-tokens", "8"],
            stdout=subprocess.PIPE, stderr=subprocess.PIPE)
        sequence = 0

        def call(method, params):
            nonlocal sequence
            sequence += 1
            with socket.socket(socket.AF_UNIX, socket.SOCK_STREAM) as client:
                client.settimeout(120)
                client.connect(path)
                with client.makefile("rwb", buffering=0) as wire:
                    wire.write(json.dumps({"jsonrpc": "2.0", "id": sequence,
                                           "method": method, "params": params}).encode() + b"\n")
                    result = json.loads(wire.readline())
                    assert result["id"] == sequence and "error" not in result, result
                    return result["result"]

        def result_of(aid, mid):
            deadline = time.monotonic() + 90
            while time.monotonic() < deadline:
                try:
                    return call("message.result", {"application_id": aid, "message_id": mid})
                except AssertionError as pending:
                    if "-32004" not in str(pending):
                        raise
                    time.sleep(0.05)
            raise AssertionError("turn did not finish")

        def healthy():
            try:
                with urllib.request.urlopen(f"http://127.0.0.1:{port}/health", timeout=2) as r:
                    return r.status == 200
            except OSError:
                return False

        try:
            deadline = time.monotonic() + 120
            while time.monotonic() < deadline and not healthy():
                assert llama.poll() is None, "llama-server exited"
                time.sleep(0.2)
            deadline = time.monotonic() + 15
            while time.monotonic() < deadline:
                assert process.poll() is None, process.communicate()
                try:
                    if call("health", {})["status"] == "ok":
                        break
                except (OSError, ValueError):
                    pass
                time.sleep(0.05)
            else:
                raise AssertionError("supervisor did not become ready")

            apps = [call("application.create", {"owner_agent": "research", "wake_policy": "on_event"})
                    for _ in range(3)]
            warm, other, shared = [a["application_id"] for a in apps]

            warm_result = result_of(warm, call("message.send", {
                "application_id": warm, "idempotency_key": "warm",
                "payload": {"text": SHARED + " warm"}})["message_id"])
            assert warm_result["status"] == "completed", warm_result
            assert warm_result["usage"]["cached_tokens"] == 0, warm_result

            # Queue the evicting turn and the same-prefix turn back to back;
            # both must be pending in the same scheduler tick.
            other_id = call("message.send", {"application_id": other, "idempotency_key": "o",
                                             "payload": {"text": OTHER}})["message_id"]
            shared_id = call("message.send", {"application_id": shared, "idempotency_key": "s",
                                              "payload": {"text": SHARED + " again"}})["message_id"]
            other_result = result_of(other, other_id)
            shared_result = result_of(shared, shared_id)
            assert other_result["status"] == "completed", other_result
            assert shared_result["status"] == "completed", shared_result

            shared_cached = shared_result["usage"]["cached_tokens"]
            shared_input = shared_result["usage"]["input_tokens"]
            assert shared_cached * 4 > shared_input * 3, \
                f"affinity did not preserve the warm prefix: {shared_result['usage']}"
            assert shared_result["cache_hit_kind"] == "kv_exact", shared_result
            print(f"AOK_AFFINITY_SMOKE=pass shared_hit={shared_cached}/{shared_input} "
                  f"kind={shared_result['cache_hit_kind']} "
                  f"other_kind={other_result['cache_hit_kind']}")
            return 0
        finally:
            process.kill()
            process.communicate(timeout=10)
            llama.terminate()
            llama.wait(timeout=10)


if __name__ == "__main__":
    raise SystemExit(main())
