"""Capture Git HEAD; checkpoint reuse requires the same repository HEAD."""
from pathlib import Path
import subprocess
from common.protocol import PHASES, READ_ONLY, serve


def handle(request):
    repository = Path(request["input"]["repository"]).resolve(strict=True)
    head = subprocess.check_output(
        ["git", "-c", "safe.directory=" + str(repository), "-C", str(repository), "rev-parse", "HEAD"],
        text=True,
    ).strip()
    result = {"output": {"head": head}, "evidence": {"head": head}}
    phase = request["phase"]
    if phase == "bindings":
        return {"capabilities": PHASES, "read_only_phases": READ_ONLY,
                "bindings": {"repository": str(repository)}}
    if phase == "execute":
        return {"result": result}
    if phase == "validate_checkpoint":
        return {"valid": request["checkpoint"]["result"] == result}
    # Read-only operations have no uncertain mutation to reconcile.
    return {"outcome": "no_effect", "proof": {"read_only": True}}


serve(handle)
