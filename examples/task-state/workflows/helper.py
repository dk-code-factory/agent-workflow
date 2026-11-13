"""A deterministic, read-only business task with controller-owned bookkeeping."""

import hashlib
import json
import sys

from client import TaskState


def handle(request):
    phase = request["phase"]
    if phase == "bindings":
        return {"capabilities": ["bindings", "execute", "validate_checkpoint"],
                "read_only_phases": ["bindings", "validate_checkpoint"],
                "bindings": {"implementation": "task-state-capture-v1"}}
    value = request["input"]["value"]
    fingerprint = hashlib.sha256(value.encode()).hexdigest()
    state = TaskState(request)
    identity = {"task": "capture", "fingerprint": fingerprint,
                "evidence_fingerprint": fingerprint}
    output = {"value": value, "fingerprint": fingerprint}
    if phase == "validate_checkpoint":
        observed = state.call("get_claim", **identity)
        return {"valid": observed["reusable"] and
                request["checkpoint"]["result"].get("output") == output}
    if phase != "execute":
        raise ValueError("unsupported phase")
    operation = request["input"]["operation_id"]
    state.call("reserve", name="captures", operation_id=operation, limit=3, amount=1)
    claim = state.call("claim", operation_id=operation, **identity)
    if claim["dispatch"]:
        # The example's reference is an application identifier for this pure
        # value. Real helpers retain their own result artifact/receipt and must
        # validate its current contents before presenting its evidence identity.
        state.call("complete", operation_id=operation,
                   result_reference="sha256:" + fingerprint, **identity)
    elif not claim["reusable"]:
        raise RuntimeError("claim has no reusable completion; reconcile its original operation before retrying")
    return {"result": {"output": output, "evidence": {"fingerprint": fingerprint}}}


request = json.load(sys.stdin)
if request.get("protocol") != "action-v1":
    raise ValueError("unsupported protocol")
response = {"protocol": "action-v1", "phase": request["phase"], **handle(request)}
json.dump(response, sys.stdout)
sys.stdout.write("\n")
