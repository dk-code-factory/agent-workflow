"""Installer and package conformance using local archives and fake GitHub only."""
import concurrent.futures
import hashlib
import importlib.util
import io
import json
import os
from pathlib import Path
import shutil
import stat
import subprocess
import sys
import tarfile
import tempfile
import unittest
from unittest.mock import patch
import zipfile

from plugin_package import (ROOT, PLUGIN, build_plugin, validate_plugin, verify_plugin,
                            validate_builtin_profile, write_archive, verify_archive)


def module(name, path):
    spec = importlib.util.spec_from_file_location(name, path)
    result = importlib.util.module_from_spec(spec)
    spec.loader.exec_module(result)
    return result


launcher = module("engine_bootstrap", ROOT / PLUGIN / "scripts/engine_bootstrap.py")
reports = module("plugin_reports", ROOT / PLUGIN / "scripts/bug-report.py")
VERSION = json.loads((ROOT / PLUGIN / "plugin.json").read_text())["version"]


class InstallerTests(unittest.TestCase):
    def setUp(self):
        self.temporary = tempfile.TemporaryDirectory()
        self.addCleanup(self.temporary.cleanup)
        self.root = Path(self.temporary.name).resolve()
        self.cache = self.root / "cache"
        self.os, self.arch = launcher.host()
        self.prefix = f"agent-workflow-{VERSION}-{self.os}-{self.arch}"
        self.build = {"version": VERSION, "os": self.os, "arch": self.arch,
                      "commit": "a" * 40, "source_fingerprint": "b" * 64, "dirty": "false"}
        self.archive = self.make_archive()

    def make_archive(self, extra=None, build=None, executable_build=None, engine_body=None):
        path = self.root / ("fixture-" + str(len(list(self.root.glob("*.tar.gz")))) + ".tar.gz")
        build = self.build if build is None else build
        executable_build = build if executable_build is None else executable_build
        files = {"build.json": (json.dumps(build).encode(), 0o644),
                 "docs/distribution.txt": (b"complete distribution", 0o644)}
        for name in launcher.BRIDGE_FILES:
            files["bridge/claude/" + name] = ((ROOT / "bridge/claude" / name).read_bytes(), 0o644)
        engine_body = engine_body or "print('engine stdout'); print('engine stderr',file=sys.stderr); sys.exit(7)"
        code = ("#!" + sys.executable + "\nimport json,sys\n"
                "if sys.argv[1:] == ['version']: print(" + repr(json.dumps(executable_build)) + ")\n"
                "else:\n " + engine_body + "\n").encode()
        for helper in launcher.HELPERS:
            files[helper] = (code, 0o755)
        with tarfile.open(path, "w:gz") as bundle:
            for name, (value, mode) in files.items():
                item = tarfile.TarInfo(self.prefix + "/" + name)
                item.size, item.mode = len(value), mode
                bundle.addfile(item, io.BytesIO(value))
            if extra:
                bundle.addfile(extra)
        return path

    def candidate(self, archive=None):
        archive = archive or self.archive
        return launcher.ensure(self.cache, VERSION, self.os, self.arch, archive, launcher.digest(archive))

    def test_four_host_selections_aliases_and_unsupported_hosts(self):
        for system in ("Linux", "Darwin"):
            for machine, expected in (("x86_64", "amd64"), ("AMD64", "amd64"), ("aarch64", "arm64"), ("arm64", "arm64")):
                self.assertEqual(launcher.host(system, machine), (system.lower(), expected))
        for system, machine in (("Windows", "AMD64"), ("Linux", "i686"), ("FreeBSD", "arm64")):
            with self.assertRaisesRegex(launcher.InstallError, "Unsupported host"):
                launcher.host(system, machine)

    def test_candidate_preserves_complete_distribution_and_valid_cache_is_offline(self):
        installed = self.candidate()
        self.assertEqual((installed / "docs/distribution.txt").read_bytes(), b"complete distribution")
        with patch.object(launcher, "command", side_effect=AssertionError("offline")), patch.object(launcher, "download", side_effect=AssertionError("offline")):
            self.assertEqual(self.candidate(), installed)
        self.assertEqual(stat.S_IMODE(self.cache.stat().st_mode), 0o700)

    def release_command(self, args):
        self.calls.append(args)
        if args[:3] == ["gh", "release", "download"]:
            self.assertEqual(args[3], "v" + VERSION)
            self.assertEqual(args[args.index("--repo") + 1], launcher.REPOSITORY)
            name = args[args.index("--pattern") + 1]
            directory = Path(args[args.index("--dir") + 1])
            if name == "release-manifest.json":
                (directory / name).write_text(json.dumps(self.manifest))
            elif name == "manifest-provenance.jsonl":
                (directory / name).write_text("fake bundle")
            else:
                self.assertEqual(name, self.prefix + ".tar.gz")
                shutil.copyfile(self.archive, directory / name)
            return ""
        if args[:3] == ["gh", "attestation", "verify"]:
            for flag, value in (("--repo", launcher.REPOSITORY), ("--signer-workflow", launcher.WORKFLOW),
                                ("--source-ref", "refs/tags/v" + VERSION), ("--source-digest", "a" * 40), ("--signer-digest", "a" * 40)):
                self.assertEqual(args[args.index(flag) + 1], value)
            return "verified"
        self.assertEqual(Path(args[0]).name, "agent-workflow")
        return json.dumps(self.build)

    def release(self):
        self.calls = []
        self.manifest = {"repository": launcher.REPOSITORY, "tag": "v" + VERSION,
                         "commit": "a" * 40, "source_fingerprint": "b" * 64,
                         "assets": {self.prefix + ".tar.gz": launcher.digest(self.archive)}}

    def test_release_pins_tag_provenance_and_supports_cached_offline_use(self):
        self.release()
        with patch.object(launcher, "command", side_effect=self.release_command):
            installed = launcher.ensure(self.cache, VERSION, self.os, self.arch)
        self.assertEqual(len(self.calls), 5)
        with patch.object(launcher, "command", side_effect=AssertionError("offline")):
            self.assertEqual(launcher.ensure(self.cache, VERSION, self.os, self.arch), installed)

    def test_release_rejects_failed_provenance_before_archive_download_or_execution(self):
        self.release()
        def failed(args):
            if args[:3] == ["gh", "attestation", "verify"]:
                raise launcher.InstallError("failed provenance")
            return self.release_command(args)
        with patch.object(launcher, "command", side_effect=failed), self.assertRaisesRegex(launcher.InstallError, "provenance"):
            launcher.ensure(self.cache, VERSION, self.os, self.arch)
        self.assertEqual(len(self.calls), 2)
        self.assertFalse(list(self.cache.glob(".install-*")))

    def test_wrong_manifest_identity_digest_and_build_are_rejected(self):
        for field, value in (("repository", "foreign/repo"), ("tag", "v9.9.9"), ("commit", "wrong"), ("source_fingerprint", "wrong"),
                             ("assets", {self.prefix + ".tar.gz": "0" * 64}), ("assets", None),
                             ("assets", {self.prefix + ".tar.gz": False})):
            self.release()
            self.manifest[field] = value
            with self.subTest(field=field), patch.object(launcher, "command", side_effect=self.release_command), self.assertRaises(launcher.InstallError):
                launcher.ensure(self.cache, VERSION, self.os, self.arch)
        for field, value in (("version", "9.9.9"), ("os", "unsupported"), ("arch", "wrong"), ("commit", "development")):
            archive = self.make_archive(build=dict(self.build, **{field: value}))
            with self.subTest(field=field), patch.object(launcher, "command", side_effect=AssertionError("must not execute")), self.assertRaises(launcher.InstallError):
                self.candidate(archive)
        archive = self.make_archive(executable_build=dict(self.build, version="9.9.9"))
        with self.assertRaisesRegex(launcher.InstallError, "Executable build identity"):
            self.candidate(archive)

    def test_release_requires_clean_matching_source_before_executing(self):
        for change in ({"dirty": "true"}, {"commit": "c" * 40}, {"source_fingerprint": "d" * 64}):
            self.archive = self.make_archive(build=dict(self.build, **change))
            self.release()
            with patch.object(launcher, "command", side_effect=self.release_command), self.assertRaises(launcher.InstallError):
                launcher.ensure(self.cache, VERSION, self.os, self.arch)
            self.assertFalse(any(Path(args[0]).name == "agent-workflow" for args in self.calls))

    def test_corrupted_cache_and_mode_change_are_not_executed(self):
        installed = self.candidate()
        binary = installed / "agent-workflow"
        binary.write_text("corrupted")
        with patch.object(launcher, "command", side_effect=AssertionError("must not execute")), self.assertRaisesRegex(launcher.InstallError, "corrupt"):
            self.candidate()

    def test_cached_file_mode_and_distribution_symlinks_are_rejected(self):
        installed = self.candidate()
        helper = installed / "agent-workflow-runtime"
        helper.chmod(0o644)
        with self.assertRaises(launcher.InstallError):
            self.candidate()
        helper.chmod(0o755)
        moved = installed.with_name("moved")
        installed.rename(moved)
        installed.symlink_to(moved, target_is_directory=True)
        with self.assertRaisesRegex(launcher.InstallError, "symlink"):
            self.candidate()

    def test_unsafe_archives_reject_paths_links_duplicates_and_special_files(self):
        for name, kind in (("../escape", tarfile.REGTYPE), ("/absolute", tarfile.REGTYPE),
                           (self.prefix + "/link", tarfile.SYMTYPE), (self.prefix + "/hard", tarfile.LNKTYPE),
                           (self.prefix + "/fifo", tarfile.FIFOTYPE), (self.prefix + "/build.json", tarfile.REGTYPE)):
            item = tarfile.TarInfo(name)
            item.type = kind
            item.linkname = "../../escape"
            archive = self.make_archive(extra=item)
            with self.subTest(name=name), self.assertRaises(launcher.InstallError):
                self.candidate(archive)
        self.assertFalse((self.root / "escape").exists())

    def test_wrong_candidate_digest_and_release_candidate_separation(self):
        with self.assertRaisesRegex(launcher.InstallError, "digest mismatch"):
            launcher.ensure(self.cache, VERSION, self.os, self.arch, self.archive, "0" * 64)
        candidate = self.candidate()
        with patch.object(launcher, "download", side_effect=launcher.InstallError("release unavailable")), self.assertRaisesRegex(launcher.InstallError, "unavailable"):
            launcher.ensure(self.cache, VERSION, self.os, self.arch)
        self.assertEqual(self.candidate(), candidate)

    def test_interrupted_download_cleanup_and_recovery(self):
        self.release()
        def interrupted(tag, name, directory):
            (directory / name).write_bytes(b"partial download")
            raise launcher.InstallError("interrupted download")
        with patch.object(launcher, "download", side_effect=interrupted), self.assertRaisesRegex(launcher.InstallError, "interrupted"):
            launcher.ensure(self.cache, VERSION, self.os, self.arch)
        self.assertFalse(list(self.cache.glob(".install-*")))
        stale = self.cache / ".install-killed-process"
        stale.mkdir()
        (stale / "partial").write_bytes(b"partial")
        with patch.object(launcher, "command", side_effect=self.release_command):
            launcher.ensure(self.cache, VERSION, self.os, self.arch)
        self.assertFalse(stale.exists())

    def test_symlink_and_public_cache_rejected(self):
        self.cache.mkdir(mode=0o755)
        with self.assertRaisesRegex(launcher.InstallError, "0700"):
            self.candidate()
        self.cache.rmdir()
        self.cache.symlink_to(self.root, target_is_directory=True)
        with self.assertRaisesRegex(launcher.InstallError, "symlink"):
            self.candidate()

    def test_first_install_rejects_nested_cache_symlinks_before_writing(self):
        for index, component in enumerate((Path("candidate"), Path("candidate") / VERSION)):
            with self.subTest(component=component):
                self.cache = self.root / ("nested-cache-" + str(index))
                self.cache.mkdir(mode=0o700)
                outside = self.root / ("redirected-" + str(index))
                outside.mkdir()
                link = self.cache / component
                link.parent.mkdir(parents=True, exist_ok=True)
                link.symlink_to(outside, target_is_directory=True)
                with self.assertRaisesRegex(launcher.InstallError, "symlink"):
                    self.candidate()
                self.assertEqual(list(outside.iterdir()), [])

    def fake_tools(self):
        tools = self.root / "tools"
        tools.mkdir(exist_ok=True)
        for name, output in (("codex", "codex-cli 0.167.1"), ("gh", "--source-ref --source-digest --signer-digest --signer-workflow --bundle")):
            path = tools / name
            path.write_text("#!" + sys.executable + "\nprint(" + repr(output) + ")\n")
            path.chmod(0o755)
        return dict(os.environ, PATH=str(tools) + os.pathsep + os.environ["PATH"], XDG_CACHE_HOME=str(self.root / "xdg"))

    def test_packaged_launcher_check_is_read_only_and_exec_preserves_exit_and_streams(self):
        package = build_plugin(self.root, VERSION)
        extracted = self.root / "extracted"
        with zipfile.ZipFile(package) as bundle:
            bundle.extractall(extracted)
        entry = extracted / "agent-workflow/scripts/agent-workflow"
        environment = self.fake_tools()
        check = subprocess.run([sys.executable, str(entry), "check"], env=environment, capture_output=True, text=True, timeout=20)
        self.assertEqual(check.returncode, 0, check.stderr)
        self.assertFalse(Path(environment["XDG_CACHE_HOME"]).exists())
        args = [sys.executable, str(entry), "exec", "--candidate-archive", str(self.archive), "--sha256", launcher.digest(self.archive), "--", "fixture"]
        result = subprocess.run(args, env=environment, capture_output=True, text=True, timeout=20)
        self.assertEqual(result.returncode, 7, result.stderr)
        self.assertEqual(result.stdout, "engine stdout\n")
        self.assertIn("engine stderr", result.stderr)
        self.assertIn("UNCERTIFIED CANDIDATE", result.stderr)
        self.assertFalse(list(extracted.rglob("__pycache__")))

    def test_concurrent_process_installation(self):
        environment = self.fake_tools()
        args = [sys.executable, str(ROOT / PLUGIN / "scripts/agent-workflow"), "ensure", "--candidate-archive", str(self.archive), "--sha256", launcher.digest(self.archive)]
        def install(_):
            return subprocess.run(args, env=environment, capture_output=True, text=True, timeout=20)
        with concurrent.futures.ThreadPoolExecutor(max_workers=4) as pool:
            results = list(pool.map(install, range(4)))
        for result in results:
            self.assertEqual(result.returncode, 0, result.stderr)
        self.assertEqual(len({json.loads(result.stdout)["distribution"] for result in results}), 1)
        self.assertFalse(list((Path(environment["XDG_CACHE_HOME"]) / "agent-workflow").glob(".install-*")))

    def test_missing_dependency_incompatible_codex_and_version_drift(self):
        with patch.object(launcher.shutil, "which", return_value=None), self.assertRaisesRegex(launcher.InstallError, "Missing dependency"):
            launcher.prerequisites(launcher.configuration(ROOT / PLUGIN, builtin=True), require_codex=True)
        with patch.object(launcher.shutil, "which", return_value="tool"), patch.object(launcher, "command", return_value="codex-cli 0.1.0"), self.assertRaisesRegex(launcher.InstallError, "Incompatible Codex"):
            launcher.prerequisites(launcher.configuration(ROOT / PLUGIN, builtin=True), require_codex=True)
        with self.assertRaisesRegex(ValueError, "versions must match"):
            validate_plugin("9.9.9")

    def generic_package(self):
        root = self.root / "independent-plugin"
        scripts = root / "scripts"
        scripts.mkdir(parents=True)
        shutil.copyfile(ROOT / PLUGIN / "scripts/engine_bootstrap.py", scripts / "engine_bootstrap.py")
        config = {"schema_version": 1, "engine_version": VERSION,
                  "repository": launcher.REPOSITORY, "release_workflow": launcher.WORKFLOW}
        (root / launcher.CONFIG_FILENAME).write_text(json.dumps(config))
        # Third-party host manifests may carry a version independent of the engine.
        (root / "plugin.json").write_text(json.dumps({"version": "9.8.7"}))
        entry = scripts / "engine"
        entry.write_text("import sys\nsys.dont_write_bytecode = True\nfrom pathlib import Path\n"
                         "from engine_bootstrap import main\n"
                         "sys.exit(main(Path(__file__).resolve().parents[1]))\n")
        return root, entry, config

    def test_shared_configuration_preserves_builtin_coupling_and_release_authority(self):
        root, _, config = self.generic_package()
        self.assertEqual(launcher.configuration(root), config)
        with self.assertRaisesRegex(launcher.InstallError, "Plugin/engine version mismatch"):
            launcher.configuration(root, builtin=True)
        for change in ({"schema_version": 2}, {"schema_version": True}, {"engine_version": "latest"},
                       {"engine_version": "../release"}, {"engine_version": "01.2.3"},
                       {"repository": "plugin-owner/engine"}, {"release_workflow": "foreign/workflow.yml"}):
            with self.subTest(change=change):
                (root / launcher.CONFIG_FILENAME).write_text(json.dumps(dict(config, **change)))
                with self.assertRaises(launcher.InstallError):
                    launcher.configuration(root)
        with patch.object(launcher.shutil, "which", side_effect=lambda name: "gh" if name == "gh" else None), \
                patch.object(launcher, "command", return_value="--source-ref --source-digest --signer-digest --signer-workflow --bundle") as call:
            launcher.prerequisites(config)
        self.assertEqual(call.call_args.args[0], ["gh", "attestation", "verify", "--help"])

    def test_shared_readonly_relocated_package_exec_is_offline_and_preserves_arguments(self):
        root, entry, _ = self.generic_package()
        archive = self.make_archive(engine_body="print(json.dumps(sys.argv[1:])); sys.exit(7)")
        # Warm the release cache through fake exact-tag GitHub commands.
        self.archive = archive
        self.release()
        environment = self.fake_tools()
        cache = Path(environment["XDG_CACHE_HOME"]) / "agent-workflow"
        with patch.object(launcher, "command", side_effect=self.release_command):
            distribution = launcher.ensure(cache, VERSION, self.os, self.arch)
        # Only local help inspection is allowed on this cached invocation.
        tools = Path(environment["PATH"].split(os.pathsep)[0])
        (tools / "codex").unlink()
        (tools / "gh").write_text("#!" + sys.executable + "\nimport sys\n"
                                  "if sys.argv[1:] != ['attestation','verify','--help']: sys.exit(99)\n"
                                  "print('--source-ref --source-digest --signer-digest --signer-workflow --bundle')\n")
        environment["PATH"] = str(tools)
        relocated = root.with_name("relocated-readonly")
        root.rename(relocated)
        entry = relocated / "scripts/engine"
        def restore_writable_directories():
            for directory in (relocated, relocated / "scripts"):
                directory.chmod(0o755)
        self.addCleanup(restore_writable_directories)
        for child in relocated.rglob("*"):
            child.chmod(0o555 if child.is_dir() else 0o444)
        relocated.chmod(0o555)
        before = launcher.tree_identity(relocated)
        workspace = self.root / "workspace"
        workspace.mkdir()
        (workspace / "engine_bootstrap.py").write_text("raise AssertionError('workspace import')\n")
        (workspace / "compatibility.json").write_text("invalid workspace config")
        engine_args = ["plugin", "run", "--input", "task=literal space", "--secret-env", "token=TOKEN", "skill"]
        result = subprocess.run([sys.executable, str(entry), "exec", "--", *engine_args], cwd=workspace,
                                env=environment, capture_output=True, text=True, timeout=20)
        self.assertEqual(result.returncode, 7, result.stderr)
        self.assertEqual(json.loads(result.stdout), engine_args)
        self.assertEqual(launcher.tree_identity(relocated), before)
        self.assertFalse(list(relocated.rglob("__pycache__")))
        self.assertEqual(launcher.cached(distribution.parent, VERSION, self.os, self.arch, "release"), distribution)

    def test_shared_commands_reject_operation_argument_mixing_before_installing(self):
        root, _, _ = self.generic_package()
        with patch.object(launcher, "prerequisites"), patch.object(launcher, "ensure", side_effect=AssertionError("must not install")), \
                patch.object(launcher.sys, "stderr", new_callable=io.StringIO):
            for args in (["ensure", "--", "workflow"], ["exec"], ["check", "--candidate-archive", str(self.archive), "--sha256", launcher.digest(self.archive)],
                         ["ensure", "--candidate-archive", str(self.archive)]):
                with self.subTest(args=args):
                    self.assertEqual(launcher.main(root, args), 1)

    def test_shared_cache_paths_reject_unpinned_or_escaping_identity(self):
        for version, system, arch, digest in (("latest", self.os, self.arch, None), ("../engine", self.os, self.arch, None),
                                             (VERSION, "../linux", self.arch, None), (VERSION, self.os, "../arm64", None),
                                             (VERSION, self.os, self.arch, "../candidate")):
            with self.subTest(version=version, system=system, arch=arch, digest=digest), self.assertRaises(launcher.InstallError):
                launcher.installation_path(self.cache, version, system, arch, digest)

    def fake_bridge_setup(self, arguments, *, failure_message=None):
        self.assertEqual(arguments[1:4], ["bridge", "setup", "--directory"])
        bridge = Path(arguments[4])
        package = bridge / "node_modules/@anthropic-ai/claude-agent-sdk"
        package.mkdir(parents=True)
        expected = json.loads((bridge / "package.json").read_text())["dependencies"]["@anthropic-ai/claude-agent-sdk"]
        (package / "package.json").write_text(json.dumps({"version": expected}))
        (package / "cli.js").write_text("// locked SDK fixture\n")
        bins = bridge / "node_modules/.bin"
        bins.mkdir()
        (bins / "sdk").symlink_to("../@anthropic-ai/claude-agent-sdk/cli.js")
        return ""

    def test_explicit_bridge_setup_reports_safe_node_and_npm_prerequisites(self):
        root, _, _ = self.generic_package()
        for error in ("Node 22+ is required: node not found", "Node 22+ is required"):
            with self.subTest(error=error):
                archive = self.make_archive(engine_body="print(" + repr(error + "; private-token") + ", file=sys.stderr); sys.exit(1)")
                distribution = self.candidate(archive)
                before = launcher.tree_identity(distribution)
                with patch.object(launcher, "prerequisites"), patch.object(launcher, "ensure", return_value=distribution), \
                        patch.object(launcher.sys, "stderr", new_callable=io.StringIO) as diagnostics:
                    self.assertEqual(launcher.main(root, ["ensure", "--claude-bridge"]), 1)
                self.assertIn("Node 22+", diagnostics.getvalue())
                self.assertIn("npm", diagnostics.getvalue())
                self.assertIn("ensure --claude-bridge", diagnostics.getvalue())
                self.assertNotIn("private-token", diagnostics.getvalue())
                self.assertEqual(launcher.tree_identity(distribution), before)
                self.assertFalse(list(self.cache.glob(".bridge-*")))
                self.assertFalse((self.cache / "bridges").exists())

    def test_explicit_bridge_setup_preserves_verified_distribution_and_is_cached(self):
        distribution = self.candidate()
        before = launcher.tree_identity(distribution)
        with patch.object(launcher, "command", side_effect=self.fake_bridge_setup) as setup:
            bridge = launcher.setup_claude_bridge(distribution, self.cache)
        self.assertEqual(setup.call_count, 1)
        self.assertTrue(bridge.is_relative_to(self.cache / "bridges"))
        self.assertEqual(launcher.tree_identity(distribution), before)
        self.assertEqual(self.candidate(), distribution)
        with patch.object(launcher, "command", side_effect=AssertionError("cached setup must be offline")):
            self.assertEqual(launcher.setup_claude_bridge(distribution, self.cache), bridge)
        (bridge.parent / "node_modules/@anthropic-ai/claude-agent-sdk/cli.js").write_text("corrupt")
        with patch.object(launcher, "command", side_effect=AssertionError("must not silently reinstall")), self.assertRaisesRegex(launcher.InstallError, "corrupt"):
            launcher.setup_claude_bridge(distribution, self.cache)

    def test_bridge_setup_interruption_and_escaping_links_never_publish_cache(self):
        distribution = self.candidate()
        with patch.object(launcher, "command", side_effect=launcher.InstallError("setup interrupted")), self.assertRaisesRegex(launcher.InstallError, "interrupted"):
            launcher.setup_claude_bridge(distribution, self.cache)
        self.assertFalse(list(self.cache.glob(".bridge-*")))
        outside = self.root / "outside"
        outside.write_text("untouched")
        def setup_with_link(arguments, *, failure_message=None):
            self.fake_bridge_setup(arguments)
            (Path(arguments[4]) / "outside").symlink_to(outside)
            return ""
        with patch.object(launcher, "command", side_effect=setup_with_link), self.assertRaisesRegex(launcher.InstallError, "escapes"):
            launcher.setup_claude_bridge(distribution, self.cache)
        self.assertFalse(list(self.cache.glob(".bridge-*")))
        self.assertFalse((self.cache / "bridges").exists())
        self.assertEqual(outside.read_text(), "untouched")

    def test_bridge_cache_isolates_engine_receipt_and_candidate_authority(self):
        candidate = self.candidate()
        other = self.root / "verified-release"
        shutil.copytree(candidate.parent, other)
        receipt_path = other / "receipt.json"
        receipt = json.loads(receipt_path.read_text())
        receipt["mode"] = "release"
        receipt_path.write_text(json.dumps(receipt))
        # The setup caller has already verified the selected engine receipt;
        # this fixture changes only that receipt's release/candidate binding.
        with patch.object(launcher, "command", side_effect=self.fake_bridge_setup) as setup:
            candidate_bridge = launcher.setup_claude_bridge(candidate, self.cache)
            release_bridge = launcher.setup_claude_bridge(other / "distribution", self.cache)
        self.assertEqual(setup.call_count, 2)
        self.assertNotEqual(candidate_bridge, release_bridge)
        (candidate_bridge.parent / "node_modules/@anthropic-ai/claude-agent-sdk/cli.js").write_text("candidate mutation")
        with patch.object(launcher, "command", side_effect=AssertionError("verified release cache must remain offline")):
            self.assertEqual(launcher.setup_claude_bridge(other / "distribution", self.cache), release_bridge)

    def test_bridge_dependency_setup_is_never_an_execution_side_effect(self):
        root, _, _ = self.generic_package()
        with patch.object(launcher, "prerequisites"), patch.object(launcher.sys, "stderr", new_callable=io.StringIO), \
                patch.object(launcher, "ensure", side_effect=AssertionError("invalid operation must not install")), \
                patch.object(launcher, "setup_claude_bridge", side_effect=AssertionError("must not setup on exec/check")):
            self.assertEqual(launcher.main(root, ["exec", "--claude-bridge", "--", "version"]), 1)
            self.assertEqual(launcher.main(root, ["check", "--claude-bridge"]), 1)


class PackageTests(unittest.TestCase):
    def test_generic_archive_checks_are_independent_of_builtin_identity(self):
        files = {"third-party/scripts/run": (b"#!/bin/sh\n", 0o755),
                 "third-party/plugin.json": (b'{"version":"9.8.7"}', 0o644)}
        with tempfile.TemporaryDirectory() as directory:
            path = Path(directory) / "plugin.zip"
            write_archive(path, files)
            original = path.read_bytes()
            verify_archive(path, files)
            write_archive(path, dict(reversed(list(files.items()))))
            self.assertEqual(path.read_bytes(), original)
            for invalid in (".", "../escape", "/absolute", "bad\\path", "line\nbreak"):
                with self.assertRaisesRegex(ValueError, "unsafe"):
                    write_archive(path, {invalid: (b"bad", 0o644)})
                with self.assertRaisesRegex(ValueError, "unsafe"):
                    verify_archive(path, {invalid: (b"bad", 0o644)})
                self.assertEqual(path.read_bytes(), original)

    def test_builtin_profile_keeps_release_policy_and_exact_inventory(self):
        for changed in ("identity", "license", "version", "inventory"):
            with self.subTest(changed=changed), tempfile.TemporaryDirectory() as directory:
                root = Path(directory)
                shutil.copytree(ROOT / PLUGIN, root / PLUGIN)
                (root / "images").mkdir()
                shutil.copyfile(ROOT / "images/pins.json", root / "images/pins.json")
                for name in ("LICENSE", "NOTICE"):
                    shutil.copyfile(ROOT / name, root / name)
                plugin = root / PLUGIN
                if changed in ("identity", "version"):
                    manifest = json.loads((plugin / "plugin.json").read_text())
                    manifest["name" if changed == "identity" else "version"] = "different" if changed == "identity" else "9.9.9"
                    (plugin / "plugin.json").write_text(json.dumps(manifest))
                elif changed == "license":
                    (plugin / "LICENSE").write_text("changed")
                else:
                    skill = plugin / "skills/unexpected"
                    skill.mkdir()
                    (skill / "SKILL.md").write_text("unexpected skill")
                with self.assertRaises(ValueError):
                    validate_builtin_profile(VERSION, root)
    skills = {"setup", "build-workflow", "run-workflow", "report-bug", "convert-agent"}

    def copy_source(self, destination):
        root = Path(destination)
        shutil.copytree(ROOT / PLUGIN, root / PLUGIN)
        for name in ("LICENSE", "NOTICE", "images/pins.json"):
            target = root / name
            target.parent.mkdir(parents=True, exist_ok=True)
            shutil.copyfile(ROOT / name, target)
        return root

    def test_builtin_inventory_rejects_each_missing_skill_and_unexpected_skill(self):
        for name in sorted(self.skills):
            with self.subTest(missing=name), tempfile.TemporaryDirectory() as directory:
                root = self.copy_source(directory)
                (root / PLUGIN / "skills" / name / "SKILL.md").unlink()
                with self.assertRaisesRegex(ValueError, "all five"):
                    validate_plugin(VERSION, root)
        with tempfile.TemporaryDirectory() as directory:
            root = self.copy_source(directory)
            extra = root / PLUGIN / "skills/unexpected/SKILL.md"
            extra.parent.mkdir()
            extra.write_text("---\nname: unexpected\ndescription: Unexpected skill.\n---\n")
            with self.assertRaisesRegex(ValueError, "all five"):
                validate_plugin(VERSION, root)

    def test_conversion_metadata_and_repository_licenses_remain_required(self):
        with tempfile.TemporaryDirectory() as directory:
            root = self.copy_source(directory)
            metadata = root / PLUGIN / "skills/convert-agent/SKILL.md"
            metadata.write_text(metadata.read_text().replace("name: convert-agent", "name: another-agent", 1))
            with self.assertRaisesRegex(ValueError, "invalid skill metadata: convert-agent"):
                validate_plugin(VERSION, root)
        for name in ("LICENSE", "NOTICE"):
            with self.subTest(license=name), tempfile.TemporaryDirectory() as directory:
                root = self.copy_source(directory)
                (root / PLUGIN / name).write_text("different license")
                with self.assertRaisesRegex(ValueError, "preserve repository licensing: " + name):
                    validate_plugin(VERSION, root)

    def test_builtin_release_identity_and_pinned_compatibility_remain_required(self):
        for name, field in (("plugin.json", "version"), (".codex-plugin/plugin.json", "version"), (".claude-plugin/plugin.json", "version"),
                            ("compatibility.json", "engine_version"), ("compatibility.json", "codex_versions")):
            with self.subTest(manifest=name, field=field), tempfile.TemporaryDirectory() as directory:
                root = self.copy_source(directory)
                path = root / PLUGIN / name
                content = json.loads(path.read_text())
                content[field] = ["0.0.0"] if field == "codex_versions" else "9.9.9"
                path.write_text(json.dumps(content))
                with self.assertRaisesRegex(ValueError, "compatibility must match|versions must match"):
                    validate_plugin(VERSION, root)

    def test_codex_catalog_discovers_all_five_builtin_skill_identities(self):
        catalog = json.loads((ROOT / ".agents/plugins/marketplace.json").read_text())
        entry, = [plugin for plugin in catalog["plugins"] if plugin["name"] == "agent-workflow"]
        self.assertEqual(entry["source"], {"source": "local", "path": "./plugins/agent-workflow"})
        plugin = ROOT / entry["source"]["path"]
        portable = json.loads((plugin / "plugin.json").read_text())
        native = json.loads((plugin / ".codex-plugin/plugin.json").read_text())
        self.assertEqual(portable["name"], native["name"])
        self.assertEqual(native["name"], entry["name"])
        names = {path.read_text().splitlines()[1].removeprefix("name: ")
                 for path in (plugin / native["skills"]).glob("*/SKILL.md")}
        self.assertEqual(names, self.skills)
        claude_catalog = json.loads((ROOT / ".claude-plugin/marketplace.json").read_text())
        claude_entry, = claude_catalog["plugins"]
        self.assertEqual(claude_entry, {"name": "agent-workflow", "source": "./plugins/agent-workflow"})
        claude = json.loads((plugin / ".claude-plugin/plugin.json").read_text())
        self.assertEqual(claude["name"], entry["name"])
        self.assertEqual(claude["version"], native["version"])
        self.assertEqual(claude["skills"], native["skills"])

    def test_reproducible_complete_package_modes_and_references(self):
        with tempfile.TemporaryDirectory() as directory:
            output = Path(directory)
            first = build_plugin(output, VERSION)
            original = first.read_bytes()
            self.assertEqual(build_plugin(output, VERSION).read_bytes(), original)
            verify_plugin(first, VERSION)
            with zipfile.ZipFile(first) as bundle:
                self.assertEqual(bundle.getinfo("agent-workflow/scripts/agent-workflow").external_attr >> 16, stat.S_IFREG | 0o755)
                self.assertEqual({n for n in bundle.namelist() if n.endswith("SKILL.md")},
                                 {"agent-workflow/skills/" + name + "/SKILL.md" for name in self.skills})
                for name in ("LICENSE", "NOTICE"):
                    self.assertEqual(bundle.read("agent-workflow/" + name), (ROOT / name).read_bytes())
            with zipfile.ZipFile(first, "a") as bundle:
                bundle.writestr("agent-workflow/unexpected", "foreign")
            with self.assertRaisesRegex(ValueError, "asset set"):
                verify_plugin(first, VERSION)


class ReportingTests(unittest.TestCase):
    def setUp(self):
        self.temporary = tempfile.TemporaryDirectory()
        self.addCleanup(self.temporary.cleanup)
        self.root = Path(self.temporary.name)
        self.facts = dict(title="Fixture engine failure", **{key: "minimal local fixture" for key in reports.FIELDS})
        self.body = self.root / "report.md"
        self.body.write_text(reports.draft(self.facts))
        self.checksum = hashlib.sha256(self.body.read_bytes()).hexdigest()
        self.journal = self.root / "journal.json"

    def test_sanitized_draft_requires_minimal_evidence(self):
        self.facts["evidence"] = "Authorization: Bearer sensitive\napi_key=sk-secret ghp_token\n/home/person/private/file\n/Users/person/private"
        result = reports.draft(self.facts)
        for private in ("sensitive", "sk-secret", "ghp_token", "/home/person", "/Users/person"):
            self.assertNotIn(private, result)
        del self.facts["expected"]
        with self.assertRaises(ValueError):
            reports.draft(self.facts)

    def test_declined_or_changed_posting_never_creates_an_issue(self):
        with patch.object(reports.subprocess, "run", side_effect=AssertionError("no remote calls")):
            # Drafting alone, including a declined posting decision, is wholly local.
            reports.draft(self.facts)
            with self.assertRaisesRegex(ValueError, "reviewed"):
                reports.post(self.body, self.facts["title"], "0" * 64, self.journal)

    def test_duplicate_returns_existing_issue_without_creating(self):
        marker = self.body.read_text().split("<!-- ")[1].split(" -->")[0]
        issue = {"url": "https://github.com/dk-code-factory/agent-workflow/issues/123", "body": marker}
        with patch.object(reports, "search", return_value=[issue]), patch.object(reports.subprocess, "run", side_effect=AssertionError("no creation")):
            self.assertEqual(reports.post(self.body, self.facts["title"], self.checksum, self.journal), issue["url"])

    def test_uncertain_creation_reconciles_and_never_blindly_retries(self):
        with patch.object(reports, "search", return_value=[]), patch.object(reports.subprocess, "run", return_value=subprocess.CompletedProcess([], 1, "", "timeout")) as call:
            for _ in range(2):
                with self.assertRaisesRegex(ValueError, "uncertain"):
                    reports.post(self.body, self.facts["title"], self.checksum, self.journal)
            self.assertEqual(call.call_count, 1)
        marker = json.loads(self.journal.read_text())["marker"]
        issue = {"url": "https://github.com/dk-code-factory/agent-workflow/issues/123", "body": marker}
        with patch.object(reports, "search", return_value=[issue]):
            self.assertEqual(reports.post(self.body, self.facts["title"], self.checksum, self.journal), issue["url"])

    def test_search_failure_is_not_absence_and_searches_closed_issues(self):
        with patch.object(reports.subprocess, "run", return_value=subprocess.CompletedProcess([], 1, "", "denied")):
            with self.assertRaisesRegex(ValueError, "search failed"):
                reports.search("fixture")
        with patch.object(reports.subprocess, "run", return_value=subprocess.CompletedProcess([], 0, "[]", "")) as call:
            self.assertEqual(reports.search("fixture"), [])
            self.assertIn("all", call.call_args.args[0])

    def test_post_sends_reviewed_bytes_and_records_intent_before_creation(self):
        url = "https://github.com/dk-code-factory/agent-workflow/issues/123"
        def create(arguments, **kwargs):
            self.assertEqual(json.loads(self.journal.read_text())["state"], "uncertain")
            self.assertEqual(Path(arguments[arguments.index("--body-file") + 1]).read_bytes(), self.body.read_bytes())
            return subprocess.CompletedProcess(arguments, 0, url + "\n", "")
        with patch.object(reports, "search", return_value=[]), patch.object(reports.subprocess, "run", side_effect=create):
            self.assertEqual(reports.post(self.body, self.facts["title"], self.checksum, self.journal), url)
        self.assertEqual(json.loads(self.journal.read_text())["state"], "created")


if __name__ == "__main__":
    unittest.main()
