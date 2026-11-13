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
    path = fakebin / "codex"
    path.write_text("""#!/usr/bin/env python3
import json,sys
if '--version' in sys.argv:
    print('codex-cli 0.167.0');sys.exit(0)
for line in sys.stdin:
    f=json.loads(line); method=f.get('method'); i=f.get('id')
    def send(x): print(json.dumps(x),flush=True)
    if method=='initialize': send({'id':i,'result':{'userAgent':'codex_cli_rs/0.167.0 (fixture)'}})
    elif method in ('account/login/start','turn/interrupt'): send({'id':i,'result':{}})
    elif method in ('thread/start','thread/resume'): send({'id':i,'result':{'thread':{'id':'archive-fixture'}}})
    elif method=='turn/start':
        send({'id':i,'result':{'turn':{'id':'turn-1'}}})
        send({'method':'thread/tokenUsage/updated','params':{'threadId':'archive-fixture','tokenUsage':{'total':{'inputTokens':1,'outputTokens':1}}}})
        send({'method':'item/completed','params':{'threadId':'archive-fixture','turnId':'turn-1','item':{'type':'agentMessage','text':'ARCHIVE_SMOKE_OK'}}})
        send({'method':'turn/completed','params':{'threadId':'archive-fixture','turn':{'id':'turn-1','status':'completed'}}})
""")
    path.chmod(0o700)
    env = dict(os.environ, PATH=str(fakebin) + os.pathsep + os.environ["PATH"], TMPDIR=str(root), AW_ARCHIVE_FIXTURE_KEY="disposable-local-key")
    def run(*args):
        return subprocess.check_output(list(args), cwd=workspace, env=env, text=True)
    version = json.loads(run(str(binary), "version"))
    run(str(binary), "init", "--workspace", str(workspace))
    (workspace / "agents.yml").write_text("agents:\n  codex:\n    provider: codex-app-server\n    authentication: {mode: api-key, credential-env: AW_ARCHIVE_FIXTURE_KEY}\n")
    run("git", "init", "-b", "main")
    run("git", "config", "user.name", "Archive Fixture")
    run("git", "config", "user.email", "archive@example.invalid")
    run("git", "add", ".")
    run("git", "commit", "-m", "Disposable archive fixture")
    run(str(binary), "validate", "--workspace", str(workspace), "workflows/parallel-review.yml")
    doctor = json.loads(run(str(binary), "doctor", "--workspace", str(workspace)))
    (workspace / "workflows" / "smoke.yml").write_text("jobs: {x: {steps: [{uses: agent/run, with: {agent: agents/task.toml, prompt: fixture}}]}}\n")
    result = json.loads(run(str(binary), "run", "--foreground", "--workspace", str(workspace), "workflows/smoke.yml"))
    assert result["status"] == "succeeded", result
    assert result["normalized_usage"]["input_tokens"] == 1, result
    assert result["jobs"]["x"]["steps"]["step_1"]["outputs"]["text"] == "ARCHIVE_SMOKE_OK", result
    print(json.dumps({"archive": archive.name, "build": version, "assertions": ["extract", "version", "init", "validate", "doctor", "fake-provider-run", "accounting"], "model_calls": 0}))
