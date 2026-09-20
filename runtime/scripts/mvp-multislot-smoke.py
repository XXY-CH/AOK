#!/usr/bin/env python3
"""P2 multi-slot joint measurement: llama-server runs -np 4 slots, the
supervisor runs -parallel-turns 4, and the /slots endpoint must observe at
least two slots processing at the same time while all turns complete. This
is the backend-side half of 真正并行; supervisor-side concurrency has its
own barrier regression. Skips without llama/model."""
import json
import os
from pathlib import Path
import shutil
import socket
import subprocess
import tempfile
import threading
import time
import urllib.request


def main():
    server = os.environ.get("AOK_LLAMA_SERVER") or shutil.which("llama-server")
    model = os.environ.get("AOK_LLAMA_MODEL")
    fixture_args = []
    repo = Path(__file__).resolve().parents[2]
    if not model:
        candidate = repo / "kernel" / ".build" / "stories15M-q4_0.gguf"
        model = str(candidate) if candidate.exists() else None
        # This fixture trains at 128 tokens per slot; the smoke only measures
        # joint execution, not report quality.
        fixture_args = ["--override-kv", "llama.context_length=int:2048"]
    if not server or not model:
        print(f"AOK_MVP_MULTISLOT=skip server={server} model={model}")
        return 0
    port = int(os.environ.get("AOK_LLAMA_PORT", "8154"))
    slots = int(os.environ.get("AOK_MVP_SLOTS", "4"))
    parallel = int(os.environ.get("AOK_MVP_PARALLEL_TURNS", "4"))
    root = Path(__file__).resolve().parents[1]
    with tempfile.TemporaryDirectory(prefix="aok-multislot-", dir="/tmp") as directory:
        work = Path(directory)
        llama_log = (work / "llama.log").open("w+")
        llama = subprocess.Popen([server, "-m", model, "--port", str(port),
                                  "--host", "127.0.0.1", "--ctx-size", "512",
                                  "-np", str(slots), "-ngl", "0", "-cb"] + fixture_args,
                                 stdout=llama_log, stderr=llama_log)
        process = None
        try:
            deadline = time.monotonic() + 120
            while time.monotonic() < deadline:
                try:
                    with urllib.request.urlopen(f"http://127.0.0.1:{port}/health", timeout=2) as r:
                        if r.status == 200:
                            break
                except OSError:
                    if llama.poll() is not None:
                        raise RuntimeError("llama server exited before becoming ready")
                    time.sleep(0.2)
            else:
                raise TimeoutError("llama server did not become ready")

            def slot_states():
                with urllib.request.urlopen(f"http://127.0.0.1:{port}/slots", timeout=2) as r:
                    return json.load(r)

            subprocess.run(["go", "build", "-o", str(work / "supervisor"),
                           "./cmd/aok-supervisor"], cwd=root, check=True)
            (work / "manifest.yaml").write_text("engine: llama\ncapabilities:\n  net: false\n")
            process = subprocess.Popen([str(work / "supervisor"), "-state", str(work / "state"),
                                       "-manifest", str(work / "manifest.yaml"),
                                       "-llama-endpoint", f"http://127.0.0.1:{port}",
                                       "-max-tokens", "48", "-parallel-turns", str(parallel)],
                                      stdout=subprocess.PIPE, stderr=subprocess.PIPE)
            path = str(work / "state" / "control.sock")
            seq = 0

            def call(method, params, timeout=120):
                nonlocal seq
                seq += 1
                with socket.socket(socket.AF_UNIX, socket.SOCK_STREAM) as client:
                    client.settimeout(timeout)
                    client.connect(path)
                    with client.makefile("rwb", buffering=0) as wire:
                        wire.write(json.dumps({"jsonrpc": "2.0", "id": seq,
                                               "method": method, "params": params}).encode() + b"\n")
                        result = json.loads(wire.readline())
                        if "error" in result:
                            raise RuntimeError(f"{method}: {result['error']}")
                        return result["result"]

            def wait(aid, mid, budget=120):
                deadline = time.monotonic() + budget
                while time.monotonic() < deadline:
                    try:
                        return call("message.result", {"application_id": aid, "message_id": mid})
                    except RuntimeError as pending:
                        if "-32004" not in str(pending):
                            raise
                        time.sleep(0.02)
                raise AssertionError("turn timeout")

            deadline = time.monotonic() + 15
            while time.monotonic() < deadline:
                try:
                    if call("health", {})["status"] == "ok":
                        break
                except (OSError, ValueError):
                    time.sleep(0.05)

            brief = ("Why do agents fail long tasks Shared brief: "
                     "the little robot Pip walked far. ") * 2

            def one_turn(tag):
                app = call("application.create", {"owner_agent": "research",
                                                 "wake_policy": "on_event"})
                sent = call("message.send", {"application_id": app["application_id"],
                                             "idempotency_key": tag,
                                             "payload": {"text": brief + f" Focus {tag}."}})
                return app["application_id"], sent["message_id"]

            # Warmup: one turn loads the model and measures single-slot latency.
            single_start = time.monotonic()
            aid, mid = one_turn("warmup")
            single = wait(aid, mid)
            single_ms = (time.monotonic() - single_start) * 1000
            assert single["status"] == "completed", single

            jobs = [one_turn(f"m{i}") for i in range(parallel)]
            max_busy = [0]
            stop = threading.Event()

            def poll_slots():
                while not stop.is_set():
                    try:
                        busy = sum(1 for s in slot_states() if s.get("is_processing"))
                        max_busy[0] = max(max_busy[0], busy)
                    except (OSError, ValueError):
                        pass
                    time.sleep(0.01)

            poller = threading.Thread(target=poll_slots)
            poller.start()
            batch_start = time.monotonic()
            results = [wait(a, m) for a, m in jobs]
            batch_ms = (time.monotonic() - batch_start) * 1000
            stop.set()
            poller.join()
            assert all(r["status"] == "completed" for r in results), results
            audit = call("audit.list", {})
            denies = [r for r in audit if r["decision"] == "deny"]
            assert not denies, denies[:3]
            assert max_busy[0] >= 2, (
                f"backend never processed two slots at once (max_busy={max_busy[0]}); "
                f"batch_ms={batch_ms:.0f} single_ms={single_ms:.0f}")
            rates = [round(r["usage"].get("cached_tokens", 0) / r["usage"]["input_tokens"], 2)
                     if r["usage"]["input_tokens"] else 0.0 for r in results]
            line = (f"AOK_MVP_MULTISLOT=pass slots={slots} parallel_turns={parallel} "
                    f"max_busy_slots={max_busy[0]} batch_ms={batch_ms:.0f} "
                    f"single_ms={single_ms:.0f} rates={rates} audit={len(audit)}")
            print(line)
            log = os.environ.get("AOK_SMOKE_LOG")
            if log:
                os.makedirs(os.path.dirname(log), exist_ok=True)
                with open(log, "a") as handle:
                    handle.write(line + "\n")
            return 0
        except Exception:
            llama_log.seek(0)
            print("".join(llama_log.readlines()[-40:]))
            raise
        finally:
            if process is not None:
                process.terminate()
                process.wait(timeout=10)
            llama.terminate()
            llama.wait(timeout=10)


if __name__ == "__main__":
    raise SystemExit(main())
