#!/usr/bin/env python3
"""Real supervisor process, crash recovery, headless timer and LSFS wake probe."""
import argparse
import json
import os
from pathlib import Path
import socket
import subprocess
import tempfile
import time


def main():
    parser = argparse.ArgumentParser()
    parser.add_argument("--llama-endpoint")
    args = parser.parse_args()
    root = Path(__file__).resolve().parents[1]
    with tempfile.TemporaryDirectory(prefix="aok-core-", dir="/tmp") as directory:
        work = Path(directory)
        binary = work / "supervisor"
        subprocess.run(["go", "build", "-o", str(binary), "./cmd/aok-supervisor"], cwd=root, check=True)
        manifest = work / "manifest.yaml"
        manifest.write_text("engine: " + ("llama" if args.llama_endpoint else "echo") + "\ncapabilities:\n  net: false\n")
        state = work / "state"
        path = str(state / "control.sock")
        command = [str(binary), "-state", str(state), "-manifest", str(manifest), "-max-tokens", "32"]
        if args.llama_endpoint:
            command += ["-llama-endpoint", args.llama_endpoint]
        process = None
        sequence = 0

        def call(method, params):
            nonlocal sequence
            sequence += 1
            with socket.socket(socket.AF_UNIX, socket.SOCK_STREAM) as client:
                client.settimeout(10)
                client.connect(path)
                with client.makefile("rwb", buffering=0) as wire:
                    wire.write(json.dumps({"jsonrpc": "2.0", "id": sequence, "method": method, "params": params}).encode() + b"\n")
                    result = json.loads(wire.readline())
                    assert result["id"] == sequence and "error" not in result, result
                    return result["result"]

        def start():
            nonlocal process
            process = subprocess.Popen(command, stdout=subprocess.PIPE, stderr=subprocess.PIPE)
            deadline = time.monotonic() + 15
            while time.monotonic() < deadline:
                assert process.poll() is None, process.communicate()
                try:
                    if call("health", {})["status"] == "ok":
                        return
                except (OSError, ValueError):
                    pass
                time.sleep(0.02)
            raise AssertionError("supervisor did not become ready")

        try:
            start()
            app = call("application.create", {"owner_agent": "research", "wake_policy": "manual"})
            aid = app["application_id"]
            message = {"application_id": aid, "idempotency_key": "one", "payload": {"text": "Once upon a time there was a little girl named Lily."}}
            sent = call("message.send", message)
            assert call("message.send", message)["message_id"] == sent["message_id"]
            assert len(call("message.claim", {"application_id": aid})) == 1
            process.kill()
            process.communicate(timeout=10)
            start()
            recovered = call("application.inspect", {"application_id": aid})
            assert recovered["application_id"] == aid and recovered["context_id"] == app["context_id"]
            claimed = call("message.claim", {"application_id": aid})
            assert len(claimed) == 1 and claimed[0]["message_id"] == sent["message_id"]
            call("message.ack", {"application_id": aid, "message_id": sent["message_id"]})
            active = call("application.create", {"owner_agent": "headless", "wake_policy": "on_event"})
            call("event_source.create", {"application_id": active["application_id"], "timer_id": "wake", "delay_ms": 20, "payload": message["payload"]})
            # No client stays connected while the durable timer runs inference.
            deadline = time.monotonic() + 60
            while time.monotonic() < deadline:
                current = call("application.inspect", {"application_id": active["application_id"]})
                if current.get("checkpoint_ref"):
                    assert current["tokens_used"] > 0
                    break
                time.sleep(0.1)
            else:
                raise AssertionError("headless timer did not commit inference")
            watcher = call("application.create", {"owner_agent": "watcher", "wake_policy": "on_event"})
            wid = watcher["application_id"]
            source = {"application_id": wid, "source_kind": "lsfs", "binding_id": "artifacts"}
            call("event_source.create", source)
            call("event_source.disable", source)
            artifact = {"application_id": wid, "idempotency_key": "report", "payload": {"report": "ready"}}
            imported = call("artifact.import", artifact)
            process.kill()
            process.communicate(timeout=10)
            start()
            binding = call("event_source.list", source)[0]
            assert not binding["enabled"] and binding["cursor"] == 0, binding
            assert call("artifact.import", artifact) == imported
            call("event_source.bind", source)
            deadline = time.monotonic() + 60
            while time.monotonic() < deadline:
                mailbox = call("mailbox.list", {"application_id": wid})
                if len(mailbox) == 1 and mailbox[0]["status"] == "acked":
                    assert mailbox[0]["payload"]["commit"]["handle"] == imported["handle"]
                    result = call("message.result", {"application_id": wid, "message_id": mailbox[0]["message_id"]})
                    assert result["status"] == "completed" and result["checkpoint"]
                    break
                time.sleep(0.1)
            else:
                raise AssertionError("headless LSFS event did not commit inference")
            verdict = call("capability.check", {"application_id": aid, "object": "net", "action": "connect"})
            assert not verdict["allowed"]
            process.terminate()
            process.communicate(timeout=10)
            assert process.returncode == 0
            start()
            assert call("message.claim", {"application_id": aid}) in (None, [])
            restored = call("application.inspect", {"application_id": active["application_id"]})
            assert restored["checkpoint_ref"] == current["checkpoint_ref"]
            assert restored["tokens_used"] == current["tokens_used"]
            # Wait for the restored source cursor to pass the runner writebacks.
            artifact_cursor = mailbox[0]["payload"]["commit"]["cursor"]
            deadline = time.monotonic() + 10
            while call("event_source.list", source)[0]["cursor"] <= artifact_cursor:
                assert time.monotonic() < deadline, "LSFS cursor did not pass writebacks"
                time.sleep(0.02)
            recovered_mailbox = call("mailbox.list", {"application_id": wid})
            assert len(recovered_mailbox) == 1 and recovered_mailbox[0]["message_id"] == mailbox[0]["message_id"]
            print(json.dumps({"check": "AOK_CORE_SMOKE", "status": "pass", "provider": "llama.cpp" if args.llama_endpoint else "echo", "crash_replay": True, "headless_timer": True, "headless_lsfs": True, "lsfs_restart": True, "tokens_used": current["tokens_used"]}))
        finally:
            if process is not None and process.poll() is None:
                process.terminate()
                try:
                    process.communicate(timeout=10)
                except subprocess.TimeoutExpired:
                    process.kill()
                    process.communicate()


if __name__ == "__main__":
    main()
