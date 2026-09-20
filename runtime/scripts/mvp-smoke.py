#!/usr/bin/env python3
"""Exercise the shared driver, retained report, restart, and socket reuse."""
import json
import os
from pathlib import Path
import subprocess
import tempfile
import time

from mvp_flow import ControlClient


def verify_report(output):
    receipt = json.loads((output / "receipt.json").read_text())
    assert (output / "report.txt").read_text() == receipt["report"]["text"]
    assert receipt["report"]["checkpoint"]
    assert all(value > 0 for value in receipt["vtc_usage"])
    return receipt


def main():
    root = Path(__file__).resolve().parents[1]
    scripts = root / "scripts"
    subprocess.run(["python3", str(scripts / "mvp_flow_test.py")], check=True)
    with tempfile.TemporaryDirectory(prefix="aok-mvp-", dir="/tmp") as directory:
        work = Path(directory)
        owned, reused = work / "owned", work / "reused"
        env = dict(os.environ)
        env.pop("AOK_CTL_SOCKET", None)
        # Run the actual shell entrypoint; its cleanup must keep these files.
        subprocess.run(["sh", str(scripts / "mvp-research.sh"), "-o", str(owned)], env=env, check=True)
        receipt = verify_report(owned)
        assert (owned / "state" / "state.db").is_file()

        binary = work / "supervisor"
        subprocess.run(["go", "build", "-o", str(binary), "./cmd/aok-supervisor"], cwd=root, check=True)
        with (work / "restart.log").open("w") as log:
            process = subprocess.Popen([str(binary), "-state", str(owned / "state"),
                                        "-manifest", str(owned / "manifest.yaml")], stdout=log, stderr=log)
            try:
                client = ControlClient(str(owned / "state" / "control.sock"))
                deadline = time.monotonic() + 15
                while time.monotonic() < deadline:
                    try:
                        if client.call("health", {})["status"] == "ok":
                            break
                    except (OSError, ValueError):
                        pass
                    time.sleep(0.05)
                else:
                    raise AssertionError("supervisor did not restart")
                result = receipt["report"]
                restored = client.call("message.result", {"application_id": result["application_id"],
                                                          "message_id": result["message_id"]})
                assert restored == result, "report or checkpoint lost after restart"
                env["AOK_CTL_SOCKET"] = client.path
                subprocess.run(["sh", str(scripts / "mvp-research.sh"), "-o", str(reused)], env=env, check=True)
                verify_report(reused)
                assert process.poll() is None, "reuse mode killed the external supervisor"
                assert client.call("health", {})["status"] == "ok"
            finally:
                process.terminate()
                process.wait(timeout=10)
        verify_report(owned)
        verify_report(reused)
        line = "AOK_MVP_SMOKE=pass planner_dataflow=pass retained_report=pass restart=pass reuse=pass"
        print(line)
        log = os.environ.get("AOK_SMOKE_LOG")
        if log:
            with open(log, "a") as handle:
                handle.write(line + "\n")
    return 0


if __name__ == "__main__":
    raise SystemExit(main())
