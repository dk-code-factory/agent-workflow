"""Developer walkthrough and cached conversion; fake release service, no hosts/providers."""
import hashlib
import io
import json
import os
from pathlib import Path
import subprocess
import sys
import tarfile
import tempfile
import unittest
from unittest.mock import patch
import zipfile

import test_plugin as plugin_tests
from test_plugin import ROOT, PLUGIN, VERSION, launcher


class AuthoringTests(unittest.TestCase):
    @classmethod
    def setUpClass(cls):
        cls.build_dir = tempfile.TemporaryDirectory(prefix="aw-authoring-")
        cls.engine = Path(cls.build_dir.name) / "agent-workflow"
        prefix = "github.com/dk-code-factory/agent-workflow/internal/buildinfo."
        flags = " ".join("-X " + prefix + field + "=" + value for field, value in
                         (("Commit", "a" * 40), ("Dirty", "false"), ("SourceFingerprint", "b" * 64)))
        subprocess.run(["go", "build", "-ldflags", flags, "-o", str(cls.engine), "./cmd/agent-workflow"],
                       cwd=ROOT, capture_output=True, check=True, timeout=120)

    @classmethod
    def tearDownClass(cls):
        cls.build_dir.cleanup()

    def fixture(self):
        fixture = plugin_tests.InstallerTests(methodName="runTest")
        fixture.setUp()
        self.addCleanup(fixture.doCleanups)
        # Install a real converter binary through the ordinary fake-provenance
        # release path. No test manufactures an admitted release cache directly.
        fixture.build = json.loads(subprocess.check_output([str(self.engine), "version"], text=True))
        archive = fixture.root / "real-engine.tar.gz"
        with tarfile.open(fixture.archive) as source, tarfile.open(archive, "w:gz") as target:
            for entry in source.getmembers():
                data = source.extractfile(entry).read()
                if entry.name.endswith("/agent-workflow"):
                    data = self.engine.read_bytes()
                elif entry.name.endswith("/build.json"):
                    data = json.dumps(fixture.build).encode()
                entry.size = len(data)
                target.addfile(entry, io.BytesIO(data))
        fixture.archive = archive
        fixture.cache = fixture.root / "xdg" / "agent-workflow"
        fixture.release()
        with patch.object(launcher, "command", side_effect=fixture.release_command):
            distribution = launcher.ensure(fixture.cache, VERSION, fixture.os, fixture.arch)
        return fixture, distribution

    def environment(self, fixture):
        # Empty PATH prevents gh/Codex/Claude discovery. Python network APIs are
        # denied too; the actual converter subprocess has no provider code path.
        deny = fixture.root / "deny-network"
        deny.mkdir()
        (deny / "sitecustomize.py").write_text("import socket\ndef deny(*a, **k): raise AssertionError('network denied')\nsocket.socket = deny\nsocket.create_connection = deny\n")
        return dict(os.environ, PATH="", XDG_CACHE_HOME=str(fixture.cache.parent),
                    PYTHONPATH=str(deny), PYTHONDONTWRITEBYTECODE="1")

    def invoke(self, environment, *args):
        return subprocess.run([sys.executable, str(ROOT / PLUGIN / "scripts/agent-workflow"), *args],
                              env=environment, capture_output=True, text=True, timeout=20)

    def test_builtin_cached_conversion_both_formats_without_host_tools_or_network(self):
        fixture, distribution = self.fixture()
        environment = self.environment(fixture)
        before = launcher.tree_identity(distribution)
        checked = self.invoke(environment, "check", "--cached-only")
        self.assertEqual(checked.returncode, 0, checked.stderr)
        self.assertEqual(json.loads(checked.stdout)["installed"], str(distribution))
        for source_format, body in (("codex", "name='reviewer'\ndescription='Review'\ndeveloper_instructions='Inspect only'\nhooks=['touch SHOULD_NOT_EXIST']\n"),
                                    ("claude", "---\nname: reviewer\ndescription: Review\nhooks: {start: 'touch SHOULD_NOT_EXIST'}\n---\nInspect only\n")):
            source = fixture.root / (source_format + ".native")
            output = fixture.root / (source_format + ".yaml")
            body = body.replace("SHOULD_NOT_EXIST", str(fixture.root / "SHOULD_NOT_EXIST"))
            source.write_text(body)
            args = ["exec", "--cached-only", "--", "agent", "convert", "--from", source_format,
                    "--input", str(source), "--output", str(output)]
            failed = self.invoke(environment, *args)
            self.assertNotEqual(failed.returncode, 0)
            self.assertEqual(json.loads(failed.stdout)["incompatible-options"], ["hooks"])
            self.assertFalse(output.exists())
            converted = self.invoke(environment, *args, "--acknowledge-loss", "hooks")
            self.assertEqual(converted.returncode, 0, converted.stderr)
            self.assertEqual(json.loads(converted.stdout)["output"], str(output))
            self.assertIn('name: "reviewer"', output.read_text())
            self.assertEqual(source.read_text(), body)
            collision = self.invoke(environment, *args, "--acknowledge-loss", "hooks")
            self.assertNotEqual(collision.returncode, 0)
        self.assertFalse((fixture.root / "SHOULD_NOT_EXIST").exists())
        self.assertEqual(launcher.tree_identity(distribution), before)

    def test_cached_only_rejects_wrong_authority_corruption_and_missing_release(self):
        fixture, distribution = self.fixture()
        environment = self.environment(fixture)
        receipt_path = distribution.parent / "receipt.json"
        original = receipt_path.read_bytes()
        args = ["exec", "--cached-only", "--", "agent", "convert", "--help"]
        for change in ({"mode": "uncertified-candidate"}, {"manifest": None},
                       {"manifest": dict(fixture.manifest, repository="other/engine")},
                       {"manifest": dict(fixture.manifest, tag="v9.9.9")},
                       {"archive_sha256": "0" * 64}):
            with self.subTest(change=change):
                receipt = json.loads(original)
                receipt.update(change)
                receipt_path.write_text(json.dumps(receipt))
                self.assertNotEqual(self.invoke(environment, *args).returncode, 0)
        receipt_path.write_bytes(original)
        fixture.cache.chmod(0o755)
        self.assertNotEqual(self.invoke(environment, *args).returncode, 0)
        fixture.cache.chmod(0o700)
        binary = distribution / "agent-workflow"
        binary.chmod(0o644)
        self.assertNotEqual(self.invoke(environment, *args).returncode, 0)
        binary.chmod(0o755)
        (distribution / "docs/distribution.txt").write_text("corrupt")
        self.assertNotEqual(self.invoke(environment, *args).returncode, 0)
        # A candidate cache can coexist, but cannot satisfy release-only lookup.
        candidate = fixture.cache / "candidate" / VERSION / (fixture.os + "-" + fixture.arch)
        candidate.parent.mkdir(parents=True)
        distribution.parent.rename(candidate)
        missing = self.invoke(environment, *args)
        self.assertNotEqual(missing.returncode, 0)
        self.assertIn("cache unavailable", missing.stderr)
        self.assertFalse((fixture.cache / "release" / VERSION / candidate.name).exists())

    def test_cached_only_cannot_bypass_runtime_setup_or_candidate_boundaries(self):
        fixture, _ = self.fixture()
        environment = self.environment(fixture)
        for args in (["ensure", "--cached-only"], ["ensure", "--cached-only", "--claude-bridge"],
                     ["exec", "--cached-only", "--", "run", "workflow.yml"],
                     ["exec", "--cached-only", "--", "plugin", "test", "some-project"],
                     ["check", "--cached-only", "--candidate-archive", str(fixture.archive), "--sha256", launcher.digest(fixture.archive)]):
            with self.subTest(args=args):
                self.assertNotEqual(self.invoke(environment, *args).returncode, 0)
        self.assertNotEqual(self.invoke(environment, "check", "--host", "unknown").returncode, 0)

    def test_claude_host_prerequisites_are_separate_from_workspace_runtime_selection(self):
        fixture, distribution = self.fixture()
        environment = self.environment(fixture)
        tools = fixture.root / "tools"
        tools.mkdir()
        gh = tools / "gh"
        gh.write_text("#!" + sys.executable + "\nimport sys\n"
                      "if sys.argv[1:] != ['attestation','verify','--help']: sys.exit(99)\n"
                      "print('--source-ref --source-digest --signer-digest --signer-workflow --bundle')\n")
        gh.chmod(0o755)
        environment["PATH"] = str(tools)
        checked = self.invoke(environment, "check", "--host", "claude")
        self.assertEqual(checked.returncode, 0, checked.stderr)
        self.assertEqual(json.loads(checked.stdout)["installed"], str(distribution))
        version = self.invoke(environment, "exec", "--host", "claude", "--", "version")
        self.assertEqual(version.returncode, 0, version.stderr)
        self.assertEqual(json.loads(version.stdout)["version"], VERSION)
        config = launcher.configuration(ROOT / PLUGIN, builtin=True)
        with patch.object(launcher.shutil, "which", side_effect=lambda name: "gh" if name == "gh" else None), \
                patch.object(launcher, "command", return_value="--source-ref --source-digest --signer-digest --signer-workflow --bundle"), \
                patch.object(launcher.sys, "stdout", new_callable=io.StringIO):
            with tempfile.TemporaryDirectory() as directory, patch.dict(os.environ, XDG_CACHE_HOME=directory):
                self.assertEqual(launcher.main(ROOT / PLUGIN, ["check", "--host", "claude"], builtin=True), 0)
                with patch.object(launcher.sys, "stderr", new_callable=io.StringIO):
                    self.assertEqual(launcher.main(ROOT / PLUGIN, ["check"], builtin=True), 1)
        self.assertIn("codex_versions", config)
        source = ROOT / "examples/workflow-plugin"
        tested = self.engine_command("plugin", "test", "--json", str(source), environment=dict(os.environ, PATH=""))
        results = json.loads(tested.stdout)
        self.assertTrue(results["passed"])
        self.assertEqual({call["runtime"] for case in results["cases"] for call in case["invocations"]},
                         {"codex-app-server", "claude-sdk"})

    def engine_command(self, *args, cwd=None, environment=None):
        result = subprocess.run([str(self.engine), *args], cwd=cwd, env=environment,
                                capture_output=True, text=True, timeout=120)
        self.assertEqual(result.returncode, 0, result.stdout + result.stderr)
        return result

    def test_clean_checkout_starter_and_maintained_example_build_reproducibly(self):
        with tempfile.TemporaryDirectory(prefix="aw-walkthrough-") as directory:
            root = Path(directory)
            self.engine_command("plugin", "init", "starter", cwd=root)
            self.engine_command("plugin", "check", "--json", str(root / "starter"))
            self.engine_command("plugin", "test", "--json", str(root / "starter"))
            example = ROOT / "examples/workflow-plugin"
            self.engine_command("plugin", "check", "--json", str(example))
            self.engine_command("plugin", "test", "--json", str(example))
            for target in ("first output", "second output"):
                self.engine_command("plugin", "build", "--target", "codex,claude", "--output", str(root / target), str(example))
            first, second = root / "first output", root / "second output"
            self.assertEqual((first / "brief-workflow-0.1.0.zip").read_bytes(), (second / "brief-workflow-0.1.0.zip").read_bytes())
            for line in (first / "SHA256SUMS").read_text().splitlines():
                digest, name = line.split("  ", 1)
                self.assertEqual(hashlib.sha256((first / name).read_bytes()).hexdigest(), digest)
            extracted = root / "extracted space"
            with zipfile.ZipFile(first / "brief-workflow-0.1.0.zip") as archive:
                archive.extractall(extracted)
                for entry in archive.infolist():
                    (extracted / entry.filename).chmod((entry.external_attr >> 16) & 0o777)
            package = extracted / "brief-workflow/plugins/brief-workflow"
            self.assertEqual((package / "workflows/brief.yml").read_bytes(), (example / "workflows/brief.yml").read_bytes())
            self.assertEqual((package / "references/scope.md").read_bytes(), (example / "references/scope.md").read_bytes())
            self.assertFalse((package / "config").exists())
            self.assertFalse((package / "fixtures").exists())
            portable = json.loads((package / "plugin.json").read_text())
            self.assertEqual((portable["name"], portable["version"]), ("brief-workflow", "0.1.0"))
            for host in ("codex", "claude"):
                manifest = json.loads((package / ("." + host + "-plugin/plugin.json")).read_text())
                self.assertEqual((manifest["name"], manifest["version"], manifest["skills"]),
                                 (portable["name"], portable["version"], "./skills/"))
            marketplace = extracted / "brief-workflow"
            codex = json.loads((marketplace / ".agents/plugins/marketplace.json").read_text())["plugins"]
            claude = json.loads((marketplace / ".claude-plugin/marketplace.json").read_text())["plugins"]
            self.assertEqual(len(codex), 1)
            self.assertEqual(len(claude), 1)
            self.assertEqual(codex[0]["source"]["path"], "./plugins/brief-workflow")
            self.assertEqual(claude[0]["source"], "./plugins/brief-workflow")
            self.assertEqual(codex[0]["name"], claude[0]["name"])
            self.assertEqual({p.parent.name for p in (package / "skills").glob("*/SKILL.md")}, {"brief"})
            config = json.loads((package / "compatibility.json").read_text())
            self.assertEqual(config["engine_version"], VERSION)
            self.assertEqual(config["repository"], launcher.REPOSITORY)
            self.assertEqual(config["release_workflow"], launcher.WORKFLOW)


if __name__ == "__main__":
    unittest.main()
