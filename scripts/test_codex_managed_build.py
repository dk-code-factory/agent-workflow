import argparse
import hashlib
import importlib.util
import io
import json
import os
from pathlib import Path
import signal
import subprocess
import sys
import tempfile
import time
import unittest
from unittest.mock import patch


SPEC = importlib.util.spec_from_file_location("codex_managed_build", Path(__file__).with_name("codex_managed_build.py"))
BUILD = importlib.util.module_from_spec(SPEC)
SPEC.loader.exec_module(BUILD)


class ManagedSourceBuildTests(unittest.TestCase):
    def test_cache_preparation_copies_one_index_for_multiple_locked_versions(self):
        with tempfile.TemporaryDirectory() as directory, patch.object(BUILD, "MIN_FREE", 0):
            root = Path(directory)
            prepared, origin = root / "prepared", root / "original"
            (prepared / "cargo-home").mkdir(parents=True)
            (prepared / "source/codex-rs").mkdir(parents=True)
            (prepared / BUILD.RECEIPT).write_text(json.dumps({"inputs": {"patch_sha256": "fixture"}, "build_id": "fixture"}))
            registry = "index.crates.io-1949cf8c6b5b557f"
            index = origin / "registry/index" / registry
            (index / ".cache/3/a").mkdir(parents=True)
            (index / "config.json").write_text("{}")
            (index / ".cache/3/a/aes").write_bytes(b"index covers both locked versions")
            packages = []
            for version in ("0.8.4", "0.9.3"):
                archive = origin / "registry/cache" / registry / ("aes-" + version + ".crate")
                archive.parent.mkdir(parents=True, exist_ok=True)
                archive.write_bytes(version.encode())
                packages.append('\n[[package]]\nname = "aes"\nversion = "' + version + '"\nsource = "registry+https://github.com/rust-lang/crates.io-index"\nchecksum = "' + BUILD.digest(archive) + '"\n')
            (prepared / "source/codex-rs/Cargo.lock").write_text("".join(packages))
            original = {path: BUILD.digest(path) for path in origin.rglob("*") if path.is_file()}
            with patch.object(BUILD, "manifest", return_value=({"patch_sha256": "fixture"}, None)), patch("sys.stdout", new=io.StringIO()):
                BUILD.prepare_cache(argparse.Namespace(output=prepared, from_cache=origin))
            report = json.loads((prepared / "cache.json").read_text())
            self.assertEqual(report["missing"], [])
            self.assertEqual(report["copied"].count("registry/index/" + registry + "/.cache/3/a/aes"), 1)
            for path, checksum in original.items():
                self.assertEqual(BUILD.digest(path), checksum)
                self.assertEqual(BUILD.digest(prepared / "cargo-home" / path.relative_to(origin)), checksum)

    def test_cache_copy_reserves_tree_before_any_write(self):
        with tempfile.TemporaryDirectory() as directory, patch.object(BUILD, "MIN_FREE", 0):
            root = Path(directory)
            source, cache = root / "input", root / "owned"
            source.mkdir(); cache.mkdir()
            (source / "pack").write_bytes(b"12345678")
            with patch.object(BUILD, "MIN_FREE", 24), patch.object(BUILD, "MAX_COMBINED", 16), patch.object(BUILD.shutil, "disk_usage", return_value=type("Space", (), {"free": 30})()):
                with self.assertRaisesRegex(ValueError, "resource limit"):
                    BUILD.bounded_cache_copy(source, cache / "git", root, cache, time.monotonic())
                self.assertEqual(list(cache.iterdir()), [])
            with patch.object(BUILD, "MIN_FREE", 0), patch.object(BUILD, "MAX_COMBINED", 7):
                with self.assertRaisesRegex(ValueError, "resource limit"):
                    BUILD.bounded_cache_copy(source, cache / "git", root, cache, time.monotonic())
                self.assertEqual(list(cache.iterdir()), [])
            BUILD.bounded_cache_copy(source, cache / "git", root, cache, time.monotonic())
            self.assertEqual((cache / "git/pack").read_bytes(), b"12345678")

    def test_cache_copy_rejects_links_and_checks_deadline_between_chunks(self):
        with tempfile.TemporaryDirectory() as directory, patch.object(BUILD, "MIN_FREE", 0):
            root = Path(directory)
            source, cache = root / "input", root / "owned"
            source.mkdir(); cache.mkdir()
            (source / "escape").symlink_to(root)
            with self.assertRaisesRegex(ValueError, "not a regular"):
                BUILD.bounded_cache_copy(source, cache / "git", root, cache, time.monotonic())
            (source / "escape").unlink()
            data = source / "pack"
            data.write_bytes(b"x" * (2 << 20))
            calls = 0
            def clock():
                nonlocal calls
                calls += 1
                return 0 if calls < 10 else BUILD.DEADLINE
            with patch.object(BUILD.time, "monotonic", side_effect=clock):
                with self.assertRaisesRegex(ValueError, "resource limit"):
                    BUILD.bounded_cache_copy(source, cache / "git", root, cache, 0)
            self.assertLess((cache / "git/pack").stat().st_size, data.stat().st_size)

    def test_runtime_admission_gate_does_not_change_source_inputs(self):
        with tempfile.TemporaryDirectory() as directory:
            root = Path(directory)
            source_patch, wrapper, profile = root / "source.patch", root / "offline.py", root / "profile.json"
            source_patch.write_text("immutable source patch")
            wrapper.write_text("immutable offline wrapper")
            value = {"schema": 1, "enabled": False, "patch": source_patch.name, "patch_sha256": BUILD.digest(source_patch), "recipe_sha256": BUILD.digest(Path(BUILD.__file__)), "offline_wrapper": wrapper.name, "offline_wrapper_sha256": BUILD.digest(wrapper), "source_sha256": "a" * 64}
            with patch.object(BUILD, "REPO", root), patch.object(BUILD, "MANIFEST", profile):
                profile.write_text(json.dumps(value))
                disabled, _ = BUILD.manifest()
                profile.write_text(json.dumps(dict(value, enabled=True, status="ready")))
                enabled, _ = BUILD.manifest()
                for field in ("patch_sha256", "recipe_sha256", "offline_wrapper_sha256", "source_sha256"):
                    self.assertEqual(disabled[field], enabled[field])
                BUILD.verify_canonical_source(enabled, "a" * 64)
                with self.assertRaisesRegex(ValueError, "canonical source digest"):
                    BUILD.verify_canonical_source(enabled, "b" * 64)
                for invalid in (None, 0, 1, "false"):
                    profile.write_text(json.dumps(dict(value, enabled=invalid)))
                    with self.assertRaisesRegex(ValueError, "admission gate"):
                        BUILD.manifest()
                profile.write_text(json.dumps({key: item for key, item in dict(value, enabled=True).items() if key != "source_sha256"}))
                with self.assertRaisesRegex(ValueError, "requires a canonical source digest"):
                    BUILD.manifest()
                for invalid in (False, "", "0" * 63, "z" * 64):
                    profile.write_text(json.dumps(dict(value, source_sha256=invalid)))
                    with self.assertRaisesRegex(ValueError, "source digest is invalid"):
                        BUILD.manifest()

    def test_wrong_patch_digest_is_rejected_before_preparation(self):
        with tempfile.TemporaryDirectory() as directory:
            root = Path(directory)
            (root / "source.patch").write_text("different patch")
            manifest = root / "profile.json"
            manifest.write_text(json.dumps({"schema": 1, "enabled": False, "patch": "source.patch", "patch_sha256": "0" * 64}))
            with patch.object(BUILD, "REPO", root), patch.object(BUILD, "MANIFEST", manifest):
                with self.assertRaisesRegex(ValueError, "patch digest changed"):
                    BUILD.prepare(argparse.Namespace(output=root / "output", upstream_git=root / "never-read"))
            self.assertFalse((root / "output").exists())

    def test_mutated_source_rejects_before_any_compiler_process(self):
        with tempfile.TemporaryDirectory() as directory:
            root = Path(directory)
            for name in ("source", "cargo-home", "target", "tmp", "home"):
                (root / name).mkdir()
            source = root / "source/input.rs"
            source.write_text("original")
            value = {key: "fixture" for key in ("upstream_sha", "patch_sha256", "recipe_sha256", "protocol", "rust_toolchain", "offline_wrapper_sha256")}
            inputs = dict(value, target="x86_64-unknown-linux-gnu")
            identity = hashlib.sha256(json.dumps(inputs, sort_keys=True, separators=(",", ":")).encode()).hexdigest()
            (root / BUILD.RECEIPT).write_text(json.dumps({"inputs": inputs, "build_id": identity, "source_sha256": BUILD.tree_digest(root / "source")}))
            source.write_text("changed")
            with patch.object(BUILD, "manifest", return_value=(value, None)), patch.object(BUILD.subprocess, "Popen") as spawn:
                with self.assertRaisesRegex(ValueError, "source tree changed"):
                    BUILD.check(argparse.Namespace(output=root, host="x86_64-unknown-linux-gnu"))
                spawn.assert_not_called()

    def test_workspace_lock_correction_cannot_change_external_dependencies(self):
        before = {"version": 4, "package": [{"name": "workspace-" + str(i), "version": "0.0.0", "dependencies": ["external"]} for i in range(171)] + [{"name": "external", "version": "1.2.3", "source": "registry+fixture", "checksum": "original"}]}
        after = json.loads(json.dumps(before))
        for package in after["package"][:-1]:
            package["version"] = "0.167.1"

        def encode(value):
            rows = ["version = 4"]
            for package in value["package"]:
                rows.append("[[package]]")
                rows.extend(key + " = " + json.dumps(item) for key, item in package.items())
            return "\n".join(rows).encode()

        BUILD.verify_workspace_lock(encode(before), encode(after))
        for field, changed in [("version", "2.0.0"), ("checksum", "changed"), ("source", "registry+changed")]:
            with self.subTest(field=field):
                invalid = json.loads(json.dumps(after))
                invalid["package"][-1][field] = changed
                with self.assertRaisesRegex(ValueError, "external packages"):
                    BUILD.verify_workspace_lock(encode(before), encode(invalid))
        invalid = json.loads(json.dumps(after))
        invalid["package"][0]["dependencies"] = []
        with self.assertRaisesRegex(ValueError, "external packages"):
            BUILD.verify_workspace_lock(encode(before), encode(invalid))

    def test_resource_monitor_tolerates_removed_compiler_objects(self):
        with tempfile.TemporaryDirectory() as directory:
            root = Path(directory)
            (root / "stable").write_bytes(b"1234")
            original = Path.lstat

            def stat(path):
                if path.name == "transient.rcgu.o":
                    raise FileNotFoundError(path)
                return original(path)

            with patch.object(BUILD.os, "walk", return_value=[(str(root), [], ["stable", "transient.rcgu.o"])]), patch.object(Path, "lstat", stat):
                self.assertEqual(BUILD.owned_size(root), 4)

    def test_test_stage_requires_actual_selected_tests(self):
        self.assertEqual(BUILD.selected_test_count("test result: ok. 10 passed; 0 failed; 0 ignored;"), 10)
        self.assertEqual(BUILD.selected_test_count("test result: ok. 0 passed; 0 failed; 10 filtered out"), 0)
        self.assertEqual(BUILD.selected_test_count("test result: FAILED. 1 passed; 1 failed;"), 0)
        self.assertEqual(BUILD.selected_test_count("test result: ok. 1 passed; 0 failed;\ntest result: ok. 1 passed; 0 failed;"), 0)

    def test_shared_cache_aliases_are_rejected(self):
        with tempfile.TemporaryDirectory() as directory:
            root = Path(directory)
            owned, foreign = root / "owned", root / "foreign"
            owned.mkdir()
            foreign.write_text("fixture")
            (owned / "alias").symlink_to(foreign)
            with self.assertRaisesRegex(ValueError, "outside owned storage"):
                BUILD.validate_cache(owned)
            (owned / "alias").unlink()
            os.link(foreign, owned / "linked")
            with self.assertRaisesRegex(ValueError, "shared hard link"):
                BUILD.validate_cache(owned)
            self.assertEqual(foreign.read_text(), "fixture")
            (owned / "linked").unlink()
            internal = owned / "internal"
            internal.write_text("owned fixture")
            os.link(internal, owned / "second-owned-name")
            BUILD.validate_cache(owned)

    @unittest.skipUnless(sys.platform.startswith("linux"), "owned Linux process-group monitor")
    def test_group_escalation_survives_leader_exit_and_preserves_other_group(self):
        with tempfile.TemporaryDirectory() as directory:
            ready = Path(directory) / "ready"
            leader = subprocess.Popen([sys.executable, "-c", "import time; time.sleep(30)"], process_group=0)
            child = unrelated = None
            try:
                child = subprocess.Popen([sys.executable, "-c", "import signal,sys,time; from pathlib import Path; signal.signal(signal.SIGTERM, signal.SIG_IGN); Path(sys.argv[1]).write_text('ready'); time.sleep(30)", str(ready)], process_group=leader.pid)
                unrelated = subprocess.Popen([sys.executable, "-c", "import time; time.sleep(30)"], process_group=0)
                deadline = time.monotonic() + 2
                while not ready.exists() and time.monotonic() < deadline:
                    time.sleep(0.01)
                self.assertTrue(ready.exists(), "fixture did not initialize")
                receipt = BUILD.terminate_group(leader, grace=0.1)
                self.assertEqual(child.wait(timeout=2), -signal.SIGKILL)
                self.assertIsNotNone(leader.returncode)
                self.assertIsNone(unrelated.poll(), "termination escaped the owned group")
                self.assertFalse(any(member["state"] != "Z" for member in receipt["after"]))
            finally:
                for process in (child, leader, unrelated):
                    if process is not None:
                        if process.poll() is None:
                            process.kill()
                        process.wait(timeout=2)


if __name__ == "__main__":
    unittest.main()
