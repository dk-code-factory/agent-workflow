#!/usr/bin/env python3
"""Build local release artifacts. Never publish or invoke a real provider."""
import argparse
import hashlib
import io
import json
import os
from pathlib import Path
import platform
import shutil
import subprocess
import tarfile
import tempfile
from urllib.parse import quote

ROOT = Path(__file__).resolve().parents[1]
parser = argparse.ArgumentParser(description=__doc__)
parser.add_argument("--output", default=str(ROOT / "dist"))
parser.add_argument("--platform", action="append", choices=["linux/amd64", "linux/arm64", "darwin/amd64", "darwin/arm64"])
parser.add_argument("--smoke", action="store_true", help="smoke-test the native extracted archive using fake providers only")
args = parser.parse_args()
output = Path(args.output).resolve()
output.mkdir(parents=True, exist_ok=True)
scratch = ROOT / ".agent-workflow" / "release-build"
scratch.mkdir(parents=True, exist_ok=True)

def run(command, **kwargs):
    return subprocess.run(command, cwd=ROOT, check=True, **kwargs)

locked_go_files = {name: (ROOT / name).read_bytes() for name in ["go.mod", "go.sum"]}
run(["go", "mod", "download", "all"])
if any((ROOT / name).read_bytes() != value for name, value in locked_go_files.items()):
    raise SystemExit("Go dependency locks were incomplete; review and commit them before packaging")

metadata = json.loads(subprocess.check_output(["go", "run", "./cmd/agent-workflow", "version"], cwd=ROOT))
version = metadata["version"]
commit = subprocess.check_output(["git", "rev-parse", "HEAD"], cwd=ROOT, text=True).strip()
dirty = bool(subprocess.check_output(["git", "status", "--porcelain"], cwd=ROOT))
epoch = int(subprocess.check_output(["git", "show", "-s", "--format=%ct", "HEAD"], cwd=ROOT))
def fingerprint_source():
    source_hash = hashlib.sha256()
    files = subprocess.check_output(["git", "ls-files", "--cached", "--others", "--exclude-standard", "-z"], cwd=ROOT).decode().split("\0")
    for name in sorted(set(files)):
        path = ROOT / name
        if path.is_file() and (path.suffix in [".go", ".mod", ".sum", ".mjs", ".json", ".py", ".yml", ".yaml", ".md"] or path.name in ["LICENSE", "NOTICE", "Makefile"]):
            source_hash.update(name.encode() + b"\0" + path.read_bytes() + b"\0")
    return source_hash.hexdigest()
source_fingerprint = fingerprint_source()
ldflags = "-s -w -X github.com/dkropachev/agent-workflow/internal/buildinfo.Commit=" + commit + " -X github.com/dkropachev/agent-workflow/internal/buildinfo.Dirty=" + str(dirty).lower() + " -X github.com/dkropachev/agent-workflow/internal/buildinfo.SourceFingerprint=" + source_fingerprint
targets = args.platform or ["linux/amd64", "linux/arm64", "darwin/amd64", "darwin/arm64"]
raw = subprocess.check_output(["go", "list", "-m", "-json", "all"], cwd=ROOT, text=True)
decoder = json.JSONDecoder()
modules = []
while raw.strip():
    value, end = decoder.raw_decode(raw.lstrip())
    raw = raw.lstrip()[end:]
    modules.append(value)
goroot = Path(subprocess.check_output(["go", "env", "GOROOT"], text=True).strip())
archives = []
for target in targets:
    goos, arch = target.split("/")
    name = f"agent-workflow-{version}-{goos}-{arch}"
    with tempfile.TemporaryDirectory(dir=scratch) as temporary:
        stage = Path(temporary) / name
        stage.mkdir()
        env = dict(os.environ, CGO_ENABLED="0", GOOS=goos, GOARCH=arch)
        for command in ["agent-workflow", "agent-workflow-guardian", "workflow-conformance"]:
            run(["go", "build", "-trimpath", "-ldflags", ldflags, "-o", str(stage / command), "./cmd/" + command], env=env)
        for item in ["README.md", "DESIGN.md", "LICENSE", "NOTICE", "agents.yml", "go.mod", "go.sum"]:
            shutil.copyfile(ROOT / item, stage / item)
        for directory in ["workflows", "docs", "conformance"]:
            shutil.copytree(ROOT / directory, stage / directory)
        licenses = stage / "licenses"
        licenses.mkdir()
        shutil.copyfile(goroot / "LICENSE", licenses / "Go-LICENSE.txt")
        license_index = []
        for module in modules:
            if "Dir" not in module:
                raise SystemExit("missing module source for license inventory: " + module["Path"])
            source = Path(module["Dir"])
            copied = []
            for text in sorted(source.iterdir()) if source.is_dir() else []:
                if text.is_file() and text.name.upper().startswith(("LICENSE", "LICENCE", "COPYING", "NOTICE")):
                    license_name = hashlib.sha256(module["Path"].encode()).hexdigest()[:12] + "-" + text.name
                    shutil.copyfile(text, licenses / license_name)
                    copied.append(license_name)
            license_index.append({"module": module["Path"], "version": module.get("Version", "development"), "files": copied})
        (licenses / "index.json").write_text(json.dumps(license_index, indent=2) + "\n")
        bridge = stage / "bridge" / "claude"
        bridge.mkdir(parents=True)
        for item in ["bridge.mjs", "package.json", "package-lock.json"]:
            shutil.copyfile(ROOT / "bridge" / "claude" / item, bridge / item)
        build = dict(metadata, os=goos, arch=arch, commit=commit, dirty=str(dirty).lower(), source_fingerprint=source_fingerprint)
        (stage / "build.json").write_text(json.dumps(build, indent=2) + "\n")
        archive = output / (name + ".tar.gz")
        def normalize(info):
            info.uid = info.gid = 0
            info.uname = info.gname = ""
            info.mtime = epoch
            return info
        with tarfile.open(archive, "w:gz") as bundle:
            bundle.add(stage, arcname=name, filter=normalize)
        archives.append(archive)

# Dependency inventories are generated from the actual locked inputs.
lock = json.loads((ROOT / "bridge" / "claude" / "package-lock.json").read_text())
public_modules = [{key: value for key, value in module.items() if key in ["Path", "Version", "Sum", "GoModSum", "GoVersion"]} for module in modules]
inventory = {"engine": dict(metadata, commit=commit, dirty=dirty), "go_modules": public_modules, "npm_packages": lock["packages"]}
(output / "dependencies.json").write_text(json.dumps(inventory, indent=2) + "\n")
components = [{"type": "library", "name": value["Path"], "version": value.get("Version", "development"), "purl": "pkg:golang/" + value["Path"] + "@" + value.get("Version", "development")} for value in modules]
for path, package in lock["packages"].items():
    if path and "version" in package:
        name = path.split("node_modules/")[-1]
        components.append({"type": "library", "name": name, "version": package["version"], "purl": "pkg:npm/" + quote(name, safe="/") + "@" + package["version"]})
(output / "sbom.cdx.json").write_text(json.dumps({"bomFormat": "CycloneDX", "specVersion": "1.6", "version": 1, "components": components}, indent=2) + "\n")

if args.smoke:
    native_os = {"Linux": "linux", "Darwin": "darwin"}.get(platform.system())
    native_arch = {"x86_64": "amd64", "aarch64": "arm64", "arm64": "arm64"}.get(platform.machine())
    native = output / f"agent-workflow-{version}-{native_os}-{native_arch}.tar.gz"
    if native not in archives:
        parser.error("native archive must be selected for --smoke")
    run(["python3", "scripts/smoke-release.py", str(native)])

if fingerprint_source() != source_fingerprint:
    raise SystemExit("source changed during artifact construction; rebuild from a stable tree")
checksums = []
for artifact in sorted(output.iterdir()):
    if artifact.is_file() and artifact.name != "SHA256SUMS":
        digest = hashlib.sha256()
        with artifact.open("rb") as stream:
            for chunk in iter(lambda: stream.read(1024 * 1024), b""):
                digest.update(chunk)
        checksums.append(digest.hexdigest() + "  " + artifact.name)
(output / "SHA256SUMS").write_text("\n".join(checksums) + "\n")
print(json.dumps({"version": version, "archives": [str(path) for path in archives], "published": False}))
