"""Retained, replay-safe preparation operations. No agent or remote mutations."""
from pathlib import Path

from .core import (STATE, artifact, atomic_json, bounded_json, digest,
                   owned_directory, read_artifact, text)


def validate_request(request):
    if not isinstance(request, dict) or set(request) - {"pr", "logical_run", "instructions", "authorizations", "maintain_other"}:
        raise ValueError("request supports PR identity, instructions, logical_run and authorizations only; move orchestration to YAML inputs")
    from .core import identity, auth
    identity(request.get("pr"))
    text(request.get("instructions"), "exact original instruction")
    for value in request.get("authorizations", {}).values():
        auth(value)
    if "maintain_other" in request:
        auth(request["maintain_other"])
    return request


def handle(operation, request, root, provider):
    phase = request["phase"]
    inputs = request.get("input")
    context = request.get("context", {})
    key = text(context.get("operation_key"), "engine operation identity")
    execution = text(context.get("execution_fingerprint"), "admitted execution fingerprint")
    if not isinstance(inputs, dict):
        raise ValueError("preparation input must be an object")
    ident = digest([operation, key])
    fingerprint = digest([operation, inputs, execution])
    response = {"protocol": "action-v1", "phase": phase}
    # Read-only phases must neither initialize state nor read a mutable request
    # again after its first snapshot. The engine pins inputs and helper bundles.
    base = Path(root) / STATE
    saved = None
    if base.exists():
        owned_directory(root, STATE)
        directory = base / "preparations"
        if directory.is_symlink():
            raise ValueError("preparation directory must not be a symlink")
        receipt = directory / (ident + ".json")
        if receipt.is_symlink():
            raise ValueError("preparation receipt must not be a symlink")
        if receipt.exists():
            saved = bounded_json(receipt.read_bytes())
            if saved.get("input") != fingerprint:
                raise ValueError("preparation identity conflict")
            value = read_artifact(root, saved["artifact"])
            if value.get("input") != fingerprint:
                raise ValueError("preparation evidence differs")
            saved = value["result"]
    if phase == "bindings":
        return {**response, "capabilities": ["bindings", "execute", "validate_checkpoint", "reconcile"],
                "read_only_phases": ["bindings", "validate_checkpoint", "reconcile"],
                "bindings": {"input": fingerprint, "execution_fingerprint": execution}}
    if phase == "reconcile":
        if saved is not None:
            return {**response, "outcome": "completed", "proof": {"preparation": ident}, "result": saved}
        # Only immutable local artifacts/ledger initialization can precede the
        # atomic receipt. Repeating reads cannot dispatch or publish effects.
        return {**response, "outcome": "no_effect", "proof": {"no_dispatch_or_publication": ident}}
    if phase == "validate_checkpoint":
        return {**response, "valid": saved is not None and saved == request.get("checkpoint", {}).get("result")}
    if saved is not None:
        return {**response, "result": saved}
    owned_directory(root, STATE, create=True)
    if operation == "snapshot_request":
        if set(inputs) != {"request"}:
            raise ValueError("snapshot requires request path")
        relative = Path(text(inputs["request"], "request path"))
        if relative.is_absolute() or ".." in relative.parts:
            raise ValueError("request path must be workspace-relative")
        path = root / relative
        for parent in [path, *path.parents]:
            if parent == root:
                break
            if parent.is_symlink():
                raise ValueError("request must not traverse symlinks")
        # Retain the first complete read before subsequent result bookkeeping.
        # A lost receipt cannot silently replace the exact original request.
        staged = base / "preparations" / (ident + ".request.json")
        if staged.is_symlink():
            raise ValueError("request snapshot receipt must not be a symlink")
        if staged.exists():
            retained = bounded_json(staged.read_bytes())
            if retained.get("input") != fingerprint:
                raise ValueError("request snapshot identity conflict")
            reference = retained["snapshot"]
            validate_request(read_artifact(root, reference))
        else:
            if not path.is_file() or path.stat().st_size > 1 << 20:
                raise ValueError("request must be a bounded regular JSON file")
            snapshot = validate_request(bounded_json(path.read_bytes()))
            reference = artifact(root, snapshot)
            atomic_json(staged, {"input": fingerprint, "snapshot": reference})
        output = {"snapshot": reference}
    elif operation == "prepare":
        if set(inputs) != {"snapshot", "controls"}:
            raise ValueError("prepare requires immutable snapshot and YAML controls")
        from .prepare import prepare
        snapshot = validate_request(read_artifact(root, inputs["snapshot"]))
        output = prepare(root, snapshot, inputs["controls"], execution, provider)
    else:
        raise ValueError("unknown preparation operation")
    result = {"output": output, "evidence": {"preparation": ident, "input": fingerprint}}
    reference = artifact(root, {"input": fingerprint, "result": result})
    atomic_json(base / "preparations" / (ident + ".json"), {"input": fingerprint, "artifact": reference})
    return {**response, "result": result}
