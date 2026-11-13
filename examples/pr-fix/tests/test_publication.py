"""Deterministic publication safety fixtures; no external services are used."""
import copy
import json
from pathlib import Path
import sqlite3
import sys
import tempfile
import subprocess
import threading
import unittest

sys.path.insert(0, str(Path(__file__).resolve().parents[1] / "workflows" / "helpers"))
from prfix.core import Ledger, canonical, digest, identity
from prfix.publication import Publication, PublicationError, UncertainEffect

HEAD = "a" * 40
BASE = "b" * 40
OTHER = "c" * 40
MERGE = "d" * 40
AUTH = {"request_id": "user-1", "instruction": "Fix and publish this PR; merge when ready", "source": "chat:1"}


class FakeGitHub:
    def __init__(self):
        self.ev = {
            "metadata": {"html_url": "https://github.com/o/r/pull/7", "number": 7,
                         "state": "open", "merged": False, "draft": False,
                         "mergeable": True, "mergeable_state": "clean",
                         "title": "Before", "body": "Old description",
                         "head": {"sha": HEAD, "ref": "feature", "repo": {"full_name": "o/r"}},
                         "base": {"sha": BASE, "ref": "main", "repo": {"full_name": "o/r"}}},
            "viewer": {"id": 11, "login": "maintainer"},
            "comments": [], "reviews": [], "inline": [], "threads": [],
        }
        self.calls = []
        self.lose_response = False
        self.delay_creation = False
        self.capture_error = False
        self.run = {"id": 14, "head_sha": HEAD, "run_attempt": 1, "status": "completed", "conclusion": "failure"}
        self.requested = {"users": [], "teams": []}
        self.merge_state = {"headRefOid": HEAD, "isDraft": False, "mergeable": "MERGEABLE", "mergeStateStatus": "CLEAN", "reviewDecision": "APPROVED"}
        self.parents = [BASE, HEAD]
        self.split_pulls = []
        self.split_sha = HEAD

    def capture(self, pr):
        if self.capture_error:
            raise OSError("fixture observation unavailable")
        return copy.deepcopy(self.ev)

    def pages(self, path):
        return [copy.deepcopy(self.ev["metadata"]), *copy.deepcopy(self.split_pulls)]

    def request(self, method, path, payload=None):
        self.calls.append((method, path, copy.deepcopy(payload)))
        if method == "GET" and path.endswith("requested_reviewers"):
            return copy.deepcopy(self.requested)
        if method == "GET" and "/actions/runs/" in path:
            return copy.deepcopy(self.run)
        if method == "GET" and "/git/commits/" in path:
            return {"sha": MERGE, "parents": [{"sha": s} for s in self.parents]}
        if method == "POST" and path == "graphql" and payload["query"].startswith("query"):
            return {"data": {"repository": {"pullRequest": copy.deepcopy(self.merge_state)}}}
        if method == "PATCH":
            self.ev["metadata"].update(payload)
            result = copy.deepcopy(self.ev["metadata"])
        elif method == "POST" and path.endswith("/comments"):
            result = {"id": 123, "body": payload["body"], "user": copy.deepcopy(self.ev["viewer"])}
            if not self.delay_creation:
                self.ev["comments"].append(copy.deepcopy(result))
        elif method == "POST" and path.endswith("/replies"):
            result = {"id": 124, "in_reply_to_id": int(path.split("/")[-2]),
                      "body": payload["body"], "user": copy.deepcopy(self.ev["viewer"])}
            if not self.delay_creation:
                self.ev["inline"].append(copy.deepcopy(result))
        elif method == "POST" and path.endswith("requested_reviewers"):
            self.requested = {"users": [{"login": n} for n in payload.get("reviewers", [])],
                              "teams": [{"slug": n} for n in payload.get("team_reviewers", [])]}
            result = copy.deepcopy(self.requested)
        elif method == "POST" and path.endswith("rerun-failed-jobs"):
            self.run["run_attempt"] += 1
            result = {}
        elif method == "POST" and path == "graphql":
            thread = next(t for t in self.ev["threads"] if t["id"] == payload["variables"]["id"])
            thread["isResolved"] = True
            result = {"data": {"resolveReviewThread": {"thread": copy.deepcopy(thread)}}}
        elif method == "POST" and path.endswith("/pulls"):
            owner, branch = payload["head"].split(":", 1)
            result = {"number": 8, "state": "open", "draft": True, "title": payload["title"],
                      "body": payload["body"], "user": copy.deepcopy(self.ev["viewer"]),
                      "head": {"sha": self.split_sha, "ref": branch, "repo": {"full_name": "o/r"}},
                      "base": copy.deepcopy(self.ev["metadata"]["base"])}
            self.split_pulls.append(copy.deepcopy(result))
        elif method == "PUT" and path.endswith("/merge"):
            self.ev["metadata"].update(state="closed", merged=True, merge_commit_sha=MERGE, merged_by={"id": 11})
            result = {"merged": True, "sha": MERGE}
        else:
            raise AssertionError((method, path, payload))
        if self.lose_response:
            raise OSError("fixture response lost after effect")
        return result

    @property
    def writes(self):
        return [v for v in self.calls if v[0] in ("PATCH", "PUT") or v[0] == "POST" and not (v[1] == "graphql" and v[2]["query"].startswith("query"))]


class PublicationTests(unittest.TestCase):
    def setUp(self):
        self.tmp = tempfile.TemporaryDirectory()
        self.addCleanup(self.tmp.cleanup)
        self.root = Path(self.tmp.name)
        self.pr = identity("https://github.com/o/r/pull/7")
        self.plan = {"logical_run": "fixture", "pr": self.pr, "instructions": "Original exact instruction",
                     "authorizations": {"iterate": AUTH, "merge": AUTH},
                     "options": {"permissions": {key: True for key in (
                         "push_commits", "rewrite_commits", "edit_pr", "reply_to_feedback", "resolve_threads", "create_draft_prs", "request_review")},
                         "limits": {"ci_reruns_per_head": 3}}}
        self.ledger = Ledger(self.root, "fixture", self.pr["url"])
        self.addCleanup(self.ledger.close)
        self.provider = FakeGitHub()
        self.ready = {"ready": True, "head": HEAD, "target": BASE, "clean_review": True, "verified": True,
                      "ci": {"head": HEAD, "success": True}, "evidence": ["accepted:live"]}
        self.publication = Publication(self.ledger, self.provider, self.root, self.plan,
                                       readiness=lambda intent: copy.deepcopy(self.ready), source_validator=lambda source: source.startswith("accepted:"))

    def propose(self, kind="reply", payload=None, ident="op-1", **kwargs):
        return self.publication.prepare({"id": ident, "kind": kind, "head": HEAD, "target": BASE,
                                         "payload": payload if payload is not None else {"body": "Fixed with a regression fixture."},
                                         "sources": ["accepted:report-1"], **kwargs})

    def audience(self, prepared):
        return {"fingerprint": prepared["fingerprint"], "suitable": True,
                "audience": "PR participants", "evidence": ["accepted:audience-1"]}

    def test_reply_receipt_and_replay_do_not_duplicate(self):
        prepared = self.propose()
        receipt = self.publication.execute(prepared["id"], self.audience(prepared))
        self.assertEqual(receipt["result"]["id"], 123)
        self.assertTrue(self.publication.validate(prepared["id"], receipt))
        self.assertEqual(self.publication.execute(prepared["id"], self.audience(prepared)), receipt)
        self.assertEqual(len(self.provider.writes), 1)

    def test_response_loss_reconciles_exact_actor_body_and_baseline(self):
        prepared = self.propose()
        self.provider.lose_response = True
        with self.assertRaisesRegex(OSError, "response lost"):
            self.publication.execute(prepared["id"], self.audience(prepared))
        self.assertEqual(self.ledger.db.execute("SELECT state FROM publication_intents").fetchone()[0], "uncertain")
        before = self.ledger.db.total_changes
        observed = self.publication.reconcile(prepared["id"])
        self.assertEqual(observed["outcome"], "completed")
        self.assertEqual(before, self.ledger.db.total_changes)
        self.provider.lose_response = False
        self.publication.execute(prepared["id"], self.audience(prepared))
        self.assertEqual(len(self.provider.writes), 1)

    def test_missing_creation_never_grants_replay(self):
        prepared = self.propose()
        self.provider.delay_creation = self.provider.lose_response = True
        with self.assertRaises(OSError):
            self.publication.execute(prepared["id"], self.audience(prepared))
        self.provider.lose_response = False
        self.assertEqual(self.publication.reconcile(prepared["id"])["outcome"], "unknown")
        with self.assertRaises(UncertainEffect):
            self.publication.execute(prepared["id"], self.audience(prepared))
        self.assertEqual(len(self.provider.writes), 1)

    def test_observation_failure_is_unknown_without_writes(self):
        prepared = self.propose()
        self.provider.capture_error = True
        before = self.ledger.db.total_changes
        self.assertEqual(self.publication.reconcile(prepared["id"])["outcome"], "unknown")
        self.assertEqual(self.ledger.db.total_changes, before)
        self.assertEqual(self.provider.writes, [])

    def test_distinct_ids_alias_original_reply_including_uncertain(self):
        first = self.propose()
        self.provider.delay_creation = self.provider.lose_response = True
        with self.assertRaises(OSError):
            self.publication.execute(first["id"], self.audience(first))
        second = self.propose(ident="different-id")
        self.assertEqual(second["id"], first["id"])
        with self.assertRaises(UncertainEffect):
            self.publication.execute("different-id", self.audience(first))
        with self.assertRaisesRegex(PublicationError, "conflicting"):
            self.propose(payload={"body": "Different answer"}, ident="different-id")
        self.assertEqual(len(self.provider.writes), 1)

    def test_same_id_changed_body_conflicts(self):
        self.propose()
        with self.assertRaisesRegex(PublicationError, "conflicting"):
            self.propose(payload={"body": "Different"})

    def test_exact_existing_answer_is_adopted_without_write(self):
        self.provider.ev["comments"].append({"id": 88, "body": "Fixed with a regression fixture.", "user": {"id": 11}})
        prepared = self.propose()
        receipt = self.publication.execute(prepared["id"], self.audience(prepared))
        self.assertEqual(receipt["result"]["id"], 88)
        self.assertEqual(self.provider.writes, [])

    def test_actor_incoming_discussion_head_target_and_audience_drift_block(self):
        for mutation in (
            lambda: self.provider.ev["viewer"].update(id=99),
            lambda: self.provider.ev["metadata"]["head"].update(sha=OTHER),
            lambda: self.provider.ev["metadata"]["base"].update(sha=OTHER),
            lambda: self.provider.ev["comments"].append({"id": 99, "body": "new", "user": {"id": 12}}),
        ):
            with self.subTest(mutation=mutation):
                self.ledger.db.execute("DELETE FROM publication_intents")
                self.provider.ev = FakeGitHub().ev
                prepared = self.propose()
                mutation()
                with self.assertRaises(PublicationError):
                    self.publication.execute(prepared["id"], self.audience(prepared))
                self.assertEqual(self.provider.writes, [])
        self.provider.ev = FakeGitHub().ev
        self.ledger.db.execute("DELETE FROM publication_intents")
        prepared = self.propose()
        audience = self.audience(prepared)
        audience["fingerprint"] = "wrong"
        with self.assertRaises(PublicationError):
            self.publication.execute(prepared["id"], audience)

    def test_authority_permission_and_source_are_checked(self):
        for action in (
            lambda: self.plan["authorizations"].clear(),
            lambda: self.plan["options"]["permissions"].update(reply_to_feedback=False),
            lambda: setattr(self.publication, "source_validator", lambda source: False),
        ):
            with self.subTest(action=action):
                old = copy.deepcopy(self.plan)
                action()
                with self.assertRaises((PublicationError, ValueError)):
                    self.propose()
                self.plan.clear()
                self.plan.update(old)
                self.publication.source_validator = lambda source: source.startswith("accepted:")
        self.assertEqual(self.provider.writes, [])

    def test_edited_receipt_or_missing_effect_invalidates_checkpoint(self):
        prepared = self.propose()
        receipt = self.publication.execute(prepared["id"], self.audience(prepared))
        changed = copy.deepcopy(receipt)
        changed["actor_id"] = 999
        self.assertFalse(self.publication.validate(prepared["id"], changed))
        self.provider.ev["comments"].clear()
        self.assertFalse(self.publication.validate(prepared["id"], receipt))
        with self.assertRaisesRegex(PublicationError, "no longer validates"):
            self.publication.execute(prepared["id"], self.audience(prepared))
        self.assertEqual(len(self.provider.writes), 1)

    def test_edit_reconciles_response_loss_and_preserves_original_baseline(self):
        prepared = self.propose("edit_pr", {"title": "After", "body": "New description"})
        self.provider.lose_response = True
        with self.assertRaises(OSError):
            self.publication.execute(prepared["id"], self.audience(prepared))
        self.provider.lose_response = False
        self.assertEqual(self.publication.reconcile(prepared["id"])["outcome"], "completed")
        replay = self.propose("edit_pr", {"title": "After", "body": "New description"})
        self.assertEqual(replay["fingerprint"], prepared["fingerprint"])
        self.publication.execute(prepared["id"], self.audience(prepared))
        self.assertEqual(len(self.provider.writes), 1)

    def test_inline_reply_preserves_destination_and_thread_resolution_needs_answer(self):
        source = {"id": 15, "body": "Please fix", "user": {"id": 21}}
        self.provider.ev["inline"] = [copy.deepcopy(source)]
        self.provider.ev["threads"] = [{"id": "thread15", "isResolved": False,
                                         "comments": {"nodes": [{"databaseId": 15}]}}]
        prepared = self.propose("reply_inline", {"body": "Fixed", "comment_id": 15}, source={"kind": "inline", "id": 15, "fingerprint": digest(source)})
        self.publication.execute(prepared["id"], self.audience(prepared))
        resolve = self.propose("resolve_thread", {"thread_id": "thread15", "reply_operation": prepared["id"]}, "resolve-1")
        receipt = self.publication.execute(resolve["id"], self.audience(resolve))
        self.assertTrue(receipt["result"]["isResolved"])
        self.assertEqual(len(self.provider.writes), 2)

    def test_thread_resolution_cannot_use_general_reply(self):
        prepared = self.propose()
        self.publication.execute(prepared["id"], self.audience(prepared))
        resolve = self.propose("resolve_thread", {"thread_id": "thread15", "reply_operation": prepared["id"]}, "resolve-1")
        with self.assertRaisesRegex(PublicationError, "inline answer"):
            self.publication.execute(resolve["id"], self.audience(resolve))
        self.assertEqual(len(self.provider.writes), 1)

    def test_review_request_adopts_existing_set_and_missing_request_stays_unknown(self):
        prepared = self.propose("request_review", {"reviewers": ["reviewer"], "team_reviewers": ["team"]})
        receipt = self.publication.execute(prepared["id"], self.audience(prepared))
        self.assertEqual(len(self.provider.writes), 1)
        self.assertTrue(self.publication.validate(prepared["id"], receipt))
        self.provider.requested = {"users": [], "teams": []}
        self.assertEqual(self.publication.reconcile(prepared["id"])["outcome"], "unknown")
        self.assertFalse(self.publication.validate(prepared["id"], receipt))
        with self.assertRaises(PublicationError):
            self.publication.execute(prepared["id"], self.audience(prepared))
        self.assertEqual(len(self.provider.writes), 1)

    def test_ci_rerun_binds_head_run_attempt_and_retained_budget(self):
        self.plan["options"]["limits"]["ci_reruns_per_head"] = 1
        prepared = self.propose("ci_rerun", {"run_id": 14, "attempt": 1, "reason": "Verified exact-attempt transient failure"})
        self.publication.execute(prepared["id"], self.audience(prepared))
        self.assertEqual(self.provider.run["run_attempt"], 2)
        second = self.propose("ci_rerun", {"run_id": 14, "attempt": 2, "reason": "One more verified retry"}, "rerun-2")
        with self.assertRaisesRegex(PublicationError, "allowance exhausted"):
            self.publication.execute(second["id"], self.audience(second))
        self.assertEqual(len(self.provider.writes), 1)

    def test_merge_requires_separate_authority_and_exact_fresh_readiness(self):
        self.plan["authorizations"].pop("merge")
        with self.assertRaisesRegex(PublicationError, "authorization"):
            self.propose("merge", {"merge_method": "merge"})
        self.plan["authorizations"]["merge"] = AUTH
        prepared = self.propose("merge", {"merge_method": "merge"})
        self.ready["ci"]["head"] = OTHER
        with self.assertRaisesRegex(PublicationError, "exact-head"):
            self.publication.execute(prepared["id"], self.audience(prepared))
        self.ready["ci"]["head"] = HEAD
        self.provider.merge_state["mergeStateStatus"] = "BLOCKED"
        with self.assertRaisesRegex(PublicationError, "requirements"):
            self.publication.execute(prepared["id"], self.audience(prepared))
        self.provider.merge_state["mergeStateStatus"] = "CLEAN"
        receipt = self.publication.execute(prepared["id"], self.audience(prepared))
        self.assertTrue(receipt["result"]["pr"]["merged"])
        self.assertEqual(self.provider.writes[0][2]["sha"], HEAD)

    def test_merge_reconciliation_rejects_wrong_actor_and_parents(self):
        prepared = self.propose("merge", {"merge_method": "merge"})
        self.provider.lose_response = True
        with self.assertRaises(OSError):
            self.publication.execute(prepared["id"], self.audience(prepared))
        self.provider.ev["metadata"]["merged_by"]["id"] = 99
        self.assertEqual(self.publication.reconcile(prepared["id"])["outcome"], "unknown")
        self.provider.ev["metadata"]["merged_by"]["id"] = 11
        self.provider.parents = [OTHER, HEAD]
        self.assertEqual(self.publication.reconcile(prepared["id"])["outcome"], "unknown")
        self.provider.parents = [BASE, HEAD]
        self.assertEqual(self.publication.reconcile(prepared["id"])["outcome"], "completed")

    def test_restart_retains_unknown_creation_and_original_intent(self):
        prepared = self.propose()
        self.provider.delay_creation = self.provider.lose_response = True
        with self.assertRaises(OSError):
            self.publication.execute(prepared["id"], self.audience(prepared))
        self.ledger.close()
        reopened = Ledger(self.root, "fixture", self.pr["url"])
        self.addCleanup(reopened.close)
        publication = Publication(reopened, self.provider, self.root, self.plan,
                                  source_validator=lambda source: True)
        self.provider.lose_response = False
        self.assertEqual(publication.prepare({"id": prepared["id"], "kind": "reply", "head": HEAD,
            "target": BASE, "payload": {"body": "Fixed with a regression fixture."},
            "sources": ["accepted:report-1"]})["fingerprint"], prepared["fingerprint"])
        with self.assertRaises(UncertainEffect):
            publication.execute(prepared["id"], self.audience(prepared))
        self.assertEqual(len(self.provider.writes), 1)

    def test_concurrent_execution_retains_one_dispatch(self):
        prepared = self.propose()
        entered, release, finished = threading.Event(), threading.Event(), threading.Event()
        outcomes = []
        original_request = self.provider.request
        def blocking_request(method, path, payload=None):
            if method == "POST" and path.endswith("/comments"):
                entered.set()
                if not release.wait(5):
                    raise TimeoutError("fixture release missing")
            return original_request(method, path, payload)
        self.provider.request = blocking_request
        def first_dispatch():
            ledger = None
            try:
                ledger = Ledger(self.root, "fixture", self.pr["url"])
                publisher = Publication(ledger, self.provider, self.root, self.plan,
                                        source_validator=lambda source: True)
                outcomes.append(publisher.execute(prepared["id"], self.audience(prepared)))
            except BaseException as error:
                outcomes.append(error)
            finally:
                if ledger:
                    ledger.close()
                finished.set()
        thread = threading.Thread(target=first_dispatch)
        thread.start()
        try:
            self.assertTrue(entered.wait(5), "first dispatch did not reach fake transport")
            with self.assertRaises(UncertainEffect):
                self.publication.execute(prepared["id"], self.audience(prepared))
        finally:
            release.set()
            thread.join(5)
        self.assertFalse(thread.is_alive(), "fixture dispatch did not join")
        self.assertTrue(finished.is_set())
        self.assertEqual(len(outcomes), 1)
        self.assertIsInstance(outcomes[0], dict, outcomes)
        self.assertEqual(len(self.provider.writes), 1)

    def test_real_disposable_git_push_lease_and_rewrite_guards(self):
        source, remote = self.root / "checkout", self.root / "remote.git"
        source.mkdir()
        def git(root, *args):
            result = subprocess.run(["git", "-c", "commit.gpgsign=false", *args], cwd=root, stdout=subprocess.PIPE,
                                    stderr=subprocess.PIPE, check=True, timeout=10)
            return result.stdout.decode().strip()
        git(self.root, "init", "--bare", str(remote))
        git(source, "init")
        (source / "tracked.txt").write_text("first\n")
        git(source, "add", "tracked.txt")
        git(source, "-c", "user.name=Fixture", "-c", "user.email=fixture@example.invalid", "commit", "-m", "Initial fixture")
        git(source, "branch", "-M", "feature")
        initial = git(source, "rev-parse", "HEAD")
        git(source, "push", str(remote), "HEAD:refs/heads/feature")
        (source / "tracked.txt").write_text("second\n")
        git(source, "add", "tracked.txt")
        git(source, "-c", "user.name=Fixture", "-c", "user.email=fixture@example.invalid", "commit", "-m", "Advance fixture")
        desired = git(source, "rev-parse", "HEAD")
        self.provider.ev["metadata"]["head"]["sha"] = initial
        self.provider.ev["metadata"]["base"]["sha"] = initial
        class LocalPublication(Publication):
            def _git_remote(self, intent):
                return str(remote)
        publication = LocalPublication(self.ledger, self.provider, source, self.plan,
                                       source_validator=lambda source: True)
        prepared = publication.prepare({"id": "git-push", "kind": "push", "head": desired,
            "lease": initial, "target": initial, "payload": {"new_head": desired},
            "sources": ["accepted:report-1"]})
        receipt = publication.execute(prepared["id"], self.audience(prepared))
        self.assertEqual(receipt["result"]["head"], desired)
        self.assertEqual(git(self.root, "--git-dir", str(remote), "rev-parse", "refs/heads/feature"), desired)
        self.assertTrue(publication.validate(prepared["id"], receipt))
        self.provider.ev["metadata"]["head"]["sha"] = desired
        split = publication.prepare({"id": "split-push", "kind": "split_push", "head": desired,
            "target": initial, "payload": {"branch": "split/fixture", "new_head": desired,
                "paths": ["tracked.txt"], "reason": "Accepted independent fixture boundary"},
            "sources": ["accepted:report-1"]})
        publication.execute(split["id"], self.audience(split))
        self.assertEqual(git(self.root, "--git-dir", str(remote), "rev-parse", "refs/heads/split/fixture"), desired)
        self.provider.split_sha = desired
        draft = publication.prepare({"id": "split-draft", "kind": "split_pr", "head": desired,
            "target": initial, "payload": {"title": "Isolated fixture", "body": "Accepted split scope",
                "head": "o:split/fixture", "base": "main", "draft": True, "push_operation": split["id"]},
            "sources": ["accepted:report-1"]})
        draft_receipt = publication.execute(draft["id"], self.audience(draft))
        self.assertEqual(draft_receipt["result"]["number"], 8)
        self.assertTrue(publication.validate(draft["id"], draft_receipt))
        # Deleting a created draft is never proof that it was not created.
        self.provider.split_pulls.clear()
        self.assertEqual(publication.reconcile(draft["id"])["outcome"], "unknown")
        # A stale lease cannot roll the remote back, even with a valid desired
        # commit and explicitly configured rewrite permission.
        self.provider.ev["metadata"]["head"]["sha"] = desired
        rewrite = publication.prepare({"id": "git-rewrite", "kind": "rewrite", "head": initial,
            "lease": desired, "target": initial, "payload": {"new_head": initial},
            "sources": ["accepted:report-1"]})
        git(self.root, "--git-dir", str(remote), "update-ref", "refs/heads/feature", initial)
        with self.assertRaisesRegex(PublicationError, "local head|remote branch"):
            publication.execute(rewrite["id"], self.audience(rewrite))
        self.assertEqual(git(self.root, "--git-dir", str(remote), "rev-parse", "refs/heads/feature"), initial)

    def test_read_only_ledger_can_reconcile_and_validate(self):
        prepared = self.propose()
        receipt = self.publication.execute(prepared["id"], self.audience(prepared))
        readonly = Ledger(self.root, "fixture", self.pr["url"], read_only=True)
        self.addCleanup(readonly.close)
        publication = Publication(readonly, self.provider, self.root, self.plan)
        self.assertEqual(publication.reconcile(prepared["id"])["outcome"], "completed")
        self.assertTrue(publication.validate(prepared["id"], receipt))
        with self.assertRaises((ValueError, RuntimeError, sqlite3.OperationalError)):
            publication.prepare({"kind": "reply", "head": HEAD, "target": BASE,
                                 "payload": {"body": "Another"}, "sources": ["accepted:report"]})


if __name__ == "__main__":
    unittest.main()
