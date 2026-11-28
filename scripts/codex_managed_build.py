#!/usr/bin/env python3
"""Prepare/check the repository-owned managed Codex source profile.

No fetch, dependency installation, provider execution, image build or publication.
Build caches must be disposable copies inside the newly prepared owned directory.
"""

import argparse
import configparser
import hashlib
import json
import os
import re
from pathlib import Path
import shutil
import signal
import stat
import subprocess
import tarfile
import time
import tomllib
from urllib.parse import urlparse

REPO = Path(__file__).resolve().parents[1]
MANIFEST = REPO / "patches/codex/profile.json"
RECEIPT = "managed-source.json"
MAX_TARGET = 12 << 30
MIN_FREE = 24 << 30
DEADLINE = 30 * 60
MAX_COMBINED = 16 << 30


def digest(path):
    with path.open("rb") as source:
        return hashlib.file_digest(source, "sha256").hexdigest()


def manifest():
    value = json.loads(MANIFEST.read_text())
    if type(value.get("schema")) is not int or value["schema"] != 1 or type(value.get("enabled")) is not bool:
        raise ValueError("managed profile schema or runtime admission gate is invalid")
    canonical = value.get("source_sha256")
    if canonical is not None and (not isinstance(canonical, str) or re.fullmatch("[0-9a-f]{64}", canonical) is None):
        raise ValueError("canonical managed source digest is invalid")
    if value["enabled"] and canonical is None:
        raise ValueError("runtime-enabled profile requires a canonical source digest")
    patch = REPO / value["patch"]
    if digest(patch) != value["patch_sha256"]:
        raise ValueError("managed source patch digest changed")
    if digest(Path(__file__)) != value["recipe_sha256"]:
        raise ValueError("managed build recipe digest changed")
    if digest(REPO / value["offline_wrapper"]) != value["offline_wrapper_sha256"]:
        raise ValueError("offline execution wrapper digest changed")
    return value, patch


def verify_canonical_source(value, actual):
    # enabled/status are runtime admission metadata, not build inputs. An
    # enabled descriptor must still name the exact reviewed patched source.
    if value.get("source_sha256") is not None and value["source_sha256"] != actual:
        raise ValueError("patched source differs from the canonical source digest")


def tree_digest(root):
    result = hashlib.sha256()
    for path in sorted(root.rglob("*")):
        relative = path.relative_to(root).as_posix()
        mode = path.lstat().st_mode & 0o777
        if path.is_symlink():
            kind, contents = "link", os.readlink(path)
        elif path.is_file():
            kind, contents = "file", digest(path)
        elif path.is_dir():
            continue
        else:
            raise ValueError("unsupported source entry: " + relative)
        result.update(json.dumps([relative, kind, mode, contents], separators=(",", ":")).encode())
        result.update(b"\n")
    return result.hexdigest()



def verify_workspace_lock(original, patched):
    """Only the exact release tag's stale workspace versions may change."""
    before, after = tomllib.loads(original.decode()), tomllib.loads(patched.decode())
    expected = json.loads(json.dumps(before))
    changed = 0
    for package in expected["package"]:
        if "source" not in package:
            if package["version"] != "0.0.0":
                raise ValueError("unexpected original workspace package version")
            package["version"] = "0.167.1"
            changed += 1
    if expected != after or changed != 171:
        raise ValueError("managed lock correction changes external packages or non-version data")


def prepare(args):
    value, patch = manifest()
    output = args.output.absolute()
    if output.exists() or output.is_symlink():
        raise ValueError("source preparation requires a new owned output directory")
    commit = subprocess.check_output(
        ["git", "-C", str(args.upstream_git), "rev-parse", value["upstream_sha"] + "^{commit}"],
        text=True,
    ).strip()
    if commit != value["upstream_sha"]:
        raise ValueError("upstream source object differs from the immutable pin")
    output.mkdir(mode=0o700)
    source = output / "source"
    source.mkdir()
    archive = output / "upstream.tar"
    try:
        with archive.open("wb") as stream:
            subprocess.run(["git", "-C", str(args.upstream_git), "archive", commit], stdout=stream, check=True)
        with tarfile.open(archive) as stream:
            stream.extractall(source, filter="data")
        source_lock = source / "codex-rs/Cargo.lock"
        original_lock = source_lock.read_bytes()
        if hashlib.sha256(original_lock).hexdigest() != value["upstream_lock_sha256"]:
            raise ValueError("upstream source lock differs from the reviewed release pin")
        subprocess.run(["git", "apply", "--check", "--whitespace=error-all", str(patch)], cwd=source, check=True)
        subprocess.run(["git", "apply", "--whitespace=error-all", str(patch)], cwd=source, check=True)
        verify_workspace_lock(original_lock, source_lock.read_bytes())
        if digest(source_lock) != value["source_lock_sha256"]:
            raise ValueError("patched source lock differs from the reviewed workspace-version correction")
        inputs = {
            "upstream_sha": commit,
            "patch_sha256": value["patch_sha256"],
            "recipe_sha256": value["recipe_sha256"],
            "protocol": value["protocol"],
            "target": args.target,
            "rust_toolchain": value["rust_toolchain"],
            "offline_wrapper_sha256": value["offline_wrapper_sha256"],
        }
        build_id = hashlib.sha256(json.dumps(inputs, sort_keys=True, separators=(",", ":")).encode()).hexdigest()
        source_sha = tree_digest(source)
        verify_canonical_source(value, source_sha)
        receipt = {"inputs": inputs, "build_id": build_id, "source_sha256": source_sha, "verified_binary": False}
        (output / RECEIPT).write_text(json.dumps(receipt, indent=2) + "\n")
        shutil.copy2(__file__, output / "build-recipe.py")
        shutil.copy2(MANIFEST, output / "profile.json")
        shutil.copy2(patch, output / "source.patch")
        shutil.copy2(REPO / value["offline_wrapper"], output / "offline-exec.py")
        for name in ("cargo-home", "target", "tmp", "home"):
            (output / name).mkdir()
        print(json.dumps(receipt, indent=2))
    finally:
        archive.unlink(missing_ok=True)


def owned_size(root):
    total = 0
    for directory, _, files in os.walk(root):
        for name in files:
            path = Path(directory) / name
            try:
                total += path.lstat().st_size
            except FileNotFoundError:
                # Compilers atomically rename/remove temporary objects while
                # this bounded monitor walks their exclusively owned target.
                continue
    return total


def validate_cache(cache):
    # A writable alias into a shared cache would defeat this recipe's ownership
    # boundary. Full disposable copies are required on filesystems without reflinks.
    if cache.is_symlink():
        raise ValueError("Cargo cache points outside owned storage")
    inodes = {}
    for path in cache.rglob("*"):
        if path.is_symlink():
            if not path.resolve().is_relative_to(cache):
                raise ValueError("Cargo cache points outside owned storage")
            continue
        if path.is_file():
            metadata = path.stat()
            key = metadata.st_dev, metadata.st_ino
            count, links = inodes.get(key, (0, metadata.st_nlink))
            inodes[key] = count + 1, links
    # Cargo makes hard links between its owned bare repositories and checkouts.
    # Accept those only when every link belongs to this disposable cache.
    if any(count != links for count, links in inodes.values()):
        raise ValueError("Cargo cache contains a shared hard link")


def group_members(group):
    members = []
    for directory in Path("/proc").iterdir():
        if not directory.name.isdecimal():
            continue
        try:
            fields = (directory / "stat").read_text().rsplit(")", 1)[1].split()
        except (FileNotFoundError, ProcessLookupError):
            continue
        if int(fields[2]) == group:
            members.append({"pid": int(directory.name), "state": fields[0]})
    return members


def terminate_group(process, grace=5):
    before = group_members(process.pid)
    if before:
        try:
            os.killpg(process.pid, signal.SIGTERM)
        except ProcessLookupError:
            pass
    deadline = time.monotonic() + grace
    while time.monotonic() < deadline and any(member["state"] != "Z" for member in group_members(process.pid)):
        time.sleep(0.05)
    # A leader may have exited while one of its children ignored SIGTERM.
    # Escalation belongs to the entire owned group, independent of leader.poll().
    if group_members(process.pid):
        try:
            os.killpg(process.pid, signal.SIGKILL)
        except ProcessLookupError:
            pass
    process.wait(timeout=5)
    deadline = time.monotonic() + 5
    after = group_members(process.pid)
    while time.monotonic() < deadline and any(member["state"] != "Z" for member in after):
        time.sleep(0.05)
        after = group_members(process.pid)
    return {"before": before, "after": after}


def bounded_cache_copy(source, destination, root, cache, started):
    """Copy regular pinned inputs without crossing the owned byte/free ceiling.

    A whole tree is reserved before its first write, and every bounded chunk
    rechecks both the deadline and actual remaining free space. Symlinks and
    special files are rejected rather than followed into unbounded inputs.
    """
    entries, total = [], 0

    def guard(reserve=0):
        if (time.monotonic() - started >= DEADLINE or
                shutil.disk_usage(root).free - reserve < MIN_FREE or
                owned_size(cache) + reserve > MAX_COMBINED):
            raise ValueError("owned cache preparation resource limit exceeded")

    def inventory(path, target):
        nonlocal total
        guard()
        info = path.lstat()
        if stat.S_ISREG(info.st_mode):
            total += info.st_size
            entries.append((path, target, info))
        elif stat.S_ISDIR(info.st_mode):
            entries.append((path, target, info))
            for child in sorted(path.iterdir()):
                inventory(child, target / child.name)
        else:
            raise ValueError("cache copy input is not a regular file/directory")
        if len(entries) > 100000:
            raise ValueError("cache copy entry limit exceeded")
        guard(total)

    inventory(source, destination)
    guard(total)
    for path, target, expected in entries:
        guard()
        if stat.S_ISDIR(expected.st_mode):
            target.mkdir(parents=True, exist_ok=False)
            target.chmod(stat.S_IMODE(expected.st_mode))
            continue
        target.parent.mkdir(parents=True, exist_ok=True)
        descriptor = os.open(path, os.O_RDONLY | os.O_NOFOLLOW | os.O_NONBLOCK)
        with os.fdopen(descriptor, "rb") as original:
            actual = os.fstat(original.fileno())
            if (not stat.S_ISREG(actual.st_mode) or actual.st_dev != expected.st_dev or
                    actual.st_ino != expected.st_ino or actual.st_size != expected.st_size):
                raise ValueError("cache copy input changed after reservation")
            guard(expected.st_size)
            with target.open("xb") as copied:
                remaining = expected.st_size
                while remaining:
                    size = min(remaining, 1 << 20)
                    guard(size)
                    chunk = original.read(size)
                    if not chunk:
                        raise ValueError("cache copy input truncated")
                    copied.write(chunk)
                    copied.flush()
                    remaining -= len(chunk)
                if original.read(1):
                    raise ValueError("cache copy input grew after reservation")
        shutil.copystat(path, target, follow_symlinks=False)
        guard()


def prepare_cache(args):
    value, _ = manifest()
    root = args.output.resolve(strict=True)
    receipt = json.loads((root / RECEIPT).read_text())
    if receipt["inputs"]["patch_sha256"] != value["patch_sha256"]:
        raise ValueError("cache preparation requires the current source patch")
    cache = root / "cargo-home"
    if cache.is_symlink() or not cache.is_dir() or any(cache.iterdir()):
        raise ValueError("cache preparation requires empty owned Cargo storage")
    lock = tomllib.loads((root / "source/codex-rs/Cargo.lock").read_text())
    origin = args.from_cache.resolve(strict=True)
    registry = "index.crates.io-1949cf8c6b5b557f"
    missing, copied = [], []
    started = time.monotonic()

    def guard():
        if time.monotonic() - started >= DEADLINE or shutil.disk_usage(root).free < MIN_FREE or owned_size(cache) >= MAX_COMBINED:
            raise ValueError("owned cache preparation resource limit exceeded")

    def copy(source, destination):
        guard()
        bounded_cache_copy(source, destination, root, cache, started)
        copied.append(str(source.relative_to(origin)))

    source_index = origin / "registry/index" / registry
    copy(source_index / "config.json", cache / "registry/index" / registry / "config.json")
    copied_indexes = set()
    for package in lock["package"]:
        if not package.get("source", "").startswith("registry+"):
            continue
        name = package["name"]
        index_name = name.lower()
        if len(index_name) <= 2:
            index = f"{len(index_name)}/{index_name}"
        elif len(index_name) == 3:
            index = f"3/{index_name[0]}/{index_name}"
        else:
            index = f"{index_name[:2]}/{index_name[2:4]}/{index_name}"
        index = ".cache/" + index
        if index not in copied_indexes:
            copied_indexes.add(index)
            if (source_index / index).is_file():
                copy(source_index / index, cache / "registry/index" / registry / index)
            else:
                missing.append("index:" + name)
        archive = origin / "registry/cache" / registry / (name + "-" + package["version"] + ".crate")
        if archive.is_file():
            if digest(archive) != package["checksum"]:
                raise ValueError("locked registry archive checksum differs: " + name)
            copy(archive, cache / archive.relative_to(origin))
        else:
            missing.append("archive:" + archive.name)
    pending_git = [source[4:].partition("#")[::2] for source in sorted({package.get("source", "") for package in lock["package"] if package.get("source", "").startswith("git+")})]
    seen_git, pinned_git = set(), []
    while pending_git:
        url, commit = pending_git.pop(0)
        if (url, commit) in seen_git:
            continue
        seen_git.add((url, commit))
        if len(seen_git) > 128:
            raise ValueError("pinned Git/submodule graph exceeds preparation bound")
        name = Path(urlparse(url).path).name.removesuffix(".git")
        candidates = []
        for candidate in (origin / "git/db").iterdir():
            if not candidate.name.lower().startswith(name.lower() + "-"):
                continue
            if subprocess.run(["git", "--git-dir", str(candidate), "cat-file", "-e", commit + "^{commit}"], stdout=subprocess.DEVNULL, stderr=subprocess.DEVNULL).returncode == 0:
                candidates.append(candidate)
        if not candidates:
            missing.append("git:" + url + "#" + commit)
            continue
        source_db = min(candidates, key=owned_size)
        if (source_db / "objects/info/alternates").exists():
            raise ValueError("shared Git alternates are not a disposable cache copy")
        guard()
        destination = cache / "git/db" / source_db.name
        if not destination.exists():
            bounded_cache_copy(source_db, destination, root, cache, started)
            copied.append(str(source_db.relative_to(origin)))
        pinned_git.append({"url": url, "commit": commit, "owned_db": str(destination.relative_to(cache))})
        modules = subprocess.run(["git", "--git-dir", str(source_db), "show", commit + ":.gitmodules"], capture_output=True, text=True)
        if modules.returncode == 0:
            config = configparser.ConfigParser()
            config.read_string(modules.stdout)
            for section in config.sections():
                if config.get(section, "update", fallback="").strip() == "none":
                    continue
                module_path, module_url = config[section]["path"], config[section]["url"]
                entry = subprocess.check_output(["git", "--git-dir", str(source_db), "ls-tree", commit, "--", module_path], text=True).split()
                if len(entry) < 3 or entry[0] != "160000" or not module_url.startswith("https://"):
                    raise ValueError("unsupported pinned Git submodule declaration")
                pending_git.append((module_url, entry[2]))
    validate_cache(cache)
    guard()
    report = {"build_id": receipt["build_id"], "cache_bytes": owned_size(cache), "copied": copied, "missing": missing, "pinned_git": pinned_git}
    (root / "cache.json").write_text(json.dumps(report, indent=2) + "\n")
    print(json.dumps({key: value for key, value in report.items() if key != "copied"}, indent=2))


def check(args):
    value, _ = manifest()
    root = args.output.resolve(strict=True)
    for name in ("source", "cargo-home", "target", "tmp", "home"):
        path = root / name
        if path.is_symlink() or not path.is_dir() or path.resolve().parent != root:
            raise ValueError("prepared directory escaped owned storage: " + name)
    receipt = json.loads((root / RECEIPT).read_text())
    expected = receipt["inputs"]
    for key in ("upstream_sha", "patch_sha256", "recipe_sha256", "protocol", "rust_toolchain", "offline_wrapper_sha256"):
        if expected[key] != value[key]:
            raise ValueError("prepared source no longer matches current build inputs")
    build_id = hashlib.sha256(json.dumps(expected, sort_keys=True, separators=(",", ":")).encode()).hexdigest()
    if build_id != receipt["build_id"]:
        raise ValueError("prepared build identity differs from immutable inputs")
    source = root / "source"
    if tree_digest(source) != receipt["source_sha256"]:
        raise ValueError("prepared source tree changed after patch application")
    verify_canonical_source(value, receipt["source_sha256"])
    cache, target = root / "cargo-home", root / "target"
    validate_cache(cache)
    toolchain = Path.home() / ".rustup/toolchains" / (value["rust_toolchain"] + "-" + args.host)
    cargo, rustc = toolchain / "bin/cargo", toolchain / "bin/rustc"
    if not cargo.is_file() or not rustc.is_file():
        raise ValueError("exact Rust toolchain must already be installed")
    if args.phase == "metadata":
        command = [str(cargo), "metadata", "--locked", "--offline", "--format-version", "1", "--filter-platform", expected["target"]]
    else:
        admission = json.loads((root / "metadata-admission.json").read_text())
        if admission["build_id"] != build_id or digest(root / admission["metadata_file"]) != admission["metadata_sha256"]:
            raise ValueError("compiler stage requires exact successful metadata admission")
        if args.phase in ("runtime-tests", "submission-tests", "journal-tests"):
            admission = admission["core_unit"]
        if admission["unprepared_native_artifacts"]:
            raise ValueError("compiler stage has an unprepared native dependency artifact")
        for script in admission["build_scripts"]:
            if digest(Path(script["path"])) != script["sha256"]:
                raise ValueError("dependency build script changed after metadata audit")
        common = ["--locked", "--offline", "--target", expected["target"]]
        if args.phase == "check":
            command = [str(cargo), "check", *common, "-p", "codex-app-server"]
        elif args.phase == "build":
            command = [str(cargo), "build", *common, "-p", "codex-app-server", "--bin", "codex-app-server"]
        elif args.phase == "runtime-tests":
            command = [str(cargo), "test", *common, "-p", "codex-core", "--lib", "agent_workflow_managed::tests::", "--", "--test-threads=2"]
        elif args.phase == "journal-tests":
            command = [str(cargo), "test", *common, "-p", "codex-core", "--lib", "agent_workflow_journal::tests::", "--", "--test-threads=2"]
        elif args.phase == "submission-tests":
            command = [str(cargo), "test", *common, "-p", "codex-core", "--lib", "session::tests::managed_submission_ceiling_rejects_manual_and_unbound_work_before_queueing", "--", "--exact", "--test-threads=2"]
        else:
            raise ValueError("unsupported managed source stage")
    attempt = 1
    while (root / f"{args.phase}-{attempt}.json").exists():
        attempt += 1
    label = f"{args.phase}-{attempt}"
    filter_receipt = root / (label + ".filter.json")
    wrapper = root / "offline-exec.py"
    if digest(wrapper) != expected["offline_wrapper_sha256"]:
        raise ValueError("executed offline wrapper differs from prepared build input")
    command = [os.path.realpath(os.sys.executable), str(wrapper), "--receipt", str(filter_receipt), "--", *command]
    environment = {key: os.environ[key] for key in ("PATH", "LANG", "LC_ALL") if key in os.environ}
    environment.update({
        "HOME": str(root / "home"), "TMPDIR": str(root / "tmp"),
        "CARGO_HOME": str(cache), "CARGO_TARGET_DIR": str(target),
        "RUSTC": str(rustc), "CARGO_BUILD_JOBS": "2", "CARGO_INCREMENTAL": "0",
        "CARGO_PROFILE_DEV_DEBUG": "0", "CARGO_PROFILE_TEST_DEBUG": "0",
        "AGENT_WORKFLOW_CODEX_BUILD_ID": receipt["build_id"],
    })
    if shutil.disk_usage(root).free < MIN_FREE or owned_size(target) >= MAX_TARGET or owned_size(target) + owned_size(cache) >= MAX_COMBINED:
        raise ValueError("owned build resource limits already exceeded")
    started, reason, termination = time.monotonic(), "completed", None
    report = {"build_id": receipt["build_id"], "command": command, "limits": {"seconds": DEADLINE, "target_bytes": MAX_TARGET, "combined_cache_target_bytes": MAX_COMBINED, "minimum_free_bytes": MIN_FREE}}
    output_path = root / (label + ".output.json")
    with (root / (label + ".log")).open("wb") as log, output_path.open("wb") as output:
        process = subprocess.Popen(command, cwd=source / "codex-rs", env=environment, stdout=output if args.phase == "metadata" else log, stderr=log, start_new_session=True)
        try:
            while process.poll() is None:
                if time.monotonic() - started >= DEADLINE:
                    reason = "deadline"
                elif owned_size(target) >= MAX_TARGET:
                    reason = "target_limit"
                elif owned_size(target) + owned_size(cache) >= MAX_COMBINED:
                    reason = "combined_cache_target_limit"
                elif shutil.disk_usage(root).free < MIN_FREE:
                    reason = "free_space_floor"
                if reason != "completed":
                    termination = terminate_group(process)
                    break
                time.sleep(1)
        except BaseException as error:
            reason = "monitor_error"
            report["monitor_error"] = type(error).__name__ + ": " + str(error)
            raise
        finally:
            if process.poll() is None or group_members(process.pid):
                if reason == "completed":
                    reason = "retained_descendants"
                termination = terminate_group(process)
            code = process.wait()
            final_source = tree_digest(source)
            if final_source != receipt["source_sha256"]:
                reason = "source_changed_during_check"
            offline = json.loads(filter_receipt.read_text()) if filter_receipt.exists() else None
            if offline is None or offline["pid"] != process.pid or offline["wrapper_sha256"] != expected["offline_wrapper_sha256"] or offline["installed"] is not True:
                reason = "offline_filter_not_attested"
            report.update({"reason": reason, "exit_code": code, "elapsed_seconds": time.monotonic() - started, "target_bytes": owned_size(target), "free_bytes": shutil.disk_usage(root).free, "source_before_sha256": receipt["source_sha256"], "source_after_sha256": final_source, "termination": termination, "retained_group_members": group_members(process.pid), "offline_filter": offline, "phase": args.phase})
            (root / (label + ".json")).write_text(json.dumps(report, indent=2) + "\n")
    if code == 0 and reason == "completed" and args.phase in ("runtime-tests", "submission-tests", "journal-tests"):
        count = selected_test_count((root / (label + ".log")).read_text(errors="replace"))
        expected_count = {"runtime-tests": 11, "submission-tests": 1, "journal-tests": 11}[args.phase]
        report["selected_test_count"] = count
        if count != expected_count:
            report["reason"] = reason = "selected_test_count_mismatch"
        (root / (label + ".json")).write_text(json.dumps(report, indent=2) + "\n")
    if code == 0 and reason == "completed" and args.phase == "build":
        binary = target / expected["target"] / "debug/codex-app-server"
        if binary.is_symlink() or not binary.is_file() or not os.access(binary, os.X_OK):
            report["reason"] = "missing_built_executable"
            (root / (label + ".json")).write_text(json.dumps(report, indent=2) + "\n")
            raise ValueError("successful build did not produce the exact Linux app-server executable")
        with binary.open("rb") as stream:
            header = stream.read(4)
        if header != b"\x7fELF":
            report["reason"] = "invalid_built_executable"
            (root / (label + ".json")).write_text(json.dumps(report, indent=2) + "\n")
            raise ValueError("built app-server is not a Linux executable")
        candidate = {"build_id": build_id, "inputs": expected, "source_sha256": final_source, "binary_path": str(binary), "binary_sha256": digest(binary), "binary_bytes": binary.stat().st_size, "startup_kind": "standalone-app-server", "startup_argv": ["--config", "project_doc_max_bytes=0", "--config", 'cli_auth_credentials_store="file"'], "build_receipt": label + ".json", "verified_executed_binary": False, "conformance_passed": False}
        (root / "managed-binary.json").write_text(json.dumps(candidate, indent=2) + "\n")
    print(json.dumps(report, indent=2))
    if code != 0 or reason != "completed":
        raise SystemExit(1)
    if args.phase == "metadata":
        audit_metadata(root, output_path, receipt)


def selected_test_count(output):
    matches = re.findall(r"test result: ok\. ([0-9]+) passed; 0 failed;", output)
    return int(matches[0]) if len(matches) == 1 else 0


def target_metadata(root, metadata, root_name, include_root_dev=False):
    packages = {package["id"]: package for package in metadata["packages"]}
    nodes = {node["id"]: node for node in metadata["resolve"]["nodes"]}
    roots = [package["id"] for package in metadata["packages"] if package["name"] == root_name]
    if len(roots) != 1:
        raise ValueError("metadata must identify exactly one target package")
    pending, closure = roots[:], set()
    while pending:
        identity = pending.pop()
        if identity in closure:
            continue
        closure.add(identity)
        for edge in nodes[identity]["deps"]:
            if any(kind["kind"] in (None, "build") or include_root_dev and identity == roots[0] and kind["kind"] == "dev" for kind in edge["dep_kinds"]):
                pending.append(edge["pkg"])
    scripts = []
    for identity in sorted(closure):
        package = packages[identity]
        for target in package["targets"]:
            if "custom-build" not in target["kind"]:
                continue
            path = Path(target["src_path"]).resolve(strict=True)
            if not path.is_relative_to(root / "source") and not path.is_relative_to(root / "cargo-home"):
                raise ValueError("dependency build script escapes owned source/cache")
            scripts.append({"package": package["name"], "version": package["version"], "path": str(path), "sha256": digest(path)})
    # No V8 native library or generated binding has been prepared. Do not let a
    # runtime feature switch masquerade as a compile-time dependency guarantee.
    native = [packages[identity]["name"] for identity in closure if packages[identity]["name"] in ("v8", "codex-code-mode-runtime")]
    return {"root_package": root_name, "root_dev_dependencies": include_root_dev, "packages": sorted(packages[identity]["name"] + "@" + packages[identity]["version"] for identity in closure), "build_scripts": scripts, "unprepared_native_artifacts": sorted(native)}


def audit_metadata(root, metadata_file, receipt):
    metadata = json.loads(metadata_file.read_text())
    admission = {"build_id": receipt["build_id"], "metadata_file": metadata_file.name, "metadata_sha256": digest(metadata_file), **target_metadata(root, metadata, "codex-app-server")}
    admission["core_unit"] = target_metadata(root, metadata, "codex-core", include_root_dev=True)
    (root / "metadata-admission.json").write_text(json.dumps(admission, indent=2) + "\n")


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    commands = parser.add_subparsers(dest="action", required=True)
    initial = commands.add_parser("prepare")
    initial.add_argument("--upstream-git", type=Path, required=True)
    initial.add_argument("--output", type=Path, required=True)
    initial.add_argument("--target", default="x86_64-unknown-linux-gnu", choices=["x86_64-unknown-linux-gnu"])
    initial.set_defaults(run=prepare)
    cache = commands.add_parser("cache")
    cache.add_argument("--output", type=Path, required=True)
    cache.add_argument("--from-cache", type=Path, required=True)
    cache.set_defaults(run=prepare_cache)
    build = commands.add_parser("check")
    build.add_argument("--output", type=Path, required=True)
    build.add_argument("--host", default="x86_64-unknown-linux-gnu", choices=["x86_64-unknown-linux-gnu"])
    build.add_argument("--phase", choices=["metadata", "check", "runtime-tests", "submission-tests", "journal-tests", "build"], default="metadata")
    build.set_defaults(run=check)
    args = parser.parse_args()
    args.run(args)


if __name__ == "__main__":
    main()
