import copy
import hashlib
import json
import os
from pathlib import Path
import sys
import tempfile
import unittest
from unittest.mock import patch
from importlib import import_module
import git_fixture

sys.path.insert(0, str(Path(__file__).resolve().parents[1] / "workflows" / "helpers"))
from prfix import core
from prfix.prepare import prepare
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

    def prepare(self, request=None):
        result = prepare(self.root, request or self.request, provider=self.fixture)
        return result, core.read_artifact(self.root, result["plan"])

    def test_empty_selected_plan_still_has_live_completion_and_pinned_closure(self):
        result, plan = self.prepare()
        self.assertEqual(plan["tasks"], [])
        self.assertEqual(plan["stages"], [])
        self.assertEqual(plan["fingerprint"], core.digest({k: v for k, v in plan.items() if k != "fingerprint"}))
        self.assertEqual(result["inputs"], {"plan": result["plan"]})
        directory = (self.root / result["workflow"]).parent
        manifest = json.loads((directory / "manifest.json").read_text())
        self.assertEqual(manifest["plan"], result["plan"])
        for name, expected in manifest["files"].items():
            self.assertEqual(hashlib.sha256((directory / name).read_bytes()).hexdigest(), expected, name)
        main = json.loads((directory / "main.yml").read_text())
        self.assertIn("completion", main["jobs"])
        self.assertEqual(main["jobs"]["completion"]["steps"][0]["argv"][-1], "completion")

    def test_sample_native_write_policy_reaches_generated_runtime(self):
        sample = json.loads((Path(__file__).resolve().parents[1] / "request.example.json").read_text())
        self.request["runtime"] = sample["runtime"]
        self.request["options"]["tasks"] = ["dk-workflows:dk-pr-feedback-address"]
        result, plan = self.prepare()
        self.assertTrue(any(task["writes"] for task in plan["tasks"]))
        configuration = json.loads((self.root / result["config"]).read_text())
        self.assertEqual(configuration["agents"]["codex"]["sandbox"], "workspace-write")
        config = json.loads((self.root / result["config"]).read_text())
        self.assertEqual(config["execution"]["image"], self.request["image"])
        self.assertEqual(config["execution"]["network"], "bridge")
        self.assertEqual(config["agents"]["codex"]["provider"], "codex-app-server")
        self.assertEqual(config["agents"]["codex"]["execution"], {"mode": "local"})

    def test_writer_preparation_requires_local_identity_despite_host_defaults(self):
        self.request["options"]["tasks"] = ["dk-workflows:dk-pr-feedback-address"]
        global_config = self.root / ".git" / "host-global-config"
        global_config.write_text('[user]\nname = Host User\nemail = host@example.invalid\n')
        for key, value in (("user.name", "Fixture"), ("user.email", "fixture@example.invalid")):
            git(self.root, "config", "--local", "--unset", key)
            try:
                with self.subTest(key=key), patch.dict(os.environ, {
                    "GIT_CONFIG_GLOBAL": str(global_config),
                    "GIT_AUTHOR_NAME": "Host Author", "GIT_AUTHOR_EMAIL": "author@example.invalid",
                    "GIT_COMMITTER_NAME": "Host Committer", "GIT_COMMITTER_EMAIL": "committer@example.invalid",
                }), self.assertRaisesRegex(ValueError, "repository-local Git user.name and user.email"):
                    self.prepare()
            finally:
                git(self.root, "config", "--local", key, value)

    def test_read_only_plan_does_not_require_git_commit_identity(self):
        git(self.root, "config", "--local", "--unset", "user.name")
        git(self.root, "config", "--local", "--unset", "user.email")
        _, plan = self.prepare()
        self.assertEqual(plan["tasks"], [])

    def test_writer_preparation_uses_local_identity_with_private_home(self):
        self.request["options"]["tasks"] = ["dk-workflows:dk-pr-feedback-address"]
        private_home = self.root / ".git" / "private-helper-home"
        private_home.mkdir()
        with patch.dict(os.environ, {"HOME": str(private_home)}):
            _, plan = self.prepare()
        self.assertTrue(any(task["writes"] for task in plan["tasks"]))

    def test_review_plan_has_unique_tasks_independent_verifier_and_session_continuation(self):
        self.request["options"] = {"tasks": ["dk-workflows:dk-pr-reviewers-run"], "finalizers": [],
                                   "reviewers": [{"skill": "local:review"}]}
        self.request["adapters"] = {"local:review": {"kind": "review", "writes": False,
            "definition": "reviewer", "assignment": "Review the actual changes."}}
        result, plan = self.prepare()
        ids = [task["id"] for task in plan["tasks"]]
        self.assertEqual(len(ids), len(set(ids)))
        self.assertEqual({task["kind"] for task in plan["tasks"]}, {"review", "verification", "fix"})
        directory = (self.root / result["workflow"]).parent
        native_references = []
        local_references = []
        def walk(value):
            if isinstance(value, dict):
                if value.get("uses") == "agent/run":
                    native_references.append(value["with"]["agent"])
                    self.assertEqual(value["with"]["output"]["repair-attempts"], 0)
                    self.assertEqual(value["with"]["output"]["schema"]["properties"]["complete"], {"type": "boolean"})
                elif "uses" in value:
                    local_references.append(value["uses"])
                for child in value.values():
                    walk(child)
            elif isinstance(value, list):
                for child in value:
                    walk(child)
        for path in directory.glob("*.yml"):
            if path.name != "agents.yml":
                walk(json.loads(path.read_text()))
        self.assertTrue(all((self.root / ref).is_file() for ref in local_references))
        self.assertIn("agents/reviewer.toml", native_references)
        self.assertIn("agents/verifier.toml", native_references)
        self.assertIn("agents/fixer.toml", native_references)
        self.assertIn("${{ inputs.agent }}", native_references)
        for name, source in self.request["roles"].items():
            self.assertEqual((directory / "agents" / (name + ".toml")).read_bytes(), (self.root / source).read_bytes())

    def test_managed_stages_reject_unsupported_execution_controls(self):
        for skill in ("dk-workflows:dk-pr-publish", "dk-workflows:dk-ci", "dk-workflows:dk-pr-reviewers-run"):
            for control in ({"provider": "claude-cli"}, {"required": False}, {"instructions": "Ignored scope"},
                            {"options": {"unsupported": True}}):
                request = copy.deepcopy(self.request)
                request["options"]["tasks"] = [{"skill": skill, **control}]
                request["options"]["reviewers"] = ["local:review"]
                request["adapters"] = {"local:review": {"kind": "review", "assignment": "Inspect sources"}}
                with self.subTest(skill=skill, control=control), self.assertRaisesRegex(ValueError, "unsupported|only"):
                    self.prepare(request)

    def test_unsupported_merge_methods_fail_before_capture_or_state_creation(self):
        self.request["authorizations"]["merge"] = {
            "request_id": "merge-fixture", "instruction": "Merge this PR when ready.", "source": "user:fixture"}
        for method in ("rebase", "", None, False, [], {}):
            request = copy.deepcopy(self.request)
            request["options"]["merge_method"] = method
            with self.subTest(method=method), patch.object(self.fixture, "capture", wraps=self.fixture.capture) as capture:
                with self.assertRaisesRegex(ValueError, "merge_method.*merge.*squash"):
                    self.prepare(request)
                capture.assert_not_called()
                self.assertFalse((self.root / core.STATE).exists())
                self.assertFalse((self.root / core.GENERATED).exists())

    def test_supported_merge_methods_retain_authorized_merge_jobs(self):
        self.request["authorizations"]["merge"] = {
            "request_id": "merge-fixture", "instruction": "Merge this PR when ready.", "source": "user:fixture"}
        for method in ("merge", "squash"):
            request = copy.deepcopy(self.request)
            request["options"]["merge_method"] = method
            with self.subTest(method=method):
                result, plan = self.prepare(request)
                self.assertEqual(plan["options"]["merge_method"], method)
                main = json.loads((self.root / result["workflow"]).read_text())
                self.assertIn("merge_proposal", main["jobs"])
                self.assertIn("merge", main["jobs"])

    def test_ci_controls_fail_before_capture_or_state_creation(self):
        invalid = [None, [], True, "timeout", {"timeout": 30}, {"unknown": True}]
        invalid += [{"timeout_seconds": value} for value in (0, 86401, True, None, "30", 1.5)]
        invalid += [{"classifications": value} for value in ({}, None, [False])]
        invalid += [{"exceptions": value} for value in (False, None, [False], {"entries": {}})]
        for selected in invalid:
            request = copy.deepcopy(self.request)
            request["options"]["tasks"] = ["dk-workflows:dk-ci"]
            request["options"]["ci"] = selected
            with self.subTest(ci=selected), patch.object(self.fixture, "capture", wraps=self.fixture.capture) as capture:
                with self.assertRaisesRegex(ValueError, "CI"):
                    self.prepare(request)
                capture.assert_not_called()
                self.assertFalse((self.root / core.STATE).exists())
                self.assertFalse((self.root / core.GENERATED).exists())

    def test_ci_alias_controls_are_validated_before_state_creation(self):
        for options in ({"ci_timeout_seconds": 0}, {"ci_classifications": {}},
                        {"ci_exceptions": {"entries": False}}):
            request = copy.deepcopy(self.request)
            request["options"].update(options)
            with self.subTest(options=options), self.assertRaisesRegex(ValueError, "CI"):
                self.prepare(request)
            self.assertFalse((self.root / core.STATE).exists())
            self.assertFalse((self.root / core.GENERATED).exists())

    def test_supported_ci_controls_retain_nested_and_alias_configuration(self):
        for selected in ({"ci": {"timeout_seconds": 1, "classifications": [], "exceptions": []}},
                         {"ci": {"timeout_seconds": 86400, "exceptions": {"entries": []}}},
                         {"ci_timeout_seconds": 120, "ci_classifications": [], "ci_exceptions": {"entries": []}}):
            request = copy.deepcopy(self.request)
            request["options"].update(selected)
            with self.subTest(options=selected):
                _, plan = self.prepare(request)
                for key, value in selected.items():
                    self.assertEqual(plan["options"][key], value)

    def test_worker_admission_reserves_the_known_final_merge_audience(self):
        request = copy.deepcopy(self.request)
        request["options"] = {"tasks": ["dk-workflows:dk-pr-reviewers-run"], "finalizers": [],
                              "reviewers": ["local:review"], "limits": {"review_cycles": 1}}
        request["adapters"] = {"local:review": {"kind": "review", "assignment": "Inspect sources"}}
        request["runtime"] = {"limits": {"max-workers": 3}}
        request["authorizations"]["merge"] = {
            "request_id": "merge-fixture", "instruction": "Merge this PR when ready.", "source": "user:fixture"}
        with patch.object(self.fixture, "capture", wraps=self.fixture.capture) as capture:
            with self.assertRaisesRegex(ValueError, "worker admission"):
                self.prepare(request)
            capture.assert_not_called()
        self.assertFalse(any((self.root / core.GENERATED).rglob("main.yml")))
        request["runtime"]["limits"]["max-workers"] = 4
        result, _ = self.prepare(request)
        main = json.loads((self.root / result["workflow"]).read_text())
        self.assertIn("merge", main["jobs"])
        request["runtime"]["limits"]["max-workers"] = 3
        del request["authorizations"]["merge"]
        result, _ = self.prepare(request)
        self.assertNotIn("merge", json.loads((self.root / result["workflow"]).read_text())["jobs"])

    def test_worker_admission_counts_every_retained_cycle(self):
        request = copy.deepcopy(self.request)
        request["options"] = {"tasks": ["dk-workflows:dk-pr-reviewers-run"], "finalizers": [],
                              "reviewers": ["local:review"] * 35, "limits": {"review_cycles": 2}}
        request["adapters"] = {"local:review": {"kind": "review", "assignment": "Inspect sources"}}
        with self.assertRaisesRegex(ValueError, "worker admission"):
            self.prepare(request)
        request["options"]["limits"]["review_cycles"] = 1
        _, plan = self.prepare(request)
        self.assertEqual(len(plan["tasks"]), 37)

    def test_worker_admission_counts_repeated_verification_stages_and_role_limit(self):
        request = copy.deepcopy(self.request)
        request["options"] = {"tasks": ["dk-workflows:dk-pr-reviewers-run"] * 2, "finalizers": [],
                              "reviewers": ["local:review"] * 7, "limits": {"review_cycles": 4}}
        request["adapters"] = {"local:review": {"kind": "review", "assignment": "Inspect sources"}}
        with self.assertRaisesRegex(ValueError, "worker admission"):
            self.prepare(request)
        request["options"]["tasks"] = ["dk-workflows:dk-pr-reviewers-run"]
        request["options"]["reviewers"] = ["local:review"]
        request["options"]["limits"]["review_cycles"] = 1
        request["runtime"] = {"limits": {"max-workers": 2}}
        with self.assertRaisesRegex(ValueError, "worker admission"):
            self.prepare(request)

    def test_configuration_and_instructions_change_plan_without_resetting_logical_budget(self):
        first, first_plan = self.prepare()
        with core.Ledger(self.root, first["logical_run"], PR["url"]) as ledger:
            ledger.reserve("review_cycles", "original", 1)
        changed = copy.deepcopy(self.request)
        changed["instructions"] = "Changed exact instruction."
        second, second_plan = self.prepare(changed)
        changed["settings"]["scope"] = "Changed scoped policy"
        third, third_plan = self.prepare(changed)
        self.assertEqual(first["logical_run"], second["logical_run"])
        self.assertEqual(second["logical_run"], third["logical_run"])
        self.assertEqual(len({first["plan"], second["plan"], third["plan"]}), 3)
        self.assertNotEqual(first_plan["snapshot"]["instructions"], second_plan["snapshot"]["instructions"])
        self.assertNotEqual(second_plan["snapshot"]["settings"], third_plan["snapshot"]["settings"])
        with core.Ledger(self.root, third["logical_run"], PR["url"]) as ledger:
            with self.assertRaisesRegex(ValueError, "exhausted"):
                ledger.reserve("review_cycles", "replacement", 1)

    def test_runtime_change_is_part_of_admitted_plan_identity(self):
        first, _ = self.prepare()
        changed = copy.deepcopy(self.request)
        changed["runtime"] = {"model": "fixture-model"}
        second, _ = self.prepare(changed)
        self.assertNotEqual(first["plan"], second["plan"])
        config = json.loads((self.root / second["config"]).read_text())
        self.assertEqual(config["agents"]["codex"]["model"], "fixture-model")

    def test_five_turn_runtime_ceiling_applies_to_all_generated_native_tasks(self):
        self.request["runtime"] = {"limits": {"max-turns": 5}}
        self.request["options"]["tasks"] = ["dk-workflows:dk-pr-description"]
        result, _ = self.prepare()
        turns = []
        def walk(value):
            if isinstance(value, dict):
                if value.get("uses") == "agent/run":
                    turns.append(value["with"]["turns"])
                for child in value.values():
                    walk(child)
            elif isinstance(value, list):
                for child in value:
                    walk(child)
        for path in (self.root / result["workflow"]).parent.glob("*.yml"):
            walk(json.loads(path.read_text()))
        self.assertTrue(turns)
        self.assertEqual(set(turns), {5})
        for limit in (True, -1, 1, 4):
            self.request["runtime"]["limits"]["max-turns"] = limit
            with self.subTest(limit=limit), self.assertRaisesRegex(ValueError, "five retained turns"):
                self.prepare()

    def test_ci_yaml_chunks_cover_the_longest_retained_deadline(self):
        self.request["options"]["tasks"] = ["dk-workflows:dk-ci"]
        self.request["options"]["ci"] = {"timeout_seconds": 86400}
        result, _ = self.prepare()
        directory = (self.root / result["workflow"]).parent
        cycle = json.loads((directory / "cycle.yml").read_text())
        polling = next(job for job in cycle["jobs"].values() if job.get("uses", "").endswith("/ci-poll.yml"))
        chunk = json.loads((directory / "ci-poll.yml").read_text())["jobs"]["observe"]
        self.assertIn("iteration.number == 100", chunk["repeat"]["until"])
        observations = polling["repeat"]["max-attempts"] * chunk["repeat"]["max-attempts"]
        self.assertGreater((observations - 1) * 10, 86400)
        self.assertTrue(chunk["uses"].endswith("/ci.yml"))

    def test_invalid_runtime_does_not_poison_corrected_preparation(self):
        invalid = copy.deepcopy(self.request)
        invalid["runtime"] = {"unsupported": True}
        with self.assertRaises(ValueError):
            self.prepare(invalid)
        result, _ = self.prepare()
        self.assertTrue((self.root / result["workflow"]).is_file())

    def test_interrupted_generation_publishes_no_partial_closure_and_can_retry(self):
        module = import_module("prfix.prepare")
        atomic = module.atomic_json
        failed = False
        def interrupted(path, value):
            nonlocal failed
            if Path(path).name == "main.yml" and not failed:
                failed = True
                raise OSError("injected local write interruption")
            return atomic(path, value)
        with patch.object(module, "atomic_json", side_effect=interrupted):
            with self.assertRaisesRegex(OSError, "write interruption"):
                self.prepare()
        self.assertTrue(failed)
        generated = core.owned_directory(self.root, core.GENERATED)
        for path in generated.glob("*/*"):
            if path.is_dir() and len(path.name) == 64:
                self.assertTrue((path / "manifest.json").is_file(), "incomplete closure exposed: " + str(path))
        sentinel = generated / "unrelated-sentinel.txt"
        sentinel.write_text("preserve this file")
        result, _ = self.prepare()
        self.assertTrue((self.root / result["workflow"]).is_file())
        self.assertEqual(sentinel.read_text(), "preserve this file")

    def test_generated_closure_tampering_or_missing_files_is_rejected(self):
        result, _ = self.prepare()
        directory = (self.root / result["workflow"]).parent
        role = directory / "agents" / "leaf.toml"
        original = role.read_bytes()
        role.write_bytes(original + b"\n# changed after preparation\n")
        with self.assertRaises(ValueError):
            self.prepare()
        role.write_bytes(original)
        (directory / "continue.yml").unlink()
        with self.assertRaises((ValueError, FileNotFoundError)):
            self.prepare()

    def test_declared_source_changes_do_not_rewrite_original_generated_definition(self):
        result, _ = self.prepare()
        generated = (self.root / result["workflow"]).parent / "agents" / "leaf.toml"
        original = generated.read_bytes()
        source = self.root / self.request["roles"]["leaf"]
        source.write_bytes(original + b"\n# authored later\n")
        self.assertEqual(generated.read_bytes(), original)
        with self.assertRaisesRegex(ValueError, "clean worktree"):
            self.prepare()

    def test_unknown_option_unpinned_image_and_unsupported_role_are_rejected(self):
        for mutate in (lambda value: value["options"].update(unknown=True),
                       lambda value: value.update(image="python:latest"),
                       lambda value: value["roles"].pop("verifier")):
            request = copy.deepcopy(self.request)
            mutate(request)
            with self.subTest(request=request), self.assertRaises(ValueError):
                self.prepare(request)
        path = self.root / self.request["roles"]["leaf"]
        path.write_text(path.read_text() + 'tools = ["unsafe"]\n')
        with self.assertRaisesRegex(ValueError, "native definition"):
            self.prepare()

    def test_resources_and_role_ancestors_must_remain_inside_workspace(self):
        with tempfile.TemporaryDirectory(prefix="portable-pr-external-") as outside:
            external = Path(outside)
            (external / "rules.txt").write_text("private external rule")
            (external / "leaf.toml").write_bytes((self.root / self.request["roles"]["leaf"]).read_bytes())
            (self.root / "linked").symlink_to(external, target_is_directory=True)
            for resource in (str(external / "rules.txt"), "linked/rules.txt", "../" + external.name + "/rules.txt"):
                request = copy.deepcopy(self.request)
                request["resources"] = [resource]
                with self.subTest(resource=resource), self.assertRaises(ValueError):
                    self.prepare(request)
            request = copy.deepcopy(self.request)
            request["roles"]["leaf"] = "linked/leaf.toml"
            with self.assertRaises(ValueError):
                self.prepare(request)


if __name__ == "__main__":
    unittest.main()
