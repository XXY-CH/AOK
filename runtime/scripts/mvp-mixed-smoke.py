#!/usr/bin/env python3
"""P5 single llama backend smoke (legacy target name): researchers run against llama.cpp, the
second and later turns reuse the shared brief prefix (nonzero cache hit
rates), and the audit chain stays clean. Skips without llama/model."""
import json
import os
from pathlib import Path
import shutil
import socket
import subprocess
import tempfile
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
        # This fixture trains at 128 tokens. Expand its test-only context
        # for the full dataflow; this does not establish report quality.
        fixture_args = ["--override-kv", "llama.context_length=int:2048"]
    if not server or not model:
        print(f"AOK_MVP_MIXED=skip server={server} model={model}")
        return 0
    port = int(os.environ.get("AOK_LLAMA_PORT", "8152"))
    root = Path(__file__).resolve().parents[1]
    with tempfile.TemporaryDirectory(prefix="aok-mvp-mix-", dir="/tmp") as directory:
        work = Path(directory)
        llama_log = (work / "llama.log").open("w+")
        llama = subprocess.Popen([server, "-m", model, "--port", str(port),
                                  "--host", "127.0.0.1", "--ctx-size", "2048",
                                  "-np", "1", "-ngl", "0"] + fixture_args,
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
            subprocess.run(["go", "build", "-o", str(work / "supervisor"),
                           "./cmd/aok-supervisor"], cwd=root, check=True)
            (work / "manifest.yaml").write_text("engine: llama\ncapabilities:\n  net: false\n")
            process = subprocess.Popen([str(work / "supervisor"), "-state", str(work / "state"),
                                       "-manifest", str(work / "manifest.yaml"),
                                       "-llama-endpoint", f"http://127.0.0.1:{port}",
                                       "-max-tokens", "24"],
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

            def wait(aid, mid, budget=90):
                deadline = time.monotonic() + budget
                while time.monotonic() < deadline:
                    try:
                        return call("message.result", {"application_id": aid, "message_id": mid})
                    except RuntimeError as pending:
                        if "-32004" not in str(pending):
                            raise
                        time.sleep(0.05)
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
            ids, mids = [], []
            for i in range(3):
                app = call("application.create", {"owner_agent": "research",
                                                 "wake_policy": "on_event"})
                sent = call("message.send", {"application_id": app["application_id"],
                                             "idempotency_key": f"r{i}",
                                             "payload": {"text": brief + f" Focus {i}."}})
                ids.append(app["application_id"])
                mids.append(sent["message_id"])
            results = [wait(aid, mid) for aid, mid in zip(ids, mids)]
            assert all(r["status"] == "completed" for r in results), results
            hits = [r["usage"].get("cached_tokens", 0) for r in results]
            inputs = [r["usage"]["input_tokens"] for r in results]
            rates = [round(h / i, 2) if i else 0 for h, i in zip(hits, inputs)]
            nonzero = [r for r in rates if r > 0]
            assert nonzero, f"no prefix reuse across researchers: {rates}"
            audit = call("audit.list", {})
            denies = [r for r in audit if r["decision"] == "deny"]
            assert not denies, denies[:3]
            # Also run the actual planner/researcher/aggregator driver with
            # non-echo output. This small fixture model is not a quality benchmark.
            output = work / "report"
            env = dict(os.environ, AOK_CTL_SOCKET=path)
            subprocess.run(["sh", str(root / "scripts" / "mvp-research.sh"),
                            "-o", str(output)], env=env, check=True, timeout=180)
            receipt = json.loads((output / "receipt.json").read_text())
            assert receipt["report"]["provider"] == "llama.cpp", receipt["report"]
            assert (output / "report.txt").read_text() == receipt["report"]["text"]
            line = (f"AOK_MVP_MIXED=pass scope=single_backend full_flow=pass backend=llama.cpp rates={rates} "
                    f"nonzero={len(nonzero)}/{len(rates)} audit={len(audit)}")
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
                process.kill()
                process.communicate(timeout=10)
            llama.terminate()
            llama.wait(timeout=10)
            llama_log.close()


if __name__ == "__main__":
    raise SystemExit(main())
