#!/usr/bin/env python3
"""Build/save digest-addressed worker images locally. No publication/model calls."""
import argparse
import hashlib
import json
import platform
from pathlib import Path
from release_lib import git
import subprocess
import tempfile
import os

root = Path(__file__).resolve().parents[1]
parser = argparse.ArgumentParser(description=__doc__)
parser.add_argument("--runtime", choices=["podman", "docker"], default="podman")
parser.add_argument("--arch", choices=["amd64", "arm64"], required=True)
parser.add_argument("--output", default=str(root / "dist"))
args = parser.parse_args()
pins = json.loads((root / "images" / "pins.json").read_text())
out = Path(args.output).resolve()
out.mkdir(parents=True, exist_ok=True)
host_arch = {"x86_64": "amd64", "aarch64": "arm64", "arm64": "arm64"}[platform.machine()]
iid = out / ("worker-linux-" + args.arch + ".iid")
epoch = git("show", "-s", "--format=%ct", "HEAD")
builder = ["docker", "buildx", "build", "--output", "type=docker,rewrite-timestamp=true", "--provenance=false"] if args.runtime == "docker" else ["podman", "build", "--timestamp", epoch]
command = builder + ["--platform", "linux/" + args.arch,
    "--build-arg", "SOURCE_DATE_EPOCH=" + epoch,
    "--build-arg", "BUILDPLATFORM=linux/" + host_arch,
    "--build-arg", "TARGETARCH=" + args.arch,
    "--build-arg", "GO_IMAGE=" + pins["go_image"],
    "--build-arg", "NODE_IMAGE=" + pins["node_image"],
    "--iidfile", str(iid), "-f", "images/worker.Dockerfile", "."]
with tempfile.TemporaryDirectory(prefix="workflow-public-registry-") as directory:
    # Public bases do not require or modify the caller's native registry login.
    # A stale private auth entry must not silently change this build's inputs.
    authentication = Path(directory) / "auth.json"
    (Path(directory) / 'config.json').write_text('{"auths":{}}\n')
    authentication.write_text('{"auths":{}}\n')
    authentication.chmod(0o600)
    if args.runtime == "podman":
        command[2:2] = ["--authfile", str(authentication)]
    subprocess.run(command, cwd=root, check=True, env=dict(os.environ, REGISTRY_AUTH_FILE=str(authentication), **({"DOCKER_CONFIG": directory} if args.runtime == "docker" else {})))
image = iid.read_text().strip()
if not image.startswith("sha256:") or len(image) != 71:
    raise SystemExit("runtime did not produce a digest-addressed image")
metadata = json.loads(subprocess.check_output([args.runtime, "image", "inspect", image], text=True))[0]
expected = {"agent-workflow.guardian-protocol": pins["guardian_protocol"], "agent-workflow.bridge-protocol": pins["bridge_protocol"], "agent-workflow.claude-sdk": pins["claude_sdk"], "agent-workflow.codex-cli": pins["codex_cli"]}
for key, value in expected.items():
    if metadata["Config"]["Labels"].get(key) != value:
        raise SystemExit("worker image protocol label mismatch")
if str(metadata["Config"].get("User", "")).split(":")[0] in ["", "0", "root"]:
    raise SystemExit("worker image default user must be non-root")
archive_format = "oci-archive" if args.runtime == "podman" else "docker-archive"
archive = out / ("worker-linux-" + args.arch + (".oci.tar" if args.runtime == "podman" else ".docker.tar"))
save = [args.runtime, "save", "--output", str(archive)]
if args.runtime == "podman":
    save += ["--format", "oci-archive"]
subprocess.run(save + [image], check=True)
with archive.open("rb") as stream:
    archive_hash = hashlib.sha256()
    for chunk in iter(lambda: stream.read(1024 * 1024), b""):
        archive_hash.update(chunk)
report = {"archive_format": archive_format, "image": image, "architecture": args.arch, "base_images": pins, "archive": archive.name, "archive_sha256": archive_hash.hexdigest(), "published": False}
(out / ("worker-linux-" + args.arch + ".json")).write_text(json.dumps(report, indent=2) + "\n")
print(json.dumps(report))
