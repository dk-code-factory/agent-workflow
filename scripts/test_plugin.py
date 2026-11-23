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

from plugin_package import ROOT, PLUGIN, build_plugin, validate_plugin, verify_plugin


def module(name, path):
    spec = importlib.util.spec_from_file_location(name, path)
    result = importlib.util.module_from_spec(spec)
    spec.loader.exec_module(result)
    return result


launcher = module("plugin_launcher", ROOT / PLUGIN / "scripts/launcher.py")
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

    def make_archive(self, extra=None, build=None, executable_build=None):
        path = self.root / ("fixture-" + str(len(list(self.root.glob("*.tar.gz")))) + ".tar.gz")
        build = self.build if build is None else build
        executable_build = build if executable_build is None else executable_build
        files = {"build.json": (json.dumps(build).encode(), 0o644),
                 "docs/distribution.txt": (b"complete distribution", 0o644)}
        code = ("#!" + sys.executable + "\nimport json,sys\n"
                "if sys.argv[1:] == ['version']: print(" + repr(json.dumps(executable_build)) + ")\n"
                "else:\n print('engine stdout'); print('engine stderr',file=sys.stderr); sys.exit(7)\n").encode()
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
            launcher.prerequisites(launcher.configuration())
        with patch.object(launcher.shutil, "which", return_value="tool"), patch.object(launcher, "command", return_value="codex-cli 0.1.0"), self.assertRaisesRegex(launcher.InstallError, "Incompatible Codex"):
            launcher.prerequisites(launcher.configuration())
        with self.assertRaisesRegex(ValueError, "versions must match"):
            validate_plugin("9.9.9")


class PackageTests(unittest.TestCase):
    def test_reproducible_complete_package_modes_and_references(self):
        with tempfile.TemporaryDirectory() as directory:
            output = Path(directory)
            first = build_plugin(output, VERSION)
            original = first.read_bytes()
            self.assertEqual(build_plugin(output, VERSION).read_bytes(), original)
            verify_plugin(first, VERSION)
            with zipfile.ZipFile(first) as bundle:
                self.assertEqual(bundle.getinfo("agent-workflow/scripts/agent-workflow").external_attr >> 16, stat.S_IFREG | 0o755)
                self.assertEqual(len([n for n in bundle.namelist() if n.endswith("SKILL.md")]), 4)
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
