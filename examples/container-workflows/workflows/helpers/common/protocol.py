"""Small action-v1 framing utility; stdout is reserved for the response."""
import json
import sys

PHASES = ["bindings", "execute", "validate_checkpoint", "reconcile"]
READ_ONLY = ["bindings", "validate_checkpoint", "reconcile"]


def serve(handler):
    request = json.load(sys.stdin)
    if request["protocol"] != "action-v1" or request["phase"] not in PHASES:
        raise ValueError("unsupported protocol/phase")
    fields = handler(request)
    json.dump({"protocol": "action-v1", "phase": request["phase"], **fields}, sys.stdout)
    sys.stdout.write("\n")
