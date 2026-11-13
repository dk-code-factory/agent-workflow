"""Container protocol recovery and read-only action-hook boundaries."""
import copy
from pathlib import Path
import sys
import tempfile
import unittest
from unittest.mock import patch
import git_fixture

sys.path.insert(0, str(Path(__file__).resolve().parents[1] / "workflows" / "helpers"))
import action
from prfix import app, publishing
from prfix.core import Ledger, artifact, digest, read_artifact
from prfix.prepare import prepare
from test_app import AppFixture, REVIEWER, VERIFIER
from test_publication import AUTH


class ActionTests(unittest.TestCase):
    def setUp(self):
        git_fixture.isolate(self)
        self.temp = tempfile.TemporaryDirectory(prefix="portable-pr-action-")
        self.addCleanup(self.temp.cleanup)
        self.root = Path(self.temp.name)
        self.fixture = AppFixture(self.root, review=False)
        self.plan, self.provider = self.fixture.plan, self.fixture.provider
        self.prepared = self.fixture.prepared

    def request(self, phase, inputs=None, operation="fixture-operation", checkpoint=None):
        result = {"protocol": "action-v1", "phase": phase,
                  "input": {"plan": self.prepared["plan"], **(inputs or {})},
                  "context": {"operation_key": operation}}
        if checkpoint is not None:
            result["checkpoint"] = {"result": checkpoint}
        return result

    def handle(self, operation, phase, inputs=None, key="fixture-operation", checkpoint=None):
        return action.handle(operation, self.request(phase, inputs, key, checkpoint),
                             self.root, self.provider)

    def ledger(self, read_only=False):
        return Ledger(self.root, self.plan["logical_run"], self.plan["pr"]["url"], read_only=read_only)

    def state(self):
        with self.ledger(read_only=True) as ledger:
            records = {name: [tuple(row) for row in ledger.db.execute("SELECT * FROM " + name + " ORDER BY rowid")]
                       for name in ("operations", "claims", "submissions", "accepted_reports", "publication_intents", "reservations")}
        artifacts = sorted(path.name for path in (self.root / ".pr-fix-state" / "artifacts").glob("*.json"))
        return records, artifacts

    def test_bindings_and_completed_checkpoint_validation_are_read_only(self):
        before = self.state()
        bindings = self.handle("coverage", "bindings")
        self.assertEqual(bindings["read_only_phases"], ["bindings", "validate_checkpoint", "reconcile"])
        self.assertEqual(bindings["bindings"]["plan"], self.plan["fingerprint"])
        self.assertEqual(before, self.state())
        executed = self.handle("coverage", "execute")
        self.assertTrue(executed["result"]["output"]["complete"])
        before = self.state()
        self.assertTrue(self.handle("coverage", "validate_checkpoint", checkpoint=executed["result"])["valid"])
        self.assertEqual(self.handle("coverage", "reconcile")["outcome"], "completed")
        self.assertEqual(before, self.state())

    def test_lost_acknowledgement_returns_original_result_without_another_dispatch(self):
        original = action.dispatch
        with patch.object(action, "dispatch", wraps=original) as dispatched:
            executed = self.handle("coverage", "execute")
            recovered = self.handle("coverage", "reconcile")
            self.assertEqual(recovered["outcome"], "completed")
            self.assertEqual(recovered["result"], executed["result"])
            replay = self.handle("coverage", "execute")
            self.assertEqual(replay["result"], executed["result"])
            self.assertEqual(dispatched.call_count, 1)

    def test_uncommitted_local_action_effect_stays_unknown_and_cannot_reexecute(self):
        real_artifact = action.artifact
        def lost_result(root, value):
            if value.get("operation"):
                raise OSError("fixture result commit failed")
            return real_artifact(root, value)
        def effect(operation, root, provider, ledger, plan, inputs):
            ledger.reserve("fixture-effects", "one-effect", 1)
            return {"done": True}
        with patch.object(action, "dispatch", side_effect=effect), patch.object(action, "artifact", side_effect=lost_result):
            with self.assertRaisesRegex(OSError, "result commit failed"):
                self.handle("coverage", "execute")
        before = self.state()
        self.assertEqual(self.handle("coverage", "reconcile")["outcome"], "unknown")
        self.assertEqual(before, self.state())
        with self.assertRaisesRegex(ValueError, "remains unresolved"):
            self.handle("coverage", "execute")
        with self.ledger(read_only=True) as ledger:
            self.assertEqual(ledger.db.execute("SELECT sum(amount) FROM reservations WHERE name='fixture-effects'").fetchone()[0], 1)

    def test_absent_original_operation_is_read_only_proof_of_no_effect(self):
        before = self.state()
        observation = self.handle("coverage", "reconcile", key="never-dispatched")
        self.assertEqual(observation["outcome"], "no_effect")
        self.assertEqual(before, self.state())

    def test_changed_input_under_original_engine_operation_conflicts(self):
        self.handle("coverage", "execute", {"request": "first"})
        for phase in ("execute", "reconcile", "validate_checkpoint"):
            with self.subTest(phase=phase), self.assertRaisesRegex(ValueError, "identity conflict"):
                self.handle("coverage", phase, {"request": "changed"})

    def test_claim_checkpoint_revalidates_workspace_before_provider_dispatch(self):
        request = copy.deepcopy(self.fixture.request)
        request["options"]["tasks"] = ["local:review"]
        self.prepared = prepare(self.root, request, self.provider)
        self.plan = read_artifact(self.root, self.prepared["plan"])
        task = next(t for t in self.plan["tasks"] if t["kind"] == "review")
        inputs = {"task": task["id"]}
        claimed = self.handle("claim", "execute", inputs, key="claim-before-native")
        self.assertTrue(claimed["result"]["output"]["invoke"])
        self.assertTrue(self.handle("claim", "validate_checkpoint", inputs,
            key="claim-before-native", checkpoint=claimed["result"])["valid"])
        (self.root / "code.py").write_text("changed_after_claim = True\n")
        before = self.state()
        self.assertFalse(self.handle("claim", "validate_checkpoint", inputs,
            key="claim-before-native", checkpoint=claimed["result"])["valid"])
        self.assertEqual(before, self.state())

    def review_claim(self):
        request = copy.deepcopy(self.fixture.request)
        request["options"]["tasks"] = ["local:review"]
        self.prepared = prepare(self.root, request, self.provider)
        self.plan = read_artifact(self.root, self.prepared["plan"])
        return self.handle("claim", "execute", {"task": self.plan["tasks"][0]["id"]})["result"]["output"]["claim"]

    def test_retry_checkpoint_rechecks_governing_evidence_until_the_claim_finishes(self):
        claim = self.review_claim()
        inputs = {"claim": claim, "agent": REVIEWER, "status": "failed",
                  "failure_class": "usage_exhausted", "unsafe_failure_class": "", "error": "exhausted"}
        retry = self.handle("turn_result", "execute", inputs, key="eligible-retry")
        self.assertTrue(retry["result"]["output"]["retry"])
        self.provider.ev["comments"].append({"id": 88, "body": "New request", "user": {"id": 21}})
        self.assertFalse(self.handle("turn_result", "validate_checkpoint", inputs,
            key="eligible-retry", checkpoint=retry["result"])["valid"])
        self.provider.ev["comments"].clear()
        self.assertTrue(self.handle("turn_result", "validate_checkpoint", inputs,
            key="eligible-retry", checkpoint=retry["result"])["valid"])
        with self.ledger() as ledger:
            app.accept(self.root, self.provider, ledger, self.plan,
                {"claim": claim, "agent": REVIEWER, "report": self.fixture.report()})
        self.provider.ev["comments"].append({"id": 89, "body": "Later request", "user": {"id": 21}})
        self.assertTrue(self.handle("turn_result", "validate_checkpoint", inputs,
            key="eligible-retry", checkpoint=retry["result"])["valid"])

    def test_checklist_dispatch_and_checkpoint_reject_governing_drift(self):
        claim = self.review_claim()
        partial = self.fixture.report(complete=False)
        partial["checklist"][-1]["status"] = "incomplete"
        with self.ledger() as ledger:
            app.accept(self.root, self.provider, ledger, self.plan,
                {"claim": claim, "agent": REVIEWER, "report": partial})
        inputs = {"claim": claim, "agent": REVIEWER, "round": 1}
        continuation = self.handle("check_prepare", "execute", inputs, key="checklist")
        (self.root / "AGENTS.md").write_text("Changed governing instructions\n")
        self.assertFalse(self.handle("check_prepare", "validate_checkpoint", inputs,
            key="checklist", checkpoint=continuation["result"])["valid"])
        with self.assertRaisesRegex(ValueError, "governing inputs changed"):
            self.handle("check_prepare", "execute", inputs, key="new-checklist")

    def test_corrupt_original_helper_result_and_closure_block_recovery(self):
        executed = self.handle("coverage", "execute")
        reference = executed["result"]["evidence"]["artifact"]
        path = self.root / reference
        original = path.read_bytes()
        path.write_text("{}")
        with self.assertRaisesRegex(ValueError, "artifact identity"):
            self.handle("coverage", "validate_checkpoint", checkpoint=executed["result"])
        with self.assertRaisesRegex(ValueError, "artifact identity"):
            self.handle("coverage", "reconcile")
        path.write_bytes(original)
        directory = (self.root / self.prepared["workflow"]).parent
        source = directory / "helpers" / "prfix" / "app.py"
        source.write_bytes(source.read_bytes() + b"\n# unadmitted mutation\n")
        with self.assertRaisesRegex(ValueError, "helper closure differs"):
            self.handle("coverage", "bindings")

    def test_original_writer_checkpoint_accepts_edits_but_retains_governing_and_owner_bindings(self):
        request = copy.deepcopy(self.fixture.request)
        request["options"]["tasks"] = ["local:writer"]
        request["adapters"]["local:writer"] = {"kind": "synchronization", "writes": True,
            "definition": "fixer", "assignment": "Synchronize the exact assigned source."}
        request["authorizations"] = {"iterate": {"request_id": "writer", "instruction": "Fix locally", "source": "chat:writer"}}
        self.prepared = prepare(self.root, request, self.provider)
        self.plan = read_artifact(self.root, self.prepared["plan"])
        inputs = {"task": self.plan["tasks"][0]["id"]}
        claimed = self.handle("claim", "execute", inputs, key="writer-before-native")
        (self.root / "code.py").write_text("value = 3\n")
        def validate():
            return self.handle("claim", "validate_checkpoint", inputs, key="writer-before-native", checkpoint=claimed["result"])["valid"]
        self.assertTrue(validate())
        instructions = self.root / "AGENTS.md"
        original = instructions.read_bytes()
        instructions.write_text("Changed governing instruction\n")
        self.assertFalse(validate())
        instructions.write_bytes(original)
        self.assertTrue(validate())
        import sqlite3
        from contextlib import closing
        with closing(sqlite3.connect(self.root / ".pr-fix-state" / "workspace-writer.sqlite")) as database:
            database.execute("UPDATE writer SET claim_id='another-claim'")
            database.commit()
        with self.assertRaisesRegex(ValueError, "writer ownership changed"):
            validate()

    def test_selected_completion_is_distinct_from_merge_readiness(self):
        completed = self.handle("completion", "execute")
        self.assertTrue(completed["result"]["output"]["complete"])
        self.assertFalse(completed["result"]["output"]["ready"])
        before = self.state()
        self.assertTrue(self.handle("completion", "validate_checkpoint", checkpoint=completed["result"])["valid"])
        self.assertEqual(before, self.state())

    def test_completion_survives_authorized_merge_with_lost_response_before_reconciliation(self):
        request = copy.deepcopy(self.fixture.request)
        request["options"]["tasks"] = ["local:review"]
        request["authorizations"]["merge"] = AUTH
        self.prepared = prepare(self.root, request, self.provider)
        self.plan = read_artifact(self.root, self.prepared["plan"])
        head, target = self.fixture.head, self.fixture.target
        self.provider.merge_state["headRefOid"] = head
        self.provider.parents = [target, head]
        with self.ledger() as ledger:
            for task in self.plan["tasks"]:
                claim = app.prepare_claim(self.root, self.provider, ledger, self.plan, {"task": task["id"]})
                app.accept(self.root, self.provider, ledger, self.plan, {"claim": claim["claim"],
                    "agent": VERIFIER if task["kind"] == "verification" else REVIEWER,
                    "report": self.fixture.report(dispositions=[])})
            completion = self.handle("completion", "execute")
            self.assertTrue(completion["result"]["output"]["ready"], completion)
            proposal = publishing.merge_proposal(self.root, self.provider, ledger, self.plan, {})["proposal"]
            prepared = publishing.publication_prepare(self.root, self.provider, ledger, self.plan, {"proposal": proposal})
        inputs = {"id": prepared["id"], "agent": "agent_" + "4" * 32,
                  "report": self.fixture.report(audience={"fingerprint": prepared["fingerprint"], "suitable": True,
                                                         "audience": "PR participants", "evidence": ["source"]})}
        self.provider.lose_response = True
        with self.assertRaisesRegex(OSError, "response lost"):
            self.handle("publication_execute", "execute", inputs, key="merge")
        self.assertTrue(self.provider.ev["metadata"]["merged"])
        before = self.state()
        self.assertTrue(self.handle("completion", "validate_checkpoint", checkpoint=completion["result"])["valid"])
        self.assertEqual(before, self.state())
        reconciled = self.handle("publication_execute", "reconcile", inputs, key="merge")
        self.assertEqual(reconciled["outcome"], "completed")
        self.assertEqual(len(self.provider.writes), 1)
        self.provider.ev["metadata"]["merged_by"]["id"] = 999
        self.assertFalse(self.handle("completion", "validate_checkpoint", checkpoint=completion["result"])["valid"])

    def test_publication_record_checkpoint_invokes_service_receipt_validator(self):
        value = {"id": "fixture-publication", "receipt": {"id": "fixture-publication"}}
        inputs = {"id": value["id"], "receipt": value["receipt"]}
        with patch.object(action, "dispatch", return_value=value):
            executed = self.handle("publication_record", "execute", inputs)
        with patch.object(publishing, "validate_operation", return_value=False) as validate:
            checkpoint = self.handle("publication_record", "validate_checkpoint", inputs, checkpoint=executed["result"])
            self.assertFalse(checkpoint["valid"])
            validate.assert_called_once()
        with patch.object(publishing, "validate_operation", return_value=True):
            self.assertTrue(self.handle("publication_record", "validate_checkpoint", inputs, checkpoint=executed["result"])["valid"])

    def test_real_publication_response_loss_reconciles_read_only_then_records_explicitly(self):
        request = copy.deepcopy(self.fixture.request)
        request["options"] = {"tasks": ["local:description"], "finalizers": [],
                              "permissions": {"reply_to_feedback": True}}
        request["adapters"]["local:description"] = {"kind": "description", "writes": False,
            "definition": "leaf", "assignment": "Prepare an exact sourced reply proposal."}
        self.prepared = prepare(self.root, request, self.provider)
        self.plan = read_artifact(self.root, self.prepared["plan"])
        with self.ledger() as ledger:
            task = self.plan["tasks"][0]
            claimed = app.prepare_claim(self.root, self.provider, ledger, self.plan, {"task": task["id"]})
            report = self.fixture.report(operations=[{"kind": "reply", "head": self.fixture.head,
                "target": self.fixture.target, "payload": {"body": "Fixed with current source evidence."}}])
            app.accept(self.root, self.provider, ledger, self.plan,
                {"claim": claimed["claim"], "agent": REVIEWER, "report": report, "round": 0})
            proposal = publishing.publication_list(self.root, self.provider, ledger, self.plan, {})["items"][0]
            prepared = publishing.publication_prepare(self.root, self.provider, ledger, self.plan, {"proposal": proposal})
        audience = self.fixture.report(audience={"fingerprint": prepared["fingerprint"], "suitable": True,
            "audience": "PR participants", "evidence": ["source"]})
        inputs = {"id": prepared["id"], "agent": VERIFIER, "report": audience}
        self.provider.lose_response = True
        with self.assertRaisesRegex(OSError, "response lost"):
            self.handle("publication_execute", "execute", inputs, key="remote-effect")
        before = self.state()
        recovered = self.handle("publication_execute", "reconcile", inputs, key="remote-effect")
        self.assertEqual(recovered["outcome"], "completed")
        self.assertEqual(before, self.state())
        self.assertTrue(self.handle("publication_execute", "validate_checkpoint", inputs,
            key="remote-effect", checkpoint=recovered["result"])["valid"])
        with self.ledger(read_only=True) as ledger:
            self.assertEqual(ledger.db.execute("SELECT state FROM publication_intents").fetchone()[0], "uncertain")
        record_inputs = {"id": prepared["id"], "receipt": recovered["result"]["output"]["receipt"]}
        recorded = self.handle("publication_record", "execute", record_inputs, key="record-original-receipt")
        self.assertTrue(self.handle("publication_record", "validate_checkpoint", record_inputs,
            key="record-original-receipt", checkpoint=recorded["result"])["valid"])
        self.assertEqual(len(self.provider.writes), 1)


if __name__ == "__main__":
    unittest.main()
