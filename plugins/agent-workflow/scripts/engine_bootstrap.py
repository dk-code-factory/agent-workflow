"""Shared exact-tag engine bootstrap, using Python 3.11+ stdlib only.

Distribution contract for generated plugins:
* Copy this maintained file unchanged to scripts/engine_bootstrap.py.
* Write compatibility.json in the package root with schema_version=1,
  engine_version=<exact X.Y.Z>, repository=REPOSITORY and
  release_workflow=WORKFLOW. Plugin versions are independent of engine versions.
* A package-relative entrypoint sets sys.dont_write_bytecode before importing
  this module and calls main(Path(__file__).resolve().parents[1], argv).
* Keep check, ensure and exec -- <engine argument array> as separate operations.
  This module does not infer workflows, parse workflow inputs or select roles.

The built-in plugin calls main(..., builtin=True) to retain its manifest version
coupling and Codex compatibility check. Neither mode accepts a different release
repository/workflow, logs in, installs dependencies or falls back to a PATH engine.
"""
import argparse
from contextlib import contextmanager
import hashlib
import json
import os
from pathlib import Path, PurePosixPath
import platform
import re
import shutil
import stat
import subprocess
import sys
import tarfile
import tempfile

CONFIG_FILENAME = "compatibility.json"
REPOSITORY = "dk-code-factory/agent-workflow"
WORKFLOW = REPOSITORY + "/.github/workflows/release.yml"
HEX = re.compile(r"[0-9a-f]{64}\Z")
VERSION = re.compile(r"(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)\Z")
HELPERS = ("agent-workflow", "agent-workflow-runtime", "agent-workflow-guardian", "workflow-conformance")


class InstallError(Exception):
    pass


def require(condition, message):
    if not condition:
        raise InstallError(message)


def digest(path):
    value = hashlib.sha256()
    with Path(path).open("rb") as stream:
        for chunk in iter(lambda: stream.read(1024 * 1024), b""):
            value.update(chunk)
    return value.hexdigest()


def configuration(package_root, *, builtin=False):
    """Read package-owned engine selection, without granting execution authority."""
    package_root = Path(package_root)
    config = json.loads((package_root / CONFIG_FILENAME).read_text())
    require(isinstance(config, dict) and type(config.get("schema_version")) is int and config["schema_version"] == 1,
            "Unsupported bootstrap configuration schema.")
    version = config.get("engine_version")
    require(isinstance(version, str) and VERSION.fullmatch(version),
            "Invalid packaged engine version; require exact X.Y.Z.")
    require(config.get("repository") == REPOSITORY and config.get("release_workflow") == WORKFLOW,
            "Invalid packaged release identity.")
    if builtin:
        for path in (package_root / "plugin.json", package_root / ".codex-plugin/plugin.json"):
            require(json.loads(path.read_text())["version"] == version, "Plugin/engine version mismatch.")
        versions = config.get("codex_versions")
        require(isinstance(versions, list) and versions and all(isinstance(v, str) and v for v in versions),
                "Invalid packaged Codex compatibility versions.")
    return config


def host(system=None, machine=None):
    system, machine = system or platform.system(), machine or platform.machine()
    goos = {"linux": "linux", "darwin": "darwin"}.get(system.lower())
    arch = {"x86_64": "amd64", "amd64": "amd64", "aarch64": "arm64", "arm64": "arm64"}.get(machine.lower())
    require(goos and arch, f"Unsupported host {system}/{machine}. Use Linux or macOS on amd64 or arm64.")
    return goos, arch


def command(arguments):
    result = subprocess.run(arguments, capture_output=True, text=True)
    # Do not echo gh stderr: it can contain credentials, private paths or HTTP headers.
    require(result.returncode == 0, f"{arguments[0]} {arguments[1]} failed. Check installed tooling, release availability and existing authentication; no login was attempted.")
    return result.stdout


def prerequisites(config, *, require_codex=False):
    require(sys.version_info >= (3, 11), "Python 3.11+ is required.")
    for tool in (("gh", "codex") if require_codex else ("gh",)):
        require(shutil.which(tool), f"Missing dependency: {tool}. Install it explicitly, then retry setup.")
    if require_codex:
        version = command(["codex", "--version"]).strip()
        require(version.removeprefix("codex-cli ") in config["codex_versions"],
                "Incompatible Codex version. Expected " + ", ".join(config["codex_versions"]) + "; found " + version + ". Select a compatible installation explicitly.")
    # Local help inspection verifies support without contacting GitHub.
    help_text = command(["gh", "attestation", "verify", "--help"])
    require(all(flag in help_text for flag in ("--source-ref", "--source-digest", "--signer-digest", "--signer-workflow", "--bundle")),
            "GitHub CLI lacks required attestation verification flags. Upgrade gh explicitly.")


def private_directory(path):
    # Refuse symlinks in every existing cache component, including redirected XDG paths.
    path = path.absolute()
    for parent in reversed((path, *path.parents)):
        if parent.is_symlink():
            raise InstallError("Cache paths must not contain symlinks.")
    path.mkdir(mode=0o700, parents=True, exist_ok=True)
    info = path.stat()
    require(info.st_uid == os.getuid() and stat.S_IMODE(info.st_mode) == 0o700,
            "Cache directory must be owned by this user with mode 0700: " + str(path))


@contextmanager
def locked(root):
    import fcntl  # Loaded only after supported-host selection (not available on Windows).
    private_directory(root)
    fd = os.open(root / ".install.lock", os.O_CREAT | os.O_RDWR | os.O_NOFOLLOW, 0o600)
    try:
        require(stat.S_ISREG(os.fstat(fd).st_mode), "Invalid install lock.")
        fcntl.flock(fd, fcntl.LOCK_EX)
        yield
    finally:
        os.close(fd)


def tree_identity(root):
    result = {}
    for path in sorted(root.rglob("*")):
        require(not path.is_symlink(), "Cached installation contains a link.")
        require(path.is_dir() or path.is_file(), "Cached installation contains a special file.")
        if path.is_file():
            result[path.relative_to(root).as_posix()] = [digest(path), stat.S_IMODE(path.stat().st_mode)]
    return result


def validate_build(root, version, goos, arch, manifest=None):
    build = json.loads((root / "build.json").read_text())
    require(isinstance(build, dict), "Invalid extracted build identity document.")
    require(all(build.get(k) == v for k, v in {"version": version, "os": goos, "arch": arch}.items()),
            "Extracted build version/platform mismatch.")
    require(re.fullmatch(r"[0-9a-f]{40}", build.get("commit", "")) and HEX.fullmatch(build.get("source_fingerprint", "")),
            "Build lacks a recorded commit/source identity.")
    if manifest:
        require(build.get("dirty") == "false" and all(build.get(k) == manifest[k] for k in ("commit", "source_fingerprint")),
                "Build identity differs from the certified release manifest.")
    for helper in HELPERS:
        path = root / helper
        require(path.is_file() and not path.is_symlink() and path.stat().st_mode & 0o111,
                "Distribution missing executable " + helper)
    return build


def extract(archive, destination, prefix):
    """Validate the entire member graph before materializing regular files ourselves."""
    with tarfile.open(archive, "r:gz") as bundle:
        members = bundle.getmembers()
        seen = set()
        total = 0
        require(bool(members), "Empty distribution archive.")
        for item in members:
            path = PurePosixPath(item.name)
            require(not path.is_absolute() and path.parts and path.parts[0] == prefix
                    and ".." not in path.parts and "\\" not in item.name
                    and (item.isfile() or item.isdir()) and not item.mode & 0o7000,
                    "Unsafe distribution archive member: " + item.name)
            require(path.as_posix() not in seen, "Duplicate archive member.")
            seen.add(path.as_posix())
            total += item.size
        require(len(members) <= 100000 and total <= 2 * 1024**3, "Distribution exceeds extraction limits.")
        for item in members:
            path = destination.joinpath(*PurePosixPath(item.name).parts)
            if item.isdir():
                path.mkdir(parents=True, exist_ok=True)
            else:
                path.parent.mkdir(parents=True, exist_ok=True)
                with bundle.extractfile(item) as source, path.open("xb") as target:
                    shutil.copyfileobj(source, target)
                path.chmod(0o755 if item.mode & 0o111 else 0o644)
    return destination / prefix


def download(tag, name, directory):
    try:
        command(["gh", "release", "download", tag, "--repo", REPOSITORY, "--pattern", name, "--dir", str(directory)])
    except InstallError as error:
        raise InstallError(f"Release {REPOSITORY}@{tag} asset {name} is unavailable or GitHub access failed. Check release availability and existing access explicitly.") from error
    require((directory / name).is_file(), "Release asset unavailable: " + name)


def release_archive(version, goos, arch, directory):
    tag = "v" + version
    for name in ("release-manifest.json", "manifest-provenance.jsonl"):
        download(tag, name, directory)
    manifest = json.loads((directory / "release-manifest.json").read_text())
    require(isinstance(manifest, dict) and isinstance(manifest.get("assets"), dict), "Invalid release manifest document.")
    require(manifest.get("repository") == REPOSITORY and manifest.get("tag") == tag
            and re.fullmatch(r"[0-9a-f]{40}", manifest.get("commit", ""))
            and HEX.fullmatch(manifest.get("source_fingerprint", "")), "Release manifest identity mismatch.")
    command(["gh", "attestation", "verify", str(directory / "release-manifest.json"),
             "--repo", REPOSITORY, "--signer-workflow", WORKFLOW,
             "--source-ref", "refs/tags/" + tag, "--source-digest", manifest["commit"],
             "--signer-digest", manifest["commit"], "--bundle", str(directory / "manifest-provenance.jsonl")])
    name = f"agent-workflow-{version}-{goos}-{arch}.tar.gz"
    expected = manifest["assets"].get(name, "")
    require(isinstance(expected, str) and HEX.fullmatch(expected), "Release manifest is missing the exact platform archive digest.")
    download(tag, name, directory)
    require(digest(directory / name) == expected, "Release archive digest mismatch.")
    return directory / name, expected, manifest


def cached(path, version, goos, arch, mode):
    require(not any(parent.is_symlink() for parent in (path, *path.parents)), "Installation paths must not contain symlinks.")
    require(not (path / "receipt.json").is_symlink() and not (path / "distribution").is_symlink(),
            "Installation receipt/distribution must not be a symlink.")
    receipt = json.loads((path / "receipt.json").read_text())
    require(receipt["mode"] == mode, "Candidate/release cache mismatch.")
    build = validate_build(path / "distribution", version, goos, arch, receipt.get("manifest"))
    require(tree_identity(path / "distribution") == receipt["files"], "Cached distribution is corrupt; remove this version's cache directory and retry ensure.")
    require(build == receipt["build"], "Cached build identity changed.")
    return path / "distribution"


def installation_path(cache, version, goos, arch, candidate_digest=None):
    require(isinstance(version, str) and VERSION.fullmatch(version), "Invalid exact engine version.")
    require(goos in ("linux", "darwin") and arch in ("amd64", "arm64"), "Unsupported engine platform.")
    require(candidate_digest is None or isinstance(candidate_digest, str) and HEX.fullmatch(candidate_digest),
            "Invalid candidate cache digest.")
    return cache / ("candidate" if candidate_digest else "release") / version / (goos + "-" + arch + ("-" + candidate_digest if candidate_digest else ""))


def ensure(cache, version, goos, arch, candidate=None, expected=None):
    mode = "uncertified-candidate" if candidate else "release"
    if candidate:
        require(expected and HEX.fullmatch(expected), "Candidate mode requires an expected SHA-256 digest.")
        require(digest(candidate) == expected, "Candidate archive digest mismatch.")
    path = installation_path(cache, version, goos, arch, expected if candidate else None)
    with locked(cache):
        require(not any(parent.is_symlink() for parent in (path, *path.parents)),
                "Installation paths must not contain symlinks.")
        if path.exists():
            return cached(path, version, goos, arch, mode)
        # A process killed during download leaves only disposable staging; never a valid install.
        for stale in cache.glob(".install-*"):
            require(stale.is_dir() and not stale.is_symlink(), "Unsafe stale installer staging path.")
            shutil.rmtree(stale)
        with tempfile.TemporaryDirectory(prefix=".install-", dir=cache) as temporary:
            stage = Path(temporary)
            manifest = None
            if candidate:
                # Copy first to pin bytes against concurrent modification of the input.
                archive = stage / "candidate.tar.gz"
                shutil.copyfile(candidate, archive)
                require(digest(archive) == expected, "Candidate changed during installation.")
            else:
                archive, expected, manifest = release_archive(version, goos, arch, stage)
            root = extract(archive, stage / "unpacked", f"agent-workflow-{version}-{goos}-{arch}")
            build = validate_build(root, version, goos, arch, manifest)
            # build.json is verified before any downloaded program is executed.
            actual = json.loads(command([str(root / "agent-workflow"), "version"]))
            require(actual == build, "Executable build identity differs from build.json.")
            install = stage / "complete"
            install.mkdir(mode=0o700)
            root.rename(install / "distribution")
            receipt = {"mode": mode, "archive_sha256": expected, "build": build,
                       "manifest": manifest, "files": tree_identity(install / "distribution")}
            (install / "receipt.json").write_text(json.dumps(receipt, sort_keys=True) + "\n")
            path.parent.mkdir(mode=0o700, parents=True, exist_ok=True)
            install.rename(path)
        return cached(path, version, goos, arch, mode)


def main(package_root, argv=None, *, builtin=False):
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("operation", choices=["check", "ensure", "exec"])
    parser.add_argument("--candidate-archive", type=Path, help="explicit local uncertified archive; never used for release fallback")
    parser.add_argument("--sha256", help="required expected digest with --candidate-archive")
    # Split at -- so engine flags are never interpreted as installer options.
    args = list(sys.argv[1:] if argv is None else argv)
    split = args.index("--") if "--" in args else len(args)
    opts = parser.parse_args(args[:split])
    engine_args = args[split + 1:]
    try:
        config = configuration(package_root, builtin=builtin)
        goos, arch = host()
        prerequisites(config, require_codex=builtin)
        require(bool(opts.candidate_archive) == bool(opts.sha256), "Candidate mode requires both --candidate-archive and --sha256.")
        require(opts.operation == "exec" or not engine_args, "Engine arguments require exec --.")
        require(opts.operation != "exec" or bool(engine_args), "Supply exec -- <engine arguments>.")
        cache = Path(os.environ.get("XDG_CACHE_HOME", str(Path.home() / ".cache"))) / "agent-workflow"
        if opts.operation == "check":
            require(not opts.candidate_archive, "check is read-only; use ensure for candidate installation.")
            path = installation_path(cache, config["engine_version"], goos, arch)
            installed = cached(path, config["engine_version"], goos, arch, "release") if path.exists() else None
            print(json.dumps({"version": config["engine_version"], "platform": goos + "/" + arch,
                              "installed": str(installed) if installed else None, "prerequisites": "ok"}))
            return 0
        root = ensure(cache, config["engine_version"], goos, arch, opts.candidate_archive, opts.sha256)
        if opts.candidate_archive:
            print("agent-workflow: UNCERTIFIED CANDIDATE installation", file=sys.stderr)
        if opts.operation == "ensure":
            print(json.dumps({"distribution": str(root), "certified_release": not bool(opts.candidate_archive)}))
            return 0
        # Replace this process to preserve signals, stdout/stderr and exact engine exit status.
        executable = str(root / "agent-workflow")
        os.execv(executable, [executable, *engine_args])
    except (InstallError, OSError, ValueError, KeyError, TypeError, AttributeError, tarfile.TarError) as error:
        print("agent-workflow: " + str(error), file=sys.stderr)
        return 1
