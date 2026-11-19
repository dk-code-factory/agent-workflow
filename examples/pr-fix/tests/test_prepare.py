import hashlib
import json
from pathlib import Path
import sys
import tempfile
import unittest
from unittest.mock import patch
import git_fixture

sys.path.insert(0, str(Path(__file__).resolve().parents[1] / "workflows" / "helpers"))
from prfix import core
from prfix.prepare import prepare
from prepared_fixture import DEFAULT_ADAPTERS
from test_capture import Fixture, PR, git


class PrepareTests(unittest.TestCase):
    def setUp(self):
        git_fixture.isolate(self)
        self.temp = tempfile.TemporaryDirectory(prefix="portable-pr-prepare-")
        self.addCleanup(self.temp.cleanup)
        self.root = Path(self.temp.name)
        self.fixture = Fixture(self.root)
        (self.root / "roles").mkdir()
        roles = {}
        for name in ("leaf", "reviewer", "verifier", "fixer", "audience"):
            path = self.root / "roles" / (name + ".toml")
            path.write_text('name = "' + name + '"\ndescription = "Fixture task"\n'
                            'developer_instructions = "Retain evidence and follow assigned scope."\n')
            roles[name] = path.relative_to(self.root).as_posix()
        git(self.root, "add", "roles")
        git(self.root, "commit", "-m", "Declare native roles")
        self.fixture.metadata["head"]["sha"] = git(self.root, "rev-parse", "HEAD")
        self.request = {"pr": PR["url"], "instructions": "Fix the selected issue.",
                        "image": "ghcr.io/fixture/python-git-gh@sha256:" + "a" * 64,
                        "roles": roles, "options": {"tasks": [], "finalizers": []},
                        "settings": {"scope": "selected issue only"}, "authorizations": {}}

    def prepare(self, request=None, controls=None, fingerprint="fixture-execution"):
        original = request or {key: value for key, value in self.request.items()
                               if key in {"pr", "instructions", "authorizations", "logical_run"}}
        selected = {"adapters": DEFAULT_ADAPTERS, **(controls if controls is not None else
                    {**self.request["options"], "settings": self.request["settings"]})}
        from prfix.prepare import _definition
        definitions = {key: _definition(self.root, value) for key, value in self.request["roles"].items()}
        result = prepare(self.root, original, selected, fingerprint, self.fixture, definitions)
        return result, core.read_artifact(self.root, result["plan"])

    def test_empty_selections_prepare_only_immutable_data(self):
        before = {path.relative_to(self.root): path.read_bytes() for path in self.root.rglob("*")
                  if path.is_file() and ".git" not in path.parts}
        result, plan = self.prepare()
        self.assertEqual(plan["tasks"], [])
        self.assertEqual(result["stages"], [])
        self.assertEqual(plan["fingerprint"], core.digest({k: v for k, v in plan.items() if k != "fingerprint"}))
        self.assertEqual(set(result), {"logical_run", "plan", "stages", "limits", "turns", "merge_authorized"})
        self.assertFalse((self.root / core.GENERATED).exists())
        after = {path.relative_to(self.root): path.read_bytes() for path in self.root.rglob("*")
                 if path.is_file() and ".git" not in path.parts and core.STATE not in path.parts}
        self.assertEqual(before, after)

    def test_request_no_longer_contains_deployment_or_orchestration(self):
        for field in ("image", "runtime", "options", "roles", "settings", "adapters", "resources"):
            request = {"pr": PR["url"], "instructions": "Fix this", field: {}}
            with self.subTest(field=field), self.assertRaisesRegex(ValueError, "YAML inputs"):
                self.prepare(request)

    def test_order_occurrences_parallel_groups_and_finalizers_are_stable_data(self):
        controls = {"tasks": ["dk-workflows:dk-pr-description", ["dk-workflows:dk-pr-feedback-read", "dk-workflows:dk-pr-decisions"],
                              "dk-workflows:dk-pr-publish", "dk-workflows:dk-pr-description"],
                    "finalizers": ["dk-workflows:dk-commit-split"], "limits": {"review_cycles": 1}}
        result, plan = self.prepare(controls=controls)
        self.assertEqual([stage["kind"] for stage in result["stages"]], ["tasks", "tasks", "publication", "tasks", "tasks"])
        self.assertEqual([stage["id"] for stage in result["stages"]], ["stage_" + str(n) for n in range(1, 6)])
        self.assertEqual(result["stages"][1]["concurrency"], 2)
        self.assertEqual(result["stages"][-1]["phase"], "finalizers")
        self.assertEqual([task["id"] for task in plan["tasks"]], ["task_" + str(n) for n in range(1, 6)])
        self.assertEqual(plan["tasks"][0]["skill"], plan["tasks"][3]["skill"])
        self.assertNotEqual(plan["tasks"][0]["id"], plan["tasks"][3]["id"])

    def test_review_groups_declare_independent_roles_without_inserting_execution_stages(self):
        controls = {"tasks": ["dk-workflows:dk-pr-reviewers-run"], "finalizers": [],
                    "reviewers": [["local:review", "local:review"], "local:review"],
                    "adapters": {"local:review": {"kind": "review", "definition": "reviewer", "assignment": "Inspect sources"}}}
        result, plan = self.prepare(controls=controls)
        self.assertEqual(len(result["stages"]), 1)
        self.assertEqual(result["stages"][0]["kind"], "reviews")
        self.assertEqual([g["concurrency"] for g in result["stages"][0]["groups"]], [2, 1])
        self.assertEqual([t["definition"] for t in plan["tasks"]], ["reviewer"] * 3 + ["verifier", "fixer"])
        self.assertFalse(any("uses" in stage or "jobs" in stage for stage in result["stages"]))

    def test_managed_stages_preserve_outer_settings_and_resources(self):
        controls = {"tasks": ["dk-workflows:dk-pr-publish", "dk-workflows:dk-ci"],
                    "finalizers": [], "settings": {"scope": "retained configuration"},
                    "resources": ["roles/leaf.toml"]}
        _, plan = self.prepare(controls=controls)
        self.assertEqual(plan["settings"], controls["settings"])
        self.assertEqual(plan["resources"], {"roles/leaf.toml":
                         hashlib.sha256((self.root / "roles/leaf.toml").read_bytes()).hexdigest()})

    def test_parallel_writers_and_invalid_managed_controls_are_rejected(self):
        controls = {"tasks": [["dk-workflows:dk-pr-feedback-address", "dk-workflows:dk-pr-description"]]}
        with self.assertRaisesRegex(ValueError, "parallel writing"):
            self.prepare(controls=controls)
        for skill in ("dk-workflows:dk-pr-publish", "dk-workflows:dk-ci", "dk-workflows:dk-pr-reviewers-run"):
            for extras in ({"instructions": "ignored"}, {"required": False}, {"options": {"provider": "ignored"}}):
                with self.subTest(skill=skill, extras=extras), self.assertRaisesRegex(ValueError, "managed stage controls"):
                    self.prepare(controls={"tasks": [{"skill": skill, **extras}]})

    def test_invalid_limits_turns_and_adapters_fail_before_dispatch(self):
        for limits in ({"review_cycles": 0}, {"review_cycles": 11}, {"retries_per_task": 2},
                       {"ci_repairs_per_cycle": 6}, {"ci_reruns_per_head": True}, {"extra": 1}):
            with self.subTest(limits=limits), self.assertRaisesRegex(ValueError, "allowance"):
                self.prepare(controls={"limits": limits})
        for turns in (True, -1, 4, "8", 2147483648):
            with self.subTest(turns=turns), self.assertRaisesRegex(ValueError, "five retained turns"):
                self.prepare(controls={"turns": turns})
        with self.assertRaisesRegex(ValueError, "unavailable selected adapter"):
            self.prepare(controls={"tasks": ["unknown"]})
        with self.assertRaisesRegex(ValueError, "reviewers cannot write"):
            self.prepare(controls={"tasks": ["local:review"], "adapters": {"local:review":
                {"kind": "review", "writes": True, "assignment": "Bad adapter"}}})

    def test_writing_tasks_require_repository_local_git_identity(self):
        for key in ("user.name", "user.email"):
            original = git(self.root, "config", "--local", "--get", key)
            git(self.root, "config", "--local", "--unset", key)
            try:
                with self.subTest(key=key), self.assertRaisesRegex(ValueError, "repository-local Git"):
                    self.prepare(controls={"tasks": ["dk-workflows:dk-pr-feedback-address"]})
                self.prepare()  # Read-only plans do not require commit identity.
            finally:
                git(self.root, "config", "--local", key, original)

    def test_changed_inputs_and_configuration_preserve_budgets(self):
        first, plan = self.prepare()
        with core.Ledger(self.root, first["logical_run"], PR["url"]) as ledger:
            ledger.reserve("review_cycles", "original", 1)
        second, _ = self.prepare(fingerprint="changed-admitted-config")
        third, _ = self.prepare(request={"pr": PR["url"], "instructions": "Changed exact instruction"})
        self.assertEqual(first["logical_run"], second["logical_run"])
        self.assertEqual(second["logical_run"], third["logical_run"])
        self.assertEqual(len({first["plan"], second["plan"], third["plan"]}), 3)
        with core.Ledger(self.root, third["logical_run"], PR["url"]) as ledger:
            with self.assertRaisesRegex(ValueError, "exhausted"):
                ledger.reserve("review_cycles", "replacement", 1)

    def test_only_explicit_merge_authorization_enables_merge_output(self):
        result, _ = self.prepare()
        self.assertFalse(result["merge_authorized"])
        request = {"pr": PR["url"], "instructions": "Fix and merge", "authorizations": {"merge":
                   {"request_id": "merge", "instruction": "Merge this PR", "source": "user:fixture"}}}
        result, _ = self.prepare(request=request)
        self.assertTrue(result["merge_authorized"])

    def test_unknown_state_files_remain_untouched(self):
        self.prepare()
        sentinel = self.root / core.STATE / "unrelated.txt"
        sentinel.write_text("Preserve this")
        self.prepare()
        self.assertEqual(sentinel.read_text(), "Preserve this")

class PreparationOperationTests(unittest.TestCase):
    def setUp(self):
        git_fixture.isolate(self)
        self.temp = tempfile.TemporaryDirectory(prefix="prfix-preparation-")
        self.addCleanup(self.temp.cleanup)
        self.root = Path(self.temp.name)
        self.provider = Fixture(self.root)
        self.request_path = self.root / ".git" / "request.json"
        self.original = {"pr": PR["url"], "instructions": "Original exact request"}
        self.request_path.write_text(json.dumps(self.original))

    def handle(self, operation, phase, inputs, checkpoint=None, execution="fixture-admission"):
        import action
        request = {"protocol": "action-v1", "phase": phase, "input": inputs,
                   "context": {"operation_key": operation + ":fixture", "execution_fingerprint": execution}}
        if checkpoint is not None:
            request["checkpoint"] = {"result": checkpoint}
        return action.handle(operation, request, self.root, self.provider)

    def test_snapshot_survives_response_loss_and_request_replacement(self):
        inputs = {"request": ".git/request.json"}
        self.handle("snapshot_request", "bindings", inputs)
        self.assertFalse((self.root / core.STATE).exists())
        self.assertEqual(self.handle("snapshot_request", "reconcile", inputs)["outcome"], "no_effect")
        result = self.handle("snapshot_request", "execute", inputs)["result"]
        self.request_path.write_text(json.dumps({**self.original, "instructions": "Changed later"}))
        self.assertEqual(self.handle("snapshot_request", "reconcile", inputs)["result"], result)
        self.assertTrue(self.handle("snapshot_request", "validate_checkpoint", inputs, result)["valid"])
        self.assertEqual(core.read_artifact(self.root, result["output"]["snapshot"]), self.original)
        with self.assertRaisesRegex(ValueError, "identity conflict"):
            self.handle("snapshot_request", "execute", inputs, execution="changed-config")

    def test_plan_preparation_retains_result_without_generated_definitions(self):
        snapshot = self.handle("snapshot_request", "execute", {"request": ".git/request.json"})["result"]["output"]["snapshot"]
        inputs = {"snapshot": snapshot, "controls": {"tasks": [], "finalizers": []}}
        result = self.handle("prepare", "execute", inputs)["result"]
        self.assertEqual(self.handle("prepare", "reconcile", inputs)["result"], result)
        self.assertTrue(self.handle("prepare", "validate_checkpoint", inputs, result)["valid"])
        plan = core.read_artifact(self.root, result["output"]["plan"])
        self.assertEqual(plan["execution_fingerprint"], "fixture-admission")
        self.assertFalse((self.root / core.GENERATED).exists())
        self.assertFalse(any(self.root.rglob("*.yml")))

    def test_interrupted_preparation_can_retry_only_local_immutable_work(self):
        import prfix.preparation as module
        real = module.atomic_json
        failed = False
        def interrupted(path, value):
            nonlocal failed
            if "preparations" in Path(path).parts and not str(path).endswith(".request.json") and not failed:
                failed = True
                raise OSError("lost receipt checkpoint")
            return real(path, value)
        inputs = {"request": ".git/request.json"}
        with patch.object(module, "atomic_json", side_effect=interrupted), self.assertRaisesRegex(OSError, "lost receipt"):
            self.handle("snapshot_request", "execute", inputs)
        self.assertEqual(self.handle("snapshot_request", "reconcile", inputs)["outcome"], "no_effect")
        self.request_path.write_text(json.dumps({**self.original, "instructions": "Changed during interruption"}))
        result = self.handle("snapshot_request", "execute", inputs)["result"]
        self.assertEqual(core.read_artifact(self.root, result["output"]["snapshot"]), self.original)
        self.assertTrue(self.handle("snapshot_request", "validate_checkpoint", inputs, result)["valid"])


if __name__ == "__main__":
    unittest.main()
