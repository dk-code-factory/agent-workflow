"""Native report provenance at the publication and final-readiness boundaries."""
import copy
import hashlib
from pathlib import Path
import sys
import tempfile
import unittest
from unittest.mock import patch

sys.path.insert(0, str(Path(__file__).resolve().parents[1] / "workflows" / "helpers"))
from prfix import app, publishing
from prfix.core import Ledger, artifact, canonical, digest, identity
from prfix.prepare import CHECKLIST
from action import dispatch as application_dispatch
from prfix.publication import PublicationError, UncertainEffect
from test_publication import AUTH, BASE, HEAD, FakeGitHub

PRODUCER = "agent_" + "1" * 32
AUDIENCE = "agent_" + "2" * 32
VERIFIER = "agent_" + "3" * 32


class PublishingTests(unittest.TestCase):
    def setUp(self):
        self.temp = tempfile.TemporaryDirectory()
        self.addCleanup(self.temp.cleanup)
        self.root = Path(self.temp.name)
        (self.root / "source.txt").write_text("reviewed current source\n")
        self.pr = identity("https://github.com/o/r/pull/7")
        self.plan = {"logical_run": "fixture", "pr": self.pr, "instructions": "Exact original user instruction",
                     "options": {"permissions": {"reply_to_feedback": True, "edit_pr": True}},
                     "authorizations": {"iterate": AUTH, "merge": AUTH},
                     "tasks": [self.task("description", "description"), self.task("review", "review"), self.task("finding_verification", "verification")]}
        self.plan.update(definitions={"audience": "audience-definition"}, helper_manifest={},
                         limits={"retries_per_task": 1})
        self.ledger = Ledger(self.root, "fixture", self.pr["url"])
        self.addCleanup(self.ledger.close)
        self.provider = FakeGitHub()
        self.snapshot = {"pr": self.pr, "author_id": 11, "actor_id": 11,
            "head_repository": "github.com/o/r", "head_branch": "feature", "target_branch": "main",
            "target_tree": "target-tree", "base_tree": "base-tree", "local_tree": "reviewed-tree",
            "diff": "diff", "discussion": "discussion", "instructions": "instructions", "settings": "settings",
            "resource_digests": {}, "definition_digests": {}, "head": HEAD, "target": BASE,
            "local_head": HEAD, "dirty": False}
        self.snapshot_patch = patch.object(app, "_snapshot", side_effect=lambda *args: copy.deepcopy(self.snapshot))
        self.snapshot_patch.start()
        self.addCleanup(self.snapshot_patch.stop)
        self.covered = {"complete": True, "unresolved": [], "snapshot": self.snapshot}
        self.provider.observe_ci = lambda pr, head: {"head": head, "checks": [
            {"id": "check-1", "status": "completed", "conclusion": "success"}], "evidence": "fixture-ci"}

    def task(self, key, kind):
        return {"id": key, "kind": kind, "definition_digest": "definition-" + key,
                "resources": [], "checklist": CHECKLIST, "required": True, "writes": False}

    def report(self, **values):
        evidence = [{"id": "source", "path": "source.txt", "sha256": hashlib.sha256((self.root / "source.txt").read_bytes()).hexdigest()}]
        report = {"complete": True, "checklist": [{"id": item["id"], "status": "completed", "evidence": ["source"]} for item in CHECKLIST],
                  "findings": [], "evidence": evidence, "blockers": []}
        report.update(values)
        return report

    def accepted(self, key, report=None, agent=PRODUCER):
        task = next(t for t in self.plan["tasks"] if t["id"] == key)
        report = report or self.report()
        fingerprint = app._fingerprint(self.snapshot, task, self.ledger)
        claim = "claim_" + key
        reference = artifact(self.root, {"claim": claim, "agent": agent, "round": 0, "report": report})
        self.ledger.db.execute("INSERT INTO claims(id,task,fingerprint,state,agent,assignment,report,coverage) VALUES(?,?,?,'complete',?,?,?,?)",
            (claim, key, fingerprint, agent, "fixture-assignment", reference, fingerprint))
        self.ledger.db.execute("INSERT INTO accepted_reports VALUES(?,?,?,?,?,?)",
            (reference, key, task["kind"], fingerprint, agent, canonical(report).decode()))
        return reference

    def proposal(self):
        return {"kind": "reply", "head": HEAD, "target": BASE,
                "payload": {"body": "Fixed with source-backed evidence."}}

    def setup_publication(self):
        reference = self.accepted("description", self.report(operations=[self.proposal()]))
        listed = publishing.dispatch("publication_list", self.root, self.provider, self.ledger, self.plan, {})["items"]
        self.assertEqual(listed[0]["sources"], [reference])
        prepared = publishing.dispatch("publication_prepare", self.root, self.provider, self.ledger, self.plan, {"proposal": listed[0]})
        report = self.report(audience={"fingerprint": prepared["fingerprint"], "suitable": True,
                                      "audience": "PR participants", "evidence": ["source"]})
        return reference, listed[0], prepared, report

    def test_exact_original_report_proposal_and_independent_audience_dispatch(self):
        reference, proposal, prepared, report = self.setup_publication()
        result = publishing.dispatch("publication_execute", self.root, self.provider, self.ledger, self.plan,
                                     {"id": prepared["id"], "agent": AUDIENCE, "report": report})
        self.assertEqual(result["receipt"]["result"]["body"], proposal["payload"]["body"])
        self.assertEqual(len(self.provider.writes), 1)
        self.assertEqual(self.ledger.db.execute("SELECT count(*) FROM accepted_reports WHERE kind='audience'").fetchone()[0], 1)
        self.assertTrue(publishing.validate_operation(self.root, self.provider, self.ledger, self.plan,
            {"id": prepared["id"]}, result))

    def test_input_cannot_change_payload_while_reusing_accepted_source(self):
        _, proposal, _, _ = self.setup_publication()
        changed = copy.deepcopy(proposal)
        changed["payload"]["body"] = "Unreviewed publication body"
        with self.assertRaises(PublicationError):
            publishing.publication_prepare(self.root, self.provider, self.ledger, self.plan, {"proposal": changed})
        self.assertEqual(self.provider.writes, [])

    def test_changed_original_artifact_or_source_bytes_block_proposals(self):
        reference, proposal, _, _ = self.setup_publication()
        path = self.root / reference
        original = path.read_bytes()
        path.write_text("{}")
        with self.assertRaisesRegex(ValueError, "artifact identity"):
            publishing.publication_list(self.root, self.provider, self.ledger, self.plan, {})
        path.write_bytes(original)
        (self.root / "source.txt").write_text("changed code\n")
        with self.assertRaisesRegex(ValueError, "changed source"):
            publishing.publication_list(self.root, self.provider, self.ledger, self.plan, {})
        self.assertEqual(self.provider.writes, [])

    def test_changed_governing_inputs_make_original_report_stale(self):
        self.setup_publication()
        self.snapshot["instructions"] = "new user instruction fingerprint"
        self.assertEqual(publishing.publication_list(self.root, self.provider, self.ledger, self.plan, {})["items"], [])
        self.assertEqual(self.provider.writes, [])

    def test_replacement_plan_ignores_proposals_from_removed_tasks(self):
        removed = self.accepted("description", self.report(operations=[self.proposal()]))
        self.plan["tasks"] = [task for task in self.plan["tasks"] if task["id"] != "description"]
        current = self.proposal()
        current["payload"]["body"] = "Current selected task proposal."
        retained = self.accepted("review", self.report(operations=[current]))
        listed = publishing.publication_list(self.root, self.provider, self.ledger, self.plan, {})["items"]
        self.assertEqual(len(listed), 1)
        self.assertEqual(listed[0]["sources"], [retained])
        self.assertEqual(listed[0]["payload"], current["payload"])
        self.assertIsNotNone(self.ledger.db.execute(
            "SELECT reference FROM accepted_reports WHERE reference=?", (removed,)).fetchone())
        with self.assertRaisesRegex(PublicationError, "outside admitted plan"):
            publishing._source(self.root, self.provider, self.ledger, self.plan, removed)
        self.assertEqual(self.provider.writes, [])

    def test_audience_cannot_be_producer_or_replace_exact_retained_assessment(self):
        _, _, prepared, report = self.setup_publication()
        with self.assertRaisesRegex(PublicationError, "independent"):
            publishing.publication_execute(self.root, self.provider, self.ledger, self.plan,
                {"id": prepared["id"], "agent": PRODUCER, "report": report})
        self.provider.delay_creation = self.provider.lose_response = True
        with self.assertRaises(OSError):
            publishing.publication_execute(self.root, self.provider, self.ledger, self.plan,
                {"id": prepared["id"], "agent": AUDIENCE, "report": report})
        with self.assertRaisesRegex(PublicationError, "assessment changed"):
            publishing.publication_execute(self.root, self.provider, self.ledger, self.plan,
                {"id": prepared["id"], "agent": VERIFIER, "report": report})
        self.assertEqual(len(self.provider.writes), 1)

    def test_audience_retains_claim_retry_and_checklist_before_publication(self):
        _, _, prepared, report = self.setup_publication()
        def dispatch(operation, **inputs):
            return application_dispatch(operation, self.root, self.provider, self.ledger, self.plan,
                {"publication": prepared["id"], **inputs})
        claimed = dispatch("claim", task="publication_audience")
        self.assertEqual(claimed["task_input"]["payload"]["fingerprint"], prepared["fingerprint"])
        failed = {"claim": claimed["claim"], "agent": AUDIENCE, "status": "failed",
                  "failure_class": "usage_exhausted", "unsafe_failure_class": "", "error": "retained usage limit"}
        self.assertTrue(dispatch("turn_result", **failed)["retry"])
        self.assertTrue(dispatch("turn_result", **failed)["retry"])
        self.assertEqual(self.ledger.db.execute("SELECT SUM(amount) FROM reservations WHERE name=?",
            ("native-retry:" + claimed["claim"],)).fetchone()[0], 1)
        incomplete = copy.deepcopy(report)
        incomplete["complete"] = False
        incomplete["checklist"][0]["status"] = "incomplete"
        observed = dispatch("turn_result", claim=claimed["claim"], agent=AUDIENCE,
                            status="succeeded", report=incomplete, retried=True)
        self.assertTrue(observed["checklist"])
        check = dispatch("check_prepare", claim=claimed["claim"], agent=AUDIENCE, round=1)
        self.assertEqual(check["task_input"]["claim"], claimed["claim"])
        with self.assertRaisesRegex(ValueError, "original incomplete session"):
            dispatch("check_prepare", claim=claimed["claim"], agent=VERIFIER, round=1)
        with self.assertRaisesRegex(ValueError, "skip an original round"):
            dispatch("check_prepare", claim=claimed["claim"], agent=AUDIENCE, round=2)
        self.snapshot["settings"] = "changed"
        with self.assertRaisesRegex(PublicationError, "no longer covers the current inputs"):
            dispatch("check_prepare", claim=claimed["claim"], agent=AUDIENCE, round=1)
        self.snapshot["settings"] = "settings"
        self.assertTrue(dispatch("accept", claim=claimed["claim"], agent=AUDIENCE, report=report, round=1)["complete"])
        final = dispatch("task_status", claim=claimed["claim"])
        self.assertEqual((final["agent"], final["structured"]), (AUDIENCE, report))
        publishing.publication_execute(self.root, self.provider, self.ledger, self.plan,
            {"id": prepared["id"], "agent": final["agent"], "report": final["structured"]})
        self.assertEqual(len(self.provider.writes), 1)

    def test_audience_exhaustion_and_unsafe_failure_remain_blocking(self):
        _, _, prepared, report = self.setup_publication()
        def dispatch(operation, **inputs):
            return application_dispatch(operation, self.root, self.provider, self.ledger, self.plan,
                {"publication": prepared["id"], **inputs})
        claimed = dispatch("claim", task="publication_audience")
        with self.assertRaisesRegex(ValueError, "without eligible retained retry"):
            dispatch("turn_result", claim=claimed["claim"], agent=AUDIENCE, status="failed",
                failure_class="usage_exhausted", unsafe_failure_class="integrity", error="unsafe")
        incomplete = {**report, "complete": False}
        for number in range(4):
            if number:
                dispatch("check_prepare", claim=claimed["claim"], agent=AUDIENCE, round=number)
            dispatch("accept", claim=claimed["claim"], agent=AUDIENCE, report=incomplete, round=number)
        with self.assertRaisesRegex(ValueError, "allowance exceeded"):
            dispatch("check_prepare", claim=claimed["claim"], agent=AUDIENCE, round=4)
        with self.assertRaisesRegex(ValueError, "remains incomplete"):
            dispatch("task_status", claim=claimed["claim"])
        with self.assertRaisesRegex(ValueError, "unfinished logical claim"):
            dispatch("claim", task="publication_audience")
        self.assertEqual(self.provider.writes, [])

    def test_read_only_lost_response_reconciliation_cannot_commit_or_replay(self):
        _, _, prepared, report = self.setup_publication()
        self.provider.lose_response = True
        with self.assertRaises(OSError):
            publishing.publication_execute(self.root, self.provider, self.ledger, self.plan,
                {"id": prepared["id"], "agent": AUDIENCE, "report": report})
        readonly = Ledger(self.root, "fixture", self.pr["url"], read_only=True)
        self.addCleanup(readonly.close)
        before = readonly.db.total_changes
        observed = publishing.reconcile_operation(self.root, self.provider, readonly, self.plan, {"id": prepared["id"]})
        self.assertEqual(observed["outcome"], "completed")
        self.assertEqual(readonly.db.total_changes, before)
        self.assertEqual(len(self.provider.writes), 1)

    def test_explicit_record_commits_reconciled_receipt_without_another_remote_write(self):
        _, _, prepared, report = self.setup_publication()
        self.provider.lose_response = True
        with self.assertRaises(OSError):
            publishing.publication_execute(self.root, self.provider, self.ledger, self.plan,
                {"id": prepared["id"], "agent": AUDIENCE, "report": report})
        observed = publishing.reconcile_operation(self.root, self.provider, self.ledger, self.plan, {"id": prepared["id"]})
        self.assertEqual(observed["outcome"], "completed")
        self.assertEqual(self.ledger.db.execute("SELECT state FROM publication_intents").fetchone()[0], "uncertain")
        recorded = publishing.dispatch("publication_record", self.root, self.provider, self.ledger, self.plan,
                                      {"id": prepared["id"], "receipt": observed["result"]})
        self.assertEqual(self.ledger.db.execute("SELECT state FROM publication_intents").fetchone()[0], "completed")
        self.assertEqual(publishing.publication_record(self.root, self.provider, self.ledger, self.plan,
            {"id": prepared["id"], "receipt": observed["result"]}), recorded)
        self.assertEqual(len(self.provider.writes), 1)
        corrupt = copy.deepcopy(observed["result"])
        corrupt["actor_id"] = 999
        with self.assertRaisesRegex(PublicationError, "receipt identity"):
            publishing.publication_record(self.root, self.provider, self.ledger, self.plan,
                {"id": prepared["id"], "receipt": corrupt})

    def test_record_requires_retained_native_audience_and_exact_service_effect(self):
        _, _, prepared, report = self.setup_publication()
        result = publishing.publication_execute(self.root, self.provider, self.ledger, self.plan,
            {"id": prepared["id"], "agent": AUDIENCE, "report": report})
        row = self.ledger.db.execute("SELECT reference FROM accepted_reports WHERE kind='audience'").fetchone()
        audience_path = self.root / row[0]
        original = audience_path.read_bytes()
        audience_path.write_text("{}")
        self.assertFalse(publishing.validate_operation(self.root, self.provider, self.ledger, self.plan,
            {"id": prepared["id"]}, result))
        with self.assertRaises(ValueError):
            publishing.publication_record(self.root, self.provider, self.ledger, self.plan,
                {"id": prepared["id"], "receipt": result["receipt"]})
        audience_path.write_bytes(original)
        self.provider.ev["comments"].clear()
        with self.assertRaisesRegex(PublicationError, "exact current effect"):
            publishing.publication_record(self.root, self.provider, self.ledger, self.plan,
                {"id": prepared["id"], "receipt": result["receipt"]})
        self.assertEqual(len(self.provider.writes), 1)

    def test_merge_proposals_wait_for_selected_coverage_independent_review_and_fresh_ci(self):
        self.accepted("description")
        review = self.accepted("review")
        verified = self.accepted("finding_verification", agent=VERIFIER)
        with patch.object(app, "coverage", side_effect=lambda *args: copy.deepcopy(self.covered)):
            current = publishing.readiness(self.root, self.provider, self.ledger, self.plan)
            self.assertTrue(current["ready"], current)
            merge = publishing.merge_proposal(self.root, self.provider, self.ledger, self.plan, {})
            self.assertTrue(merge["invoke"])
            self.assertEqual(merge["proposal"]["sources"], [review, verified])
            self.covered["complete"] = False
            self.assertFalse(publishing.merge_proposal(self.root, self.provider, self.ledger, self.plan, {})["invoke"])
            self.covered["complete"] = True
            self.provider.observe_ci = lambda pr, head: {"head": "c" * 40, "checks": [], "evidence": "stale"}
            stale = publishing.readiness(self.root, self.provider, self.ledger, self.plan)
            self.assertFalse(stale["ready"])
            self.assertTrue(any("another head" in item for item in stale["blockers"]))

    def test_readiness_checks_graphql_requirements_without_publication(self):
        self.accepted("description")
        self.accepted("review")
        self.accepted("finding_verification", agent=VERIFIER)
        original = copy.deepcopy(self.provider.merge_state)
        with patch.object(app, "coverage", side_effect=lambda *args: copy.deepcopy(self.covered)):
            for fields in ({"mergeStateStatus": "BLOCKED"}, {"mergeable": "CONFLICTING"},
                           {"reviewDecision": "CHANGES_REQUESTED"}, {"headRefOid": "c" * 40}):
                with self.subTest(fields=fields):
                    self.provider.merge_state = {**original, **fields}
                    result = publishing.readiness(self.root, self.provider, self.ledger, self.plan)
                    self.assertFalse(result["ready"], result)
                    self.assertTrue(any("merge requirements" in item for item in result["blockers"]))
            self.provider.merge_state = {**original, "reviewDecision": None}
            self.assertTrue(publishing.readiness(self.root, self.provider, self.ledger, self.plan)["ready"])
            with patch.object(self.provider, "request", side_effect=OSError("requirements unavailable")):
                result = publishing.readiness(self.root, self.provider, self.ledger, self.plan)
                self.assertFalse(result["ready"])
                self.assertIn("requirements unavailable", result["blockers"])
        self.assertEqual(self.provider.writes, [])

    def test_controlled_commit_receipt_supplies_exact_push_before_other_proposals(self):
        self.plan["tasks"][0]["writes"] = True
        self.plan["options"]["permissions"]["push_commits"] = True
        desired = "c" * 40
        self.snapshot["local_head"] = desired
        self.snapshot["local_tree"] = "new-reviewed-tree"
        reference = self.accepted("description", self.report(operations=[self.proposal()]))
        self.ledger.db.execute("INSERT INTO local_commits VALUES(?,?,?,?,?,?)",
            ("claim_description", HEAD, desired, "new-reviewed-tree", "Fix accepted issue", "completed"))
        listed = publishing.publication_list(self.root, self.provider, self.ledger, self.plan, {})
        self.assertTrue(listed["unpublished"])
        self.assertEqual(len(listed["items"]), 1)
        push = listed["items"][0]
        self.assertEqual(push["kind"], "push")
        self.assertEqual(push["head"], desired)
        self.assertEqual(push["lease"], HEAD)
        self.assertEqual(push["payload"], {"new_head": desired})
        self.assertEqual(push["sources"], [reference])
        prepared = publishing.publication_prepare(self.root, self.provider, self.ledger, self.plan, {"proposal": push})
        self.assertEqual(prepared["intent"]["head"], desired)
        # A selected publication stage does not invent permission to push.
        self.plan["options"]["permissions"]["push_commits"] = False
        blocked = publishing.publication_list(self.root, self.provider, self.ledger, self.plan, {})
        self.assertEqual(blocked["items"], [])
        self.assertTrue(blocked["unpublished"])
        self.assertEqual(self.provider.writes, [])

    def test_empty_selected_plan_never_implies_merge_readiness(self):
        self.plan["tasks"] = []
        with patch.object(app, "coverage", return_value=copy.deepcopy(self.covered)):
            current = publishing.readiness(self.root, self.provider, self.ledger, self.plan)
            self.assertFalse(current["ready"])
            self.assertFalse(current["clean_review"])
            self.assertFalse(current["verified"])
        self.plan["authorizations"].pop("merge")
        self.assertEqual(publishing.merge_proposal(self.root, self.provider, self.ledger, self.plan, {}), {"invoke": False, "proposal": {}})

    def test_audience_finding_or_wrong_payload_fingerprint_blocks_before_write(self):
        _, _, prepared, report = self.setup_publication()
        report["audience"]["fingerprint"] = "different"
        with self.assertRaisesRegex(PublicationError, "exact publication"):
            publishing.publication_execute(self.root, self.provider, self.ledger, self.plan,
                {"id": prepared["id"], "agent": AUDIENCE, "report": report})
        report["audience"]["fingerprint"] = prepared["fingerprint"]
        report["findings"] = [{"id": "bad", "text": "Public body contains internal details", "sources": ["source"]}]
        with self.assertRaisesRegex(PublicationError, "unresolved payload"):
            publishing.publication_execute(self.root, self.provider, self.ledger, self.plan,
                {"id": prepared["id"], "agent": AUDIENCE, "report": report})
        self.assertEqual(self.provider.writes, [])


if __name__ == "__main__":
    unittest.main()
