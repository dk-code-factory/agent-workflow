#!/usr/bin/env python3
"""Generate pinned upstream protocol fixtures. This never starts a model turn."""
import argparse
import hashlib
import json
from pathlib import Path
import shutil
import subprocess
import tempfile

parser = argparse.ArgumentParser(description=__doc__)
parser.add_argument("--version", required=True)
parser.add_argument("--codex-binary", default="codex")
args = parser.parse_args()
if not args.version or any(c not in "0123456789.-" for c in args.version):
    parser.error("version must be an exact upstream numeric version")
reported = subprocess.check_output([args.codex_binary, "--version"], text=True).strip()
if reported != "codex-cli " + args.version:
    parser.error("installed provider version does not match --version")
root = Path(__file__).resolve().parents[1]
temporary = root / ".agent-workflow" / "protocol-fixtures"
temporary.mkdir(parents=True, exist_ok=True, mode=0o700)
destination = root / "pkg" / "clirunner" / "testdata" / "codex" / args.version
sources = [
    "v1/InitializeParams.json", "v2/ThreadStartParams.json",
    "v2/ThreadResumeParams.json", "v2/TurnInterruptParams.json",
    "v2/LoginAccountParams.json", "DynamicToolCallParams.json",
    "DynamicToolCallResponse.json",
]
with tempfile.TemporaryDirectory(dir=temporary) as directory:
    subprocess.run([args.codex_binary, "app-server", "generate-json-schema",
                    "--experimental", "--out", directory], check=True)
    generated = Path(directory)
    bundle = hashlib.sha256()
    for path in sorted(generated.rglob("*.json")):
        bundle.update(str(path.relative_to(generated)).encode() + b"\0")
        bundle.update(path.read_bytes() + b"\0")
    manifest = {
        "provider": reported, "source": "upstream CLI generate-json-schema --experimental",
        "documentation": "https://learn.chatgpt.com/docs/app-server",
        "bundle_sha256": bundle.hexdigest(), "files": {},
        "managed_policy_certified": False, "live_provider_verified": False,
    }
    for source in sources:
        target = destination / source
        target.parent.mkdir(parents=True, exist_ok=True)
        shutil.copyfile(generated / source, target)
        manifest["files"][source] = hashlib.sha256(target.read_bytes()).hexdigest()
    (destination / "manifest.json").write_text(json.dumps(manifest, indent=2) + "\n")
