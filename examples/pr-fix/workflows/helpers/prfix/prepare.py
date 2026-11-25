"""Validate YAML selections and prepare immutable application evidence.

No workflow/configuration files, agent dispatch, publication or execution loops
belong here. The checked-in YAML owns all orchestration.
"""
import hashlib
import os
from pathlib import Path

from .core import (FORMAT, STATE, Ledger, artifact, auth, digest,
                   identity, owned_directory, text)

CHECKLIST = [
    {"id": "assignment", "text": "Perform the exact assigned task against current source and retained original instructions."},
    {"id": "evidence", "text": "Attach source files with current SHA-256 evidence and preserve every original finding/source identity."},
    {"id": "scope", "text": "Account for requirements, blockers and all proposed changes or publication operations without expanding user authority."},
]

# Legacy names remain selectors, not installed plugin invocations.
TASK_ALIASES = {
    "dk-workflows:dk-pr-feedback-read": "read-feedback",
    "dk-workflows:dk-pr-decisions": "reconcile-decisions",
    "dk-workflows:dk-pr-feedback-address": "address-feedback",
    "dk-workflows:dk-pr-split": "assess-scope",
    "dk-workflows:dk-pr-description": "describe-pr",
    "dk-workflows:dk-commit-split": "inspect-commits",
    "dk-workflows:dk-preferences": "sync-guidance",
    "local:review": "review",
}
ADAPTER_ALIASES = {**TASK_ALIASES, **{name: legacy for legacy, name in TASK_ALIASES.items()}}
STAGE_ALIASES = {"dk-workflows:dk-pr-publish": "publication", "dk-workflows:dk-ci": "ci",
                 "dk-workflows:dk-pr-reviewers-run": "reviews"}


def _selection(raw):
    call = {"task": raw} if isinstance(raw, str) else raw
    if not isinstance(call, dict) or len(set(call) & {"task", "skill", "stage"}) != 1:
        raise ValueError("select exactly one task or stage")
    if "skill" in call:
        name = call["skill"]
        if isinstance(name, str) and name in STAGE_ALIASES:
            allowed = {"reviewers"} if STAGE_ALIASES[name] == "reviews" else set()
            options = call.get("options", {})
            if (set(call) - {"skill", "options", "required", "instructions"}
                    or not isinstance(options, dict) or set(options) - allowed
                    or call.get("required", True) is not True or call.get("instructions", "") != ""):
                raise ValueError("unsupported managed stage controls")
            call = {"stage": STAGE_ALIASES[name], **options}
        else:
            call = {"task": name, **{key: value for key, value in call.items() if key != "skill"}}
    elif "task" in call and isinstance(call["task"], str) and call["task"] in STAGE_ALIASES:
        # Bare legacy strings were historically accepted alongside skill objects.
        return _selection({"skill": call["task"], **{key: value for key, value in call.items() if key != "task"}})
    if "stage" in call:
        stage = call["stage"]
        if not isinstance(stage, str) or stage not in {"publication", "reviews", "ci"}:
            raise ValueError("unsupported managed stage")
        if set(call) - ({"stage", "reviewers"} if stage == "reviews" else {"stage"}):
            raise ValueError("unsupported managed stage controls")
    elif set(call) - {"task", "options", "required", "instructions"}:
        raise ValueError("portable tasks support task/options/required/instructions only")
    return call


def _groups(raw):
    if not isinstance(raw, list):
        raise ValueError("selected plan must be an array")
    result = []
    for entry in raw:
        group = entry if isinstance(entry, list) else [entry]
        if not group or any(isinstance(item, list) for item in group):
            raise ValueError("invalid parallel group")
        result.append([_selection(item) for item in group])
    return result


def _definition(root, raw):
    path = Path(text(raw, "native definition"))
    if path.is_absolute() or ".." in path.parts:
        raise ValueError("native definitions must be workspace-relative")
    path = root / path
    for parent in [path, *path.parents]:
        if parent == root:
            break
        if parent.is_symlink():
            raise ValueError("native definition must not traverse symlinks")
    if path.is_symlink() or not path.is_file() or path.stat().st_size > 1 << 20:
        raise ValueError("native definition must be a bounded regular file")
    data = path.read_bytes()
    if path.suffix.lower() not in {".yaml", ".yml"}:
        raise ValueError("agent definition requires YAML; use agent convert for native sources")
    # The engine validates the complete YAML definition closure before starting
    # this helper. This helper records original source evidence, not a second
    # YAML interpreter or a source of execution authority.
    return data


def _helper_manifest(source):
    result = {}
    for path in sorted(source.rglob("*")):
        if "__pycache__" in path.parts or path.suffix == ".pyc":
            continue
        if path.is_symlink():
            raise ValueError("helper bundle must not contain symlinks")
        if path.is_file():
            if path.stat().st_size > 4 << 20:
                raise ValueError("helper file too large")
            result[path.relative_to(source).as_posix()] = hashlib.sha256(path.read_bytes()).hexdigest()
    if len(result) > 4096:
        raise ValueError("helper bundle has too many files")
    return result


def _validate_commit_identity(root):
    from .capture import run
    # Helpers have a private HOME and receive no host Git identity overrides.
    env = {key: value for key, value in os.environ.items()
           if not key.startswith(("GIT_CONFIG", "GIT_AUTHOR", "GIT_COMMITTER")) and key != "EMAIL"}
    env.update(GIT_CONFIG_NOSYSTEM="1", GIT_CONFIG_GLOBAL=os.devnull)
    try:
        for key in ("user.name", "user.email"):
            text(run(root, ["git", "config", "--local", "--get", key],
                     env=env, limit=4096).decode("utf-8"), key)
        for variable in ("GIT_AUTHOR_IDENT", "GIT_COMMITTER_IDENT"):
            run(root, ["git", "-c", "user.useConfigOnly=true", "var", variable],
                env=env, limit=4096)
    except (RuntimeError, ValueError, UnicodeError) as error:
        raise ValueError("writing tasks require valid repository-local Git user.name and user.email; "
                         "configure both before preparation") from error



def prepare(root, request, controls, execution_fingerprint, provider=None, definitions=None):
    root = Path(root).resolve(strict=True)
    allowed = {"pr", "logical_run", "instructions", "authorizations", "maintain_other"}
    if not isinstance(request, dict) or set(request) - allowed:
        raise ValueError("request supports PR identity, instructions, logical_run and authorizations only; move orchestration to YAML inputs")
    pr = identity(request["pr"])
    instructions = text(request.get("instructions"), "exact original instruction")
    execution_fingerprint = text(execution_fingerprint, "admitted execution fingerprint")
    if not isinstance(controls, dict) or set(controls) - {"tasks", "finalizers", "reviewers", "limits", "permissions", "accounts", "ci", "merge_method", "settings", "resources", "adapters", "turns"}:
        raise ValueError("unsupported YAML preparation input")
    turns = controls.get("turns", 8)
    if type(turns) is not int or not 5 <= turns <= 2147483647:
        raise ValueError("turn allowance must support at least five retained turns")
    source = Path(__file__).resolve().parent.parent
    definitions = definitions or {role: _definition(source.parent, "agents/" + role + ".yaml")
                                  for role in ("leaf", "reviewer", "verifier", "fixer", "audience")}
    if set(definitions) != {"leaf", "reviewer", "verifier", "fixer", "audience"}:
        raise ValueError("five explicit native definitions required")
    options = {key: value for key, value in controls.items()
               if key not in {"settings", "resources", "adapters", "turns"}}
    allowed_options = {"tasks", "finalizers", "reviewers", "limits", "permissions", "accounts", "ci", "merge_method",
                       "ci_timeout_seconds", "ci_classifications", "ci_exceptions", "before_cycle", "after_cycle"}
    if set(options) - allowed_options:
        raise ValueError("unsupported portable option: " + ", ".join(sorted(set(options) - allowed_options)))
    from .ci import configuration
    configuration(options)
    if options.get("merge_method", "merge") not in ("merge", "squash"):
        raise ValueError("merge_method supports merge/squash only")
    limits = {"review_cycles": 5, "retries_per_task": 1, "ci_repairs_per_cycle": 5, "ci_reruns_per_head": 3,
              **options.get("limits", {})}
    bounds = {"review_cycles": (1, 10), "retries_per_task": (0, 1), "ci_repairs_per_cycle": (0, 5), "ci_reruns_per_head": (0, 3)}
    if set(limits) != set(bounds) or any(type(limits[key]) is not int or not low <= limits[key] <= high for key, (low, high) in bounds.items()):
        raise ValueError("unsupported retained allowance")
    authorizations = {key: auth(value) for key, value in request.get("authorizations", {}).items()}
    if request.get("maintain_other"):
        authorizations["maintain_other"] = auth(request["maintain_other"])
    logical_run = request.get("logical_run", "pr_" + digest(pr)[:32])
    owned_directory(root, STATE, create=True)
    ledger = Ledger(root, logical_run, pr["url"])
    ledger.close()
    adapters = controls.get("adapters", {})
    tasks, stages = [], []
    serial = 0

    def task_call(call, fallback=None):
        nonlocal serial
        if "stage" in call:
            raise ValueError("reviewer selection must use read-only review adapters")
        name = text(call.get("task"), "selected task")
        adapter_name = name if name in adapters else ADAPTER_ALIASES.get(name, name)
        if adapter_name in adapters:
            adapter = adapters[adapter_name]
            if not isinstance(adapter, dict) or set(adapter) - {"kind", "writes", "assignment", "definition", "checklist", "resources", "history", "head_sensitive", "cacheable"}:
                raise ValueError("unsupported adapter fields")
        else:
            raise ValueError("unavailable selected adapter: " + name)
        kind = adapter.get("kind", fallback or "task")
        if kind not in {"review", "inspection", "decisions", "fix", "scope", "description", "cleanup", "synchronization", "task"}:
            raise ValueError("unsupported adapter kind")
        role = adapter.get("definition", "reviewer" if kind == "review" else "leaf")
        if role not in definitions:
            raise ValueError("adapter references undeclared native definition")
        serial += 1
        task = {"id": "task_" + str(serial), "skill": name, "kind": kind,
                "definition": role, "definition_digest": hashlib.sha256(definitions[role]).hexdigest(),
                "assignment": text(adapter.get("assignment"), "adapter assignment") + "\n" + call.get("instructions", ""),
                "required": call.get("required", True), "writes": bool(adapter.get("writes", False)),
                "checklist": adapter.get("checklist", CHECKLIST), "options": call.get("options", {}),
                "resources": adapter.get("resources", []), "history": bool(adapter.get("history", kind == "cleanup")),
                "head_sensitive": bool(adapter.get("head_sensitive", False)), "cacheable": bool(adapter.get("cacheable", True))}
        if type(task["required"]) is not bool or not isinstance(task["options"], dict):
            raise ValueError("invalid task controls")
        if not task["checklist"] or len(task["checklist"]) > 32 or len({item["id"] for item in task["checklist"]}) != len(task["checklist"]):
            raise ValueError("invalid task checklist")
        if task["writes"] and kind == "review":
            raise ValueError("reviewers cannot write")
        tasks.append(task)
        return task["id"]

    selections = [("tasks", options.get("tasks", [])),
                  ("finalizers", options.get("finalizers", []))]
    for phase, selected in selections:
        for group in _groups(selected):
            if len(group) > 1 and any("stage" in call for call in group):
                raise ValueError("managed publication/review/CI stages cannot be in a parallel group")
            stage = group[0].get("stage")
            if stage in {"publication", "ci"}:
                stages.append({"kind": stage, "phase": phase})
                continue
            if stage == "reviews":
                reviewers = group[0].get("reviewers", options.get("reviewers", []))
                if not reviewers:
                    raise ValueError("selected reviewer stage requires explicit reviewers")
                groups = []
                for reviewer_group in _groups(reviewers):
                    keys = [task_call(call, "review") for call in reviewer_group]
                    if any(next(t for t in tasks if t["id"] == key)["kind"] != "review" for key in keys):
                        raise ValueError("reviewer selection must use read-only review adapters")
                    groups.append({"id": "group_" + str(len(groups) + 1),
                                   "tasks": [{"id": key} for key in keys], "concurrency": len(keys)})
                stages.append({"kind": "reviews", "groups": groups, "review": True, "phase": phase})
                continue
            keys = [task_call(call) for call in group]
            if len(keys) > 1 and any(next(t for t in tasks if t["id"] == key)["writes"] for key in keys):
                raise ValueError("parallel writing tasks share a Git index and are unsupported")
            stages.append({"kind": "tasks", "phase": phase,
                           "groups": [{"id": "group_1", "tasks": [{"id": key} for key in keys], "concurrency": len(keys)}],
                           "review": any(next(t for t in tasks if t["id"] == key)["kind"] == "review" for key in keys)})

    if any(task["kind"] == "review" for task in tasks):
        for key, kind, role, assignment in [
            ("finding_verification", "verification", "verifier", "Independently verify every retained original finding. Preserve exact finding_id and report source; return dispositions confirmed/rejected with concrete source evidence and reason. Do not edit or publish."),
            ("finding_fix", "fix", "fixer", "Address independently confirmed findings. Return every finding_id disposition fixed/accepted/deferred/rejected, current evidence IDs and exact changed-file hashes/commit_message. Preserve original report/source references in deliverables. Accepted/deferred dispositions require retained exact user authority. Do not publish or commit.")]:
            tasks.append({"id": key, "skill": key, "kind": kind, "definition": role,
                "definition_digest": hashlib.sha256(definitions[role]).hexdigest(), "assignment": assignment,
                "required": True, "writes": kind == "fix", "checklist": CHECKLIST,
                "options": {}, "resources": [], "history": False, "head_sensitive": False, "cacheable": True})
    if any(stage["kind"] == "ci" for stage in stages):
        tasks.append({"id": "ci_repair", "skill": "ci_repair", "kind": "fix", "definition": "fixer",
            "definition_digest": hashlib.sha256(definitions["fixer"]).hexdigest(),
            "assignment": "Repair only the exact evidenced failed CI head/run/job/attempt in this assignment. Inspect original logs and relevant tests. Return source evidence, complete changed-file manifest and native commit message. Do not publish or commit; do not reinterpret unknown failures as flakes.",
            "required": False, "writes": True, "checklist": CHECKLIST, "options": {}, "resources": [],
            "history": False, "head_sensitive": True, "cacheable": False})
    plan = {"format": FORMAT, "logical_run": logical_run, "pr": pr, "instructions": instructions,
            "options": options, "settings": controls.get("settings", {}),
            "runtime": {"execution_fingerprint": execution_fingerprint}, "execution_fingerprint": execution_fingerprint, "turns": turns,
            "authorizations": authorizations, "limits": limits, "tasks": tasks, "stages": stages,
            "resources": {}, "definitions": {key: hashlib.sha256(value).hexdigest() for key, value in definitions.items()},
            "review_contract": digest([task for task in tasks if task["kind"] in {"review", "verification"}])}
    source = Path(__file__).resolve().parent.parent
    plan["helper_manifest"] = _helper_manifest(source)
    for task in tasks:
        task["implementation"] = digest(plan["helper_manifest"])
        task["runtime_fingerprint"] = execution_fingerprint
    if options.get("before_cycle") or options.get("after_cycle"):
        raise ValueError("portable cycle hooks require explicit task placement; legacy hook stores are not imported")
    from .capture import resource_digests
    plan["resources"] = resource_digests(root, controls.get("resources", []))
    if any(name not in plan["resources"] for task in tasks for name in task["resources"]):
        raise ValueError("adapter resource is undeclared")
    for number, stage in enumerate(stages, 1):
        stage["id"] = "stage_" + str(number)
    # Bound selected plan size separately from the runtime worker budget.
    # Conditional CI repairs and publication audiences do not reserve workers here.
    selection_weight_per_cycle = sum(sum(len(group["tasks"]) for group in stage["groups"]) + (2 if stage["review"] else 0)
                           if stage["kind"] in {"tasks", "reviews"} else
                           1 if stage["kind"] == "ci" else 0 for stage in stages)
    selection_weight = selection_weight_per_cycle * limits["review_cycles"] + (1 if "merge" in authorizations else 0)
    if len(tasks) > 40 or selection_weight > 64:
        raise ValueError("portable plan exceeds bounded selection size")
    if any(task["writes"] for task in tasks):
        _validate_commit_identity(root)
    from .capture import GitHub, capture
    provider = provider or GitHub(root)
    plan["snapshot"] = capture(root, provider, plan, clean=True)
    plan["fingerprint"] = digest(plan)
    plan_reference = artifact(root, plan)
    return {"logical_run": logical_run, "plan": plan_reference, "stages": stages,
            "limits": limits, "turns": turns, "merge_authorized": "merge" in authorizations}
