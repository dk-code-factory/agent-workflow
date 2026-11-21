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


def decode_native_report(report):
    """Decode open-ended fields from the strict native wire schema.

    Codex requires closed objects whose properties are all required. Task-specific
    deliverables and publication payloads have open-ended keys, so only those
    fields use JSON strings on the wire. Existing retained object reports keep
    their original representation and are accepted unchanged.
    """
    def object_field(value, name):
        try:
            value = bounded_json(value) if isinstance(value, str) else value
            # Parsing can produce nonfinite floats or unpaired surrogates.
            # Check serialization while the original wire report is retained.
            canonical(value)
        except (ValueError, RecursionError) as error:
            raise ReportContractError("native " + name + " must encode a bounded JSON object: " + str(error)) from error
        if not isinstance(value, dict):
            raise ReportContractError("native " + name + " must encode an object")
        return value

    decoded = dict(report)
    for key in ("commit_message", "audience", "deliverables"):
        if decoded.get(key) is None:
            decoded.pop(key, None)
    operations = decoded.get("operations", [])
    if "operations" in decoded and isinstance(operations, list):
        decoded["operations"] = []
        for value in operations:
            decoded["operations"].append(object_field(value, "publication operation"))
    deliverables = decoded.get("deliverables")
    if isinstance(deliverables, str):
        decoded["deliverables"] = object_field(deliverables, "deliverables")
    return decoded


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


class ReportContractError(ValueError):
    """An invalid report grants no coverage and needs a bounded correction."""


class ChecklistContractError(ReportContractError):
    """A bounded same-session correction can repair the checklist envelope."""


def verify_findings(report, identities):
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


def verify_report(root, task, report):
    if not isinstance(report, dict) or type(report.get("complete")) is not bool:
        raise ValueError("report must declare boolean complete")
    if len(canonical(report)) > 512 << 10:
        raise ValueError("report exceeds application byte limit")
    identities = verify_evidence(root, report)
    verify_findings(report, identities)
    blockers = report.get("blockers", [])
    if not isinstance(blockers, list) or any(not isinstance(item, str) for item in blockers):
        raise ValueError("invalid blocker list")
    checks = report.get("checklist")
    if not isinstance(checks, list):
        raise ChecklistContractError("report checklist missing")
    expected = {item["id"] for item in task["checklist"]}
    observed = set()
    all_complete = True
    for check in checks:
        if not isinstance(check, dict) or check.get("id") not in expected or check["id"] in observed:
            raise ChecklistContractError("unknown or duplicate checklist item; return only assigned IDs")
        observed.add(check["id"])
        if check.get("status") not in {"completed", "incomplete"}:
            raise ChecklistContractError("invalid checklist disposition")
        references = check.get("evidence", [])
        if (not isinstance(references, list) or any(ref not in identities for ref in references)
                or check["status"] == "completed" and not references):
            raise ChecklistContractError("checklist evidence does not name verified source evidence")
        all_complete = all_complete and check["status"] == "completed"
    if observed != expected:
        raise ChecklistContractError("report does not account for its exact assigned checklist")
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


def _fingerprint(snapshot, task, ledger=None):
    from .capture import fingerprint
    result = fingerprint(snapshot, task)
    if ledger is not None and task["kind"] in {"verification", "fix"} and task["id"] != "ci_repair":
        originals = candidates(ledger.root, ledger, None)
        verification = [dict(row) for row in ledger.db.execute(
            "SELECT reference,report FROM accepted_reports WHERE kind='verification' ORDER BY rowid")] if task["kind"] == "fix" else []
        result = digest([result, originals, verification])
    return result


def _claims(ledger, task=None):
    query = "SELECT * FROM claims"
    params = ()
    if task:
        query += " WHERE task=?"
        params = (task,)
    return [dict(row) for row in ledger.db.execute(query + " ORDER BY rowid", params)]


def _current(ledger, snapshot, task):
    wanted = _fingerprint(snapshot, task, ledger)
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
    fingerprint = digest([_fingerprint(snapshot, task, ledger), payload]) if payload else _fingerprint(snapshot, task, ledger)
    unfinished = ledger.db.execute("SELECT id FROM claims WHERE task=? AND state IN ('claimed','incomplete') LIMIT 1", (task["id"],)).fetchone()
    if unfinished:
        raise ValueError("unfinished logical claim for original task: resume its originating engine execution")
    current = _current(ledger, snapshot, task) if task.get("cacheable", True) else None
    if current:
        return {"claim": current["id"], "invoke": False, "complete": True,
                "definition": task["definition"], "prompt": "", "task_input": {}, "report": current["report"]}
    invocation = ledger.db.execute("SELECT count(*) FROM claims WHERE task=?", (task["id"],)).fetchone()[0]
    fingerprint = digest([fingerprint, invocation])
    key = "claim_" + digest([plan["logical_run"], task["id"], fingerprint, invocation])
    writer_lease(root, plan["logical_run"], key)
    # Fresh reviewers receive frozen source/specification/discussion, never
    # hypotheses from earlier or sibling reviewers. Verification and fixing
    # retain the original findings and their identities below.
    findings = [] if task["kind"] == "review" else candidates(root, ledger, plan)
    if task["id"] == "finding_fix":
        verdicts = {}
        for row in ledger.db.execute("SELECT report FROM accepted_reports WHERE kind='verification' ORDER BY rowid"):
            for disposition in bounded_json(row["report"]).get("dispositions", []):
                verdicts[disposition["finding_id"]] = disposition["status"]
        findings = [finding for finding in findings if verdicts.get(finding["id"]) == "confirmed"]
    existing_fixes = {finding["id"]: retained for finding in findings
                      if task["kind"] == "fix"
                      and (retained := _retained_fix(root, ledger, finding["id"]))}
    assignment = {"format": FORMAT, "claim": key, "task": task,
                  "snapshot": snapshot, "instructions": plan["instructions"],
                  "settings": plan.get("settings", {}), "findings": findings,
                  "verified_existing_fixes": existing_fixes,
                  "payload": payload,
                  "invocation": invocation,
                  "authorization": plan.get("authorizations", {}),
                  "report_contract": "Source evidence is {id,path,sha256}; path must name a current regular repository "
                  "source file, relative to the workspace (for example AGENTS.md or the reviewed code). "
                  "Never put .pr-fix-state, .agent-workflow, .git, generated workflow files or "
                  "absolute paths in evidence. Retained snapshot/artifact references are context, "
                  "not source evidence; cite them in deliverables instead. Checklist evidence names "
                  "only verified source evidence IDs. Return exactly the assigned checklist IDs, "
                  "without extra items; put required test results and supplemental checks in deliverables. "
                  "Do not publish, commit, coordinate stages or start provider processes. "
                  "Keep original finding IDs in dispositions. Disposition sources must name only evidence "
                  "IDs from this report; preserve original report/artifact references in deliverables. "
                  "A fixed disposition without new changes requires verified_existing_fixes for that "
                  "original finding: the earlier controlled fix commit must remain an ancestor and "
                  "all its changed source bytes must remain intact. Do not make redundant edits."}
    reference = artifact(root, assignment)
    # Reviewers share each input-sensitive round. A fresh invocation of a
    # noncacheable reviewer consumes another round even when inputs are unchanged.
    # A new process, execution or generated plan never replenishes this ledger.
    if task["kind"] == "review":
        cycle = digest([plan["review_contract"], invocation,
                        [_fingerprint(snapshot, review) for review in plan["tasks"]
                         if review["kind"] == "review"]])
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
            "definition": task["definition"], "prompt": prompt, "task_input": assignment, "report": ""}


def _claim(ledger, key):
    row = ledger.db.execute("SELECT * FROM claims WHERE id=?", (text(key, "claim"),)).fetchone()
    if not row:
        raise ValueError("claim is outside retained logical run")
    return dict(row)


def continuation_current(root, provider, ledger, plan, claim, assignment):
    task = assignment["task"]
    current = _snapshot(root, provider, plan)
    if task["writes"]:
        from .core import verify_writer_lease
        verify_writer_lease(root, plan["logical_run"], claim["id"])
        if any(current[key] != assignment["snapshot"][key] for key in ("head", "local_head")):
            return False
        # The original fenced writer keeps its unfinished edits. Governing
        # instructions, remote evidence and task resources still remain bound.
        current = {**current, **{key: assignment["snapshot"][key] for key in ("local_tree", "diff", "history")}}
    return _fingerprint(current, task, ledger) == _fingerprint(assignment["snapshot"], task, ledger)


def _retained_fix(root, ledger, finding_id):
    """Prove a prior accepted controlled fix is still present, without new edits."""
    head = git(root, "rev-parse", "HEAD").decode().strip()
    for row in ledger.db.execute("SELECT reference,report FROM accepted_reports WHERE kind='fix' ORDER BY rowid DESC"):
        report = bounded_json(row["report"])
        if not any(item.get("finding_id") == finding_id and item.get("status") == "fixed"
                   for item in report.get("dispositions", [])) or not report.get("changes"):
            continue
        wrapper = read_artifact(root, row["reference"])
        if wrapper.get("report") != report:
            raise ValueError("retained fix lost its accepted native report")
        commit = ledger.db.execute("SELECT head FROM local_commits WHERE id=? AND state='completed'",
                                   (wrapper["claim"],)).fetchone()
        if not commit or git(root, "merge-base", commit["head"], head, check=False).decode().strip() != commit["head"]:
            continue
        intact = True
        for change in report["changes"]:
            path = Path(root) / change["path"]
            if change["after"] is None:
                intact = intact and not (path.exists() or path.is_symlink())
            else:
                intact = intact and path.is_file() and source_file(root, change["path"]) == change["after"]
        if intact:
            return {"claim": wrapper["claim"], "report": row["reference"],
                    "head": commit["head"], "changes": report["changes"]}
    return None


def _dispositions(root, ledger, plan, task, report, agent, assignment):
    original = assignment["findings"]
    if task["kind"] == "verification" and any(candidate["agent"] == agent for candidate in original):
        raise ValueError("finding verification must use an independent native agent")
    evidence_ids = {item["id"] for item in report["evidence"]}
    dispositions = report.get("dispositions", [])
    if not isinstance(dispositions, list) or any(not isinstance(item, dict) for item in dispositions):
        raise ReportContractError("dispositions must be an array of objects")
    for disposition in dispositions:
        sources = disposition.get("sources")
        if (not isinstance(sources, list) or not sources or
                any(not isinstance(source, str) or source not in evidence_ids for source in sources)):
            raise ReportContractError("disposition sources must name only evidence IDs in this report; "
                                      "retain original report/source references in deliverables")
    if task["kind"] == "verification":
        wanted = {finding["id"] for finding in original}
        observed = {}
        for disposition in report.get("dispositions", []):
            key = disposition.get("finding_id")
            if (key not in wanted or key in observed
                    or disposition.get("status") not in {"confirmed", "rejected"}
                    or not disposition.get("sources") or not disposition.get("reason")):
                raise ReportContractError("independent verification does not account for original finding")
            observed[key] = disposition
        if set(observed) != wanted:
            raise ReportContractError("independent verification omitted original findings")
    elif task["kind"] == "fix":
        wanted = {finding["id"] for finding in original}
        seen = set()
        for disposition in report.get("dispositions", []):
            key = disposition.get("finding_id")
            if key not in wanted or key in seen or not disposition.get("reason") or not disposition.get("sources"):
                raise ReportContractError("fix disposition does not name original finding and evidence")
            seen.add(key)
            status = disposition.get("status")
            if status in {"accepted", "deferred"}:
                authority = auth(disposition.get("authorization"))
                if authority != plan["authorizations"].get("finding:" + key):
                    raise ValueError("accepted/deferred finding needs exact retained user authority")
            elif status == "fixed":
                if not report.get("changes") and not _retained_fix(root, ledger, key):
                    raise ReportContractError("a fixed disposition needs verified code changes")
            elif status != "rejected":
                raise ReportContractError("unsupported finding disposition")
        if seen != wanted:
            raise ReportContractError("fix report omitted independently confirmed original findings")


def _preserve_rejected_findings(ledger, claim, report):
    current = {item["id"]: item for item in report.get("findings", [])}
    for row in ledger.db.execute("SELECT report FROM report_rejections WHERE claim_id=?", (claim["id"],)):
        for original in bounded_json(row["report"]).get("findings", []):
            if current.get(original["id"]) != original:
                raise ValueError("report correction lost or rewrote an original finding")


def _reject_report(root, provider, ledger, plan, claim, assignment, report, agent, round_number, error):
    if claim["state"] not in {"claimed", "incomplete"}:
        raise ValueError("a rejected report cannot replace completed task coverage")
    if not continuation_current(root, provider, ledger, plan, claim, assignment):
        raise ValueError("governing inputs changed before report correction")
    _preserve_rejected_findings(ledger, claim, report)
    reason = str(error)
    reference = artifact(root, {"claim": claim["id"], "agent": agent, "round": round_number,
                                "report": report, "contract_error": reason})
    encoded = canonical(report).decode()
    with ledger.transaction():
        if ledger.db.execute("SELECT 1 FROM submissions WHERE claim_id=? AND round=?",
                             (claim["id"], round_number)).fetchone():
            raise ValueError("a rejected report conflicts with an accepted original round")
        old = ledger.db.execute("SELECT agent,report,reason FROM report_rejections WHERE claim_id=? AND round=?",
                                (claim["id"], round_number)).fetchone()
        if old and tuple(old) != (agent, encoded, reason):
            raise ValueError("rejected native submission identity conflicts with original report")
        ledger.db.execute("INSERT OR IGNORE INTO report_rejections VALUES(?,?,?,?,?)",
                          (claim["id"], round_number, agent, encoded, reason))
        ledger.db.execute("UPDATE claims SET state='incomplete',agent=?,report=?,coverage='' WHERE id=?",
                          (agent, reference, claim["id"]))
    if round_number == 3:
        raise ValueError("report contract remains invalid after bounded same-session corrections")
    return {"claim": claim["id"], "complete": False, "agent": agent, "settled": False,
            "feedback": {"checklist": assignment["task"]["checklist"], "prior_report": reference,
                         "original_findings": assignment["findings"], "contract_error": reason},
            "report": reference}


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
    if claim["agent"] and claim["agent"] != agent:
        raise ValueError("checklist continuation changed the original native session")
    try:
        # Invalid evidence or findings cannot become immutable correction input.
        # Apply the report byte limit after removing wire-string escaping.
        if not isinstance(report, dict):
            raise ValueError("report must declare boolean complete")
        identities = verify_evidence(root, report)
        verify_findings(report, identities)
        report = decode_native_report(report)
        complete = verify_report(root, task, report)
        _dispositions(root, ledger, plan, task, report, agent, assignment)
    except ReportContractError as error:
        return _reject_report(root, provider, ledger, plan, claim, assignment,
                              report, agent, round_number, error)
    if ledger.db.execute("SELECT 1 FROM report_rejections WHERE claim_id=? AND round=?",
                         (claim["id"], round_number)).fetchone():
        raise ValueError("a rejected original round requires a later same-session correction")
    _preserve_rejected_findings(ledger, claim, report)
    reference = artifact(root, {"claim": claim["id"], "agent": agent, "round": round_number, "report": report})
    snapshot = _snapshot(root, provider, plan)
    governing = dict(snapshot)
    if task["writes"]:
        # Source edits belong to this writer; governing instructions and remote
        # evidence still have to match the assignment it actually received.
        for key in ("local_tree", "diff", "local_head", "history"):
            if key in assignment["snapshot"]:
                governing[key] = assignment["snapshot"][key]
    if _fingerprint(governing, task, ledger) != _fingerprint(assignment["snapshot"], task, ledger):
        raise ValueError("governing inputs changed during the native turn")
    # A writing report must account for every actual path and commit through the
    # controlled Git helper before obtaining coverage on its resulting tree.
    if task["writes"] and report.get("changes") and complete:
        _commit(root, ledger, claim["id"], task, report, assignment["snapshot"])
        snapshot = _snapshot(root, provider, plan)
    elif task["writes"] and complete and (snapshot.get("local_tree") != assignment["snapshot"].get("local_tree")
            or snapshot.get("local_head") != assignment["snapshot"].get("local_head")):
        raise ValueError("writer changed the workspace without a complete changed-file manifest")
    elif not task["writes"] and snapshot.get("local_tree") != assignment["snapshot"].get("local_tree"):
        raise ValueError("read-only task changed the workspace")
    settled_optional = not complete and not task["required"] and round_number == 3
    if settled_optional and task["writes"] and (snapshot.get("local_tree") != assignment["snapshot"].get("local_tree")
            or snapshot.get("local_head") != assignment["snapshot"].get("local_head")):
        raise ValueError("incomplete optional writer has unresolved source effects")
    coverage = _fingerprint(snapshot, task, ledger)
    with ledger.transaction():
        old = ledger.db.execute("SELECT agent,report FROM submissions WHERE claim_id=? AND round=?",
                                (claim["id"], round_number)).fetchone()
        encoded = canonical(report).decode()
        if old and (old["agent"] != agent or old["report"] != encoded):
            raise ValueError("native submission identity conflicts with original report")
        ledger.db.execute("INSERT OR IGNORE INTO submissions VALUES(?,?,?,?)",
                          (claim["id"], round_number, agent, encoded))
        ledger.db.execute("UPDATE claims SET state=?,agent=?,report=?,coverage=? WHERE id=?",
                          ("complete" if complete else "optional_failed" if settled_optional else "incomplete", agent, reference, coverage, claim["id"]))
        if complete:
            ledger.db.execute("INSERT OR IGNORE INTO accepted_reports VALUES(?,?,?,?,?,?)",
                              (reference, task["id"], task["kind"], coverage, agent, encoded))
    if (complete or settled_optional) and task["writes"]:
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
        assignment = read_artifact(root, claim["assignment"])
        if not continuation_current(root, provider, ledger, plan, claim, assignment):
            raise ValueError("governing inputs changed before native retry dispatch")
        ledger.reserve("native-retry:" + claim["id"], "original-session-retry", plan["limits"]["retries_per_task"])
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
    staged = set(git(root, "diff", "--cached", "--name-only", "--no-renames", "-z").decode().split("\0")) - {""}
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
    verification, fixes = {}, {}
    for row in ledger.db.execute("SELECT kind,report FROM accepted_reports ORDER BY rowid"):
        report = bounded_json(row["report"])
        for item in report.get("dispositions", []):
            key = item.get("finding_id")
            if row["kind"] == "verification":
                verification[key] = item.get("status")
            if row["kind"] == "fix":
                fixes[key] = item.get("status")
    for candidate in originals:
        key = candidate["id"]
        if verification.get(key) != "rejected" and (verification.get(key) != "confirmed"
                or fixes.get(key) not in {"fixed", "accepted", "deferred"}):
            unresolved.append(key)
    if any(stage["kind"] == "ci" for stage in plan["stages"]):
        observed = ledger.db.execute("SELECT * FROM ci_observations ORDER BY rowid DESC LIMIT 1").fetchone()
        if not observed or observed["head"] != snapshot["head"] or observed["status"] != "success" or snapshot["local_head"] != snapshot["head"]:
            missing.append("current_head_ci")
    return {"complete": not missing and not unresolved, "ready": False,
            "missing": missing, "unresolved": unresolved,
            "head": snapshot["head"], "local_head": snapshot["local_head"],
            "target": snapshot["target"], "snapshot": snapshot}
