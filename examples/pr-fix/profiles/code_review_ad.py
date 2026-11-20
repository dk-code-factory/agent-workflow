#!/usr/bin/env python3
"""Prepare three engine-managed reviewers from an installed code-review-ad package.

Run before committing/admitting the workflow. No providers or remote writes.
"""
import argparse
import hashlib
import json
import os
from pathlib import Path

ROLES = ("toolkit", "standards", "spec")
REQUIRED = ("common/reporting.md", "common/inspection.md", "common/codex/review.md",
            "common/codex/scripts/prepare-review.js", "common/codex/scripts/aspect-driver.py",
            *("common/codex/passes/" + role + ".md" for role in ROLES))


def relative_path(value):
    path = Path(value)
    if path.is_absolute() or ".." in path.parts or not path.parts or path == Path("."):
        raise ValueError("output paths must be workspace-relative")
    return path


def _resource_files(directory):
    pending = [directory] if directory.is_dir() else []
    while pending:
        with os.scandir(pending.pop()) as entries:
            for entry in entries:
                path = Path(entry.path)
                if entry.name == "__pycache__" or path.suffix == ".pyc":
                    continue
                if entry.is_symlink():
                    raise ValueError("review resources must not contain symlinks")
                if entry.is_dir(follow_symlinks=False):
                    if len(pending) >= 4096:
                        raise ValueError("review package exceeds bounded snapshot size")
                    pending.append(path)
                elif entry.is_file(follow_symlinks=False):
                    yield path


def configure(plugin_root, workspace, output, resource_dir, contexts=()):
    plugin_root, workspace = Path(plugin_root).resolve(), Path(workspace).resolve()
    output, resource_dir = relative_path(output), relative_path(resource_dir)
    if output.is_relative_to(resource_dir) or resource_dir.is_relative_to(output):
        raise ValueError("profile output and resource directory must not overlap")
    # Validate package/context inputs and existing destination conflicts first.
    files = {}
    total_bytes = 0
    for path in _resource_files(plugin_root / "common"):
        if len(files) >= 4096:
            raise ValueError("review package exceeds bounded snapshot size")
        if path.stat().st_size > 4 << 20:
            raise ValueError("review resource exceeds 4 MiB")
        remaining = min(4 << 20, (16 << 20) - total_bytes)
        with path.open("rb") as stream:
            data = stream.read(remaining + 1)
        if len(data) > remaining:
            raise ValueError("review package exceeds bounded snapshot size")
        files[path.relative_to(plugin_root).as_posix()] = data
        total_bytes += len(data)
    files = dict(sorted(files.items()))
    if not set(REQUIRED) <= files.keys() or not any(name.startswith("common/codex/checks/") for name in files):
        raise ValueError("incomplete code-review-ad package")
    for relative in (output, resource_dir):
        path = workspace / relative
        if path.exists() or path.is_symlink():
            raise ValueError("refusing to replace existing profile assets: " + str(relative))
        if any(parent.is_symlink() for parent in path.parents if parent.is_relative_to(workspace)):
            raise ValueError("profile destination must not traverse symlinks")
        if any(parent.exists() and not parent.is_dir()
               for parent in path.parents if parent.is_relative_to(workspace)):
            raise ValueError("each profile destination ancestor must be a directory")
    context_paths = [relative_path(name) for name in contexts]
    for relative in context_paths:
        path = workspace / relative
        if (any(parent.is_symlink() for parent in (path, *path.parents) if parent.is_relative_to(workspace))
                or not path.is_file() or path.stat().st_size > 4 << 20):
            raise ValueError("context must be a bounded regular workspace file")
    resources = [(resource_dir / name).as_posix() for name in files]
    resources += [(resource_dir / "snapshot.json").as_posix(), *map(str, context_paths)]
    adapters = {}
    for role in ROLES:
        root = resource_dir.as_posix()
        assignment = f'''Execute only the {role} native pass of code-review-ad, using the exact snapshotted package at {root}.
The engine schedules Toolkit, Standards and Spec as separate fresh sessions in parallel; do not delegate or launch a model process.
Read {root}/common/codex/passes/{role}.md, {root}/common/reporting.md and {root}/common/inspection.md before inspecting source. Read the inspection/coverage and relevant check selection sections of {root}/common/codex/review.md; apply the checks selected by the actual changed behavior. Do not coordinate or execute other passes.
Use the frozen PR metadata at snapshot.evidence_reference (title, full body and complete discussion); obtain commit metadata from the package source-preparation helper, snapshot.base as merge-base, and snapshot.local_head as the reviewed head. Read applicable repository guidance and supplemental frozen context: {json.dumps(list(map(str, context_paths)))}. Inspect no prior reviewer reports or sibling findings. Original workflow instructions are scope/permissions, not another reviewer's hypotheses.
Run node {root}/common/codex/scripts/prepare-review.js --base BASE --head HEAD once for your pinned source facts. If Node is unavailable, use the package's documented read-only Git fallback and disclose it. Each engine-managed pass prepares its own facts; this replaces the skill coordinator's single shared preparation without changing pins or pass criteria.
Find supported issues immediately. After the initial broad inspection and candidate verification, run python3 {root}/common/codex/scripts/aspect-driver.py --pass {role} --base BASE --head HEAD exactly once in THIS session. Follow its printed deeper-look prompt even when no issues were found. Do not mark that workflow complete without the original command, successful exit and follow-up coverage.
Forbid source edits, tests, builds, linters, formatters, posting, model subprocesses, further agents and Git checkout/switch/reset/stash/worktree. Read-only Git inspection and the two package drivers are allowed. Do not propose fixes or implementation plans.
Return the engine's assigned report envelope with exactly its checklist IDs. Each finding text must preserve the package's severity, issue category, exact location, assertion, trigger/consequence, base/head distinction and contrary evidence. Use current regular source files for evidence IDs. In deliverables preserve your role, pins, original pass report, coverage gaps, candidate dispositions, required tables and the aspect-driver argv/exit status. Keep distinct documentation and behavioral obligations separate; merge only equivalent assertions in the same category. An evidenced no-findings report is valid.
'''
        adapters["code-review-ad-" + role] = {"kind": "review", "definition": "reviewer", "writes": False,
            "assignment": assignment, "resources": resources, "history": True,
            "head_sensitive": True, "cacheable": True}
    profile = {"name": "PR fix with code-review-ad native passes", "on": {"workflow_call": {
        "inputs": {"request": {"type": "string", "required": True}},
        "secrets": {"github_token": {"required": True}},
        "outputs": {key: {"value": "${{ jobs.fix.outputs." + key + " }}"} for key in ("complete", "ready")}}},
        "jobs": {"fix": {"uses": "examples/pr-fix/workflows/main.yml", "with": {
            "request": "${{ inputs.request }}", "tasks": [{"stage": "reviews"}], "finalizers": [],
            "reviewers": [["code-review-ad-" + role for role in ROLES]], "adapters": adapters,
            "resources": resources, "limits": {"review_cycles": 2, "retries_per_task": 0,
            "ci_repairs_per_cycle": 0, "ci_reruns_per_head": 0}},
            "secrets": {"github_token": "${{ secrets.github_token }}"}}}}
    manifest = {"package": "code-review-ad", "source": str(plugin_root), "roles": list(ROLES),
                "mode": "engine-managed native passes; no nested plugin coordinator",
                "files": {name: hashlib.sha256(data).hexdigest() for name, data in files.items()}}
    for name, data in files.items():
        path = workspace / resource_dir / name
        path.parent.mkdir(parents=True, exist_ok=True)
        path.write_bytes(data)
    (workspace / resource_dir / "snapshot.json").write_text(json.dumps(manifest, indent=2) + "\n")
    path = workspace / output
    path.parent.mkdir(parents=True, exist_ok=True)
    path.write_text(json.dumps(profile, indent=2) + "\n")
    return profile, manifest


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--plugin-root", required=True)
    parser.add_argument("--workspace", default=".")
    parser.add_argument("--context", action="append", default=[], help="additional frozen workspace context file")
    parser.add_argument("--output", default="examples/pr-fix/workflows/code-review-ad.yml")
    parser.add_argument("--resource-dir", default="examples/pr-fix/review-resources/code-review-ad")
    args = parser.parse_args()
    configure(args.plugin_root, args.workspace, args.output, args.resource_dir, args.context)
    print("Prepared Toolkit, Standards and Spec; commit the profile and resources before admission.")


if __name__ == "__main__":
    main()
