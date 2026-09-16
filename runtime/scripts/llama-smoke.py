#!/usr/bin/env python3
"""Exercise the real llama engine against an explicitly supplied llama.cpp server."""
import argparse
import json
import os
from pathlib import Path
import signal
import socket
import subprocess
import tempfile
import time


def main():
    parser = argparse.ArgumentParser()
    parser.add_argument("--endpoint", default="http://127.0.0.1:18081")
    parser.add_argument("--require-cache", action="store_true")
    args = parser.parse_args()
    root = Path(__file__).resolve().parents[1]
    prompt = "Once upon a time there was a little girl named Lily. She loved to play outside in the garden. One day she found"
    with tempfile.TemporaryDirectory(prefix="aok-llama-", dir="/tmp") as directory:
        binary = str(Path(directory) / "engine")
        subprocess.run(["go", "build", "-o", binary, "./cmd/aok-engine-llama"], cwd=root, check=True)
        address = str(Path(directory) / "engine.sock")
        with socket.socket(socket.AF_UNIX, socket.SOCK_STREAM) as listener:
            listener.bind(address)
            os.chmod(address, 0o600)
            listener.listen(8)
            process = subprocess.Popen(
                [binary, "-listener-fd", str(listener.fileno()), "-endpoint", args.endpoint,
                 "-max-tokens", "32", "-cache-prompt"],
                pass_fds=(listener.fileno(),), stdout=subprocess.PIPE, stderr=subprocess.PIPE,
            )
            try:
                turns = []
                with socket.socket(socket.AF_UNIX, socket.SOCK_STREAM) as client:
                    client.settimeout(120)
                    client.connect(address)
                    with client.makefile("rwb", buffering=0) as wire:
                        def send(identifier, method, params):
                            wire.write(json.dumps({"jsonrpc": "2.0", "id": identifier,
                                                   "method": method, "params": params}).encode() + b"\n")

                        def read():
                            message = json.loads(wire.readline())
                            assert message["jsonrpc"] == "2.0" and "error" not in message, message
                            return message

                        send(1, "initialize", {})
                        assert read()["result"]["engine"] == "llama.cpp"
                        send(2, "session/new", {})
                        session = read()["result"]["session_id"]
                        for turn in range(2):
                            send(3 + turn, "session/prompt", {"session_id": session, "text": prompt})
                            events = []
                            while True:
                                event = read()
                                if "id" in event:
                                    assert event["result"]["stop_reason"] == "completed", event
                                    break
                                events.append(event)
                            assert [event["method"] for event in events] == [
                                "session/started", "session/chunk", "session/usage", "session/done"], events
                            text = events[1]["params"]["text"]
                            usage = events[2]["params"]["usage"]
                            assert text and text != prompt, text
                            assert usage["input_tokens"] > 0 and 0 < usage["output_tokens"] <= 32, usage
                            turns.append({"text": text, "usage": usage})
                        if args.require_cache:
                            assert turns[1]["usage"]["cached_tokens"] > 0, turns
                        send(5, "session/prompt", {"session_id": session, "text": prompt})
                        assert read()["method"] == "session/started"
                        # Let the real HTTP inference start before aborting the turn.
                        time.sleep(0.01)
                        send(6, "session/abort", {"session_id": session})
                        responses = {}
                        while len(responses) < 2:
                            message = read()
                            if "id" in message:
                                responses[message["id"]] = message["result"]["stop_reason"]
                            else:
                                assert message["method"] == "session/done", message
                                assert message["params"]["stop_reason"] == "cancelled", message
                        assert responses == {5: "cancelled", 6: "cancelled"}, responses
                        send(7, "session/close", {"session_id": session})
                        assert read()["id"] == 7
                process.send_signal(signal.SIGTERM)
                stdout, stderr = process.communicate(timeout=10)
                assert process.returncode == 0 and not stdout, stderr.decode()
                print(json.dumps({"check": "AOK_LLAMA_REAL_SMOKE", "status": "pass",
                                  "provider": "llama.cpp", "turns": turns,
                                  "abort": "cancelled"}, indent=2))
            finally:
                if process.poll() is None:
                    process.kill()
                    process.communicate()


if __name__ == "__main__":
    main()
