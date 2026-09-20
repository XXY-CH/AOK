#!/usr/bin/env python3
"""Guard planner propagation and acceptance of non-echo model output."""
import unittest

from mvp_flow import run_flow


class FakeControl:
    def __init__(self):
        self.apps = {}
        self.prompts = {}

    def call(self, method, params):
        if method in ("application.create", "application.fork"):
            aid = f"app-{len(self.apps)}"
            self.apps[aid] = params["owner_agent"]
            return {"application_id": aid}
        if method == "context.pages":
            return {"application_id": params["application_id"],
                    "pages": ["page-plan"], "tail": ""}
        if method == "message.send":
            aid = params["application_id"]
            self.prompts[aid] = params["payload"]["text"]
            return {"message_id": aid + "-msg"}
        if method == "application.inspect":
            return {"tokens_used": 10}
        if method == "audit.list":
            return [{"application_id": "unrelated", "decision": "deny"}]
        raise AssertionError(method)

    def wait_result(self, aid, mid):
        role = self.apps[aid]
        text = {"planner": "1. Investigate scheduling\n2. Investigate isolation",
                "research": "Generated finding for " + aid,
                "aggregator": "A synthesized report without a magic prefix."}[role]
        return {"application_id": aid, "message_id": mid, "status": "completed", "text": text,
                "checkpoint": "checkpoint-" + aid, "usage": {"input_tokens": 10}}


class FlowTests(unittest.TestCase):
    def test_planner_output_and_findings_reach_downstream(self):
        client = FakeControl()
        receipt = run_flow(client, 2, "Test question")
        plan = receipt["planner"]["text"]
        for i, result in enumerate(receipt["researchers"]):
            prompt = client.prompts[result["application_id"]]
            self.assertIn(plan, prompt)
            self.assertIn(f"focus area {i + 1}", prompt)
        prompt = client.prompts[receipt["report"]["application_id"]]
        for result in receipt["researchers"]:
            self.assertIn(result["text"], prompt)
        self.assertEqual(receipt["report"]["text"], "A synthesized report without a magic prefix.")
        self.assertEqual(receipt["denies"], 0)

    def test_invalid_fanout_has_no_side_effects(self):
        for count in (0, -1, 17):
            client = FakeControl()
            with self.assertRaises(ValueError):
                run_flow(client, count, "Test")
            self.assertFalse(client.apps)


if __name__ == "__main__":
    unittest.main()
