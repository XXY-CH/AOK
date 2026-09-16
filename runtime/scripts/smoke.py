#!/usr/bin/env python3
"""Development supervisor/client probe; builds and runs only the offline engine."""
import json
import os
from pathlib import Path
import signal
import socket
import subprocess
import tempfile


def main():
    root = Path(__file__).resolve().parents[1]
    # Keep the Unix path short (macOS sun_path is limited). Directory mode is 0700.
    with tempfile.TemporaryDirectory(prefix="aok-", dir="/tmp") as directory:
        binary = str(Path(directory) / "aok-engine-echo")
        subprocess.run(["go", "build", "-o", binary, "./cmd/aok-engine-echo"], cwd=root, check=True)
        address = str(Path(directory) / "echo.sock")
        with socket.socket(socket.AF_UNIX, socket.SOCK_STREAM) as listener:
            listener.bind(address)
            os.chmod(address, 0o600)
            listener.listen(8)
            process = subprocess.Popen(
                [binary, "-listener-fd", str(listener.fileno())],
                pass_fds=(listener.fileno(),), stdout=subprocess.PIPE, stderr=subprocess.PIPE,
            )
            try:
                with socket.socket(socket.AF_UNIX, socket.SOCK_STREAM) as client:
                    client.settimeout(5)
                    client.connect(address)
                    with client.makefile("rwb", buffering=0) as wire:
                        def send(identifier, method, params):
                            request = {"jsonrpc": "2.0", "id": identifier, "method": method, "params": params}
                            wire.write(json.dumps(request).encode() + b"\n")

                        def read():
                            message = json.loads(wire.readline())
                            assert message["jsonrpc"] == "2.0", message
                            assert "error" not in message, message
                            return message

                        send(1, "initialize", {})
                        handshake = read()
                        assert handshake["result"]["address"] == "unix://" + address, handshake
                        assert handshake["result"]["engine"] == "echo", handshake
                        send(2, "session/new", {})
                        session = read()["result"]["session_id"]
                        for turn in (1, 2):
                            send(10 + turn, "session/prompt", {"session_id": session, "text": "hello"})
                            for offset, method in enumerate(("session/started", "session/chunk", "session/usage", "session/done"), 1):
                                event = read()
                                assert event["method"] == method, event
                                assert event["params"]["session_id"] == session, event
                                assert event["params"]["turn_id"] == turn, event
                                assert event["params"]["event_seq"] == (turn - 1) * 4 + offset, event
                            response = read()
                            assert response["id"] == 10 + turn, response
                            assert response["result"]["stop_reason"] == "completed", response
                        send(20, "session/close", {"session_id": session})
                        assert read()["id"] == 20
                        send(21, "health", {})
                        assert read()["result"]["status"] == "ok"
                process.send_signal(signal.SIGTERM)
                stdout, stderr = process.communicate(timeout=5)
                assert process.returncode == 0, stderr.decode()
                assert not stdout, "engine wrote protocol data to stdout"
            finally:
                if process.poll() is None:
                    process.kill()
                    process.communicate()
        print("AOK_ENGINE_SMOKE=pass (inherited Unix listener, handshake, two turns, close, SIGTERM)")


if __name__ == "__main__":
    main()
