"""Compile a complete portable YAML closure before the stock engine admits it.

Compilation selects definitions and expands barriers. No admitted graph is ever
rewritten, and this module never dispatches agents or executes a workflow loop.
"""

from contextlib import contextmanager
import copy
import hashlib
import json
import os
from pathlib import Path
import re
import shutil
import tomllib

from .core import (FORMAT, GENERATED, STATE, Ledger, artifact, atomic_json, auth,
                   canonical, digest, identity, owned_directory, text)

DEFAULT_TASKS = ["dk-workflows:dk-pr-feedback-read", "dk-workflows:dk-pr-decisions",
                 "dk-workflows:dk-pr-feedback-address", "dk-workflows:dk-pr-publish",
                 "dk-workflows:dk-pr-split", "dk-workflows:dk-pr-description",
                 "dk-workflows:dk-pr-reviewers-run", "dk-workflows:dk-pr-publish"]
DEFAULT_FINALIZERS = ["dk-workflows:dk-commit-split", "dk-workflows:dk-preferences", "dk-workflows:dk-ci"]
BUILTINS = {
    "dk-workflows:dk-pr-feedback-read": ("inspection", False, "Capture all actionable inline threads, review bodies and general comments; preserve source identities and obligations."),
    "dk-workflows:dk-pr-decisions": ("decisions", False, "Reconcile settled decisions from original sources, exact request and repository guidance; do not invent user authority."),
    "dk-workflows:dk-pr-feedback-address": ("fix", True, "Verify actionable feedback and implement focused changes. Account for original findings and every changed file; preserve unrelated work."),
    "dk-workflows:dk-pr-split": ("scope", False, "Assess the complete diff against the selected size/scope policies; report concrete boundaries and ownership evidence. Splitting publication requires separately supplied explicit authority."),
    "dk-workflows:dk-pr-description": ("description", False, "Prepare a repository-native PR title and description from the actual final diff and accepted issue; propose exact authorized publication operations only."),
    "dk-workflows:dk-commit-split": ("cleanup", False, "Inspect the entire commit series against selected commit policy and final tree. Report blockers and exact justified history proposals; do not rewrite Git directly."),
    "dk-workflows:dk-preferences": ("synchronization", True, "Synchronize governing repository content only where the exact original request and selected policy require changes; account for every changed path."),
}
CHECKLIST = [
    {"id": "assignment", "text": "Perform the exact assigned task against current source and retained original instructions."},
    {"id": "evidence", "text": "Attach source files with current SHA-256 evidence and preserve every original finding/source identity."},
    {"id": "scope", "text": "Account for requirements, blockers and all proposed changes or publication operations without expanding user authority."},
]
REPORT_SCHEMA = {
    "type": "object", "required": ["complete", "checklist", "findings", "evidence", "blockers"],
    "properties": {
        "complete": {"type": "boolean"}, "checklist": {"type": "array", "items": {"type": "object"}},
        "findings": {"type": "array", "items": {"type": "object"}},
        "evidence": {"type": "array", "items": {"type": "object"}},
        "blockers": {"type": "array", "items": {"type": "string"}},
        "dispositions": {"type": "array", "items": {"type": "object"}},
        "changes": {"type": "array", "items": {"type": "object"}},
        "commit_message": {"type": "string"}, "operations": {"type": "array", "items": {"type": "object"}},
        "audience": {"type": "object"}, "deliverables": {"type": "object"},
    }, "additionalProperties": False,
}


def expression(value):
    return "${{ " + value + " }}"


def helper(operation, inputs, ident=None):
    step = {"argv": ["python3", "-B", "/actions/helpers/action.py", operation],
            "protocol": "action-v1", "effect": "external_mutation",
            "input-schema": {"type": "object"}, "output-schema": {"type": "object"},
            "input": inputs,
            "env": {"GH_TOKEN": expression("secrets.github_token"),
                    "GH_ENTERPRISE_TOKEN": expression("secrets.github_token")}}
    if ident:
        step["id"] = ident
    return step


def container_job(image, steps, needs=None, outputs=None, condition=None):
    result = {"container": {"image": image, "files": ["helpers/"]}, "steps": steps}
    if needs:
        result["needs"] = needs
    if outputs:
        result["outputs"] = outputs
    if condition:
        result["if"] = expression(condition)
    return result


def interface(inputs, outputs=None):
    result = {"inputs": {key: {"type": kind, "required": True} for key, kind in inputs.items()},
              "secrets": {"github_token": {"required": True}}}
    if outputs:
        result["outputs"] = {key: {"value": value} for key, value in outputs.items()}
    return {"workflow_call": result}


def native(agent, prompt, task_input):
    return {"id": "task", "uses": "agent/run", "with": {
        "agent": agent, "turns": 8, "prompt": prompt, "input": task_input,
        "output": {"format": "json", "repair-attempts": 0, "schema": REPORT_SCHEMA}}}


def leaf_workflow(image, prefix, task):
    plan = expression("inputs.plan")
    claim = expression("needs.prepare.outputs.claim")
    workflow = {"name": "Portable PR task " + task["id"],
        "on": interface({"plan": "string"}, {"complete": expression("jobs.status.outputs.complete")}),
        "jobs": {
            "prepare": container_job(image, [helper("claim", {"plan": plan, "task": task["id"]}, "claim")],
                outputs={key: expression("steps.claim.outputs." + key) for key in ("claim", "invoke", "prompt", "task_input")}),
            "native": {"needs": "prepare", "if": expression("needs.prepare.outputs.invoke"),
                "steps": [native("agents/" + task["definition"] + ".toml", expression("needs.prepare.outputs.prompt"), expression("needs.prepare.outputs.task_input"))],
                "outputs": {"agent": expression("steps.task.outputs.agent"), "report": expression("steps.task.outputs.structured")}},
            "accept": container_job(image, [helper("accept", {"plan": plan, "claim": claim,
                "agent": expression("needs.native.outputs.agent"), "report": expression("needs.native.outputs.report"), "round": 0}, "accepted")],
                ["prepare", "native"], {"complete": expression("steps.accepted.outputs.complete")},
                "needs.prepare.outputs.invoke"),
            "checklist": {"needs": ["prepare", "native", "accept"],
                "if": expression("needs.prepare.outputs.invoke and needs.accept.outputs.complete != true"),
                "uses": prefix + "/continue.yml", "repeat": {"max-attempts": 3, "until": expression("iteration.outputs.settled")},
                "secrets": {"github_token": expression("secrets.github_token")},
                "with": {"plan": plan, "claim": claim, "agent": expression("needs.native.outputs.agent"), "round": expression("iteration.number")}},
            "status": container_job(image, [helper("task_status", {"plan": plan, "claim": claim}, "status")],
                ["prepare", "native", "accept", "checklist"], {"complete": expression("steps.status.outputs.complete")},
                "needs.prepare.status == 'succeeded' and needs.native.status != 'failed' and needs.native.status != 'needs_attention' and needs.accept.status != 'failed' and needs.accept.status != 'needs_attention' and needs.checklist.status != 'failed' and needs.checklist.status != 'needs_attention'")}}

    jobs = workflow["jobs"]
    workflow["on"]["workflow_call"]["inputs"]["cycle"] = {"type": "number", "required": True}
    jobs["prepare"]["steps"][0]["input"]["cycle"] = expression("inputs.cycle")
    jobs["native"]["steps"][0]["continue-on-error"] = True
    jobs["native"]["outputs"].update(native_status_outputs())
    observed = {"plan": plan, "claim": claim, "agent": expression("needs.native.outputs.agent"),
                "report": expression("needs.native.outputs.report"), **{key: expression("needs.native.outputs." + key)
                    for key in ("status", "failure_class", "unsafe_failure_class", "error")}}
    jobs["accept"]["steps"][0] = helper("turn_result", observed, "accepted")
    jobs["accept"]["outputs"].update({key: expression("steps.accepted.outputs." + key)
        for key in ("retry", "checklist", "prompt", "task_input")})
    jobs["retry"] = {"needs": ["prepare", "native", "accept"],
        "if": expression("needs.accept.outputs.retry"), "uses": prefix + "/retry.yml",
        "with": {"plan": plan, "claim": claim, "agent": expression("needs.native.outputs.agent"),
                 "prompt": expression("needs.accept.outputs.prompt"), "task_input": expression("needs.accept.outputs.task_input")},
        "secrets": {"github_token": expression("secrets.github_token")}}
    jobs["checklist"]["needs"].append("retry")
    jobs["checklist"]["if"] = expression("needs.accept.outputs.checklist")
    jobs["retry_checklist"] = copy.deepcopy(jobs["checklist"])
    jobs["retry_checklist"]["if"] = expression("needs.retry.outputs.checklist")
    jobs["status"]["needs"].append("retry")
    jobs["status"]["needs"].append("retry_checklist")
    jobs["status"]["if"] = jobs["status"]["if"][:-3] + " and needs.retry.status != 'failed' and needs.retry.status != 'needs_attention' and needs.retry_checklist.status != 'failed' and needs.retry_checklist.status != 'needs_attention' }}"
    return workflow


def native_status_outputs():
    return {key: expression("steps.task." + key) for key in ("status", "error", "failure_class", "unsafe_failure_class")}


def retry_workflow(image):
    plan = expression("inputs.plan")
    step = native(expression("inputs.agent"), expression("inputs.prompt"), expression("inputs.task_input"))
    step["continue-on-error"] = True
    return {"name": "One retained eligible same-session retry", "on": interface(
        {"plan": "string", "claim": "string", "agent": "string", "prompt": "string", "task_input": "object"},
        {"complete": expression("jobs.accept.outputs.complete"), "checklist": expression("jobs.accept.outputs.checklist")}),
        "jobs": {"native": {"steps": [step], "outputs": {"agent": expression("steps.task.outputs.agent"),
            "report": expression("steps.task.outputs.structured"), **native_status_outputs()}},
            "accept": container_job(image, [helper("turn_result", {"plan": plan, "claim": expression("inputs.claim"),
                "agent": expression("needs.native.outputs.agent"), "report": expression("needs.native.outputs.report"),
                "retried": True, **{key: expression("needs.native.outputs." + key)
                    for key in ("status", "error", "failure_class", "unsafe_failure_class")}}, "accepted")],
                ["native"], {"complete": expression("steps.accepted.outputs.complete"), "checklist": expression("steps.accepted.outputs.checklist")})}}


def continue_workflow(image):
    plan = expression("inputs.plan")
    return {"name": "Same-session checklist continuation", "on": interface(
        {"plan": "string", "claim": "string", "agent": "string", "round": "number"},
        {"complete": expression("jobs.accept.outputs.complete"), "settled": expression("jobs.accept.outputs.settled")}), "jobs": {
            "prepare": container_job(image, [helper("check_prepare", {"plan": plan, "claim": expression("inputs.claim"),
                "agent": expression("inputs.agent"), "round": expression("inputs.round")}, "check")],
                outputs={"input": expression("steps.check.outputs.task_input"), "prompt": expression("steps.check.outputs.prompt")}),
            "native": {"needs": "prepare", "steps": [native(expression("inputs.agent"), expression("needs.prepare.outputs.prompt"), expression("needs.prepare.outputs.input"))],
                "outputs": {"agent": expression("steps.task.outputs.agent"), "report": expression("steps.task.outputs.structured")}},
            "accept": container_job(image, [helper("accept", {"plan": plan, "claim": expression("inputs.claim"),
                "agent": expression("needs.native.outputs.agent"), "report": expression("needs.native.outputs.report"),
                "round": expression("inputs.round")}, "accepted")], ["native"], {"complete": expression("steps.accepted.outputs.complete"), "settled": expression("steps.accepted.outputs.settled")})}}


def publication_workflow(image):
    plan = expression("inputs.plan")
    return {"name": "One exact publication intent, audience verdict and receipt", "on": interface(
        {"plan": "string", "proposal": "object"}, {"receipt": expression("jobs.publish.outputs.receipt")}), "jobs": {
            "prepare": container_job(image, [helper("publication_prepare", {"plan": plan, "proposal": expression("inputs.proposal")}, "intent")],
                outputs={"intent": expression("steps.intent.outputs.intent"), "id": expression("steps.intent.outputs.id"), "fingerprint": expression("steps.intent.outputs.fingerprint")}),
            "audience": {"needs": "prepare", "steps": [native("agents/audience.toml",
                "Review this exact public payload for its audience. Return source-backed checklist and audience={fingerprint,suitable,evidence}. Do not publish or change payload.",
                {"intent": expression("needs.prepare.outputs.intent"), "fingerprint": expression("needs.prepare.outputs.fingerprint"), "proposal": expression("inputs.proposal"), "checklist": CHECKLIST})],
                "outputs": {"agent": expression("steps.task.outputs.agent"), "report": expression("steps.task.outputs.structured")}},
            "publish": container_job(image, [helper("publication_execute", {"plan": plan,
                "id": expression("needs.prepare.outputs.id"), "agent": expression("needs.audience.outputs.agent"),
                "report": expression("needs.audience.outputs.report")}, "published"),
                helper("publication_record", {"plan": plan, "id": expression("needs.prepare.outputs.id"),
                    "receipt": expression("steps.published.outputs.receipt")}, "recorded")], ["prepare", "audience"],
                {"receipt": expression("steps.recorded.outputs.receipt")})}}


def publication_barrier(image, prefix):
    plan = expression("inputs.plan")
    return {"name": "Sequential publication barrier", "on": interface({"plan": "string"}), "jobs": {
        "list": container_job(image, [helper("publication_list", {"plan": plan}, "intents")],
                              outputs={"items": expression("steps.intents.outputs.items")}),
        "publish": {"needs": "list", "uses": prefix + "/publication.yml",
                    "for-each": {"items": expression("needs.list.outputs.items"), "key": expression("item.id"), "concurrency": 1},
                    "with": {"plan": plan, "proposal": expression("item")},
                    "secrets": {"github_token": expression("secrets.github_token")}}}}


def ci_workflow(image):
    return {"name": "One CI observation with retained absolute deadline", "on": interface({"plan": "string"},
        {"settled": expression("jobs.observe.outputs.settled"), "failed": expression("jobs.observe.outputs.failed")}), "jobs": {
            "observe": container_job(image, [helper("ci", {"plan": expression("inputs.plan")}, "ci"),
                {"id": "delay", "if": expression("steps.ci.outputs.pending"), "argv": ["sleep", "10"]}],
                outputs={"settled": expression("steps.ci.outputs.pending != true"), "failed": expression("steps.ci.outputs.failed")})}}


def _call(prefix, workflow, plan, needs=None, cycle=None):
    value = {"uses": prefix + "/" + workflow, "with": {"plan": plan},
             "secrets": {"github_token": expression("secrets.github_token")}}
    if needs:
        value["needs"] = needs
        dependencies = [needs] if isinstance(needs, str) else needs
        value["if"] = expression(" and ".join("needs." + key + ".status != 'failed' and needs." + key + ".status != 'needs_attention' and needs." + key + ".status != 'canceled'" for key in dependencies))
    if cycle is not None:
        value["with"]["cycle"] = cycle
    return value


def _groups(raw):
    if not isinstance(raw, list):
        raise ValueError("selected plan must be an array")
    result = []
    for entry in raw:
        group = entry if isinstance(entry, list) else [entry]
        if not group or any(isinstance(item, list) for item in group):
            raise ValueError("invalid parallel group")
        result.append([{"skill": item} if isinstance(item, str) else item for item in group])
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
    value = tomllib.loads(data.decode())
    allowed = {"name", "description", "developer_instructions", "model", "model_reasoning_effort"}
    if set(value) - allowed or not all(isinstance(value.get(key), str) and value[key].strip()
                                       for key in ("name", "description", "developer_instructions")):
        raise ValueError("unsupported native definition")
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


@contextmanager
def staged_closure(final):
    final.parent.mkdir(parents=True, exist_ok=True, mode=0o700)
    temporary = final.parent / (".staged-" + os.urandom(16).hex())
    temporary.mkdir(mode=0o700)
    try:
        yield temporary
        for path in sorted(temporary.rglob("*")):
            if path.is_symlink():
                raise ValueError("generated closure contains a symlink")
            if path.is_file():
                fd = os.open(path, os.O_RDONLY)
                try:
                    os.fsync(fd)
                finally:
                    os.close(fd)
        for path in [*sorted((p for p in temporary.rglob("*") if p.is_dir()), reverse=True), temporary]:
            fd = os.open(path, os.O_RDONLY)
            try:
                os.fsync(fd)
            finally:
                os.close(fd)
        os.rename(temporary, final)
        fd = os.open(final.parent, os.O_RDONLY)
        try:
            os.fsync(fd)
        finally:
            os.close(fd)
    finally:
        if temporary.exists():
            shutil.rmtree(temporary)


def prepare(root, request, provider=None):
    root = Path(root).resolve(strict=True)
    allowed = {"pr", "logical_run", "instructions", "image", "roles", "options", "settings",
               "authorizations", "maintain_other", "adapters", "resources", "runtime"}
    if not isinstance(request, dict) or set(request) - allowed:
        raise ValueError("unsupported preparation fields")
    pr = identity(request["pr"])
    instructions = text(request.get("instructions"), "exact original instruction")
    image = text(request.get("image"), "helper image")
    if not re.fullmatch(r"(?:[A-Za-z0-9][A-Za-z0-9_./:-]*@)?sha256:[a-f0-9]{64}", image):
        raise ValueError("helper image must be digest-pinned")
    roles = request.get("roles", {})
    required_roles = {"leaf", "reviewer", "verifier", "fixer", "audience"}
    if set(roles) != required_roles:
        raise ValueError("explicit native definition paths required for leaf/reviewer/verifier/fixer/audience")
    definitions = {key: _definition(root, value) for key, value in roles.items()}
    runtime = request.get("runtime", {})
    if not isinstance(runtime, dict) or set(runtime) - {"authentication", "model", "limits", "sandbox"}:
        raise ValueError("unsupported runtime configuration")
    options = request.get("options", {})
    if not isinstance(options, dict):
        raise ValueError("options must be an object")
    allowed_options = {"tasks", "finalizers", "reviewers", "limits", "permissions", "accounts", "ci", "merge_method",
                       "ci_timeout_seconds", "ci_classifications", "ci_exceptions", "before_cycle", "after_cycle"}
    if set(options) - allowed_options:
        raise ValueError("unsupported portable option: " + ", ".join(sorted(set(options) - allowed_options)))
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
    owned_directory(root, GENERATED, create=True)
    ledger = Ledger(root, logical_run, pr["url"])
    ledger.close()
    adapters = request.get("adapters", {})
    tasks, stages = [], []
    serial = 0

    def task_call(call, fallback=None):
        nonlocal serial
        if not isinstance(call, dict) or set(call) - {"skill", "options", "required", "instructions"}:
            raise ValueError("portable calls support skill/options/required/instructions only")
        name = text(call.get("skill"), "selected skill")
        if name in BUILTINS:
            kind, writes, assignment = BUILTINS[name]
            adapter = {"kind": kind, "writes": writes, "assignment": assignment, "definition": "leaf"}
        elif name in adapters:
            adapter = adapters[name]
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

    selections = [("tasks", options.get("tasks", DEFAULT_TASKS)),
                  ("finalizers", options.get("finalizers", DEFAULT_FINALIZERS))]
    for phase, selected in selections:
        for group in _groups(selected):
            if len(group) > 1 and any(call.get("skill") in {"dk-workflows:dk-pr-publish", "dk-workflows:dk-ci", "dk-workflows:dk-pr-reviewers-run"} for call in group):
                raise ValueError("managed publication/review/CI stages cannot be in a parallel group")
            if len(group) == 1 and group[0].get("skill") == "dk-workflows:dk-pr-publish":
                stages.append({"kind": "publication", "phase": phase})
                continue
            if len(group) == 1 and group[0].get("skill") == "dk-workflows:dk-ci":
                stages.append({"kind": "ci", "phase": phase})
                continue
            if len(group) == 1 and group[0].get("skill") == "dk-workflows:dk-pr-reviewers-run":
                reviewers = group[0].get("options", {}).get("reviewers", options.get("reviewers", []))
                if not reviewers:
                    raise ValueError("selected reviewer stage requires explicit reviewers")
                for reviewer_group in _groups(reviewers):
                    keys = [task_call(call, "review") for call in reviewer_group]
                    if any(next(t for t in tasks if t["id"] == key)["kind"] != "review" for key in keys):
                        raise ValueError("reviewer selection must use read-only review adapters")
                    stages.append({"kind": "tasks", "tasks": keys, "phase": phase})
                stages.append({"kind": "verify_fix", "phase": phase})
                continue
            keys = [task_call(call) for call in group]
            if len(keys) > 1 and any(next(t for t in tasks if t["id"] == key)["writes"] for key in keys):
                raise ValueError("parallel writing tasks share a Git index and are unsupported")
            stages.append({"kind": "tasks", "tasks": keys, "phase": phase})
            if any(next(t for t in tasks if t["id"] == key)["kind"] == "review" for key in keys):
                stages.append({"kind": "verify_fix", "phase": phase})

    if any(stage["kind"] == "verify_fix" for stage in stages):
        for key, kind, role, assignment in [
            ("finding_verification", "verification", "verifier", "Independently verify every retained original finding. Preserve exact finding_id and report source; return dispositions confirmed/rejected with concrete source evidence and reason. Do not edit or publish."),
            ("finding_fix", "fix", "fixer", "Address independently confirmed findings. Return every finding_id disposition fixed/accepted/deferred/rejected, original sources, evidence and exact changed-file hashes/commit_message. Accepted/deferred dispositions require retained exact user authority. Do not publish or commit.")]:
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
            "image": image, "options": options, "settings": request.get("settings", {}), "runtime": runtime,
            "authorizations": authorizations, "limits": limits, "tasks": tasks, "stages": stages,
            "resources": {}, "definitions": {key: hashlib.sha256(value).hexdigest() for key, value in definitions.items()},
            "review_contract": digest([task for task in tasks if task["kind"] in {"review", "verification"}])}
    source = Path(__file__).resolve().parent.parent
    plan["helper_manifest"] = _helper_manifest(source)
    for task in tasks:
        task["implementation"] = digest(plan["helper_manifest"])
        task["runtime_fingerprint"] = digest(runtime)
    if options.get("before_cycle") or options.get("after_cycle"):
        raise ValueError("portable cycle hooks require explicit task placement; legacy hook stores are not imported")
    from .capture import resource_digests
    plan["resources"] = resource_digests(root, request.get("resources", []))
    if any(name not in plan["resources"] for task in tasks for name in task["resources"]):
        raise ValueError("adapter resource is undeclared")
    if len(tasks) > 40:
        raise ValueError("portable plan exceeds bounded engine worker admission")
    from .capture import GitHub, capture
    provider = provider or GitHub(root)
    plan["snapshot"] = capture(root, provider, plan, clean=True)
    plan["fingerprint"] = digest(plan)
    plan_reference = artifact(root, plan)
    prefix = (Path(GENERATED) / logical_run / plan["fingerprint"]).as_posix()
    directory = root / prefix
    if directory.exists():
        manifest = json.loads((directory / "manifest.json").read_text())
        if manifest.get("plan") != plan_reference:
            raise ValueError("generated closure identity conflict")
        for name, expected in manifest.get("files", {}).items():
            path = directory / name
            if path.is_symlink() or not path.is_file() or hashlib.sha256(path.read_bytes()).hexdigest() != expected:
                raise ValueError("generated closure bytes changed")
    else:
        with staged_closure(directory) as directory:
            (directory / "agents").mkdir()
            shutil.copytree(source, directory / "helpers", ignore=shutil.ignore_patterns("__pycache__", "*.pyc"))
            for key, value in definitions.items():
                (directory / "agents" / (key + ".toml")).write_bytes(value)
            documents = {"continue.yml": continue_workflow(image), "retry.yml": retry_workflow(image), "publication.yml": publication_workflow(image),
                         "publication-barrier.yml": publication_barrier(image, prefix), "ci.yml": ci_workflow(image)}
            for task in tasks:
                documents[task["id"] + ".yml"] = leaf_workflow(image, prefix, task)
            jobs, previous = {}, []
            for number, stage in enumerate(stages, 1):
                label = "stage_" + str(number)
                if stage["kind"] == "tasks":
                    current = []
                    for task_key in stage["tasks"]:
                        key = label + "_" + task_key
                        jobs[key] = _call(prefix, task_key + ".yml", expression("inputs.plan"), previous, expression("inputs.cycle"))
                        current.append(key)
                    previous = current
                elif stage["kind"] == "verify_fix":
                    jobs[label + "_verify"] = _call(prefix, "finding_verification.yml", expression("inputs.plan"), previous, expression("inputs.cycle"))
                    jobs[label + "_fix"] = _call(prefix, "finding_fix.yml", expression("inputs.plan"), [label + "_verify"], expression("inputs.cycle"))
                    previous = [label + "_fix"]
                else:
                    filename = "publication-barrier.yml" if stage["kind"] == "publication" else "ci.yml"
                    jobs[label] = _call(prefix, filename, expression("inputs.plan"), previous)
                    if stage["kind"] == "ci":
                        jobs[label]["repeat"] = {"max-attempts": 100, "until": expression("iteration.outputs.settled")}
                        jobs[label + "_repair"] = _call(prefix, "ci_repair.yml", expression("inputs.plan"), [label], expression("inputs.cycle"))
                        jobs[label + "_repair"]["if"] = expression("needs." + label + ".outputs.failed")
                        jobs[label + "_publish"] = _call(prefix, "publication-barrier.yml", expression("inputs.plan"), [label + "_repair"])
                        previous = [label, label + "_repair", label + "_publish"]
                    else:
                        previous = [label]
            jobs["coverage"] = container_job(image, [helper("coverage", {"plan": expression("inputs.plan")}, "coverage")], previous,
                                                  {"complete": expression("steps.coverage.outputs.complete")},
                " and ".join("needs." + key + ".status != 'failed' and needs." + key + ".status != 'needs_attention' and needs." + key + ".status != 'canceled'" for key in previous) if previous else None)
            documents["cycle.yml"] = {"name": "Selected task/finalizer plan in declared barrier order", "on": interface({"plan": "string", "cycle": "number"},
                {"complete": expression("jobs.coverage.outputs.complete")}), "jobs": jobs}
            documents["main.yml"] = {"name": "Portable PR maintenance", "on": interface({"plan": "string"},
                {"complete": expression("jobs.completion.outputs.complete"), "ready": expression("jobs.completion.outputs.ready")}), "jobs": {
                    "cycles": {**_call(prefix, "cycle.yml", expression("inputs.plan"), cycle=expression("iteration.number")), "repeat": {"max-attempts": limits["review_cycles"], "until": expression("iteration.outputs.complete")}},
                    "completion": container_job(image, [helper("completion", {"plan": expression("inputs.plan")}, "completion")], ["cycles"],
                        {"complete": expression("steps.completion.outputs.complete"), "ready": expression("steps.completion.outputs.ready")})}}
            if "merge" in authorizations:
                documents["main.yml"]["jobs"]["merge_proposal"] = container_job(image,
                    [helper("merge_proposal", {"plan": expression("inputs.plan")}, "proposal")], ["completion"],
                    {"invoke": expression("steps.proposal.outputs.invoke"), "proposal": expression("steps.proposal.outputs.proposal")},
                    "needs.completion.outputs.ready")
                documents["main.yml"]["jobs"]["merge"] = {
                    "needs": "merge_proposal", "if": expression("needs.merge_proposal.outputs.invoke"),
                    "uses": prefix + "/publication.yml", "with": {"plan": expression("inputs.plan"), "proposal": expression("needs.merge_proposal.outputs.proposal")},
                    "secrets": {"github_token": expression("secrets.github_token")}}
            for name, document in documents.items():
                atomic_json(directory / name, document)
            configuration = {"execution": {"mode": "container", "image": image, "runtime": "docker", "network": "bridge", "writable": True},
                             "agents": {"codex": {"provider": "codex-app-server", "execution": {"mode": "local"},
                                        **runtime, "authentication": runtime.get("authentication", {"mode": "native"})}}}
            atomic_json(directory / "agents.yml", configuration)
            manifest = {"format": FORMAT, "plan": plan_reference, "files": {}}
            for path in sorted(directory.rglob("*")):
                if path.is_file():
                    manifest["files"][path.relative_to(directory).as_posix()] = hashlib.sha256(path.read_bytes()).hexdigest()
            atomic_json(directory / "manifest.json", manifest)
    return {"logical_run": logical_run, "plan": plan_reference, "workflow": prefix + "/main.yml",
            "config": prefix + "/agents.yml", "request_id": digest([logical_run, plan["fingerprint"]]),
            "inputs": {"plan": plan_reference}}
