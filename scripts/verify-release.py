#!/usr/bin/env python3
"""Verify archive contents/checksums and pinned image metadata without providers."""
import hashlib
import json
import re
from pathlib import Path
import sys
import tarfile

root = Path(sys.argv[1] if len(sys.argv) > 1 else "dist").resolve()
def digest_file(path):
    value = hashlib.sha256()
    with path.open("rb") as stream:
        for chunk in iter(lambda: stream.read(1024 * 1024), b""):
            value.update(chunk)
    return value.hexdigest()

checksums = set()
for line in (root / "SHA256SUMS").read_text().splitlines():
    digest, name = line.split("  ", 1)
    if name in checksums or not re.fullmatch(r"[0-9a-f]{64}", digest):
        raise SystemExit("duplicate artifact or invalid checksum")
    if Path(name).name != name:
        raise SystemExit("unsafe checksum artifact path")
    checksums.add(name)
    if digest_file(root / name) != digest:
        raise SystemExit("checksum mismatch: " + name)
checked = []
for archive in sorted(root.glob("agent-workflow-*.tar.gz")):
    if archive.name not in checksums:
        raise SystemExit("archive missing from checksums: " + archive.name)
    with tarfile.open(archive) as source:
        members = source.getmembers()
        if not members or len({entry.name for entry in members}) != len(members):
            raise SystemExit("empty archive or duplicate members")
        prefix = members[0].name.split("/")[0]
        names = {entry.name.removeprefix(prefix + "/") for entry in members}
        for required in ["agent-workflow", "agent-workflow-runtime", "agent-workflow-guardian", "workflow-conformance", "agents.yml", "LICENSE", "NOTICE", "licenses/index.json", "bridge/claude/bridge.mjs", "bridge/claude/package-lock.json", "build.json"]:
            if required not in names:
                raise SystemExit("archive missing " + required)
        for entry in members:
            if not (entry.isfile() or entry.isdir()) or entry.name.startswith("/") or ".." in Path(entry.name).parts or "node_modules" in Path(entry.name).parts or ".agent-workflow" in Path(entry.name).parts:
                raise SystemExit("archive contains unsafe/private/unlocked content")
        metadata = json.load(source.extractfile(prefix + "/build.json"))
        if metadata["source_fingerprint"] == "unrecorded":
            raise SystemExit("archive lacks exact source fingerprint")
        checked.append({"archive": archive.name, "build": metadata})
if not checked:
    raise SystemExit("no distribution archives verified")
for image in (root / "worker-linux-amd64.json", root / "worker-linux-arm64.json"):
    if not image.exists():
        continue
    value = json.loads(image.read_text())
    if Path(value["archive"]).name != value["archive"] or value["archive"] not in checksums or image.name not in checksums:
        raise SystemExit("unsafe or unchecked worker image artifact")
    if digest_file(root / value["archive"]) != value["archive_sha256"]:
        raise SystemExit("worker image archive checksum mismatch")
    with tarfile.open(root / value["archive"]) as source:
        names = set(source.getnames())
        if "index.json" in names:
            index = json.load(source.extractfile("index.json"))
            if len(index["manifests"]) != 1:
                raise SystemExit("expected one worker image manifest")
            manifest_digest = index["manifests"][0]["digest"]
            manifest_bytes = source.extractfile("blobs/" + manifest_digest.replace(":", "/")).read()
            if "sha256:" + hashlib.sha256(manifest_bytes).hexdigest() != manifest_digest:
                raise SystemExit("OCI manifest digest mismatch")
            manifest = json.loads(manifest_bytes)
            config_digest = manifest["config"]["digest"]
            config = source.extractfile("blobs/" + config_digest.replace(":", "/")).read()
        elif "manifest.json" in names:
            manifest = json.load(source.extractfile("manifest.json"))
            if len(manifest) != 1:
                raise SystemExit("expected one Docker worker image")
            config = source.extractfile(manifest[0]["Config"]).read()
        else:
            raise SystemExit("unknown worker image archive format")
        if "sha256:" + hashlib.sha256(config).hexdigest() != value["image"]:
            raise SystemExit("worker image identity does not match archive config")
        config = json.loads(config)
        if config["architecture"] != value["architecture"] or config["os"] != "linux":
            raise SystemExit("worker image platform mismatch")
        if str(config["config"].get("User", "")).split(":")[0] in ["", "0", "root"]:
            raise SystemExit("worker image must default to non-root")
manifest_path = root / "release-manifest.json"
if manifest_path.exists():
    manifest = json.loads(manifest_path.read_text())
    if manifest["repository"] != "dk-code-factory/agent-workflow" or not re.fullmatch(r"[0-9a-f]{40}", manifest["commit"]):
        raise SystemExit("release manifest identity mismatch")
    if not re.fullmatch(r"[0-9a-f]{64}", manifest["source_fingerprint"]):
        raise SystemExit("release source fingerprint mismatch")
    for name, digest in manifest["assets"].items():
        if Path(name).name != name or name not in checksums or digest_file(root / name) != digest:
            raise SystemExit("release manifest asset mismatch: " + name)
    if checksums != set(manifest["assets"]) | {"release-manifest.json", "manifest-provenance.jsonl"}:
        raise SystemExit("release manifest/checksum asset set mismatch")
    platforms = set()
    for item in checked:
        build = item["build"]
        if build["commit"] != manifest["commit"] or build["dirty"] != "false" or build["source_fingerprint"] != manifest["source_fingerprint"] or "v" + build["version"] != manifest["tag"]:
            raise SystemExit("archive does not match clean release candidate")
        platforms.add((build["os"], build["arch"]))
    if len(checked) != 4 or platforms != {(os, arch) for os in ["linux", "darwin"] for arch in ["amd64", "arm64"]}:
        raise SystemExit("release must contain all four CLI platforms")
    for arch in ["amd64", "arm64"]:
        if not (root / ("worker-linux-" + arch + ".json")).exists():
            raise SystemExit("release worker platform missing")
    summary = json.loads((root / "vulnerability-summary.json").read_text())
    if summary.get("all_checks_passed") is not True or len(summary.get("reports", [])) != 3 or any(r["exit_code"] != 0 for r in summary["reports"]):
        raise SystemExit("release vulnerability audits did not pass")
print(json.dumps({"verified_archives": checked, "published": False}))
