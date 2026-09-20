#!/usr/bin/env python3
"""User-space research flow; shared by the driver and regression checks."""
import json
import os
from pathlib import Path
import socket
import time


class ControlClient:
    def __init__(self, path):
        self.path = path
        self.sequence = 0

    def call(self, method, params, timeout=60):
        self.sequence += 1
        with socket.socket(socket.AF_UNIX, socket.SOCK_STREAM) as client:
            client.settimeout(timeout)
            client.connect(self.path)
            with client.makefile("rwb", buffering=0) as wire:
                wire.write(json.dumps({"jsonrpc": "2.0", "id": self.sequence,
                                       "method": method, "params": params}).encode() + b"\n")
                result = json.loads(wire.readline())
                if "error" in result:
                    raise RuntimeError(f"{method}: {result['error']}")
                return result["result"]

    def wait_result(self, aid, mid, budget=180):
        deadline = time.monotonic() + budget
        while time.monotonic() < deadline:
            try:
                return self.call("message.result", {"application_id": aid, "message_id": mid})
            except RuntimeError as pending:
                if "-32004" not in str(pending):
                    raise
                time.sleep(0.05)
        raise AssertionError("turn did not finish")


def run_flow(client, jobs, question):
    if not 1 <= jobs <= 16:
        raise ValueError("researcher count must be between 1 and 16")
    call, wait_result = client.call, client.wait_result
    brief = "Research question: " + question
    planner = call("application.create", {"owner_agent": "planner", "wake_policy": "on_event"})
    pid = planner["application_id"]
    sent = call("message.send", {"application_id": pid, "idempotency_key": "plan",
                                "payload": {"text": brief + f"\nProduce {jobs} numbered research focus areas."}})
    plan = wait_result(pid, sent["message_id"])
    assert plan["status"] == "completed" and plan["text"].strip(), plan

    research_ids, messages = [], []
    for i in range(jobs):
        app = call("application.create", {"owner_agent": "research", "wake_policy": "on_event"})
        aid = app["application_id"]
        research_ids.append(aid)
        prompt = (brief + "\nPlanner output:\n" + plan["text"] +
                  f"\nYou are researcher {i + 1} of {jobs}. Investigate focus area {i + 1} "
                  "from this plan. Return findings and uncertainties; do not invent sources.")
        sent = call("message.send", {"application_id": aid, "idempotency_key": f"r{i}",
                                     "payload": {"text": prompt}})
        messages.append(sent["message_id"])
    results = [wait_result(aid, mid) for aid, mid in zip(research_ids, messages)]
    assert all(r["status"] == "completed" and r["text"].strip() for r in results), results

    aggregator = call("application.create", {"owner_agent": "aggregator", "wake_policy": "on_event"})
    agid = aggregator["application_id"]
    prompt = (brief + "\nSynthesize the following researcher findings into a report. "
              "Resolve disagreements and state limitations and unsupported claims.\n" +
              json.dumps([{"researcher": i + 1, "findings": r["text"]}
                          for i, r in enumerate(results)], ensure_ascii=False))
    sent = call("message.send", {"application_id": agid, "idempotency_key": "report",
                                "payload": {"text": prompt}})
    result = wait_result(agid, sent["message_id"])
    assert result["status"] == "completed" and result["text"].strip(), result
    assert result["checkpoint"], "no checkpoint committed"
    usage = [call("application.inspect", {"application_id": aid})["tokens_used"] for aid in research_ids]
    hit_rates = [round(r["usage"].get("cached_tokens", 0) / r["usage"]["input_tokens"], 2)
                 if r["usage"]["input_tokens"] else 0.0 for r in results]
    run_ids = {pid, agid, *research_ids}
    audit = [r for r in call("audit.list", {}) if r.get("application_id") in run_ids]
    denies = [r for r in audit if r["decision"] == "deny"]
    assert not denies, f"capability denials in this run: {denies[:3]}"
    return {"question": question, "planner": plan, "researchers": results,
            "report": result, "vtc_usage": usage, "hit_rates": hit_rates,
            "audit_records": len(audit), "denies": len(denies)}


def main():
    jobs = int(os.environ.get("AOK_MVP_RESEARCHERS", "3"))
    question = os.environ.get("AOK_MVP_QUESTION", "What limits current agent runtimes?")
    output = Path(os.environ["AOK_MVP_OUTPUT"]).resolve()
    output.mkdir(mode=0o700, parents=True, exist_ok=True)
    receipt = run_flow(ControlClient(os.environ["AOK_CTL_SOCKET"]), jobs, question)
    for name, content in (("report.txt", receipt["report"]["text"]),
                          ("receipt.json", json.dumps(receipt, ensure_ascii=False, indent=2) + "\n")):
        path = output / name
        with path.open("x", encoding="utf-8") as handle:
            handle.write(content)
            handle.flush()
            os.fsync(handle.fileno())
        assert path.read_text(encoding="utf-8") == content
    line = (f"AOK_MVP_RESEARCH=pass researchers={jobs} report={output / 'report.txt'} "
            f"receipt={output / 'receipt.json'} checkpoint={receipt['report']['checkpoint']} "
            f"vtc_usage={receipt['vtc_usage']} hit_rates={receipt['hit_rates']} "
            f"audit_records={receipt['audit_records']} denies={receipt['denies']}")
    print(line)
    log = os.environ.get("AOK_SMOKE_LOG")
    if log:
        Path(log).parent.mkdir(parents=True, exist_ok=True)
        with open(log, "a") as handle:
            handle.write(line + "\n")


if __name__ == "__main__":
    main()
