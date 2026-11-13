"""Container action-v1 entrypoint. Exactly one bounded operation per process."""

import json
import sys
from pathlib import Path

from prfix import app
from prfix.core import (MAX_JSON, FORMAT, GENERATED, Ledger, artifact, bounded_json,
                        canonical, digest, read_artifact)
from prfix.capture import GitHub


def verify_closure(root, plan):
    import hashlib
    directory = root / GENERATED / plan["logical_run"] / plan["fingerprint"]
    for role, expected in plan["definitions"].items():
        path = directory / "agents" / (role + ".toml")
        if path.is_symlink() or not path.is_file() or hashlib.sha256(path.read_bytes()).hexdigest() != expected:
            raise ValueError("prepared native definition differs before dispatch")
    # The engine pins the mounted helper bundle itself. Rechecking its admitted
    # source declaration prevents a mixed native/helper closure at admission.
    for name, expected in plan["helper_manifest"].items():
        path = directory / "helpers" / name
        if path.is_symlink() or not path.is_file() or hashlib.sha256(path.read_bytes()).hexdigest() != expected:
            raise ValueError("prepared helper closure differs before dispatch")


def dispatch(operation, root, provider, ledger, plan, inputs):
    audience = inputs.get("publication", "")
    if audience and operation in {"claim", "accept", "turn_result", "check_prepare", "task_status"}:
        from prfix.publishing import audience_scope
        plan, inputs = audience_scope(root, provider, ledger, plan, operation, inputs)
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
        previous = ledger.db.execute("SELECT round FROM submissions WHERE claim_id=? ORDER BY round DESC LIMIT 1", (claim["id"],)).fetchone()
        if not previous or previous[0] != round_number - 1:
            raise ValueError("checklist continuation cannot skip an original round")
        assignment = read_artifact(root, claim["assignment"])
        if not app.continuation_current(root, provider, ledger, plan, claim, assignment):
            raise ValueError("governing inputs changed before checklist dispatch")
        # Both bodies already belong to this original session. References keep
        # their complete immutable bytes available without combining two valid
        # large documents into an invalid helper/native input envelope.
        read_artifact(root, claim["report"])
        return {"prompt": "Complete only the remaining checklist in this original native session. Preserve original analysis and every finding/source ID. The exact assignment and prior report remain available at the workspace artifact references in task_input; reread them if needed. Return the full report contract.",
                "task_input": {"claim": claim["id"], "assignment_reference": claim["assignment"],
                               "prior_report_reference": claim["report"], "round": round_number}}
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


def handle(operation, request, root=None, provider=None):
    root = Path(root or Path.cwd()).resolve(strict=True)
    if request.get("protocol") != "action-v1" or request.get("phase") not in {"bindings", "execute", "validate_checkpoint", "reconcile"}:
        raise ValueError("unsupported helper protocol/phase")
    phase = request["phase"]
    inputs = request.get("input")
    if not isinstance(inputs, dict) or not isinstance(inputs.get("plan"), str):
        raise ValueError("helper input requires immutable plan reference")
    plan = app.load_plan(root, inputs["plan"])
    verify_closure(root, plan)
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
                             "image": plan["image"], "definitions": plan["definitions"]}}
    with Ledger(root, plan["logical_run"], plan["pr"]["url"], read_only=phase != "execute") as ledger:
        saved = ledger.db.execute("SELECT * FROM operations WHERE id=?", (ident,)).fetchone()
        if saved and (saved["kind"] != operation or saved["input"] != fingerprint):
            raise ValueError("helper operation identity conflict")
        if phase == "execute":
            if saved and saved["state"] == "complete":
                result = read_artifact(root, saved["result"])["result"]
                result["evidence"]["artifact"] = saved["result"]
                return {**response, "result": result}
            if saved:
                # Recheck service proof at execution time. Reconciliation hooks
                # remain read-only, and unknown effects never authorize replay.
                observed = None
                if operation in {"publication_execute", "publication_record"}:
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
            return {**response, "result": result}
        if phase == "reconcile":
            if saved and saved["state"] == "complete":
                value = read_artifact(root, saved["result"])
                result = value["result"]
                result["evidence"]["artifact"] = saved["result"]
                return {**response, "outcome": "completed", "proof": {"artifact": saved["result"], "input": fingerprint}, "result": result}
            if operation in {"publication_execute", "publication_record"} and saved:
                from prfix.publishing import reconcile_operation
                observed = reconcile_operation(root, provider, ledger, plan, inputs)
                if observed.get("outcome") == "completed":
                    result = {"output": {"id": inputs["id"], "receipt": observed["result"]},
                              "evidence": {"operation": ident, "input": fingerprint, "publication_proof": observed["proof"]}}
                    return {**response, "outcome": "completed", "proof": observed["proof"], "result": result}
                return {**response, **observed}
            if saved is None:
                return {**response, "outcome": "no_effect", "proof": {"absent_application_operation": ident}}
            return {**response, "outcome": "unknown", "proof": {"unresolved_application_operation": ident}}
        checkpoint = request.get("checkpoint", {}).get("result", {})
        evidence = checkpoint.get("evidence", {})
        valid = evidence.get("operation") == ident and evidence.get("input") == fingerprint
        if valid and evidence.get("artifact"):
            value = read_artifact(root, evidence["artifact"])
            expected = value["result"]
            expected["evidence"]["artifact"] = evidence["artifact"]
            valid = expected == checkpoint and value["operation"] == ident and value["input"] == fingerprint
        elif valid and evidence.get("publication_proof") and operation in {"publication_execute", "publication_record"}:
            from prfix.publishing import validate_operation
            valid = validate_operation(root, provider, ledger, plan, inputs, checkpoint.get("output", {}))
        else:
            valid = False
        if valid and operation in {"publication_execute", "publication_record"}:
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
        if valid and (operation == "check_prepare" or operation == "turn_result" and checkpoint["output"].get("retry")):
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
        return {**response, "valid": bool(valid)}


def main():
    if len(sys.argv) not in {2, 3}:
        raise ValueError("usage: action.py OPERATION [PREPARATION_REQUEST.json]")
    operation = sys.argv[1]
    if operation in {"prepare", "continue"}:
        from prfix.prepare import prepare
        source = Path(sys.argv[2]).read_bytes() if len(sys.argv) == 3 else sys.stdin.buffer.read(MAX_JSON + 1)
        request = bounded_json(source)
        if operation == "prepare":
            result = prepare(Path.cwd(), request)
        else:
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
