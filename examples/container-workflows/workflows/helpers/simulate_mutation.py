"""Atomic local receipt simulating an idempotent external service operation.

Use only a dedicated application-owned directory on the permitted writable
workspace. Real services must provide equally authoritative reconciliation.
"""
import json
import os
from pathlib import Path
from common.protocol import PHASES, READ_ONLY, serve


def handle(request):
    root = Path(request["input"]["receipts"]).resolve(strict=True)
    key = request["context"]["operation_key"]
    # Never let controller context become an arbitrary filesystem path.
    import hashlib
    name = hashlib.sha256(key.encode()).hexdigest() + ".json"
    receipt = root / name
    expected = {"operation_key": key, "value": request["input"]["value"]}
    result = {"output": {"value": expected["value"]}, "receipt": expected}
    phase = request["phase"]
    if phase == "bindings":
        return {"capabilities": PHASES, "read_only_phases": READ_ONLY,
                "bindings": {"receipts": str(root)}}
    if phase == "execute":
        # Link a fully fsynced private file into the application namespace. An
        # interrupted pre-link staging write leaves no application effect.
        temporary = root / (".staged-" + os.urandom(16).hex())
        try:
            with temporary.open("x") as stream:
                json.dump(expected, stream, sort_keys=True)
                stream.flush()
                os.fsync(stream.fileno())
            try:
                os.link(temporary, receipt)
            except FileExistsError:
                if json.loads(receipt.read_text()) != expected:
                    raise ValueError("operation identity conflicts with retained receipt")
            fd = os.open(root, os.O_RDONLY)
            try:
                os.fsync(fd)
            finally:
                os.close(fd)
        finally:
            temporary.unlink(missing_ok=True)
        return {"result": result}
    observed = json.loads(receipt.read_text()) if receipt.exists() else None
    if phase == "validate_checkpoint":
        return {"valid": observed == expected and request["checkpoint"]["result"] == result}
    if observed == expected:
        return {"outcome": "completed", "proof": observed, "result": result}
    if observed is None:
        return {"outcome": "no_effect", "proof": {"absent_operation": key}}
    return {"outcome": "unknown"}


serve(handle)
