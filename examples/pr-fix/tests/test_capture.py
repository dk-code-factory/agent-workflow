import copy
import json
import os
from pathlib import Path
import subprocess
import sys
import tempfile
import unittest
from unittest.mock import Mock, patch
from urllib.parse import parse_qs, urlsplit
import git_fixture

sys.path.insert(0, str(Path(__file__).resolve().parents[1] / "workflows" / "helpers"))
from prfix import capture as module
from prfix.core import GENERATED, STATE, owned_directory


PR = module.identity("https://github.com/owner/repo/pull/17")


def git(root, *args):
    return subprocess.run(git_fixture.command(*args), cwd=root, check=True, timeout=15,
                          env=git_fixture.environment(),
                          stdout=subprocess.PIPE, stderr=subprocess.PIPE).stdout.decode().strip()


class Fixture:
    def __init__(self, root):
        self.root = root
        git(root, "init", "-b", "main")
        git(root, "config", "user.email", "fixture@example.invalid")
        git(root, "config", "user.name", "Fixture")
        git(root, "config", "commit.gpgsign", "false")
        (root / "file.txt").write_text("base\n")
        (root / "AGENTS.md").write_text("Keep evidence.\n")
        git(root, "add", ".")
        git(root, "commit", "-m", "Initial")
        self.target = git(root, "rev-parse", "HEAD")
        git(root, "switch", "-c", "fix")
        (root / "file.txt").write_text("change\n")
        git(root, "commit", "-am", "Change")
        self.head = git(root, "rev-parse", "HEAD")
        self.metadata = {"html_url": PR["url"], "number": 17, "state": "open", "merged": False,
                         "draft": False, "title": "Fix", "body": "Fix a problem", "updated_at": "now",
                         "user": {"id": 1, "login": "owner"},
                         "head": {"sha": self.head, "ref": "fix", "repo": {"full_name": "owner/repo"}},
                         "base": {"sha": self.target, "ref": "main", "repo": {"full_name": "owner/repo"}}}
        self.evidence = {"metadata": self.metadata, "viewer": {"id": 1, "login": "owner"},
                         "comments": [], "reviews": [], "inline": [], "threads": []}
        self.plan = {"pr": PR, "logical_run": "fixture", "instructions": "Fix issue.",
                     "options": {}, "resources": {}, "definitions": {}}

    def capture(self, identity):
        if identity != PR:
            raise AssertionError("wrong PR")
        self.last_capture = copy.deepcopy(self.evidence)
        return self.last_capture


class CaptureTests(unittest.TestCase):
    def setUp(self):
        git_fixture.isolate(self)
        self.temp = tempfile.TemporaryDirectory(prefix="portable-pr-capture-")
        self.addCleanup(self.temp.cleanup)
        self.root = Path(self.temp.name)
        self.fixture = Fixture(self.root)

    def capture(self, **kwargs):
        return module.capture(self.root, self.fixture, self.fixture.plan, **kwargs)

    def test_canonical_identity_rejects_routes_and_credentials(self):
        for value in ("http://github.com/owner/repo/pull/17", PR["url"] + "?q=x", PR["url"] + "/",
                      "https://user@github.com/owner/repo/pull/17", "https://github.com/../repo/pull/17",
                      "https://github.com/owner/repo/pull/017", "https://github.com/owner%2frepo/repo/pull/17"):
            with self.subTest(value=value), self.assertRaises(ValueError):
                module.identity(value)

    def test_clean_capture_and_untracked_bytes_invalidate_review(self):
        first = self.capture(clean=True)
        self.assertEqual(first["paths"], ["file.txt"])
        self.assertFalse(first["dirty"])
        (self.root / "new.txt").write_text("one")
        second = self.capture()
        (self.root / "new.txt").write_text("two")
        third = self.capture()
        self.assertIn("new.txt", third["paths"])
        self.assertNotEqual(first["diff"], second["diff"])
        self.assertNotEqual(second["diff"], third["diff"])
        with self.assertRaisesRegex(ValueError, "clean worktree"):
            self.capture(clean=True)

    def test_tree_preserving_history_cleanup_reuses_content_only(self):
        task = {"id": "review", "definition_digest": "definition"}
        first = self.capture()
        git(self.root, "commit", "--amend", "-m", "Changed history only")
        self.fixture.metadata["head"]["sha"] = git(self.root, "rev-parse", "HEAD")
        self.fixture.metadata["updated_at"] = "later"
        second = self.capture()
        self.assertNotEqual(first["head"], second["head"])
        self.assertEqual(module.fingerprint(first, task), module.fingerprint(second, task))
        for key in ("history", "head_sensitive"):
            specific = dict(task, **{key: True})
            self.assertNotEqual(module.fingerprint(first, specific), module.fingerprint(second, specific))

    def test_changed_instructions_configuration_and_selected_resources_invalidate(self):
        task = {"id": "review", "definition_digest": "definition", "resources": ["rules"]}
        (self.root / ".gitignore").write_text("rules\nother\n")
        (self.root / "rules").write_text("a")
        (self.root / "other").write_text("a")
        self.fixture.plan["resources"] = {"rules": "prepared", "other": "prepared"}
        first = self.capture()
        (self.root / "other").write_text("b")
        self.fixture.plan["options"]["tasks"] = ["another"]
        sibling = self.capture()
        self.assertEqual(module.fingerprint(first, task), module.fingerprint(sibling, task))
        (self.root / "rules").write_text("b")
        self.assertNotEqual(module.fingerprint(first, task), module.fingerprint(self.capture(), task))
        self.fixture.plan["instructions"] = "New exact user instruction."
        self.assertNotEqual(first["instructions"], self.capture()["instructions"])
        self.fixture.plan["options"]["governing_rule"] = "Changed"
        self.assertNotEqual(first["settings"], self.capture()["settings"])

    def test_local_head_mismatch_requires_exact_retained_commit(self):
        git(self.root, "commit", "--allow-empty", "-m", "Local")
        local = git(self.root, "rev-parse", "HEAD")
        with self.assertRaisesRegex(ValueError, "retained local commit"):
            self.capture()
        self.fixture.plan["allow_local_head"] = local
        self.assertEqual(self.capture()["local_head"], local)
        self.fixture.plan["allow_local_head"] = True
        with self.assertRaises(ValueError):
            self.capture()

    def test_state_exclusions_require_application_marker(self):
        for directory in (STATE, GENERATED):
            owned_directory(self.root, directory, create=True)
            (self.root / directory / "private.json").write_text("private")
        self.assertFalse(self.capture(clean=True)["dirty"])
        (self.root / GENERATED / ".owned-by-portable-pr-fix").write_text("{}")
        with self.assertRaisesRegex(ValueError, "ownership marker"):
            self.capture()

    def test_owned_marker_cannot_hide_tracked_source(self):
        owned_directory(self.root, STATE, create=True)
        (self.root / STATE / "source.py").write_text("tracked = True")
        git(self.root, "add", STATE)
        with self.assertRaisesRegex(ValueError, "tracked files"):
            self.capture()

    def test_untracked_special_files_and_aggregate_bound_fail_closed(self):
        path = self.root / "pipe"
        os.mkfifo(path)
        try:
            with self.assertRaisesRegex(ValueError, "regular files"):
                module._untracked(self.root, b"pipe\0")
        finally:
            path.unlink()
        (self.root / "oversized").write_bytes(b"a" * 17)
        with patch.object(module, "MAX_FILE_BYTES", 16):
            with self.assertRaisesRegex(ValueError, "64 MiB"):
                module._untracked(self.root, b"oversized\0")

    def test_transport_bounds_output_and_timeout(self):
        with self.assertRaisesRegex(ValueError, "output exceeds"):
            module.run(self.root, [sys.executable, "-c", "import sys;sys.stdout.write('x'*10000)"], limit=32)
        with self.assertRaises(TimeoutError):
            module.run(self.root, [sys.executable, "-c", "import time;time.sleep(5)"], timeout=.05)
        with patch.object(module, "MAX_STDERR", 32):
            with self.assertRaisesRegex(ValueError, "output exceeds"):
                module.run(self.root, [sys.executable, "-c", "import sys;sys.stderr.write('x'*10000)"])
        data = b"x" * (512 << 10)
        self.assertEqual(module.run(self.root, [sys.executable, "-c",
            "import sys;sys.stdout.buffer.write(sys.stdin.buffer.read())"], data), data)
        with self.assertRaisesRegex(RuntimeError, "exited with status") as failed:
            module.run(self.root, [sys.executable, "-c",
                "import sys;sys.stderr.write('SECRET_CREDENTIAL');sys.exit(1)"])
        self.assertNotIn("SECRET_CREDENTIAL", str(failed.exception))

    def test_exited_group_permission_race_preserves_output_limit_failure(self):
        launch = subprocess.Popen
        children = []

        def start(*args, **kwargs):
            child = launch(*args, **kwargs)
            children.append(child)
            return child

        def exited_group(*args):
            children[-1].wait(timeout=5)
            raise PermissionError("exited process group")

        with patch.object(module.subprocess, "Popen", side_effect=start), \
                patch.object(module.os, "killpg", side_effect=exited_group):
            with self.assertRaisesRegex(ValueError, "output exceeds"):
                module.run(self.root, [sys.executable, "-c",
                    "import sys;sys.stdout.write('x'*10000)"], limit=32)

    def test_live_process_group_permission_failure_is_not_suppressed(self):
        child = Mock(pid=123)
        child.poll.return_value = None
        with patch.object(module.os, "killpg", side_effect=PermissionError):
            with self.assertRaises(PermissionError):
                module._kill_process_group(child)


class GitHubTests(unittest.TestCase):
    def setUp(self):
        self.provider = module.GitHub(Path.cwd())

    def test_request_rejects_foreign_urls_and_option_routes(self):
        for route in ("https://evil.invalid/a", "//evil/a", "-H", "/repos/a", "repos/a/../b", "repos/%2e%2e"):
            with self.subTest(route=route), self.assertRaises(ValueError):
                self.provider.request("GET", route)

    def test_explicit_host_and_duplicate_json_key_handling(self):
        enterprise = module.identity("https://git.example.invalid/owner/repo/pull/17")
        self.provider._bind(enterprise)
        with patch.object(module, "run", return_value=b"{}") as transport:
            self.provider.request("GET", "repos/owner/repo/pulls/17")
            self.assertIn("git.example.invalid", transport.call_args.args[1])
        with self.assertRaisesRegex(ValueError, "duplicate"):
            module._decode(b'{"id":1,"id":2}')

    def test_enterprise_token_mapping_stays_in_process_environment(self):
        enterprise = module.identity("https://git.example.invalid/owner/repo/pull/17")
        self.provider._bind(enterprise)
        with patch.dict(os.environ, {"GH_TOKEN": "runtime-secret"}, clear=True):
            with patch.object(module, "run", return_value=b"{}") as transport:
                self.provider.request("GET", "repos/owner/repo/pulls/17")
                environment = transport.call_args.kwargs["env"]
                self.assertEqual(environment["GH_ENTERPRISE_TOKEN"], "runtime-secret")
                self.assertNotIn("runtime-secret", transport.call_args.args[1])
        with patch.dict(os.environ, {"GH_TOKEN": "runtime-secret", "GH_ENTERPRISE_TOKEN": "explicit-enterprise"}, clear=True):
            self.assertIsNone(self.provider._environment())

    def test_pagination_is_exhaustive_and_rejects_overlap_or_changed_total(self):
        rows = [{"id": n} for n in range(101)]
        def request(method, route, payload=None):
            if parse_qs(urlsplit(route).query)["page"] == ["1"]:
                return {"total_count": 101, "items": rows[:100]}
            return {"total_count": 101, "items": rows[100:]}
        self.provider.request = request
        self.assertEqual(self.provider.pages("repos/owner/repo/items", key="items"), rows)
        self.provider.request = lambda *args: {"total_count": 101, "items": rows[:100]}
        with self.assertRaisesRegex(ValueError, "duplicate"):
            self.provider.pages("repos/owner/repo/items", key="items")
        self.provider.request = request
        self.assertEqual(self.provider.pages("repos/owner/repo/items?per_page=100", key="items"), rows)
        def changed_total(method, route, payload=None):
            response = request(method, route, payload)
            if parse_qs(urlsplit(route).query)["page"] == ["2"]:
                response["total_count"] = 102
            return response
        self.provider.request = changed_total
        with self.assertRaisesRegex(ValueError, "changed during"):
            self.provider.pages("repos/owner/repo/items", key="items")

    def test_thread_capture_requires_full_enumeration_and_stable_pr(self):
        metadata = {"html_url": PR["url"], "number": 17, "head": {"sha": "a" * 40},
                    "base": {"sha": "b" * 40}, "state": "open", "updated_at": "now"}
        thread = {"id": "thread", "isResolved": False, "isOutdated": False,
                  "comments": {"nodes": [{"id": "node", "databaseId": 4}]}}
        def request(method, route, payload=None):
            if route == "user":
                return {"id": 1, "login": "owner"}
            if route == "graphql":
                return {"data": {"repository": {"pullRequest": {"reviewThreads": {
                    "totalCount": 1, "nodes": [thread],
                    "pageInfo": {"hasNextPage": False, "endCursor": None}}}}}}
            return copy.deepcopy(metadata)
        self.provider.request = request
        self.provider.pages = lambda route, **kwargs: []
        result = self.provider.capture(PR)
        self.assertEqual(result["threads"], [thread])
        original = request
        def changed(method, route, payload=None):
            response = original(method, route, payload)
            if route == "graphql":
                response["data"]["repository"]["pullRequest"]["reviewThreads"]["totalCount"] = 2
            return response
        self.provider.request = changed
        with self.assertRaisesRegex(ValueError, "incomplete"):
            self.provider.capture(PR)

    def test_ci_checks_and_statuses_bind_exact_current_head(self):
        head = "a" * 40
        check = {"id": 9, "name": "Test", "head_sha": head, "status": "completed",
                 "conclusion": "success", "html_url": PR["url"],
                 "details_url": "https://github.com/owner/repo/actions/runs/2/job/3"}
        self.provider.pages = lambda route, **kwargs: [check] if "check-runs" in route else [
            {"id": 8, "context": "Legacy", "state": "success"},
            {"id": 7, "context": "Legacy", "state": "failure"}]
        def request(method, route, payload=None):
            if "/actions/jobs/" in route:
                return {"id": 3, "run_id": 2, "head_sha": head, "run_attempt": 4,
                        "check_run_url": "https://api.github.com/repos/owner/repo/check-runs/9"}
            return {"head": {"sha": head}}
        self.provider.request = request
        result = self.provider.observe_ci(PR, head)
        self.assertEqual(len(result["checks"]), 2)
        self.assertEqual(result["checks"][0]["attempt"], 4)
        self.provider.request = lambda *args: {"head": {"sha": "b" * 40}}
        with self.assertRaises(ValueError):
            self.provider.observe_ci(PR, head)


if __name__ == "__main__":
    unittest.main()
