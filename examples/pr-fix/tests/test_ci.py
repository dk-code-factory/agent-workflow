import copy
import json
from pathlib import Path
import sys
import tempfile
import unittest

sys.path.insert(0, str(Path(__file__).resolve().parents[1] / "workflows" / "helpers"))
from prfix import ci
from prfix.capture import identity
from prfix.core import Ledger


PR = identity("https://github.com/owner/repo/pull/17")
HEAD = "a" * 40


def observation(conclusion="failure"):
    return {"head": HEAD, "evidence": "original-evidence", "checks": [
        {"id": "9", "name": "Test", "status": "completed", "conclusion": conclusion,
         "run_id": "2", "job_id": "3", "attempt": 4}]}


def candidate():
    return {"check_id": "9", "run_id": "2", "job_id": "3", "attempt": 4,
            "kind": "flake", "workflow": ".github/workflows/test.yml", "job": "Test",
            "test": "TestFlaky", "signature": "intermittent gateway timeout",
            "issue": "https://github.com/owner/repo/issues/8"}


class Provider:
    def __init__(self):
        self.head = HEAD
        self.run = {"id": 2, "head_sha": HEAD, "run_attempt": 4, "path": ".github/workflows/test.yml@refs/heads/fix"}
        self.job = {"id": 3, "head_sha": HEAD, "run_id": 2, "run_attempt": 4, "name": "Test",
                    "status": "completed", "conclusion": "failure",
                    "check_run_url": "https://api.github.com/repos/owner/repo/check-runs/9",
                    "steps": [{"name": "Tests", "number": 2, "status": "completed", "conclusion": "failure"}]}
        declared = {k: candidate()[k] for k in ("workflow", "job", "test", "signature")}
        self.issue = {"state": "open", "html_url": "https://github.com/owner/repo/issues/8",
                      "title": "Known flake", "body": "```dk-flaky\n" + json.dumps(dict(declared, flaky=True)) + "\n```"}
        self.logs = "\n".join("Test\tTests\t" + line for line in (
            "=== RUN   TestFlaky", "    client.go:17: intermittent gateway timeout",
            "--- FAIL: TestFlaky (0.10s)", "FAIL", "exit status 1"))
        self.observations = 0

    def request(self, method, route, payload=None):
        if route.endswith("/actions/runs/2"):
            return copy.deepcopy(self.run)
        if route.endswith("/actions/jobs/3"):
            return copy.deepcopy(self.job)
        if route.endswith("/issues/8"):
            return copy.deepcopy(self.issue)
        raise AssertionError("unexpected route " + route)

    def failure_logs(self, pr, run, job, attempt):
        if (pr, run, job, attempt) != (PR, "2", "3", 4):
            raise AssertionError("wrong log identity")
        return self.logs

    def capture(self, pr):
        return {"metadata": {"head": {"sha": self.head}}}

    def observe_ci(self, pr, head):
        self.observations += 1
        result = observation("success")
        result["head"] = self.head
        return result


class CITests(unittest.TestCase):
    def test_configuration_validates_declared_rule_fields_without_live_evidence(self):
        flake = candidate()
        rule = {"id": "saved", "kind": "pipeline", "workflow": flake["workflow"],
                "job": flake["job"], "reason": "Explicit policy"}
        exemption = {key: value for key, value in dict(flake, kind="exception", rule="saved").items()
                     if key not in {"issue", "test", "signature"}}
        selected = {"classifications": [flake, exemption], "exceptions": {"entries": [rule]}}
        self.assertEqual(ci.configuration({"ci": selected})["classifications"], [flake, exemption])
        self.assertEqual(ci.configuration({"ci_exceptions": [dict(rule, kind="test", test="TestFlaky")]})["exceptions"][0]["test"], "TestFlaky")
        invalid = [{"classifications": [dict(flake, attempt=True)]},
                   {"classifications": [dict(flake, run_id=None)]},
                   {"classifications": [dict(flake, test="")]},
                   {"classifications": [dict(flake, unsupported=True)]},
                   {"classifications": [dict(flake, kind="unknown")]},
                   {"exceptions": [dict(rule, kind="test")]},
                   {"exceptions": [dict(rule, reason="")]},
                   {"exceptions": [rule, rule]},
                   {"exceptions": {"entries": [], "unsupported": True}}]
        for controls in invalid:
            for options in ({"ci": controls}, {"ci_" + key: value for key, value in controls.items()}):
                with self.subTest(options=options), self.assertRaisesRegex(ValueError, "CI"):
                    ci.configuration(options)

    def test_empty_pending_failed_and_success_are_distinct(self):
        self.assertEqual(ci.classify({"head": HEAD, "checks": []})["status"], "pending")
        self.assertEqual(ci.classify(observation())["status"], "failed")
        self.assertEqual(ci.classify(observation("success"))["status"], "success")
        pending = observation("success")
        pending["checks"][0]["status"] = "in_progress"
        self.assertEqual(ci.classify(pending)["status"], "pending")

    def test_open_issue_and_exact_logs_can_exempt_only_the_bound_attempt(self):
        provider = Provider()
        proofs = ci.verify_exceptions(provider, PR, observation(), [candidate()])
        self.assertEqual(ci.classify(observation(), proofs)["status"], "success")
        for key, value in (("head", "b" * 40), ("attempt", 5), ("job_id", "4")):
            changed = copy.deepcopy(proofs)
            changed[0][key] = value
            self.assertEqual(ci.classify(observation(), changed)["status"], "failed")
        provider.issue["state"] = "closed"
        with self.assertRaisesRegex(ValueError, "does not affirm"):
            ci.verify_exceptions(provider, PR, observation(), [candidate()])

    def test_unknown_diagnostic_or_another_failed_step_blocks_test_exemption(self):
        for extra in ("Test\tTests\tcompiler failure outside named test", "Test\tOther\tunknown"):
            provider = Provider()
            provider.logs += "\n" + extra
            with self.subTest(extra=extra), self.assertRaises(ValueError):
                ci.verify_exceptions(provider, PR, observation(), [candidate()])
        provider = Provider()
        provider.job["steps"].append({"name": "Cleanup", "number": 3, "status": "completed", "conclusion": "failure"})
        with self.assertRaisesRegex(ValueError, "incomplete"):
            ci.verify_exceptions(provider, PR, observation(), [candidate()])

    def test_changed_run_or_job_cannot_establish_exception(self):
        for object_name, key, value in (("run", "head_sha", "b" * 40), ("run", "run_attempt", 5),
                                        ("job", "check_run_url", "https://api.github.com/check-runs/other"),
                                        ("job", "head_sha", "b" * 40), ("job", "id", 4)):
            provider = Provider()
            getattr(provider, object_name)[key] = value
            with self.subTest(object=object_name, key=key), self.assertRaisesRegex(ValueError, "exact failed"):
                ci.verify_exceptions(provider, PR, observation(), [candidate()])

    def test_pipeline_exception_requires_original_exact_scoped_rule(self):
        provider = Provider()
        selected = dict(candidate(), kind="exception", rule="ci-saved")
        rule = {"id": "ci-saved", "kind": "pipeline", "workflow": selected["workflow"],
                "job": "Test", "reason": "Explicit operator decision"}
        provider.logs = "Test\tTests\tunsupported pipeline failure"
        proofs = ci.verify_exceptions(provider, PR, observation(), [selected], [rule])
        self.assertEqual(ci.classify(observation(), proofs)["status"], "success")
        for rules in ([], [dict(rule, job="Other")], [dict(rule, workflow=".github/workflows/other.yml")]):
            with self.subTest(rules=rules), self.assertRaisesRegex(ValueError, "scoped settings"):
                ci.verify_exceptions(provider, PR, observation(), [selected], rules)

    def test_ambiguous_or_negated_flake_prose_does_not_exempt(self):
        provider = Provider()
        declaration = candidate()
        linked = " ".join("`" + declaration[k] + "`" for k in ("workflow", "job", "test", "signature"))
        provider.issue["body"] = "Maybe this test is flaky? " + linked
        with self.assertRaises(ValueError):
            ci.verify_exceptions(provider, PR, observation(), [declaration])
        provider.issue["body"] = "Confirmed flaky test. " + linked
        self.assertTrue(ci.verify_exceptions(provider, PR, observation(), [declaration]))

    def test_deadline_survives_restart_head_change_and_changed_timeout(self):
        with tempfile.TemporaryDirectory(prefix="portable-pr-ci-") as temporary:
            root = Path(temporary)
            plan = {"pr": PR, "logical_run": "retained", "options": {"ci": {"timeout_seconds": 120}}}
            ledger = Ledger(root, "retained", PR["url"])
            provider = Provider()
            try:
                first = ci.observe(provider, plan, ledger, "first")
                self.assertTrue(first["ready"])
                self.assertEqual(provider.observations, 1)
            finally:
                ledger.close()
            ledger = Ledger(root, "retained", PR["url"])
            try:
                plan["options"]["ci"]["timeout_seconds"] = 3600
                provider.head = "b" * 40
                second = ci.observe(provider, plan, ledger, "second")
                self.assertEqual(second["deadline"], first["deadline"])
                self.assertEqual(second["head"], provider.head)
                ledger.db.execute("UPDATE deadlines SET value=0")
                with self.assertRaisesRegex(TimeoutError, "exhausted"):
                    ci.observe(provider, plan, ledger, "third")
                self.assertEqual(provider.observations, 2)
            finally:
                ledger.close()

    def test_repair_and_rerun_reservations_replay_without_renewing_allowance(self):
        with tempfile.TemporaryDirectory(prefix="portable-pr-ci-") as temporary:
            ledger = Ledger(Path(temporary), "retained", PR["url"])
            try:
                self.assertEqual(ci.reserve_repair(ledger, "cycle1", "op1", 1), 1)
                self.assertEqual(ci.reserve_repair(ledger, "cycle1", "op1", 1), 1)
                with self.assertRaisesRegex(ValueError, "exhausted"):
                    ci.reserve_repair(ledger, "cycle1", "op2", 1)
                self.assertEqual(ci.reserve_rerun(ledger, HEAD, "rerun1", 1), 1)
                with self.assertRaisesRegex(ValueError, "exhausted"):
                    ci.reserve_rerun(ledger, HEAD, "rerun2", 1)
            finally:
                ledger.close()


if __name__ == "__main__":
    unittest.main()
