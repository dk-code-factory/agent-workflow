#!/usr/bin/env python3
"""Opt-in interactive native Codex acceptance; imports never start conversations."""
import argparse
import base64
from datetime import datetime, timezone
import fcntl
import hashlib
import json
import os
from pathlib import Path
import queue
import select
import shutil
import signal
import socket
import subprocess
import sys
import tempfile
import threading
import time

from subjects import SUBJECTS, seed, snapshot

ROOT = Path(__file__).resolve().parents[2]


def run(arguments, **kwargs):
    return subprocess.run(arguments, check=True, capture_output=True, text=True, timeout=30, **kwargs).stdout


def isolated_credential(source):
    credential = json.loads(Path(source).read_text())
    tokens = credential.get("tokens")
    if credential.get("auth_mode") != "chatgpt" or not isinstance(tokens, dict):
        raise ValueError("This acceptance route requires existing native ChatGPT credentials.")
    payload = tokens["access_token"].split(".")[1]
    claims = json.loads(base64.urlsafe_b64decode(payload + "=" * (-len(payload) % 4)))
    refreshed = datetime.fromisoformat(credential["last_refresh"].replace("Z", "+00:00"))
    if claims.get("exp", 0) < time.time() + 2700 or (datetime.now(timezone.utc) - refreshed).total_seconds() > 86400 * 4:
        raise ValueError("Existing credentials would need refresh during acceptance; select a valid authorized route explicitly.")
    # Preserve the required serialized field, but remove all refresh authority.
    tokens["refresh_token"] = ""
    return credential


def discovered_skills(listing):
    return {skill["name"].removeprefix("agent-workflow:"): skill for row in listing["data"]
            for skill in row["skills"] if skill.get("pluginId") == "agent-workflow@agent-workflow" and skill.get("enabled")}


def owned_controller(engine, workspace, environment, output):
    process = subprocess.Popen([str(engine), "controller", "--workspace", str(workspace), "--parallel", "2"],
                               env=environment, stdout=output, stderr=output, start_new_session=True)
    address_file = workspace / ".agent-workflow/controller.address"
    for _ in range(200):
        if process.poll() is not None:
            raise RuntimeError("Owned controller failed to start; inspect private controller log.")
        try:
            address = address_file.read_text().strip()
            with socket.socket(socket.AF_UNIX, socket.SOCK_STREAM) as connection:
                connection.settimeout(0.05)
                connection.connect(address)
            return process
        except (OSError, ValueError):
            time.sleep(0.025)
    process.terminate()
    process.wait(timeout=5)
    raise TimeoutError("Owned controller startup deadline expired.")


class Session:
    def __init__(self, binary, environment, workspace, evidence, deadline):
        self.deadline, self.sequence, self.frames = deadline, 0, []
        self.queue = queue.Queue()
        self.trace = (evidence / "native-trace.jsonl").open("w")
        self.stderr = (evidence / "native.stderr").open("w")
        self.process = subprocess.Popen([str(binary), "app-server"], cwd=workspace, env=environment,
                                        stdin=subprocess.PIPE, stdout=subprocess.PIPE, stderr=self.stderr,
                                        text=True, start_new_session=True)
        def reader():
            for line in self.process.stdout:
                self.trace.write(line)
                self.trace.flush()
                try:
                    self.queue.put(json.loads(line))
                except ValueError:
                    self.queue.put({"invalid_frame": True})
            self.queue.put({"eof": True})
        self.reader = threading.Thread(target=reader, daemon=True)
        self.reader.start()

    def send(self, value):
        self.trace.write(json.dumps({"client": value}) + "\n")
        self.trace.flush()
        self.process.stdin.write(json.dumps(value) + "\n")
        self.process.stdin.flush()

    def next(self):
        value = self.queue.get(timeout=max(0.001, self.deadline - time.monotonic()))
        self.frames.append(value)
        if value.get("eof") or value.get("invalid_frame"):
            raise RuntimeError("Native app-server ended or emitted an invalid frame; inspect private trace.")
        return value

    def request(self, method, params):
        self.sequence += 1
        sequence = self.sequence
        self.send({"id": sequence, "method": method, "params": params})
        while True:
            frame = self.next()
            if frame.get("id") == sequence and "method" not in frame:
                if "error" in frame:
                    raise RuntimeError("Native " + method + " failed; inspect private trace.")
                return frame["result"]
            if "id" in frame and "method" in frame:
                self.send({"id": frame["id"], "error": {"code": -32601, "message": "Unconfigured request denied"}})

    def turn(self, thread, message, skill, answer_packet, approved):
        start = len(self.frames)
        params = {"threadId": thread, "input": [{"type": "text", "text": message},
                  {"type": "skill", "name": skill["name"], "path": skill["path"]}]}
        self.request("turn/start", params)
        while True:
            frame = self.next()
            if frame.get("method") == "item/tool/requestUserInput":
                if approved:
                    # Additional material requirements require a new human decision.
                    raise RuntimeError("A new input decision is required after approval; inspect trace and retain failure.")
                questions = frame["params"]["questions"]
                self.send({"id": frame["id"], "result": {"answers": {q["id"]: {"answers": [answer_packet]} for q in questions}}})
            elif "id" in frame and "method" in frame:
                self.send({"id": frame["id"], "error": {"code": -32601, "message": "Acceptance fixture denies extra authority"}})
            if frame.get("method") == "turn/completed":
                if frame["params"]["turn"]["status"] != "completed":
                    raise RuntimeError("Native turn failed; inspect private trace.")
                break
        text = "\n".join(frame["params"]["item"].get("text", "") for frame in self.frames[start:]
                         if frame.get("method") == "item/completed" and frame["params"].get("item", {}).get("type") == "agentMessage")
        print(text, flush=True)
        return text

    def close(self):
        if self.process.poll() is None:
            os.killpg(self.process.pid, signal.SIGTERM)
            try:
                self.process.wait(timeout=5)
            except subprocess.TimeoutExpired:
                os.killpg(self.process.pid, signal.SIGKILL)
                self.process.wait()
        self.reader.join(timeout=2)
        self.trace.close()
        self.stderr.close()


def recover_gate(engine, workspace, environment, evidence, run_id, deadline):
    def cli(verb, *arguments):
        return json.loads(run([str(engine), verb, "--workspace", str(workspace), *arguments], env=environment))
    before = cli("inputs", run_id)
    if len(before) != 1 or before[0]["state"] != "pending":
        raise ValueError("Recovery needs exactly one pending approval gate.")
    gate = before[0]
    record = {"same_run": False, "same_gate": False, "stale_answer_rejected": False, "processing_count": None, "report_correct": False}
    def checkpoint():
        (evidence / "recovery.json").write_text(json.dumps(record, indent=2))
    checkpoint()
    cli("stop")
    # Resume the same waiting run starts a replacement owned controller, never a replacement run.
    cli("resume", "--parallel", "2", "--detach", run_id)
    after = cli("inputs", run_id)
    if after[0]["gate_id"] != gate["gate_id"] or after[0]["evidence_id"] != gate["evidence_id"]:
        raise ValueError("Gate identity changed across recovery.")
    record.update(same_run=True, same_gate=True)
    checkpoint()
    answer = evidence / "answer.json"
    answer.write_text('"approve"')
    flags = ["--gate", gate["gate_id"], "--answer-file", str(answer)]
    stale = subprocess.run([str(engine), "answer", "--workspace", str(workspace), *flags,
                            "--answer-id", "stale-delivery", "--evidence-id", "stale-evidence", run_id], env=environment, capture_output=True, timeout=15)
    if stale.returncode == 0 or b"stale" not in stale.stderr:
        raise ValueError("Stale gate answer did not produce the expected explicit conflict.")
    record["stale_answer_rejected"] = True
    checkpoint()
    cli("answer", *flags, "--answer-id", "current-delivery", "--evidence-id", gate["evidence_id"], run_id)
    while time.monotonic() < deadline:
        state = cli("status", run_id)
        if state["status"] == "succeeded":
            break
        if state["status"] not in {"running", "queued", "waiting"}:
            record["processing_count"] = int((workspace / "fixture/processed.count").read_text())
            checkpoint()
            raise ValueError("Recovered workflow failed.")
        time.sleep(0.1)
    else:
        raise TimeoutError("Recovery exceeded subject deadline.")
    if (workspace / "fixture/processed.count").read_text() != "1" or (workspace / "fixture/report.txt").read_text() != "Total: 5\n":
        raise ValueError("Processing repeated or report incorrect.")
    record.update(same_run=state["id"] == run_id, processing_count=1, report_correct=True)
    checkpoint()
    print(json.dumps(record), flush=True)


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--scenario", choices=SUBJECTS, required=True)
    parser.add_argument("--allow-live", action="store_true", required=True)
    parser.add_argument("--codex", type=Path, required=True)
    parser.add_argument("--credential-file", type=Path, required=True)
    parser.add_argument("--candidate-archive", type=Path, required=True)
    parser.add_argument("--sha256", required=True)
    parser.add_argument("--allowance", type=Path, required=True, help="same private journal for all three subjects")
    parser.add_argument("--evidence-parent", type=Path, required=True)
    args = parser.parse_args()
    if os.environ.get("CI") or os.environ.get("GITHUB_ACTIONS"):
        parser.error("Manual acceptance refuses CI.")
    binary = args.codex.resolve()
    config = json.loads((ROOT / "plugins/agent-workflow/compatibility.json").read_text())
    if run([str(binary), "--version"]).strip() != "codex-cli " + config["codex_versions"][0]:
        parser.error("Select the repository-pinned Codex binary; normal Codex is not changed.")
    if not args.credential_file.is_file():
        parser.error("An existing authorized native credential file is required; no login is performed.")
    if hashlib.sha256(args.candidate_archive.read_bytes()).hexdigest() != args.sha256:
        parser.error("Candidate digest mismatch.")
    args.evidence_parent.mkdir(mode=0o700, parents=True, exist_ok=True)
    evidence = Path(tempfile.mkdtemp(prefix=args.scenario + "-", dir=args.evidence_parent)).resolve()
    evidence.chmod(0o700)
    home, workspace = evidence / "codex-home", evidence / "repository"
    home.mkdir(mode=0o700)
    workspace.mkdir()
    environment = {key: value for key, value in os.environ.items() if key in {"PATH", "LANG", "TERM", "SYSTEMROOT", "SSL_CERT_FILE", "SSL_CERT_DIR"}}
    # Keep AF_UNIX paths below the macOS/Linux limit, within explicitly writable fixture roots.
    cache = Path(tempfile.mkdtemp(prefix="awpc-")).resolve()
    cache.chmod(0o700)
    (evidence / "cache-location.txt").write_text(str(cache))
    environment.update(HOME=str(evidence), CODEX_HOME=str(home), XDG_CACHE_HOME=str(cache), TMPDIR=str(evidence / "tmp"), PYTHONDONTWRITEBYTECODE="1")
    Path(environment["TMPDIR"]).mkdir(mode=0o700)
    tools = evidence / "tools"
    tools.mkdir()
    (tools / "codex").symlink_to(binary)
    environment["PATH"] = str(tools) + os.pathsep + environment["PATH"]
    # Copy into isolated state and remove refresh authority. Never modify source auth.
    credential = isolated_credential(args.credential_file)
    (home / "auth.json").write_text(json.dumps(credential))
    (home / "auth.json").chmod(0o600)
    (home / "config.toml").write_text('approval_policy = "never"\nsandbox_mode = "workspace-write"\n[sandbox_workspace_write]\nnetwork_access = true\nwritable_roots = ' + json.dumps([str(evidence), str(cache)]) + '\n[plugins."agent-workflow@agent-workflow"]\nenabled = true\n[projects.' + json.dumps(str(workspace)) + ']\ntrust_level = "trusted"\n')
    run(["git", "init", "-q", str(workspace)])
    seed(workspace, args.scenario)
    # Copy the marketplace sources. Native installation, not a synthetic skill injection.
    market = evidence / "marketplace"
    (market / ".agents/plugins").mkdir(parents=True)
    shutil.copyfile(ROOT / ".agents/plugins/marketplace.json", market / ".agents/plugins/marketplace.json")
    shutil.copytree(ROOT / "plugins", market / "plugins", ignore=shutil.ignore_patterns("__pycache__"))
    # Selected provider settings are fixture configuration, not authored workflow files.
    sandbox = "workspace-write" if args.scenario == "plugin-bounded-fix" else "read-only"
    roles = "controller:\n  run-timeout: 15m\nagents:\n  codex:\n    provider: codex-app-server\n    command: " + json.dumps(str(binary)) + "\n    authentication:\n      mode: native\n      credential-file: " + json.dumps(str(home / "auth.json")) + "\n    sandbox: " + sandbox + "\n    limits: {max-turns: 4, turn-timeout: 3m}\n"
    (workspace / "agents.yml").write_text(roles)
    baseline = snapshot(workspace)
    print("PRIVATE_EVIDENCE=" + str(evidence), flush=True)
    # Check/install candidate before reserving model allowance; deterministic only.
    launcher = market / "plugins/agent-workflow/scripts/agent-workflow"
    candidate = ["--candidate-archive", str(args.candidate_archive.resolve()), "--sha256", args.sha256]
    install = json.loads(run([sys.executable, str(launcher), "ensure", *candidate], env=environment))
    distribution = Path(install["distribution"])
    engine = distribution / "agent-workflow"
    # Test only assets from the verified candidate distribution, without checkout fallback.
    shutil.rmtree(market / "plugins")
    shutil.copytree(distribution / "plugins", market / "plugins")
    shutil.copyfile(distribution / ".agents/plugins/marketplace.json", market / ".agents/plugins/marketplace.json")
    run([str(binary), "plugin", "marketplace", "add", str(market)], env=environment, cwd=workspace)
    run([str(binary), "plugin", "add", "agent-workflow@agent-workflow"], env=environment, cwd=workspace)
    args.allowance.parent.mkdir(mode=0o700, parents=True, exist_ok=True)
    fd = os.open(args.allowance, os.O_CREAT | os.O_RDWR | os.O_NOFOLLOW, 0o600)
    fcntl.flock(fd, fcntl.LOCK_EX | fcntl.LOCK_NB)
    with os.fdopen(fd, "r+") as allowance:
        raw = allowance.read()
        ledger = json.loads(raw) if raw else {"attempts": {}, "elapsed_seconds": 0}
        if args.scenario in ledger["attempts"] or ledger["elapsed_seconds"] >= 2700:
            parser.error("Allowance exhausted or this subject was already attempted. Renew explicit authorization before using a new allowance.")
        started = time.monotonic()
        deadline = started + min(900, 2700 - ledger["elapsed_seconds"])
        effect_deadline = deadline - 45  # Reserve time to stop nested work and retain evidence.
        ledger["attempts"][args.scenario] = {"started": time.time(), "status": "running"}
        # Reserve the full allowance durably so a killed harness cannot reclaim time.
        reserved = deadline - started
        ledger["elapsed_seconds"] += reserved
        def save():
            allowance.seek(0)
            json.dump(ledger, allowance)
            allowance.truncate()
            allowance.flush()
            os.fsync(allowance.fileno())
        save()
        session, approved, error, review, model_started = None, False, None, None, False
        controller, controller_output, run_requested = None, None, False
        try:
            session = Session(binary, environment, workspace, evidence, effect_deadline)
            session.request("initialize", {"clientInfo": {"name": "agent-workflow-plugin-acceptance", "version": "1"}, "capabilities": {"experimentalApi": True}})
            session.send({"method": "initialized"})
            account = session.request("account/read", {"refreshToken": False})
            if not account.get("account"):
                raise ValueError("Existing native credentials were not loaded; no login or refresh was attempted.")
            listing = session.request("skills/list", {"cwds": [str(workspace)], "forceReload": True})
            (evidence / "skills.json").write_text(json.dumps(listing))
            skills = discovered_skills(listing)
            if not {"build-workflow", "run-workflow"} <= skills.keys():
                raise RuntimeError("Native plugin skills were not discovered.")
            directive = "Development acceptance uses the explicit uncertified candidate. For every launcher ensure/exec use --candidate-archive " + str(args.candidate_archive.resolve()) + " --sha256 " + args.sha256 + ". No release fallback, remote mutations, login or publication. Use existing agents.yml. Do not run the workflow until a separate request."
            thread = session.request("thread/start", {"cwd": str(workspace), "approvalPolicy": "never", "sandbox": "workspace-write", "developerInstructions": directive})["thread"]["id"]
            packet = SUBJECTS[args.scenario]["answers"]
            model_started = True
            text = session.turn(thread, SUBJECTS[args.scenario]["request"], skills["build-workflow"], packet, False)
            while True:
                if not approved and snapshot(workspace) != baseline:
                    raise RuntimeError("Files changed before explicit plan approval.")
                plan_hash = hashlib.sha256(text.encode()).hexdigest()
                print("CONTROL: JSON {message: text}, {approve: " + plan_hash + "}, {run: true}, {recover: RUN_ID}, or {finish: REVIEW_OBJECT}. Inspect native traces for attempted writes before approval.", flush=True)
                ready, _, _ = select.select([sys.stdin], [], [], max(0.001, effect_deadline - time.monotonic()))
                if not ready:
                    raise TimeoutError("Subject's 15-minute deadline expired.")
                control = json.loads(sys.stdin.readline())
                if "fail" in control:
                    (evidence / "review.json").write_text(json.dumps(control["fail"], indent=2))
                    raise RuntimeError("Operator review found unsatisfied acceptance criteria; see private review.json.")
                if "finish" in control:
                    review = control["finish"]
                    required = {"necessary_questions", "resolved_architecture", "complete_plan", "no_implementation_attempt_before_approval", "artifact_checks"}
                    if not approved or not isinstance(review, dict) or not all(review.get(key) is True for key in required):
                        raise ValueError("Completion needs actual trace/artifact review of all acceptance criteria.")
                    (evidence / "review.json").write_text(json.dumps(review, indent=2))
                    break
                if "recover" in control:
                    if not approved or args.scenario != "plugin-gated-report-recovery":
                        raise ValueError("Recovery control is confined to the approved gate subject.")
                    recover_gate(engine, workspace, environment, evidence, control["recover"], effect_deadline)
                    continue
                if "approve" in control:
                    if approved or control["approve"] != plan_hash:
                        raise ValueError("Approval must name the exact current complete plan digest.")
                    approved = True
                    (evidence / "approved-plan.txt").write_text(text)
                    message = "I explicitly approve this complete implementation and test plan. Implement and validate it now. Do not execute the workflow until I request it."
                elif control.get("run"):
                    if not approved:
                        raise ValueError("A complete plan must be approved first.")
                    if run_requested:
                        raise ValueError("The subject already requested its one workflow run. Inspect its durable identity.")
                    run_requested = True
                    controller_output = (evidence / "owned-controller.log").open("w")
                    controller = owned_controller(engine, workspace, environment, controller_output)
                    message = "Run the approved workflow once using agent-workflow:run-workflow with --parallel 2. Preserve and report its durable run ID. If it waits for approval, leave it waiting for the harness; do not answer or restart it yourself."
                else:
                    message = control["message"]
                selected = skills["run-workflow"] if control.get("run") else skills["build-workflow"]
                text = session.turn(thread, message, selected, packet, approved)
        except Exception as failure:
            error = type(failure).__name__ + ": " + str(failure)
            print(error, file=sys.stderr)
        finally:
            if session:
                session.close()
            # Stop only the owned disposable workspace's controller; preserve durable state.
            try:
                subprocess.run([str(engine), "stop", "--workspace", str(workspace)], env=environment, capture_output=True, timeout=10)
            except (OSError, subprocess.TimeoutExpired):
                error = error or "Owned controller shutdown failed; inspect retained state."
            if controller:
                try:
                    controller.wait(timeout=5)
                except subprocess.TimeoutExpired:
                    controller.terminate()
                    try:
                        controller.wait(timeout=5)
                    except subprocess.TimeoutExpired:
                        controller.kill()
                        controller.wait()
            if controller_output:
                controller_output.close()
            elapsed = time.monotonic() - started
            ledger["elapsed_seconds"] -= max(0, reserved - elapsed)
            if elapsed > 900:
                error = error or "Subject exceeded its inclusive time allowance."
            ledger["attempts"][args.scenario].update(status="failed" if error else "passed" if review else "needs-review", elapsed_seconds=elapsed)
            if not model_started:
                # Local discovery failures precede the subject; retain them separately.
                ledger.setdefault("preflight_failures", []).append({"scenario": args.scenario, **ledger["attempts"].pop(args.scenario)})
            save()
            summary = {"scenario": args.scenario, "status": "failed" if error else "passed" if review else "needs-review", "approved_plan": approved,
                       "elapsed_seconds": elapsed, "cost_usd": None, "cost_status": "unavailable unless reported in native usage",
                       "codex_version": config["codex_versions"][0], "candidate_sha256": args.sha256,
                       "engine_usage": [], "native_usage": [], "private_error": bool(error)}
            summary["plugin_assets"] = {path.relative_to(market / "plugins/agent-workflow").as_posix(): hashlib.sha256(path.read_bytes()).hexdigest()
                                        for path in sorted((market / "plugins/agent-workflow").rglob("*")) if path.is_file() and "__pycache__" not in path.parts}
            if session:
                summary["native_usage"] = [frame["params"].get("tokenUsage") for frame in session.frames if frame.get("method") == "thread/tokenUsage/updated"]
            listing = subprocess.run([str(engine), "list", "--workspace", str(workspace)], env=environment, capture_output=True, text=True, timeout=15)
            if listing.returncode == 0:
                (evidence / "runs.json").write_text(listing.stdout)
                for row in json.loads(listing.stdout).get("items", []):
                    usage = subprocess.run([str(engine), "usage", "--workspace", str(workspace), row["id"]], env=environment, capture_output=True, text=True, timeout=15)
                    if usage.returncode == 0:
                        (evidence / ("usage-" + row["id"] + ".json")).write_text(usage.stdout)
                        summary["engine_usage"].append(json.loads(usage.stdout).get("summary", {}))
            (evidence / "sanitized-summary.json").write_text(json.dumps(summary, indent=2))
    return 1 if error else 0


if __name__ == "__main__":
    sys.exit(main())
