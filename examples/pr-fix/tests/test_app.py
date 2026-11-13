"""Application acceptance, session continuity, allowances and controlled Git."""
import copy
import hashlib
from pathlib import Path
import subprocess
import sys
import tempfile
import unittest

sys.path.insert(0, str(Path(__file__).resolve().parents[1] / "workflows" / "helpers"))
from prfix import app
from prfix.core import Ledger, artifact, canonical, digest, identity, read_artifact
from prfix.prepare import CHECKLIST, prepare
from test_publication import AUTH, FakeGitHub

REVIEWER = "agent_" + "1" * 32
VERIFIER = "agent_" + "2" * 32
FIXER = "agent_" + "3" * 32


def git(root, *args):
    result = subprocess.run(["git", *args], cwd=root, stdout=subprocess.PIPE,
                            stderr=subprocess.PIPE, timeout=15, check=True)
    return result.stdout.decode().strip()


class AppFixture:
    def __init__(self, root, *, review=True, ci=False):
        self.root = root
        git(root, "init", "--initial-branch=main")
        git(root, "config", "user.email", "fixture@example.invalid")
        git(root, "config", "user.name", "Fixture")
        git(root, "config", "commit.gpgsign", "false")
        (root / "code.py").write_text("value = 1\n")
        (root / "AGENTS.md").write_text("Preserve exact source evidence and do not publish without authority.\n")
        git(root, "add", ".")
        git(root, "commit", "-m", "Initial")
        self.target = git(root, "rev-parse", "HEAD")
        git(root, "switch", "-c", "feature")
        (root / "code.py").write_text("value = 2\n")
        (root / "roles").mkdir()
        roles = {}
        for key in ("leaf", "reviewer", "verifier", "fixer", "audience"):
            path = root / "roles" / (key + ".toml")
            path.write_text(f'name = "{key}"\ndescription = "Fixture"\ndeveloper_instructions = "Follow the exact bounded assignment and retain evidence."\n')
            roles[key] = path.relative_to(root).as_posix()
        git(root, "add", ".")
        git(root, "commit", "-m", "Create review fixture")
        self.head = git(root, "rev-parse", "HEAD")
        self.provider = FakeGitHub()
        self.provider.ev["metadata"].update(user={"id": 11, "login": "maintainer"}, updated_at="now")
        self.provider.ev["metadata"]["head"]["sha"] = self.head
        self.provider.ev["metadata"]["base"]["sha"] = self.target
        self.provider.observe_ci = lambda pr, head: {"head": head, "checks": [
            {"id": "fixture-check", "status": "completed", "conclusion": "success"}], "evidence": "fixture-ci"}
        tasks = ["local:review"] if review else []
        if ci:
            tasks.append("dk-workflows:dk-ci")
        self.request = {"pr": "https://github.com/o/r/pull/7", "instructions": "Fix the selected issue.",
            "image": "ghcr.io/fixture/python-git-gh@sha256:" + "a" * 64, "roles": roles,
            "options": {"tasks": tasks, "finalizers": []},
            "adapters": {"local:review": {"kind": "review", "definition": "reviewer", "writes": False,
                "assignment": "Review this exact fixture and retain original source findings."}},
            "authorizations": {"iterate": AUTH}}
        self.prepared = prepare(root, self.request, self.provider)
        self.plan = read_artifact(root, self.prepared["plan"])

    def report(self, **values):
        evidence = {"id": "source", "path": "AGENTS.md", "sha256": app.source_file(self.root, "AGENTS.md")}
        result = {"complete": True, "checklist": [{"id": item["id"], "status": "completed", "evidence": ["source"]} for item in CHECKLIST],
                  "findings": [], "evidence": [evidence], "blockers": []}
        result.update(values)
        return result


class AppTests(unittest.TestCase):
    def setUp(self):
        self.temp = tempfile.TemporaryDirectory(prefix="portable-pr-app-")
        self.addCleanup(self.temp.cleanup)
        self.root = Path(self.temp.name)
        self.fixture = AppFixture(self.root)
        self.plan, self.provider = self.fixture.plan, self.fixture.provider
        self.ledger = Ledger(self.root, self.plan["logical_run"], self.plan["pr"]["url"])
        self.addCleanup(self.ledger.close)
        self.review = next(t for t in self.plan["tasks"] if t["kind"] == "review")

    def claim(self, task=None, **values):
        return app.prepare_claim(self.root, self.provider, self.ledger, self.plan,
                                 {"task": task or self.review["id"], **values})

    def accept(self, claim, report=None, agent=REVIEWER, round=0):
        return app.accept(self.root, self.provider, self.ledger, self.plan,
                          {"claim": claim["claim"], "agent": agent,
                           "report": report or self.fixture.report(), "round": round})

    def original_finding(self):
        claim = self.claim()
        self.accept(claim, self.fixture.report(findings=[{"id": "R1", "text": "A sourced original defect", "sources": ["source"]}]))
        return app.candidates(self.root, self.ledger, self.plan)[0]

    def test_empty_and_forged_evidence_checklists_cannot_complete(self):
        task = self.review
        report = self.fixture.report()
        self.assertTrue(app.verify_report(self.root, task, report))
        for mutate in (
            lambda r: r.update(evidence=[]),
            lambda r: r["evidence"][0].update(sha256="0" * 64),
            lambda r: r.update(checklist=[]),
            lambda r: r["checklist"][0].update(evidence=["forged"]),
            lambda r: r["checklist"].append(copy.deepcopy(r["checklist"][0])),
            lambda r: r.update(complete="true"),
        ):
            changed = copy.deepcopy(report)
            mutate(changed)
            with self.subTest(report=changed), self.assertRaises(ValueError):
                app.verify_report(self.root, task, changed)
        report["complete"] = False
        report["checklist"][-1]["status"] = "incomplete"
        self.assertFalse(app.verify_report(self.root, task, report))

    def test_optional_final_checklist_round_settles_without_task_coverage(self):
        self.review["required"] = False
        claim = self.claim()
        report = self.fixture.report(complete=False)
        for check in report["checklist"]:
            check["status"] = "incomplete"
        for number in range(4):
            observed = self.accept(claim, report, round=number)
            self.assertFalse(observed["complete"])
            self.assertEqual(observed["settled"], number == 3)
        state = app.task_status(self.root, self.provider, self.ledger, self.plan, {"claim": claim["claim"]})
        self.assertFalse(state["complete"])
        self.assertEqual(self.ledger.db.execute("SELECT count(*) FROM accepted_reports").fetchone()[0], 0)

    def test_partial_submission_findings_survive_same_session_checklist_completion(self):
        claim = self.claim()
        partial = self.fixture.report(complete=False,
            findings=[{"id": "R1", "text": "Retained original defect", "sources": ["source"]}])
        partial["checklist"][-1]["status"] = "incomplete"
        first = self.accept(claim, partial)
        self.assertFalse(first["complete"])
        self.assertEqual(len(app.candidates(self.root, self.ledger, self.plan)), 1)
        with self.assertRaisesRegex(ValueError, "original native session"):
            self.accept(claim, self.fixture.report(), agent=VERIFIER, round=1)
        final = self.accept(claim, self.fixture.report(), round=1)
        self.assertTrue(final["complete"])
        retained = app.candidates(self.root, self.ledger, self.plan)
        self.assertEqual(retained[0]["text"], "Retained original defect")
        self.assertEqual(retained[0]["agent"], REVIEWER)
        self.assertEqual(self.ledger.db.execute("SELECT count(*) FROM submissions").fetchone()[0], 2)

    def test_completed_writer_rejects_missing_or_empty_manifest(self):
        claim = self.claim("finding_fix")
        (self.root / "code.py").write_text("value = 3\n")
        for values in ({}, {"changes": []}):
            with self.subTest(values=values), self.assertRaisesRegex(ValueError, "changed-file manifest"):
                self.accept(claim, self.fixture.report(**values), agent=FIXER)
        self.assertEqual(app._claim(self.ledger, claim["claim"])["state"], "claimed")
        self.assertEqual(self.ledger.db.execute("SELECT count(*) FROM accepted_reports").fetchone()[0], 0)
        with self.assertRaisesRegex(ValueError, "original writer"):
            self.claim()

    def test_exhausted_optional_writer_releases_fence_only_without_changes(self):
        writer = next(task for task in self.plan["tasks"] if task["id"] == "finding_fix")
        writer["required"] = False
        claim = self.claim(writer["id"])
        report = self.fixture.report(complete=False)
        report["checklist"][-1]["status"] = "incomplete"
        (self.root / "code.py").write_text("value = 3\n")
        with self.assertRaisesRegex(ValueError, "unresolved source effects"):
            self.accept(claim, report, agent=FIXER, round=3)
        (self.root / "code.py").write_text("value = 2\n")
        self.assertTrue(self.accept(claim, report, agent=FIXER, round=3)["settled"])
        self.assertEqual(app._claim(self.ledger, claim["claim"])["state"], "optional_failed")
        self.assertTrue(self.claim()["invoke"])
        again = self.claim(writer["id"])
        self.assertTrue(again["invoke"])
        self.assertNotEqual(again["claim"], claim["claim"])

    def test_noncacheable_task_can_repeat_after_a_completed_invocation(self):
        self.review["cacheable"] = False
        first = self.claim()
        self.accept(first)
        second = self.claim()
        self.assertTrue(second["invoke"])
        self.assertNotEqual(first["claim"], second["claim"])
        self.accept(second, agent=VERIFIER)
        self.assertEqual(len(app._claims(self.ledger, self.review["id"])), 2)

    def test_later_review_findings_invalidate_verification_and_fix_coverage(self):
        self.accept(self.claim())
        self.accept(self.claim("finding_verification"), agent=VERIFIER)
        self.accept(self.claim("finding_fix"), agent=FIXER)
        second_review = copy.deepcopy(self.review)
        second_review["id"] += "_second"
        self.plan["tasks"].append(second_review)
        self.accept(self.claim(second_review["id"]), self.fixture.report(findings=[
            {"id": "R2", "text": "Later independently found defect", "sources": ["source"]}]))
        verification = self.claim("finding_verification")
        self.assertTrue(verification["invoke"])
        finding = verification["task_input"]["findings"][0]
        disposition = {"finding_id": finding["id"], "status": "confirmed", "sources": ["source"], "reason": "Current independent source verification"}
        self.accept(verification, self.fixture.report(dispositions=[disposition]), agent=VERIFIER)
        fix = self.claim("finding_fix")
        self.assertTrue(fix["invoke"])
        self.assertEqual(fix["task_input"]["findings"], verification["task_input"]["findings"])

    def test_independent_verifier_must_account_for_every_original_finding(self):
        finding = self.original_finding()
        claim = self.claim("finding_verification")
        disposition = {"finding_id": finding["id"], "status": "confirmed", "sources": ["source"], "reason": "Verified against the original source"}
        report = self.fixture.report(dispositions=[disposition])
        with self.assertRaisesRegex(ValueError, "independent native"):
            self.accept(claim, report, agent=REVIEWER)
        with self.assertRaisesRegex(ValueError, "omitted original"):
            self.accept(claim, self.fixture.report(dispositions=[]), agent=VERIFIER)
        accepted = self.accept(claim, report, agent=VERIFIER)
        self.assertTrue(accepted["complete"])

    def test_forged_disposition_sources_are_not_evidence(self):
        finding = self.original_finding()
        claim = self.claim("finding_verification")
        report = self.fixture.report(dispositions=[{"finding_id": finding["id"], "status": "rejected",
            "sources": ["forged"], "reason": "Unsupported purported counterexample"}])
        with self.assertRaises(ValueError):
            self.accept(claim, report, agent=VERIFIER)

    def test_accepted_and_deferred_findings_require_exact_bound_user_authority(self):
        finding = self.original_finding()
        verification = self.claim("finding_verification")
        self.accept(verification, self.fixture.report(dispositions=[{"finding_id": finding["id"],
            "status": "confirmed", "sources": ["source"], "reason": "Independent source verification"}]), agent=VERIFIER)
        claim = self.claim("finding_fix")
        disposition = {"finding_id": finding["id"], "status": "deferred", "sources": ["source"],
                       "reason": "Exact user-authorized follow-up", "authorization": AUTH}
        report = self.fixture.report(dispositions=[disposition])
        with self.assertRaisesRegex(ValueError, "exact retained user authority"):
            self.accept(claim, report, agent=FIXER)
        self.plan["authorizations"]["finding:" + finding["id"]] = AUTH
        accepted = self.accept(claim, report, agent=FIXER)
        self.assertTrue(accepted["complete"])
        changed = copy.deepcopy(report)
        changed["dispositions"][0]["authorization"]["source"] = "different source"
        with self.assertRaisesRegex(ValueError, "exact retained user authority"):
            self.accept(claim, changed, agent=FIXER, round=1)

    def test_controlled_commit_requires_exact_manifest_and_preserves_literal_paths(self):
        claim = self.claim("finding_fix")
        name = "[literal].txt"
        (self.root / name).write_text("exact intended file\n")
        report = self.fixture.report(changes=[{"path": name, "before": None,
            "after": hashlib.sha256((self.root / name).read_bytes()).hexdigest()}], commit_message="Add literal fixture path")
        parent = git(self.root, "rev-parse", "HEAD")
        accepted = self.accept(claim, report, agent=FIXER)
        self.assertTrue(accepted["complete"])
        committed = git(self.root, "rev-parse", "HEAD")
        self.assertNotEqual(committed, parent)
        self.assertEqual(git(self.root, "rev-parse", "HEAD^"), parent)
        self.assertEqual(git(self.root, "show", "HEAD:" + name), "exact intended file")
        row = self.ledger.db.execute("SELECT * FROM local_commits WHERE id=?", (claim["claim"],)).fetchone()
        self.assertEqual(row["head"], committed)
        self.assertEqual(row["state"], "completed")
        self.assertTrue((self.root / ".git").is_dir())
        self.assertEqual(git(self.root, "diff", "--cached", "--name-only"), "")

    def test_controlled_commit_rejects_git_filter_changes_to_accepted_bytes(self):
        # Git's ordinary clean filter can change the staged blob even while
        # worktree bytes still match the accepted report's SHA-256 manifest.
        (self.root / ".gitattributes").write_text("filtered.txt filter=fixture\n")
        git(self.root, "add", ".gitattributes")
        git(self.root, "commit", "-m", "Declare fixture filter")
        self.provider.ev["metadata"]["head"]["sha"] = git(self.root, "rev-parse", "HEAD")
        git(self.root, "config", "filter.fixture.clean", "tr a-z A-Z")
        claim = self.claim("finding_fix")
        (self.root / "filtered.txt").write_text("intended bytes\n")
        report = self.fixture.report(changes=[{"path": "filtered.txt", "before": None,
            "after": hashlib.sha256((self.root / "filtered.txt").read_bytes()).hexdigest()}], commit_message="Add exact fixture bytes")
        parent = git(self.root, "rev-parse", "HEAD")
        with self.assertRaisesRegex(ValueError, "staged blob bytes"):
            self.accept(claim, report, agent=FIXER)
        self.assertEqual(git(self.root, "rev-parse", "HEAD"), parent)

    def test_controlled_commit_rejects_unaccounted_or_forged_changed_bytes(self):
        claim = self.claim("finding_fix")
        original = (self.root / "code.py").read_bytes()
        (self.root / "code.py").write_text("value = 3\n")
        (self.root / "unrelated.txt").write_text("must remain untouched")
        report = self.fixture.report(changes=[{"path": "code.py", "before": hashlib.sha256(original).hexdigest(),
            "after": app.source_file(self.root, "code.py")}], commit_message="Fix the selected fixture")
        before = git(self.root, "rev-parse", "HEAD")
        with self.assertRaisesRegex(ValueError, "every workspace change"):
            self.accept(claim, report, agent=FIXER)
        self.assertEqual(git(self.root, "rev-parse", "HEAD"), before)
        self.assertEqual((self.root / "unrelated.txt").read_text(), "must remain untouched")
        self.assertEqual(git(self.root, "diff", "--cached", "--name-only"), "")
        (self.root / "unrelated.txt").unlink()
        report["changes"][0]["after"] = "0" * 64
        with self.assertRaisesRegex(ValueError, "source bytes"):
            self.accept(claim, report, agent=FIXER)

    def test_usage_exhaustion_retains_one_same_session_retry_and_unsafe_failure_blocks(self):
        claim = self.claim()
        failure = {"claim": claim["claim"], "agent": REVIEWER, "status": "failed",
            "failure_class": "usage_exhausted", "unsafe_failure_class": "", "error": "bounded usage allowance exhausted"}
        first = app.turn_result(self.root, self.provider, self.ledger, self.plan, failure)
        self.assertTrue(first["retry"])
        replay = app.turn_result(self.root, self.provider, self.ledger, self.plan, failure)
        self.assertEqual(replay, first)
        self.assertEqual(self.ledger.db.execute("SELECT sum(amount) FROM reservations WHERE name LIKE 'native-retry:%'").fetchone()[0], 1)
        with self.assertRaisesRegex(ValueError, "without eligible retained retry"):
            app.turn_result(self.root, self.provider, self.ledger, self.plan, {**failure, "retried": True})
        with self.assertRaisesRegex(ValueError, "original session|observation identity"):
            app.turn_result(self.root, self.provider, self.ledger, self.plan, {**failure, "agent": VERIFIER, "retried": True})
        # A separate required task demonstrates that cleanup uncertainty never
        # becomes an eligible retry solely because usage classification matches.
        other = self.claim("finding_verification")
        with self.assertRaisesRegex(ValueError, "without eligible retained retry"):
            app.turn_result(self.root, self.provider, self.ledger, self.plan,
                {**failure, "claim": other["claim"], "agent": VERIFIER, "unsafe_failure_class": "cleanup"})

    def test_ci_repair_requires_current_failure_and_retained_cycle_allowance(self):
        repair = copy.deepcopy(next(t for t in self.plan["tasks"] if t["id"] == "finding_fix"))
        repair["id"] = "ci_repair"
        self.plan["tasks"].append(repair)
        self.plan["limits"]["ci_repairs_per_cycle"] = 1
        with self.assertRaisesRegex(ValueError, "current exact-head failed"):
            self.claim("ci_repair", cycle=1)
        head = self.provider.ev["metadata"]["head"]["sha"]
        observed = {"head": head, "status": "failed", "checks": [], "evidence": "fixture"}
        self.ledger.db.execute("INSERT INTO ci_observations VALUES(?,?,?,?)", ("obs-1", head, "failed", canonical(observed).decode()))
        first = self.claim("ci_repair", cycle=1)
        self.assertEqual(first["task_input"]["ci"], observed)
        with self.assertRaisesRegex(ValueError, "unfinished logical claim"):
            self.claim("ci_repair", cycle=1)
        self.assertEqual(self.ledger.db.execute("SELECT sum(amount) FROM reservations WHERE name='ci-repair:1'").fetchone()[0], 1)
        # Isolate the allowance assertion from the unfinished-owner guard:
        # simulate a settled first repair without changing consumed allowance.
        self.ledger.db.execute("UPDATE claims SET state='complete' WHERE id=?", (first["claim"],))
        self.plan["settings"] = {"changed": True}
        with self.assertRaisesRegex(ValueError, "exhausted"):
            self.claim("ci_repair", cycle=1)
        self.plan["settings"] = {"changed": "again"}
        self.ledger.db.execute("INSERT INTO ci_observations VALUES(?,?,?,?)", ("obs-2", "c" * 40, "failed", canonical(observed).decode()))
        with self.assertRaisesRegex(ValueError, "current exact-head failed"):
            self.claim("ci_repair", cycle=2)

    def test_changed_inputs_do_not_admit_another_writer_over_an_unfinished_claim(self):
        first = self.claim("finding_fix")
        self.plan["instructions"] = "A replacement exact request while the original writer is unfinished"
        with self.assertRaises(ValueError):
            self.claim("finding_fix")
        active = self.ledger.db.execute("SELECT count(*) FROM claims WHERE task='finding_fix' AND state='claimed'").fetchone()[0]
        self.assertEqual(active, 1)
        self.assertEqual(app._claim(self.ledger, first["claim"])["state"], "claimed")

    def test_corrupt_original_report_blocks_current_task_coverage(self):
        claim = self.claim()
        accepted = self.accept(claim)
        path = self.root / accepted["report"]
        path.write_text("{}")
        with self.assertRaisesRegex(ValueError, "artifact identity"):
            app.coverage(self.root, self.provider, self.ledger, self.plan, {})


if __name__ == "__main__":
    unittest.main()
