#!/usr/bin/env python3
"""Measure reproducible prefix hit rates through the real supervisor.

Starts llama.cpp with the repository's stories model, drives two turns that
share a long prompt prefix over the control API, and verifies that the
backend-reported cache accounting (timings.cache_n) flows into per-turn
results and the application's cached-token ledger. Skips when llama-server
or the model is unavailable.
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


# The stories15M model trains at a 128-token context; keep the shared
# prefix comfortably below it.
PREFIX = "Once upon a time in a quiet village lived a curious little robot named Pip. " * 5


def report(line):
    print(line)
    log = os.environ.get("AOK_SMOKE_LOG")
    if log:
        os.makedirs(os.path.dirname(log), exist_ok=True)
        with open(log, "a") as handle:
            handle.write(line + "\n")


def main():
    server = os.environ.get("AOK_LLAMA_SERVER") or shutil.which("llama-server")
    model = os.environ.get("AOK_LLAMA_MODEL")
    repo = Path(__file__).resolve().parents[2]
    if not model:
        candidate = repo / "kernel" / ".build" / "stories15M-q4_0.gguf"
        model = str(candidate) if candidate.exists() else None
    if not server or not model:
        print(f"AOK_HITRATE_SMOKE=skip server={server} model={model}")
        return 0

    root = Path(__file__).resolve().parents[1]
    with tempfile.TemporaryDirectory(prefix="aok-hitrate-", dir="/tmp") as directory:
        work = Path(directory)
        port = int(os.environ.get("AOK_LLAMA_PORT", "8137"))
        llama = subprocess.Popen(
            [server, "-m", model, "--port", str(port), "--ctx-size", "2048", "-np", "1", "-ngl", "0"],
            stdout=subprocess.DEVNULL, stderr=subprocess.DEVNULL)
        try:
            binary = work / "supervisor"
            subprocess.run(["go", "build", "-o", str(binary), "./cmd/aok-supervisor"], cwd=root, check=True)
            manifest = work / "manifest.yaml"
            manifest.write_text("engine: llama\ncapabilities:\n  net: false\n")
            state = work / "state"
            path = str(state / "control.sock")
            process = subprocess.Popen(
                [str(binary), "-state", str(state), "-manifest", str(manifest),
                 "-llama-endpoint", f"http://127.0.0.1:{port}", "-max-tokens", "24"],
                stdout=subprocess.PIPE, stderr=subprocess.PIPE)
        except BaseException:
            llama.terminate()
            llama.wait(timeout=10)
            raise
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
            assert healthy(), "llama-server did not become ready"
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

            app = call("application.create", {"owner_agent": "research", "wake_policy": "on_event"})
            aid = app["application_id"]
            turns = []
            for i, tail in enumerate(("first", "second")):
                key = f"turn-{i}"
                sent = call("message.send", {"application_id": aid, "idempotency_key": key,
                                             "payload": {"text": PREFIX + " " + tail}})
                turns.append(sent["message_id"])
                deadline = time.monotonic() + 90
                while time.monotonic() < deadline:
                    try:
                        result = call("message.result", {"application_id": aid, "message_id": turns[-1]})
                    except AssertionError as pending:
                        if "-32004" not in str(pending):
                            raise
                        time.sleep(0.1)
                        continue
                    if result.get("status") in ("completed", "failed"):
                        break
                    time.sleep(0.1)
                else:
                    raise AssertionError(f"turn {i} did not finish")
                assert result["status"] == "completed", result
                turns[i] = result

            second = turns[1]["usage"]
            assert second["cached_tokens"] > 0, f"shared prefix not cached: {second}"
            inspected = call("application.inspect", {"application_id": aid})
            assert inspected["tokens_cached"] >= turns[0]["usage"]["cached_tokens"] + second["cached_tokens"], inspected
            rates = [t["usage"]["cached_tokens"] / t["usage"]["input_tokens"] for t in turns]
            report(f"AOK_HITRATE_SMOKE=pass turns={len(turns)} "
                   f"hit_rates={'/'.join(f'{r:.2f}' for r in rates)} "
                   f"cached_total={inspected['tokens_cached']}")
            return 0
        finally:
            process.kill()
            process.communicate(timeout=10)
            llama.terminate()
            llama.wait(timeout=10)


if __name__ == "__main__":
    raise SystemExit(main())
