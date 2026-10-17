#!/usr/bin/env python3
"""Smoke an extracted native archive. Only disposable Git and fake providers."""
import json
import os
from pathlib import Path
import subprocess
import sys
import tarfile
import tempfile

archive = Path(sys.argv[1]).resolve()
with tempfile.TemporaryDirectory(prefix="workflow-archive-smoke-") as temporary:
    root = Path(temporary)
    with tarfile.open(archive) as bundle:
        bundle.extractall(root, filter="data")
    package = next(path for path in root.iterdir() if path.is_dir())
    binary = package / "agent-workflow"
    workspace = root / "workspace"
    workspace.mkdir()
    fakebin = root / "fakebin"
    fakebin.mkdir()
    for provider in ["claude", "codex"]:
        script = """#!/bin/sh
case "$1" in
  --version) echo 'fake-provider archive-conformance'; exit 0;;
  auth) echo '{"loggedIn":true}'; exit 0;;
  login) echo 'cached fixture login'; exit 0;;
esac
cat >/dev/null
"""
        if provider == "claude":
            script += "printf '%s\\n' '{\"type\":\"result\",\"result\":\"ARCHIVE_SMOKE_OK\",\"usage\":{\"input_tokens\":1,\"output_tokens\":1},\"total_cost_usd\":0}'\n"
        else:
            script += "printf '%s\\n' '{\"type\":\"item.completed\",\"item\":{\"type\":\"agent_message\",\"text\":\"ARCHIVE_SMOKE_OK\"}}' '{\"type\":\"turn.completed\",\"usage\":{\"input_tokens\":1,\"output_tokens\":1}}'\n"
        path = fakebin / provider
        path.write_text(script)
        path.chmod(0o700)
    env = dict(os.environ, PATH=str(fakebin) + os.pathsep + os.environ["PATH"], TMPDIR=str(root))
    def run(*args):
        return subprocess.check_output(list(args), cwd=workspace, env=env, text=True)
    version = json.loads(run(str(binary), "version"))
    run(str(binary), "init", "--workspace", str(workspace))
    run("git", "init", "-b", "main")
    run("git", "config", "user.name", "Archive Fixture")
    run("git", "config", "user.email", "archive@example.invalid")
    run("git", "add", ".")
    run("git", "commit", "-m", "Disposable archive fixture")
    run(str(binary), "validate", "--workspace", str(workspace), "workflows/parallel-review.yml")
    doctor = json.loads(run(str(binary), "doctor", "--workspace", str(workspace)))
    (workspace / "workflows" / "smoke.yml").write_text("jobs: {x: {steps: [{uses: agent/claude, with: {prompt: fixture}}]}}\n")
    result = json.loads(run(str(binary), "run", "--foreground", "--workspace", str(workspace), "workflows/smoke.yml"))
    assert result["status"] == "succeeded", result
    assert result["normalized_usage"]["cost_usd"] == 0, result
    print(json.dumps({"archive": archive.name, "build": version, "assertions": ["extract", "version", "init", "validate", "doctor", "fake-provider-run", "accounting"], "model_calls": 0}))
