import copy
import hashlib
import json
from pathlib import Path
import sys
import tempfile
import unittest
from unittest.mock import patch
from importlib import import_module

sys.path.insert(0, str(Path(__file__).resolve().parents[1] / "workflows" / "helpers"))
from prfix import core
from prfix.prepare import prepare
from test_capture import Fixture, PR, git


class PrepareTests(unittest.TestCase):
    def setUp(self):
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
        config = json.loads((self.root / result["config"]).read_text())
        self.assertEqual(config["execution"]["image"], self.request["image"])
        self.assertEqual(config["execution"]["network"], "bridge")
        self.assertEqual(config["agents"]["codex"]["provider"], "codex-app-server")
        self.assertEqual(config["agents"]["codex"]["execution"], {"mode": "local"})

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
