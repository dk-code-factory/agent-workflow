"""Publication adapters over accepted native reports and application coverage.

These helpers perform one boundary operation. YAML selects task order, obtains
an independent audience assessment, and schedules each mutation separately.
"""
from __future__ import annotations

import copy
import re

from .core import artifact, auth, bounded_json, canonical, digest, read_artifact, text
from .publication import Publication, PublicationError, repository_merge_requirements


_AGENT = re.compile(r"agent_[a-f0-9]{32}")


def _report_row(ledger, reference):
    row = ledger.db.execute("SELECT * FROM accepted_reports WHERE reference=?", (reference,)).fetchone()
    if not row:
        raise PublicationError("publication source is not an accepted original report")
    return dict(row)


def _source(root, provider, ledger, plan, reference, *, snapshot=None, audience_intent=None):
    """Verify immutable artifact bytes and their current application coverage."""
    from . import app
    from .prepare import CHECKLIST
    row = _report_row(ledger, text(reference, "accepted report reference"))
    wrapper = read_artifact(root, reference)
    report = bounded_json(row["report"])
    if wrapper.get("report") != report or wrapper.get("agent") != row["agent"]:
        raise PublicationError("accepted report artifact differs from its retained original")
    if not isinstance(row["agent"], str) or not _AGENT.fullmatch(row["agent"]):
        raise PublicationError("accepted report lacks an opaque native agent identity")
    if row["kind"] == "audience":
        if audience_intent is None:
            raise PublicationError("audience source requires its exact publication intent")
        if row["task"] != "audience:" + audience_intent["id"] or wrapper.get("intent") != audience_intent["id"]:
            raise PublicationError("audience source belongs to another publication")
        if row["fingerprint"] != audience_intent["fingerprint"]:
            raise PublicationError("audience source fingerprint differs")
        if not app.verify_report(root, {"checklist": CHECKLIST}, report):
            raise PublicationError("audience checklist is incomplete")
        verdict = report.get("audience", {})
        if verdict.get("fingerprint") != audience_intent["fingerprint"] or verdict.get("suitable") is not True:
            raise PublicationError("audience verdict does not accept the exact payload")
        return row
    task = app._task(plan, row["task"])
    if task["kind"] != row["kind"]:
        raise PublicationError("publication source task kind differs from admitted plan")
    claim = ledger.db.execute("SELECT * FROM claims WHERE id=?", (wrapper.get("claim"),)).fetchone()
    if not claim or claim["task"] != row["task"] or claim["agent"] != row["agent"]:
        raise PublicationError("publication source has no exact original logical claim")
    if claim["state"] != "complete" or claim["report"] != reference or claim["coverage"] != row["fingerprint"]:
        raise PublicationError("publication source is not the accepted final report for its claim")
    snapshot = snapshot if snapshot is not None else app._snapshot(root, provider, plan)
    if row["fingerprint"] != app._fingerprint(snapshot, task, ledger):
        raise PublicationError("publication source no longer covers the current inputs")
    if not app.verify_report(root, task, report):
        raise PublicationError("publication source checklist is incomplete")
    return row


def _publication(root, provider, ledger, plan, *, audience=None):
    def validate_source(reference):
        try:
            _source(root, provider, ledger, plan, reference, audience_intent=audience)
            return True
        except (ValueError, KeyError, TypeError, PublicationError):
            return False
    return Publication(ledger, provider, root, plan,
        readiness=lambda intent: readiness(root, provider, ledger, plan, {"intent": intent}),
        source_validator=validate_source)


def publication_list(root, provider, ledger, plan, inputs):
    """List current accepted proposals; never infer authority from selection."""
    from . import app
    snapshot = app._snapshot(root, provider, plan)
    selector = inputs.get("kind", "ordinary")
    if selector not in {"ordinary", "merge", "all"}:
        raise PublicationError("unsupported publication selector")
    selected, seen = [], {}
    rows = ledger.db.execute("SELECT * FROM accepted_reports ORDER BY rowid").fetchall()
    if selector != "merge" and snapshot["local_head"] != snapshot["head"]:
        # A native writer cannot predict commit-tree's eventual object ID. The
        # controlled commit receipt supplies that identity after acceptance;
        # publication still needs explicit authority and an independent exact
        # audience verdict. Publish it before proposals for the resulting PR.
        permissions = plan.get("options", {}).get("permissions", {})
        authority = plan.get("authorizations", {}).get("push", plan.get("authorizations", {}).get("iterate"))
        if permissions.get("push_commits") is not True or authority is None:
            return {"items": [], "unpublished": True,
                    "blockers": ["local controlled commits require explicit push authority and configured permission"]}
        auth(authority)
        committed = ledger.db.execute("SELECT * FROM local_commits WHERE head=? AND state='completed' ORDER BY rowid DESC LIMIT 1",
                                      (snapshot["local_head"],)).fetchone()
        if not committed:
            raise PublicationError("unpublished local head has no controlled commit receipt")
        for raw in reversed(rows):
            row = dict(raw)
            if row["kind"] == "audience":
                continue
            wrapper = read_artifact(root, row["reference"])
            if wrapper.get("claim") != committed["id"]:
                continue
            task = app._task(plan, row["task"])
            if not task.get("writes"):
                raise PublicationError("controlled commit source is not an admitted writing task")
            _source(root, provider, ledger, plan, row["reference"], snapshot=snapshot)
            proposal = {"id": "push_" + digest([plan["logical_run"], row["reference"], dict(committed)]),
                        "kind": "push", "head": snapshot["local_head"], "lease": snapshot["head"],
                        "target": snapshot["target"], "payload": {"new_head": snapshot["local_head"]},
                        "sources": [row["reference"]]}
            return {"items": [proposal], "unpublished": True}
        raise PublicationError("controlled commit lacks its accepted native writing report")
    for raw in rows:
        row = dict(raw)
        if row["kind"] == "audience":
            continue
        report = bounded_json(row["report"])
        proposals = report.get("operations", [])
        if not isinstance(proposals, list) or len(proposals) > 64:
            raise PublicationError("native publication proposal list exceeds its bound")
        if not proposals:
            continue
        # A stale report contributes no proposal. Refreshing the owning task is
        # the only way to authorize a payload on changed inputs.
        try:
            _source(root, provider, ledger, plan, row["reference"], snapshot=snapshot)
        except PublicationError:
            continue
        for number, original in enumerate(proposals):
            if not isinstance(original, dict):
                raise PublicationError("native publication proposal requires an object")
            proposal = copy.deepcopy(original)
            merge = proposal.get("kind") == "merge"
            if selector == "ordinary" and merge or selector == "merge" and not merge:
                continue
            sources = proposal.get("sources")
            if sources is None or sources == []:
                proposal["sources"] = [row["reference"]]
            elif not isinstance(sources, list) or not sources:
                raise PublicationError("native publication sources require accepted report references")
            for source in proposal["sources"]:
                _source(root, provider, ledger, plan, source, snapshot=snapshot)
            # IDs bind the original accepted report and proposal coordinates;
            # replaying another graph cannot turn an old intent into new work.
            proposal.setdefault("id", "publication_" + digest([plan["logical_run"], row["reference"], number, original]))
            key = text(proposal["id"], "publication proposal ID")
            fingerprint = digest(proposal)
            if key in seen:
                if seen[key] != fingerprint:
                    raise PublicationError("two accepted reports propose conflicting operation IDs")
                continue
            seen[key] = fingerprint
            selected.append(proposal)
    return {"items": selected}


def publication_prepare(root, provider, ledger, plan, inputs):
    proposal = inputs.get("proposal")
    if not isinstance(proposal, dict):
        raise PublicationError("publication preparation needs an exact accepted proposal")
    # Never accept arbitrary workflow inputs merely because they name an
    # accepted source. Reconstruct the source report's exact original proposal,
    # including the deterministic missing-sources/ID normalization above.
    listed = publication_list(root, provider, ledger, plan, {"kind": "all"})["items"]
    if proposal.get("kind") == "merge":
        final = merge_proposal(root, provider, ledger, plan, {})
        if final["invoke"] and proposal == final["proposal"]:
            prepared = _publication(root, provider, ledger, plan).prepare(proposal)
            return {key: prepared[key] for key in ("id", "fingerprint", "intent")}
    if not any(value == proposal for value in listed):
        # A completed or uncertain original may outlive source freshness after
        # its own publication. Its immutable ID/payload can be inspected and
        # reconciled, but this path cannot dispatch a replacement proposal.
        ident = proposal.get("id")
        if not isinstance(ident, str):
            raise PublicationError("proposal is outside accepted source reports")
        saved = _publication(root, provider, ledger, plan)._load(ident)
        original = saved["intent"]
        semantic = {key: proposal.get(key, original.get(key)) for key in ("kind", "head", "lease", "target", "payload", "sources", "source")}
        if semantic != {key: original.get(key) for key in semantic}:
            raise PublicationError("proposal differs from the retained original operation")
        if saved["state"] not in {"completed", "uncertain"}:
            raise PublicationError("publication source changed before dispatch")
        return {key: saved[key] for key in ("id", "fingerprint", "intent")}
    prepared = _publication(root, provider, ledger, plan).prepare(proposal)
    return {key: prepared[key] for key in ("id", "fingerprint", "intent")}


def publication_execute(root, provider, ledger, plan, inputs):
    from . import app
    from .prepare import CHECKLIST
    publisher = _publication(root, provider, ledger, plan)
    saved = publisher._load(text(inputs.get("id"), "publication operation"))
    # A replayed execute accepts only its previously retained native audience;
    # the original report is never replaced after a publication side effect.
    agent = text(inputs.get("agent"), "opaque audience agent")
    if not _AGENT.fullmatch(agent):
        raise PublicationError("audience requires an opaque native agent reference")
    report = inputs.get("report")
    if not app.verify_report(root, {"checklist": CHECKLIST}, report):
        raise PublicationError("native audience checklist remains incomplete")
    if report.get("findings") or report.get("blockers"):
        raise PublicationError("native audience reported unresolved payload findings")
    verdict = report.get("audience")
    if not isinstance(verdict, dict) or verdict.get("fingerprint") != saved["fingerprint"] or verdict.get("suitable") is not True:
        raise PublicationError("audience verdict does not accept this exact publication payload")
    identities = app.verify_evidence(root, report)
    evidence = verdict.get("evidence")
    if not isinstance(evidence, list) or not evidence or any(key not in identities for key in evidence):
        raise PublicationError("audience verdict lacks verified source evidence")
    intended = text(verdict.get("audience", "PR participants"), "intended audience")
    producers = {_report_row(ledger, source)["agent"] for source in saved["intent"]["sources"]}
    if agent in producers:
        raise PublicationError("audience assessment must use an independent native agent")
    reference = artifact(root, {"intent": saved["id"], "agent": agent, "report": report})
    existing = ledger.db.execute("SELECT * FROM accepted_reports WHERE task=? ORDER BY rowid DESC LIMIT 1",
                                 ("audience:" + saved["id"],)).fetchone()
    if existing and (existing["reference"] != reference or existing["agent"] != agent or existing["fingerprint"] != saved["fingerprint"]):
        raise PublicationError("retained native audience assessment changed")
    with ledger.transaction():
        ledger.db.execute("INSERT OR IGNORE INTO accepted_reports(reference,task,kind,fingerprint,agent,report) VALUES(?,?,?,?,?,?)",
                          (reference, "audience:" + saved["id"], "audience", saved["fingerprint"], agent, canonical(report).decode()))
    publisher = _publication(root, provider, ledger, plan, audience=saved)
    result = publisher.execute(saved["id"], {"fingerprint": saved["fingerprint"], "suitable": True,
        "audience": intended, "evidence": [reference]})
    return {"id": saved["id"], "receipt": result}


def _retained_audience(root, ledger, saved):
    row = ledger.db.execute("SELECT * FROM accepted_reports WHERE task=? AND kind='audience' ORDER BY rowid DESC LIMIT 1",
                            ("audience:" + saved["id"],)).fetchone()
    if not row:
        raise PublicationError("publication has no retained accepted native audience")
    wrapper = read_artifact(root, row["reference"])
    report = bounded_json(row["report"])
    if (wrapper != {"intent": saved["id"], "agent": row["agent"], "report": report}
            or row["fingerprint"] != saved["fingerprint"]
            or report.get("audience", {}).get("fingerprint") != saved["fingerprint"]
            or report.get("audience", {}).get("suitable") is not True):
        raise PublicationError("retained native audience identity changed")
    producers = {_report_row(ledger, source)["agent"] for source in saved["intent"]["sources"]}
    if row["agent"] in producers or not _AGENT.fullmatch(row["agent"]):
        raise PublicationError("retained audience is not independent of the original producers")
    return dict(row)


def publication_record(root, provider, ledger, plan, inputs):
    """Commit a reconciled external receipt in an explicit mutation stage."""
    publisher = _publication(root, provider, ledger, plan)
    saved = publisher._load(text(inputs.get("id"), "publication operation"))
    receipt = inputs.get("receipt")
    _retained_audience(root, ledger, saved)
    if saved["state"] not in {"uncertain", "completed"}:
        raise PublicationError("only a dispatched publication can record a reconciled receipt")
    if saved["state"] == "completed" and saved["receipt"] != receipt:
        raise PublicationError("completed publication receipt identity changed")
    if not publisher.validate(saved["id"], receipt):
        raise PublicationError("reconciled publication receipt does not match the exact current effect")
    with ledger.transaction():
        changed = ledger.db.execute("UPDATE publication_intents SET state='completed',receipt=? WHERE id=? AND fingerprint=? AND state IN ('uncertain','completed')",
            (canonical(receipt).decode(), saved["id"], saved["fingerprint"]))
        if changed.rowcount != 1:
            raise PublicationError("publication state changed before receipt recording")
    return {"id": saved["id"], "receipt": receipt}


def readiness(root, provider, ledger, plan, inputs=None):
    """Fresh application coverage plus live repository/CI evidence, read-only."""
    from . import app
    from .ci import classify, verify_exceptions
    result = {"complete": False, "ready": False, "clean_review": False, "verified": False,
              "head": "", "target": "", "ci": {"head": "", "success": False}, "evidence": [], "blockers": []}
    try:
        covered = app.coverage(root, provider, ledger, plan, {})
        result.update(complete=bool(covered["complete"]), missing=covered.get("missing", []), unresolved=covered.get("unresolved", []))
        snapshot = covered["snapshot"]
        result.update(head=snapshot["head"], target=snapshot["target"])
        if not covered["complete"]:
            result["blockers"].append("selected task coverage or original finding settlement incomplete")
        reviews = [task for task in plan["tasks"] if task["kind"] == "review"]
        verifications = [task for task in plan["tasks"] if task["kind"] == "verification"]
        if not reviews or not verifications:
            result["blockers"].append("current review and independent finding verification must be selected")
        current = {}
        for task in reviews + verifications:
            claim = app._current(ledger, snapshot, task)
            if not claim:
                result["blockers"].append("current " + task["kind"] + " report missing: " + task["id"])
                continue
            row = _source(root, provider, ledger, plan, claim["report"], snapshot=snapshot)
            current[task["id"]] = row
            result["evidence"].append(row["reference"])
        result["clean_review"] = bool(reviews) and all(t["id"] in current for t in reviews) and not covered["unresolved"]
        result["verified"] = bool(verifications) and all(t["id"] in current for t in verifications) and not covered["unresolved"]
        if snapshot.get("local_head") != snapshot["head"] or snapshot.get("dirty"):
            result["blockers"].append("merge requires the exact clean published head")
        observed = provider.observe_ci(plan["pr"], snapshot["head"])
        if observed.get("head") != snapshot["head"]:
            raise PublicationError("CI observation belongs to another head")
        options = plan.get("options", {})
        selected = options.get("ci", {})
        candidates = selected.get("classifications", options.get("ci_classifications", []))
        rules = selected.get("exceptions", options.get("ci_exceptions", []))
        if isinstance(rules, dict):
            rules = rules.get("entries", [])
        proofs = verify_exceptions(provider, plan["pr"], observed, candidates, rules) if candidates else []
        classified = classify(observed, proofs)
        result["ci"] = {**classified, "success": classified["status"] == "success"}
        result["evidence"].append("sha256:" + digest(observed))
        if not result["ci"]["success"]:
            result["blockers"].append("fresh exact-head CI is not successful")
        after = provider.capture(plan["pr"])
        meta = after["metadata"]
        if meta.get("head", {}).get("sha") != snapshot["head"] or meta.get("base", {}).get("sha") != snapshot["target"]:
            result["blockers"].append("PR head or target changed during readiness verification")
        if meta.get("state") != "open" or meta.get("merged") or meta.get("draft") or meta.get("mergeable_state") != "clean":
            result["blockers"].append("live PR merge requirements are not satisfied")
        requirements = repository_merge_requirements(provider, plan["pr"], meta, snapshot["head"])
        result["evidence"].append("sha256:" + digest(requirements))
        result["ready"] = bool(covered["complete"] and result["clean_review"] and result["verified"] and not result["blockers"])
    except Exception as error:
        result["blockers"].append(str(error))
    return result


def merge_proposal(root, provider, ledger, plan, inputs):
    """Construct only the final exact-head merge authorized by the user."""
    authority = plan.get("authorizations", {}).get("merge")
    if authority is None:
        return {"invoke": False, "proposal": {}}
    authority = auth(authority)
    current = readiness(root, provider, ledger, plan, {})
    if not current["ready"]:
        return {"invoke": False, "proposal": {}, "blockers": current["blockers"]}
    method = plan.get("options", {}).get("merge_method", "merge")
    if method not in {"merge", "squash"}:
        raise PublicationError("merge receipts support merge/squash only")
    sources = [reference for reference in current["evidence"] if not reference.startswith("sha256:")]
    if not sources:
        raise PublicationError("final merge requires current independent review references")
    proposal = {"id": "merge_" + digest([plan["logical_run"], current["head"], current["target"], authority, method]),
                "kind": "merge", "head": current["head"], "lease": current["head"], "target": current["target"],
                "payload": {"merge_method": method}, "sources": sources}
    return {"invoke": True, "proposal": proposal}


def completed_merge(root, provider, ledger, plan, completion):
    """Validate historical readiness through its exact authorized merge effect."""
    authority = plan.get("authorizations", {}).get("merge")
    if not completion.get("complete") or not completion.get("ready") or authority is None:
        return False
    publisher = _publication(root, provider, ledger, plan)
    for row in ledger.db.execute("SELECT id FROM publication_intents WHERE state IN ('uncertain','completed')"):
        saved = publisher._load(row["id"])
        intent = saved["intent"]
        if (intent["kind"] != "merge" or intent["head"] != completion.get("head")
                or intent["target"] != completion.get("target") or intent["authorization"] != authority):
            continue
        _retained_audience(root, ledger, saved)
        if publisher.reconcile(saved["id"]).get("outcome") == "completed":
            return True
    return False


def dispatch(operation, root, provider, ledger, plan, inputs):
    handlers = {"publication_list": publication_list, "publication_prepare": publication_prepare,
                "publication_execute": publication_execute, "publication_record": publication_record,
                "readiness": readiness, "merge_proposal": merge_proposal}
    if operation not in handlers:
        raise PublicationError("unknown publication helper operation")
    return handlers[operation](root, provider, ledger, plan, inputs)


def reconcile_operation(root, provider, ledger, plan, inputs):
    """Read-only application receipt observation for action-v1 reconciliation."""
    return _publication(root, provider, ledger, plan).reconcile(text(inputs.get("id"), "publication operation"))


def validate_operation(root, provider, ledger, plan, inputs, output):
    """Read-only checkpoint validation; missing effects never authorize replay."""
    if not isinstance(output, dict) or output.get("id") != inputs.get("id"):
        return False
    if "receipt" in inputs and inputs["receipt"] != output.get("receipt"):
        return False
    try:
        publisher = _publication(root, provider, ledger, plan)
        _retained_audience(root, ledger, publisher._load(inputs["id"]))
        return publisher.validate(inputs["id"], output.get("receipt"))
    except (PublicationError, ValueError, KeyError, TypeError):
        return False
