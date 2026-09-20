#!/usr/bin/env python3
"""P5 research flow against a control socket (shared by mvp-smoke and the
one-command mvp-research.sh driver)."""
import json
import os
import socket
import time


def main():
    path = os.environ["AOK_CTL_SOCKET"]
    jobs = int(os.environ.get("AOK_MVP_RESEARCHERS", "3"))
    question = os.environ.get("AOK_MVP_QUESTION",
                              "What limits current agent runtimes?")
    sequence = 0

    def call(method, params, timeout=60):
        nonlocal sequence
        sequence += 1
        with socket.socket(socket.AF_UNIX, socket.SOCK_STREAM) as client:
            client.settimeout(timeout)
            client.connect(path)
            with client.makefile("rwb", buffering=0) as wire:
                wire.write(json.dumps({"jsonrpc": "2.0", "id": sequence,
                                       "method": method, "params": params}).encode() + b"\n")
                result = json.loads(wire.readline())
                if "error" in result:
                    raise RuntimeError(f"{method}: {result['error']}")
                return result["result"]

    def wait_result(aid, mid, budget=60):
        deadline = time.monotonic() + budget
        while time.monotonic() < deadline:
            try:
                return call("message.result", {"application_id": aid, "message_id": mid})
            except RuntimeError as pending:
                if "-32004" not in str(pending):
                    raise
                time.sleep(0.05)
        raise AssertionError("turn did not finish")

    brief = question + " Shared research brief for all researchers. "

    planner = call("application.create", {"owner_agent": "planner", "wake_policy": "on_event"})
    pid = planner["application_id"]
    plan_msg = call("message.send", {"application_id": pid, "idempotency_key": "plan",
                                     "payload": {"text": brief + " Derive the researcher focus areas."}})
    plan = wait_result(pid, plan_msg["message_id"])
    assert plan["status"] == "completed", plan

    research_ids, r_mids = [], []
    for i in range(jobs):
        app = call("application.create", {"owner_agent": "research", "wake_policy": "on_event"})
        aid = app["application_id"]
        research_ids.append(aid)
        sent = call("message.send", {"application_id": aid, "idempotency_key": f"r{i}",
                                     "payload": {"text": brief + f" Researcher {i}: focus area {i}."}})
        r_mids.append(sent["message_id"])

    results = []
    for i, aid in enumerate(research_ids):
        result = wait_result(aid, r_mids[i])
        assert result["status"] == "completed", result
        results.append(result)

    aggregator = call("application.create", {"owner_agent": "aggregator", "wake_policy": "on_event"})
    agid = aggregator["application_id"]
    report_send = call("message.send", {"application_id": agid, "idempotency_key": "report",
                                        "payload": {"text": "AGGREGATED:" + "|".join(r["text"] for r in results)}})
    report_result = wait_result(agid, report_send["message_id"])
    assert report_result["status"] == "completed", report_result
    assert report_result["text"].startswith("AGGREGATED:"), report_result
    assert report_result["checkpoint"], "no checkpoint committed"

    inspected = [call("application.inspect", {"application_id": aid}) for aid in research_ids]
    usage = [a["tokens_used"] for a in inspected]
    hits = [r["usage"].get("cached_tokens", 0) for r in results]
    inputs = [r["usage"]["input_tokens"] for r in results]
    hit_report = [round(h / i, 2) if i else 0.0 for h, i in zip(hits, inputs)]

    audit = call("audit.list", {})
    denies = [r for r in audit if r["decision"] == "deny"]
    assert not denies, f"capability denials in MVP flow: {denies[:3]}"

    line = (f"AOK_MVP_RESEARCH=pass researchers={jobs} "
            f"report_checkpoint={report_result['checkpoint'][:16]}... "
            f"report_bytes={len(report_result['text'])} "
            f"vtc_usage={usage} hit_rates={hit_report} "
            f"audit_records={len(audit)} denies=0")
    print(line)
    log = os.environ.get("AOK_SMOKE_LOG")
    if log:
        os.makedirs(os.path.dirname(log), exist_ok=True)
        with open(log, "a") as handle:
            handle.write(line + "\n")


if __name__ == "__main__":
    main()
