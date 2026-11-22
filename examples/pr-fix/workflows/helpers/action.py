"""Container action-v1 entrypoint. Exactly one bounded operation per process."""

import json
import sys
from pathlib import Path

from prfix import app
from prfix.core import (MAX_JSON, FORMAT, Ledger, artifact, bounded_json,
                        canonical, digest, read_artifact, text)
from prfix.capture import GitHub


def verify_closure(root, plan, execution_fingerprint):
    # Recovery runs retained helpers and agent definitions from engine snapshots.
    # Workspace copies may have evolved since admission; do not reload them.
    if plan.get("execution_fingerprint") != execution_fingerprint:
        raise ValueError("prepared evidence belongs to a different admitted execution/configuration")
    from prfix.prepare import _helper_manifest
    if _helper_manifest(Path(__file__).resolve().parent) != plan["helper_manifest"]:
        raise ValueError("prepared helper closure differs before dispatch")


def dispatch(operation, root, provider, ledger, plan, inputs):
    audience = inputs.get("publication", "")
    if audience and operation in {"claim", "accept", "turn_result", "check_prepare", "task_status"}:
        from prfix.publishing import audience_scope
        plan, inputs = audience_scope(root, provider, ledger, plan, operation, inputs)
    if operation == "native_accept":
        selected, turn_inputs = native_acceptance(inputs)
        result = dispatch(selected["accept_operation"], root, provider, ledger, plan, turn_inputs)
        return {**result, "agent": selected["agent"], "native_report": selected.get("report")}
    if operation == "claim":
        return app.prepare_claim(root, provider, ledger, plan, inputs)
    if operation == "accept":
        return app.accept(root, provider, ledger, plan, inputs)
    if operation == "turn_result":
        return app.turn_result(root, provider, ledger, plan, inputs)
    if operation == "extend":
        return ledger.extend(inputs["authorization"]["request_id"], inputs["name"], inputs["amount"], inputs["authorization"])
    if operation == "task_status":
        result = app.task_status(root, provider, ledger, plan, inputs)
        if audience:
            retained = read_artifact(root, result["report"])
            result.update(agent=retained["agent"], structured=retained["report"])
        return result
    if operation == "check_prepare":
        claim = app._claim(ledger, inputs["claim"])
        if claim["agent"] != inputs["agent"] or claim["state"] != "incomplete":
            raise ValueError("checklist continuation must retain the original incomplete session")
        round_number = inputs["round"]
        if type(round_number) is not int or not 1 <= round_number <= 3:
            raise ValueError("checklist continuation allowance exceeded")
        previous = ledger.db.execute("SELECT round FROM submissions WHERE claim_id=? UNION ALL "
                                     "SELECT round FROM report_rejections WHERE claim_id=? ORDER BY round DESC LIMIT 1",
                                     (claim["id"], claim["id"])).fetchone()
        if not previous or previous[0] != round_number - 1:
            raise ValueError("checklist continuation cannot skip an original round")
        assignment = read_artifact(root, claim["assignment"])
        if not app.continuation_current(root, provider, ledger, plan, claim, assignment):
            raise ValueError("governing inputs changed before checklist dispatch")
        # Both bodies already belong to this original session. References keep
        # their complete immutable bytes available without combining two valid
        # large documents into an invalid helper/native input envelope.
        prior = read_artifact(root, claim["report"])
        return {"prompt": "Correct only the rejected report contract or complete its remaining checklist in this original native session. Return exactly the assigned checklist IDs, with no additional IDs; put test results and supplemental checks in deliverables. Disposition sources name only evidence IDs from this report; preserve original report/artifact references in deliverables. Preserve existing source edits, original analysis and every finding/source ID. The exact assignment and prior report remain available at the workspace artifact references in task_input; reread them if needed. Return the full report contract.",
                "task_input": {"claim": claim["id"], "assignment_reference": claim["assignment"],
                               "prior_report_reference": claim["report"], "round": round_number,
                               "contract_error": prior.get("contract_error", "")}}
    if operation == "coverage":
        return app.coverage(root, provider, ledger, plan, inputs)
    if operation == "completion":
        from prfix.publishing import readiness
        result = readiness(root, provider, ledger, plan)
        if not result["complete"]:
            raise ValueError("selected task/finalizer plan remains incomplete")
        return result
    if operation == "ci":
        from prfix.ci import observe
        observed = observe(provider, plan, ledger, inputs["_operation"])
        with ledger.transaction():
            ledger.db.execute("INSERT INTO ci_observations VALUES(?,?,?,?)",
                              (inputs["_operation"], observed["head"], observed["status"], canonical(observed).decode()))
        return observed
    if operation.startswith("publication_") or operation == "merge_proposal":
        from prfix.publishing import dispatch as publish
        return publish(operation, root, provider, ledger, plan, inputs)
    raise ValueError("unknown portable application operation")


def native_result(inputs):
    mode = inputs.get("mode", "initial")
    modes = {"initial", "retry", "checklist"}
    results = inputs.get("results", {})
    if mode not in modes or set(results) != modes:
        raise ValueError("invalid declared native mode")
    if any(value.get("status") != ("succeeded" if key == mode else "skipped")
           for key, value in results.items()):
        raise ValueError("native turn branches did not select exactly one result")
    return {**results[mode]["output"],
            "accept_operation": "accept" if mode == "checklist" else "turn_result"}


def native_acceptance(inputs):
    selected = native_result(inputs)
    turn_inputs = {key: inputs[key] for key in ("plan", "claim", "round", "publication") if key in inputs}
    turn_inputs.update({key: value for key, value in selected.items() if key != "accept_operation"})
    turn_inputs["retried"] = inputs.get("mode", "initial") == "retry"
    return selected, turn_inputs


def _retained_result(root, reference):
    """Restore the reference omitted from its own immutable artifact's hash."""
    value = read_artifact(root, reference)
    value["result"]["evidence"]["artifact"] = reference
    return value


def _execute(operation, root, provider, ledger, plan, inputs, ident, fingerprint, saved):
    if saved and saved["state"] == "complete":
        result = _retained_result(root, saved["result"])["result"]
        return {"result": result}
    if saved:
        # Recheck service proof at execution time. Reconciliation hooks
        # remain read-only, and unknown effects never authorize replay.
        observed = None
        if operation in {"publication_execute", "publication_record", "publication_recover"}:
            from prfix.publishing import reconcile_operation
            observed = reconcile_operation(root, provider, ledger, plan, inputs)
        if not observed or observed.get("outcome") != "no_effect":
            raise ValueError("interrupted helper effect remains unresolved; reconcile original operation")
    else:
        with ledger.transaction():
            ledger.db.execute("INSERT INTO operations(id,kind,input,state) VALUES(?,?,?,?)",
                              (ident, operation, fingerprint, "uncertain"))
    output = dispatch(operation, root, provider, ledger, plan, {**inputs, "_operation": ident})
    result = {"output": output, "evidence": {"operation": ident, "input": fingerprint}}
    reference = artifact(root, {"operation": ident, "input": fingerprint, "result": result})
    result["evidence"]["artifact"] = reference
    # The artifact stores the evidence before its own reference to avoid
    # a self-referential hash. Validation reconstructs that one field.
    with ledger.transaction():
        ledger.db.execute("UPDATE operations SET state='complete',result=? WHERE id=?", (reference, ident))
    return {"result": result}


def _reconcile(operation, root, provider, ledger, plan, inputs, ident, fingerprint, saved):
    if saved and saved["state"] == "complete":
        result = _retained_result(root, saved["result"])["result"]
        return {"outcome": "completed", "proof": {"artifact": saved["result"], "input": fingerprint}, "result": result}
    if operation in {"publication_execute", "publication_record", "publication_recover"} and saved:
        from prfix.publishing import reconcile_operation
        observed = reconcile_operation(root, provider, ledger, plan, inputs)
        if observed.get("outcome") == "completed":
            result = {"output": {"id": inputs["id"], "receipt": observed["result"]},
                      "evidence": {"operation": ident, "input": fingerprint, "publication_proof": observed["proof"]}}
            return {"outcome": "completed", "proof": observed["proof"], "result": result}
        return observed
    if saved is None:
        return {"outcome": "no_effect", "proof": {"absent_application_operation": ident}}
    return {"outcome": "unknown", "proof": {"unresolved_application_operation": ident}}


def _validate_checkpoint(operation, root, provider, ledger, plan, inputs, ident, fingerprint, checkpoint):
    evidence = checkpoint.get("evidence", {})
    valid = evidence.get("operation") == ident and evidence.get("input") == fingerprint
    if valid and evidence.get("artifact"):
        value = _retained_result(root, evidence["artifact"])
        expected = value["result"]
        valid = expected == checkpoint and value["operation"] == ident and value["input"] == fingerprint
    elif valid and evidence.get("publication_proof") and operation in {"publication_execute", "publication_record", "publication_recover"}:
        from prfix.publishing import validate_operation
        valid = validate_operation(root, provider, ledger, plan, inputs, checkpoint.get("output", {}))
    else:
        valid = False
    if valid and operation == "native_accept":
        selected, inputs = native_acceptance(inputs)
        operation = selected["accept_operation"]
    if valid and operation in {"publication_execute", "publication_record", "publication_recover"}:
        from prfix.publishing import validate_operation
        valid = validate_operation(root, provider, ledger, plan, inputs, checkpoint["output"])
    if valid and operation == "claim" and checkpoint["output"].get("invoke"):
        retained = app._claim(ledger, checkpoint["output"]["claim"])
        assignment = read_artifact(root, retained["assignment"])
        if retained["state"] not in {"complete", "optional_failed"}:
            current = app._snapshot(root, provider, plan)
            if assignment["task"]["writes"]:
                from prfix.core import verify_writer_lease
                verify_writer_lease(root, plan["logical_run"], retained["id"])
                valid = current["local_head"] == assignment["snapshot"]["local_head"] and current["head"] == assignment["snapshot"]["head"]
                # The original writer may have unfinished edits. Its final
                # report must still account for all bytes before acceptance.
                current = {**current, **{key: assignment["snapshot"][key] for key in ("local_tree", "diff")}}
            current_fingerprint = app._fingerprint(current, assignment["task"], ledger)
            payload = assignment.get("payload", {})
            if payload:
                current_fingerprint = digest([current_fingerprint, payload])
            current_fingerprint = digest([current_fingerprint, assignment["invocation"]])
            valid = valid and current_fingerprint == retained["fingerprint"]
    if valid and (operation == "check_prepare" or operation == "turn_result" and
                  (checkpoint["output"].get("retry") or checkpoint["output"].get("checklist")) or
                  operation == "accept" and not checkpoint["output"].get("complete")):
        retained = app._claim(ledger, inputs["claim"])
        if retained["state"] not in {"complete", "optional_failed"}:
            assignment = read_artifact(root, retained["assignment"])
            valid = app.continuation_current(root, provider, ledger, plan, retained, assignment)
    if valid and inputs.get("publication") and operation in {"claim", "check_prepare", "turn_result"}:
        retained = app._claim(ledger, checkpoint["output"]["claim"] if operation == "claim" else inputs["claim"])
        if retained["state"] not in {"complete", "optional_failed"}:
            from prfix.publishing import audience_scope
            from prfix.publication import PublicationError
            try:
                audience_scope(root, provider, ledger, plan, operation, inputs)
            except (ValueError, KeyError, TypeError, PublicationError):
                valid = False
    if valid and operation == "completion":
        from prfix.publishing import readiness, completed_merge
        current = readiness(root, provider, ledger, plan)
        old = checkpoint["output"]
        valid = current["complete"] and (not old["ready"] or current["ready"] and current["head"] == old["head"] and current["target"] == old["target"])
        if not valid:
            valid = completed_merge(root, provider, ledger, plan, old)
    return {"valid": bool(valid)}


def handle(operation, request, root=None, provider=None):
    root = Path(root or Path.cwd()).resolve(strict=True)
    if request.get("protocol") != "action-v1" or request.get("phase") not in {"bindings", "execute", "validate_checkpoint", "reconcile"}:
        raise ValueError("unsupported helper protocol/phase")
    if operation == "invoke":
        envelope = request.get("input")
        if (not isinstance(envelope, dict) or set(envelope) != {"operation", "payload"}
                or not isinstance(envelope["payload"], dict)):
            raise ValueError("helper envelope requires an operation and one object payload")
        operation = text(envelope["operation"], "helper operation")
        request = {**request, "input": envelope["payload"]}
    phase = request["phase"]
    if operation in {"snapshot_request", "prepare"}:
        from prfix.preparation import handle as preparation
        return preparation(operation, request, root, provider)
    inputs = request.get("input")
    if not isinstance(inputs, dict) or not isinstance(inputs.get("plan"), str):
        raise ValueError("helper input requires immutable plan reference")
    plan = app.load_plan(root, inputs["plan"])
    verify_closure(root, plan, request.get("context", {}).get("execution_fingerprint"))
    provider = provider or GitHub(root)
    provider.read_only = phase != "execute"
    key = request.get("context", {}).get("operation_key")
    if not isinstance(key, str) or not key:
        raise ValueError("engine-owned logical operation identity required")
    ident = digest([plan["logical_run"], operation, key])
    fingerprint = digest([plan["fingerprint"], inputs])
    response = {"protocol": "action-v1", "phase": phase}
    if phase == "bindings":
        return {**response, "capabilities": ["bindings", "execute", "validate_checkpoint", "reconcile"],
                "read_only_phases": ["bindings", "validate_checkpoint", "reconcile"],
                "bindings": {"format": FORMAT, "plan": plan["fingerprint"], "logical_run": plan["logical_run"],
                             "execution_fingerprint": plan["execution_fingerprint"], "definitions": plan["definitions"]}}
    with Ledger(root, plan["logical_run"], plan["pr"]["url"], read_only=phase != "execute") as ledger:
        saved = ledger.db.execute("SELECT * FROM operations WHERE id=?", (ident,)).fetchone()
        if saved and (saved["kind"] != operation or saved["input"] != fingerprint):
            raise ValueError("helper operation identity conflict")
        if phase == "execute":
            outcome = _execute(operation, root, provider, ledger, plan, inputs, ident, fingerprint, saved)
        elif phase == "reconcile":
            outcome = _reconcile(operation, root, provider, ledger, plan, inputs, ident, fingerprint, saved)
        else:
            outcome = _validate_checkpoint(operation, root, provider, ledger, plan, inputs, ident,
                                           fingerprint, request.get("checkpoint", {}).get("result", {}))
        return {**response, **outcome}


def main():
    if len(sys.argv) not in {2, 3}:
        raise ValueError("usage: action.py OPERATION [CONTINUATION.json]")
    operation = sys.argv[1]
    if operation == "continue":
        source = Path(sys.argv[2]).read_bytes() if len(sys.argv) == 3 else sys.stdin.buffer.read(MAX_JSON + 1)
        request = bounded_json(source)
        if not isinstance(request, dict) or set(request) != {"plan", "name", "amount", "authorization"}:
            raise ValueError("continuation requires plan, name, amount and exact authorization")
        root = Path.cwd()
        plan = app.load_plan(root, request["plan"])
        with Ledger(root, plan["logical_run"], plan["pr"]["url"]) as ledger:
            result = ledger.extend(request["authorization"]["request_id"], request["name"], request["amount"], request["authorization"])
    else:
        result = handle(operation, bounded_json(sys.stdin.buffer.read(MAX_JSON + 1)))
    encoded = canonical(result)
    if len(encoded) > MAX_JSON:
        raise ValueError("helper response exceeds 1 MiB")
    sys.stdout.buffer.write(encoded + b"\n")


if __name__ == "__main__":
    main()
