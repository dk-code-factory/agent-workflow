"""One operation per helper invocation; YAML owns all stage and retry order."""

import json
import os
from pathlib import Path
import re
import subprocess

from .core import (FORMAT, GENERATED, STATE, Ledger, artifact, auth, bounded_json,
                   canonical, digest, read_artifact, text, writer_lease)


def git(root, *args, check=True):
    result = subprocess.run(["git", "-C", str(root), *args], capture_output=True,
                            timeout=60, check=False,
                            env={**os.environ, "GIT_TERMINAL_PROMPT": "0"})
    if len(result.stdout) > 16 << 20 or len(result.stderr) > 1 << 20:
        raise ValueError("Git output exceeds byte limit")
    if check and result.returncode:
        raise ValueError("Git command failed: " + result.stderr.decode(errors="replace")[:4096])
    return result.stdout


def load_plan(root, reference):
    plan = read_artifact(root, reference)
    if plan.get("format") != FORMAT:
        raise ValueError("unsupported application plan")
    if plan.get("fingerprint") != digest({k: v for k, v in plan.items() if k != "fingerprint"}):
        raise ValueError("plan fingerprint differs")
    return plan


def source_file(root, name):
    relative = Path(text(name, "source path"))
    if (relative.is_absolute() or ".." in relative.parts or not relative.parts
            or relative.parts[0] in {".git", ".agent-workflow", STATE}
            or relative.is_relative_to(Path(GENERATED))):
        raise ValueError("source path outside the reviewed workspace")
    path = Path(root) / relative
    for parent in [path, *path.parents]:
        if parent == Path(root):
            break
        if parent.is_symlink():
            raise ValueError("source evidence must not traverse symlinks")
    if not path.is_file() or path.stat().st_size > 4 << 20:
        raise ValueError("source evidence must name a regular file of at most 4 MiB")
    import hashlib
    return hashlib.sha256(path.read_bytes()).hexdigest()


def verify_evidence(root, report):
    evidence = report.get("evidence")
    if not isinstance(evidence, list) or not evidence or len(evidence) > 128:
        raise ValueError("a report needs bounded source evidence")
    identities = set()
    for item in evidence:
        if not isinstance(item, dict) or set(item) != {"id", "path", "sha256"}:
            raise ValueError("evidence needs id, path and sha256")
        key = text(item["id"], "evidence ID")
        if key in identities or item["sha256"] != source_file(root, item["path"]):
            raise ValueError("duplicate or changed source evidence")
        identities.add(key)
    return identities


def verify_report(root, task, report):
    if not isinstance(report, dict) or type(report.get("complete")) is not bool:
        raise ValueError("report must declare boolean complete")
    if len(canonical(report)) > 512 << 10:
        raise ValueError("report exceeds application byte limit")
    identities = verify_evidence(root, report)
    checks = report.get("checklist")
    if not isinstance(checks, list):
        raise ValueError("report checklist missing")
    expected = {item["id"] for item in task["checklist"]}
    observed = set()
    all_complete = True
    for check in checks:
        if not isinstance(check, dict) or check.get("id") not in expected or check["id"] in observed:
            raise ValueError("unknown or duplicate checklist item")
        observed.add(check["id"])
        if check.get("status") not in {"completed", "incomplete"}:
            raise ValueError("invalid checklist disposition")
        references = check.get("evidence", [])
        if (not isinstance(references, list) or any(ref not in identities for ref in references)
                or check["status"] == "completed" and not references):
            raise ValueError("checklist evidence does not name verified source evidence")
        all_complete = all_complete and check["status"] == "completed"
    if observed != expected:
        raise ValueError("report does not account for its exact assigned checklist")
    findings = report.get("findings", [])
    if not isinstance(findings, list) or len(findings) > 128:
        raise ValueError("invalid findings")
    seen = set()
    for finding in findings:
        key = text(finding.get("id"), "finding ID")
        text(finding.get("text"), "finding text")
        if key in seen or not finding.get("sources") or any(ref not in identities for ref in finding["sources"]):
            raise ValueError("finding sources do not name verified evidence")
        seen.add(key)
    blockers = report.get("blockers", [])
    if not isinstance(blockers, list) or any(not isinstance(item, str) for item in blockers):
        raise ValueError("invalid blocker list")
    return report["complete"] and all_complete and not blockers


def _task(plan, key):
    matches = [task for task in plan["tasks"] if task["id"] == key]
    if len(matches) != 1:
        raise ValueError("task is outside admitted plan")
    return matches[0]


def _snapshot(root, provider, plan):
    from .capture import capture
    current = git(root, "rev-parse", "HEAD").decode().strip()
    with Ledger(root, plan["logical_run"], plan["pr"]["url"], read_only=True) as ledger:
        known = ledger.db.execute("SELECT head FROM local_commits WHERE head=? AND state='completed'", (current,)).fetchone()
    active = dict(plan)
    if known:
        active["allow_local_head"] = current
    snapshot = capture(root, provider, active)
    if getattr(provider, "last_capture", None) is not None and not getattr(provider, "read_only", False):
        snapshot["evidence_reference"] = artifact(root, provider.last_capture)
    return snapshot


def _fingerprint(snapshot, task):
    from .capture import fingerprint
    return fingerprint(snapshot, task)


def _claims(ledger, task=None):
    query = "SELECT * FROM claims"
    params = ()
    if task:
        query += " WHERE task=?"
        params = (task,)
    return [dict(row) for row in ledger.db.execute(query + " ORDER BY rowid", params)]


def _current(ledger, snapshot, task):
    wanted = _fingerprint(snapshot, task)
    rows = ledger.db.execute("SELECT * FROM claims WHERE task=? AND state='complete' AND coverage=? ORDER BY rowid DESC",
                             (task["id"], wanted)).fetchall()
    return dict(rows[0]) if rows else None


def candidates(root, ledger, plan):
    result = []
    for row in ledger.db.execute("SELECT s.claim_id,s.round,s.agent,s.report,c.task,c.assignment FROM submissions s JOIN claims c ON c.id=s.claim_id ORDER BY s.rowid"):
        task = read_artifact(root, row["assignment"])["task"]
        if task["kind"] not in {"review", "inspection"}:
            continue
        report = bounded_json(row["report"])
        value = {"claim": row["claim_id"], "agent": row["agent"], "round": row["round"], "report": report}
        reference = STATE + "/artifacts/" + digest(value) + ".json"
        read_artifact(root, reference)
        for finding in report.get("findings", []):
            key = "finding_" + digest([row["claim_id"], finding["id"]])
            existing = next((f for f in result if f["id"] == key), None)
            if existing:
                if existing["text"] != finding["text"] or existing["sources"] != finding["sources"]:
                    raise ValueError("checklist continuation rewrote an original finding")
                continue
            result.append({**finding, "id": key,
                           "original_id": finding["id"], "report": reference,
                           "task": row["task"], "agent": row["agent"]})
    return result


def prepare_claim(root, provider, ledger, plan, inputs):
    task = _task(plan, inputs["task"])
    snapshot = _snapshot(root, provider, plan)
    payload = inputs.get("payload", {})
    fingerprint = digest([_fingerprint(snapshot, task), payload]) if payload else _fingerprint(snapshot, task)
    unfinished = ledger.db.execute("SELECT id FROM claims WHERE task=? AND state IN ('claimed','incomplete') LIMIT 1", (task["id"],)).fetchone()
    if unfinished:
        raise ValueError("unfinished logical claim for original task: resume its originating engine execution")
    current = _current(ledger, snapshot, task) if task.get("cacheable", True) else None
    if current:
        return {"claim": current["id"], "invoke": False, "complete": True,
                "prompt": "", "task_input": {}, "report": current["report"]}
    key = "claim_" + digest([plan["logical_run"], task["id"], fingerprint])
    writer_lease(root, plan["logical_run"], key)
    findings = candidates(root, ledger, plan)
    if task["id"] == "finding_fix":
        verdicts = {}
        for row in ledger.db.execute("SELECT report FROM accepted_reports WHERE kind='verification' ORDER BY rowid"):
            for disposition in bounded_json(row["report"]).get("dispositions", []):
                verdicts[disposition["finding_id"]] = disposition["status"]
        findings = [finding for finding in findings if verdicts.get(finding["id"]) == "confirmed"]
    assignment = {"format": FORMAT, "claim": key, "task": task,
                  "snapshot": snapshot, "instructions": plan["instructions"],
                  "settings": plan.get("settings", {}), "findings": findings,
                  "payload": payload,
                  "authorization": plan.get("authorizations", {}),
                  "report_contract": "Source evidence is {id,path,sha256}; checklist evidence names these IDs. "
                  "Do not publish, commit, coordinate stages or start provider processes. "
                  "Keep all original finding IDs and sources in disposition reports."}
    reference = artifact(root, assignment)
    # A cycle is input-sensitive and shared by all review passes over those inputs.
    # A new process, execution or generated plan never replenishes this ledger.
    if task["kind"] == "review":
        cycle = digest([snapshot.get("local_tree"), snapshot.get("discussion"),
                        plan["instructions"], plan.get("settings", {}), plan["review_contract"]])
        ledger.reserve("review_cycles", cycle, plan["limits"]["review_cycles"])
    if task["id"] == "ci_repair":
        latest = ledger.db.execute("SELECT * FROM ci_observations ORDER BY rowid DESC LIMIT 1").fetchone()
        if not latest or latest["head"] != snapshot["head"] or latest["status"] != "failed":
            raise ValueError("CI repair requires the current exact-head failed observation")
        cycle = inputs.get("cycle", 0)
        if type(cycle) is not int or cycle < 1:
            raise ValueError("CI repair needs its retained logical cycle")
        ledger.reserve("ci-repair:" + str(cycle), key, plan["limits"]["ci_repairs_per_cycle"])
        assignment["ci"] = bounded_json(latest["result"])
        reference = artifact(root, assignment)
    prompt = task["assignment"] + "\nReturn the exact structured report contract and checklist."
    if len(prompt.encode()) + len(canonical(assignment)) > 900 << 10:
        raise ValueError("assignment exceeds native prompt allowance")
    if task["writes"]:
        writer_lease(root, plan["logical_run"], key, acquire=True)
    with ledger.transaction():
        unfinished = ledger.db.execute("SELECT id FROM claims WHERE task=? AND state IN ('claimed','incomplete') LIMIT 1", (task["id"],)).fetchone()
        if unfinished:
            raise ValueError("unfinished logical claim for original task: resume its originating engine execution")
        old = ledger.db.execute("SELECT * FROM claims WHERE id=?", (key,)).fetchone()
        if old:
            # The original admitted agent step must resume the same session. A
            # replacement execution cannot re-dispatch an abandoned claim.
            raise ValueError("unfinished logical claim: resume its originating engine execution")
        ledger.db.execute("INSERT INTO claims(id,task,fingerprint,state,assignment) VALUES(?,?,?,?,?)",
                          (key, task["id"], fingerprint, "claimed", reference))
    return {"claim": key, "invoke": True, "complete": False,
            "prompt": prompt, "task_input": assignment, "report": ""}


def _claim(ledger, key):
    row = ledger.db.execute("SELECT * FROM claims WHERE id=?", (text(key, "claim"),)).fetchone()
    if not row:
        raise ValueError("claim is outside retained logical run")
    return dict(row)


def _dispositions(root, ledger, plan, task, report, agent, assignment):
    original = assignment["findings"]
    evidence_ids = {item["id"] for item in report["evidence"]}
    for disposition in report.get("dispositions", []):
        sources = disposition.get("sources")
        if not isinstance(sources, list) or not sources or any(source not in evidence_ids for source in sources):
            raise ValueError("disposition sources must name verified source evidence")
    if task["kind"] == "verification":
        if any(candidate["agent"] == agent for candidate in original):
            raise ValueError("finding verification must use an independent native agent")
        wanted = {finding["id"] for finding in original}
        observed = {}
        for disposition in report.get("dispositions", []):
            key = disposition.get("finding_id")
            if (key not in wanted or key in observed
                    or disposition.get("status") not in {"confirmed", "rejected"}
                    or not disposition.get("sources") or not disposition.get("reason")):
                raise ValueError("independent verification does not account for original finding")
            observed[key] = disposition
        if set(observed) != wanted:
            raise ValueError("independent verification omitted original findings")
    elif task["kind"] == "fix":
        wanted = {finding["id"] for finding in original}
        seen = set()
        for disposition in report.get("dispositions", []):
            key = disposition.get("finding_id")
            if key not in wanted or key in seen or not disposition.get("reason") or not disposition.get("sources"):
                raise ValueError("fix disposition does not name original finding and evidence")
            seen.add(key)
            status = disposition.get("status")
            if status in {"accepted", "deferred"}:
                authority = auth(disposition.get("authorization"))
                if authority != plan["authorizations"].get("finding:" + key):
                    raise ValueError("accepted/deferred finding needs exact retained user authority")
            elif status == "fixed":
                if not report.get("changes"):
                    raise ValueError("a fixed disposition needs verified code changes")
            elif status != "rejected":
                raise ValueError("unsupported finding disposition")
        if seen != wanted:
            raise ValueError("fix report omitted independently confirmed original findings")


def accept(root, provider, ledger, plan, inputs):
    claim = _claim(ledger, inputs["claim"])
    task = _task(plan, claim["task"])
    agent = text(inputs.get("agent"), "opaque native agent reference")
    if not re.fullmatch(r"agent_[a-f0-9]{32}", agent):
        raise ValueError("invalid native agent reference")
    report = inputs["report"]
    round_number = inputs.get("round", 0)
    if type(round_number) is not int or not 0 <= round_number <= 3:
        raise ValueError("checklist allowance exceeded")
    assignment = read_artifact(root, claim["assignment"])
    complete = verify_report(root, task, report)
    if claim["agent"] and claim["agent"] != agent:
        raise ValueError("checklist continuation changed the original native session")
    _dispositions(root, ledger, plan, task, report, agent, assignment)
    reference = artifact(root, {"claim": claim["id"], "agent": agent, "round": round_number, "report": report})
    snapshot = _snapshot(root, provider, plan)
    # A writing report must account for every actual path and commit through the
    # controlled Git helper before obtaining coverage on its resulting tree.
    if task["writes"] and report.get("changes") and complete:
        _commit(root, ledger, claim["id"], task, report, assignment["snapshot"])
        snapshot = _snapshot(root, provider, plan)
    elif not task["writes"] and snapshot.get("local_tree") != assignment["snapshot"].get("local_tree"):
        raise ValueError("read-only task changed the workspace")
    coverage = _fingerprint(snapshot, task)
    with ledger.transaction():
        old = ledger.db.execute("SELECT agent,report FROM submissions WHERE claim_id=? AND round=?",
                                (claim["id"], round_number)).fetchone()
        encoded = canonical(report).decode()
        if old and (old["agent"] != agent or old["report"] != encoded):
            raise ValueError("native submission identity conflicts with original report")
        ledger.db.execute("INSERT OR IGNORE INTO submissions VALUES(?,?,?,?)",
                          (claim["id"], round_number, agent, encoded))
        ledger.db.execute("UPDATE claims SET state=?,agent=?,report=?,coverage=? WHERE id=?",
                          ("complete" if complete else "incomplete", agent, reference, coverage, claim["id"]))
        if complete:
            ledger.db.execute("INSERT OR IGNORE INTO accepted_reports VALUES(?,?,?,?,?,?)",
                              (reference, task["id"], task["kind"], coverage, agent, encoded))
    if complete and task["writes"]:
        writer_lease(root, plan["logical_run"], claim["id"], release=True)
    return {"claim": claim["id"], "complete": complete, "agent": agent,
            "settled": complete or not task["required"] and round_number == 3,
            "feedback": {"checklist": task["checklist"], "prior_report": reference,
                         "original_findings": assignment["findings"]}, "report": reference}


def turn_result(root, provider, ledger, plan, inputs):
    status = inputs.get("status")
    if status == "succeeded":
        accepted = accept(root, provider, ledger, plan, {**inputs, "round": 0})
        return {**accepted, "checklist": not accepted["complete"],
                "retry": False, "prompt": "", "task_input": {}}
    claim = _claim(ledger, inputs["claim"])
    task = _task(plan, claim["task"])
    agent = text(inputs.get("agent"), "original failed native agent")
    if not re.fullmatch(r"agent_[a-f0-9]{32}", agent):
        raise ValueError("failed native turn has no retained original session")
    classification = text(inputs.get("failure_class"), "engine failure classification")
    unsafe = inputs.get("unsafe_failure_class", "")
    diagnostic = inputs.get("error", "")
    if not isinstance(unsafe, str) or not isinstance(diagnostic, str):
        raise ValueError("invalid original failure evidence")
    retried = inputs.get("retried", False)
    if claim["agent"] and claim["agent"] != agent:
        raise ValueError("native retry changed original session identity")
    with ledger.transaction():
        old = ledger.db.execute("SELECT * FROM native_failures WHERE claim_id=? AND round=?", (claim["id"], int(bool(retried)))).fetchone()
        values = (claim["id"], int(bool(retried)), agent, classification, unsafe, diagnostic)
        if old and tuple(old) != values:
            raise ValueError("original native failure observation identity conflict")
        ledger.db.execute("INSERT OR IGNORE INTO native_failures VALUES(?,?,?,?,?,?)", values)
        ledger.db.execute("UPDATE claims SET agent=? WHERE id=?", (agent, claim["id"]))
    # These fields come from authoritative engine step metadata. Arbitrary JSON
    # and error text cannot turn cleanup, integrity or permission failures into
    # a retry; the engine's observed-failure gate still governs continuation.
    eligible = not unsafe and (classification == "usage_exhausted" or
        classification == "provider_failure" and any(token in diagnostic.lower()
            for token in ("session limit", "session_limit")))
    if eligible and not retried and plan["limits"]["retries_per_task"]:
        ledger.reserve("native-retry:" + claim["id"], "original-session-retry", plan["limits"]["retries_per_task"])
        assignment = read_artifact(root, claim["assignment"])
        return {"claim": claim["id"], "complete": False, "retry": True,
                "checklist": False,
                "prompt": "Continue the original bounded assignment in this same session after the retained eligible provider failure. Preserve original analysis and finding identities.",
                "task_input": {"assignment": assignment, "original_failure": {"classification": classification, "diagnostic": diagnostic}}}
    if task["required"] or unsafe:
        raise ValueError("native task failed without eligible retained retry: " + classification)
    if task["writes"]:
        assignment = read_artifact(root, claim["assignment"])
        snapshot = _snapshot(root, provider, plan)
        if snapshot["local_tree"] != assignment["snapshot"]["local_tree"] or snapshot["local_head"] != assignment["snapshot"]["local_head"]:
            raise ValueError("failed optional writer has unresolved source effects")
    with ledger.transaction():
        ledger.db.execute("UPDATE claims SET state='optional_failed' WHERE id=?", (claim["id"],))
    if task["writes"]:
        writer_lease(root, plan["logical_run"], claim["id"], release=True)
    return {"claim": claim["id"], "complete": False, "retry": False, "checklist": False, "prompt": "", "task_input": {}}


def _commit(root, ledger, claim_id, task, report, snapshot):
    retained = ledger.db.execute("SELECT * FROM local_commits WHERE id=?", (claim_id,)).fetchone()
    if retained:
        if retained["message"] != report.get("commit_message"):
            raise ValueError("controlled commit request identity changed")
        observed = git(root, "rev-parse", "HEAD").decode().strip()
        if observed != retained["head"]:
            raise ValueError("controlled commit is unresolved; inspect its original parent/head")
        with ledger.transaction():
            ledger.db.execute("UPDATE local_commits SET state='completed' WHERE id=?", (claim_id,))
        return
    if git(root, "diff", "--cached", "--name-only", "-z"):
        raise ValueError("refusing a preexisting Git index")
    manifest = report["changes"]
    if not isinstance(manifest, list) or not manifest or len(manifest) > 256:
        raise ValueError("invalid changed-file manifest")
    actual = set(git(root, "diff", "--name-only", "-z").decode().split("\0")) - {""}
    actual.update(set(git(root, "ls-files", "--others", "--exclude-standard", "-z").decode().split("\0")) - {""})
    actual = {name for name in actual if name.split("/")[0] != STATE
              and not Path(name).is_relative_to(Path(GENERATED))}
    names = []
    import hashlib
    for change in manifest:
        name = text(change.get("path"), "changed path")
        source_file(root, name) if change.get("after") is not None else None
        if Path(name).is_absolute() or ".." in Path(name).parts or name in names:
            raise ValueError("invalid changed-file manifest path")
        before = git(root, "show", "HEAD:" + name, check=False)
        before_exists = subprocess.run(["git", "-C", str(root), "cat-file", "-e", "HEAD:" + name],
                                       capture_output=True, timeout=60).returncode == 0
        expected_before = hashlib.sha256(before).hexdigest() if before_exists else None
        path = Path(root) / name
        expected_after = source_file(root, name) if path.exists() else None
        if change.get("before") != expected_before or change.get("after") != expected_after:
            raise ValueError("changed-file manifest does not match source bytes")
        names.append(name)
    if actual != set(names):
        raise ValueError("writer did not account for every workspace change")
    message = text(report.get("commit_message"), "native commit message")
    if len(message.encode()) > 16384:
        raise ValueError("commit message too large")
    parent = git(root, "rev-parse", "HEAD").decode().strip()
    if parent != snapshot["local_head"]:
        raise ValueError("agent changed Git history outside controlled commit")
    git(root, "--literal-pathspecs", "add", "--", *names)
    staged = set(git(root, "diff", "--cached", "--name-only", "-z").decode().split("\0")) - {""}
    if staged != set(names):
        raise ValueError("staged files differ from native accepted manifest")
    tree = git(root, "write-tree").decode().strip()
    tree_paths = set(git(root, "diff-tree", "--no-commit-id", "--name-only", "--no-renames", "-r", "-z", parent, tree).decode().split("\0")) - {""}
    if tree_paths != set(names):
        raise ValueError("proposed commit tree differs from native accepted manifest")
    for change in manifest:
        blob = git(root, "show", tree + ":" + change["path"], check=False)
        exists = subprocess.run(["git", "-C", str(root), "cat-file", "-e", tree + ":" + change["path"]],
                                capture_output=True, timeout=60).returncode == 0
        if (hashlib.sha256(blob).hexdigest() if exists else None) != change["after"]:
            raise ValueError("staged blob bytes differ from native accepted manifest")
    result = subprocess.run(["git", "-C", str(root), "commit-tree", tree, "-p", parent],
                            input=message.encode(), capture_output=True, timeout=60)
    if result.returncode:
        raise ValueError("controlled commit failed: " + result.stderr.decode(errors="replace")[:4096])
    committed = result.stdout.decode().strip()
    if not re.fullmatch(r"[a-f0-9]{40}(?:[a-f0-9]{24})?", committed):
        raise ValueError("controlled commit returned an invalid object identity")
    with ledger.transaction():
        ledger.db.execute("INSERT INTO local_commits VALUES(?,?,?,?,?,?)",
                          (claim_id, parent, committed, tree, message, "intent"))
    git(root, "update-ref", "HEAD", committed, parent)
    git(root, "reset", "--mixed", committed)
    with ledger.transaction():
        ledger.db.execute("UPDATE local_commits SET state='completed' WHERE id=?", (claim_id,))


def task_status(root, provider, ledger, plan, inputs):
    claim = _claim(ledger, inputs["claim"])
    task = _task(plan, claim["task"])
    if claim["state"] != "complete" and task["required"]:
        raise ValueError("required task checklist remains incomplete")
    return {"complete": claim["state"] == "complete", "claim": claim["id"], "report": claim["report"]}


def coverage(root, provider, ledger, plan, inputs):
    snapshot = _snapshot(root, provider, plan)
    missing = []
    for task in plan["tasks"]:
        if task["required"]:
            current = _current(ledger, snapshot, task)
            if not current:
                missing.append(task["id"])
                continue
            wrapper = read_artifact(root, current["report"])
            accepted = ledger.db.execute("SELECT * FROM accepted_reports WHERE reference=?", (current["report"],)).fetchone()
            if (not accepted or wrapper.get("claim") != current["id"]
                    or wrapper.get("agent") != current["agent"]
                    or wrapper.get("report") != bounded_json(accepted["report"])
                    or accepted["task"] != task["id"] or accepted["kind"] != task["kind"]
                    or accepted["fingerprint"] != current["coverage"]
                    or not verify_report(root, task, wrapper["report"])):
                raise ValueError("accepted task coverage lost its original native evidence")
    unresolved = []
    originals = candidates(root, ledger, plan)
    confirmed, rejected, settled = set(), set(), set()
    for row in ledger.db.execute("SELECT kind,report FROM accepted_reports ORDER BY rowid"):
        report = bounded_json(row["report"])
        for item in report.get("dispositions", []):
            key = item.get("finding_id")
            if row["kind"] == "verification":
                (confirmed if item.get("status") == "confirmed" else rejected).add(key)
            if row["kind"] == "fix" and item.get("status") in {"fixed", "accepted", "deferred"}:
                settled.add(key)
    for candidate in originals:
        key = candidate["id"]
        if key not in rejected and (key not in confirmed or key not in settled):
            unresolved.append(key)
    if any(stage["kind"] == "ci" for stage in plan["stages"]):
        observed = ledger.db.execute("SELECT * FROM ci_observations ORDER BY rowid DESC LIMIT 1").fetchone()
        if not observed or observed["head"] != snapshot["head"] or observed["status"] != "success" or snapshot["local_head"] != snapshot["head"]:
            missing.append("current_head_ci")
    return {"complete": not missing and not unresolved, "ready": False,
            "missing": missing, "unresolved": unresolved,
            "head": snapshot["head"], "local_head": snapshot["local_head"],
            "target": snapshot["target"], "snapshot": snapshot}
