"""One-shot, bounded Git/GitHub observations for the portable PR application.

This module never launches a provider, selects a workflow stage or writes Git.
The caller owns storage of ``GitHub.last_capture`` as immutable evidence.
"""

from __future__ import annotations

import hashlib
import json
import os
from pathlib import Path
import re
import selectors
import signal
import stat
import subprocess
import time
from urllib.parse import parse_qsl, urlencode, urlsplit

from .core import FORMAT, GENERATED, STATE, bounded_json


MAX_OUTPUT = 16 << 20
MAX_STDERR = 1 << 20
MAX_PAGES = 100
MAX_FILES = 10000
MAX_FILE_BYTES = 64 << 20
SHA = re.compile(r"^[0-9a-f]{40}(?:[0-9a-f]{24})?$")
HOST = re.compile(r"^[A-Za-z0-9](?:[A-Za-z0-9.-]*[A-Za-z0-9])?$")
REPO_PART = re.compile(r"^[A-Za-z0-9_.-]+$")


def canonical(value):
    return json.dumps(value, sort_keys=True, separators=(",", ":"),
                      ensure_ascii=False, allow_nan=False).encode("utf-8")


def digest(value):
    return hashlib.sha256(canonical(value)).hexdigest()


def _text(value, label):
    if not isinstance(value, str) or not value.strip() or "\0" in value:
        raise ValueError(label + " requires nonempty text")
    value.encode("utf-8", errors="strict")
    return value


def identity(raw):
    """Require canonical HTTPS identities; never derive API routes from URLs."""
    parsed = urlsplit(_text(raw, "PR URL"))
    parts = parsed.path.strip("/").split("/")
    if (parsed.scheme != "https" or parsed.netloc != parsed.hostname
            or not HOST.fullmatch(parsed.hostname or "") or parsed.query
            or parsed.fragment or "%" in parsed.path or len(parts) != 4
            or parts[2] != "pull" or not parts[3].isdigit()
            or int(parts[3]) <= 0 or str(int(parts[3])) != parts[3]
            or parsed.path != "/" + "/".join(parts)
            or any(not REPO_PART.fullmatch(p) or p in (".", "..")
                   for p in parts[:2])):
        raise ValueError("canonical HTTPS PR URL required")
    host, owner, name = parsed.hostname.lower(), parts[0].lower(), parts[1].lower()
    repository = host + "/" + owner + "/" + name
    return {"url": "https://" + repository + "/pull/" + parts[3],
            "host": host, "owner": owner, "name": name,
            "number": int(parts[3]), "repository": repository}


def _identity(value):
    expected = identity(value["url"])
    if value != expected:
        raise ValueError("PR identity fields differ from canonical URL")
    return expected


def _positive(value, label):
    if isinstance(value, bool) or not isinstance(value, int) or value <= 0:
        raise ValueError(label + " requires a positive integer")
    return value


def run(root, argv, data=None, *, timeout=60, limit=MAX_OUTPUT, env=None):
    """Bound both pipes while the process runs, including diagnostic output.

    Reading after ``communicate`` would only discover an oversized result after
    allocating it. Separate process groups also bound a failed gh credential
    subprocess; the enclosing container guardian remains the lifetime owner.
    """
    if not argv or any(not isinstance(a, str) or "\0" in a for a in argv):
        raise ValueError("invalid command arguments")
    if data is not None and len(data) > 1 << 20:
        raise ValueError("command input exceeds 1 MiB")
    with subprocess.Popen(argv, cwd=root, stdin=subprocess.PIPE if data is not None
                          else subprocess.DEVNULL, stdout=subprocess.PIPE,
                          stderr=subprocess.PIPE, start_new_session=True, env=env) as child:
        deadline = time.monotonic() + timeout
        streams = {child.stdout: (bytearray(), limit),
                   child.stderr: (bytearray(), MAX_STDERR)}
        selector = selectors.DefaultSelector()
        pending = memoryview(data or b"")
        completed = False
        try:
            for stream in streams:
                os.set_blocking(stream.fileno(), False)
                selector.register(stream, selectors.EVENT_READ)
            if child.stdin is not None:
                os.set_blocking(child.stdin.fileno(), False)
                if pending:
                    selector.register(child.stdin, selectors.EVENT_WRITE)
                else:
                    child.stdin.close()
            while selector.get_map():
                remaining = deadline - time.monotonic()
                if remaining <= 0:
                    raise TimeoutError("command deadline exceeded: " + argv[0])
                for key, events in selector.select(min(remaining, 1)):
                    stream = key.fileobj
                    if events & selectors.EVENT_WRITE:
                        try:
                            count = os.write(stream.fileno(), pending[:65536])
                        except BrokenPipeError:
                            count = len(pending)
                        pending = pending[count:]
                        if not pending:
                            selector.unregister(stream)
                            stream.close()
                        continue
                    chunk = os.read(stream.fileno(), 65536)
                    if not chunk:
                        selector.unregister(stream)
                        continue
                    output, bound = streams[stream]
                    if len(output) + len(chunk) > bound:
                        raise ValueError("command output exceeds retained limit: " + argv[0])
                    output.extend(chunk)
            code = child.wait(timeout=max(.001, deadline - time.monotonic()))
            if code:
                # Avoid copying credential-bearing gh diagnostics into durable
                # application state. The enclosing runtime owns redaction.
                raise RuntimeError(argv[0] + " exited with status " + str(code))
            completed = True
            return bytes(streams[child.stdout][0])
        finally:
            selector.close()
            if not completed or child.poll() is None:
                try:
                    os.killpg(child.pid, signal.SIGKILL)
                except ProcessLookupError:
                    pass
                child.wait(timeout=5)


def _decode(raw):
    return bounded_json(raw.decode("utf-8", errors="strict"), MAX_OUTPUT)


class GitHub:
    """A gh-backed transport. Credentials are environment references only."""

    def __init__(self, root, host="github.com"):
        self.root = Path(root)
        if not isinstance(host, str) or not HOST.fullmatch(host):
            raise ValueError("invalid GitHub host")
        self.host = host.lower()
        self.last_capture = None
        self.last_ci = None

    def _bind(self, pr):
        pr = _identity(pr)
        self.host = pr["host"]
        return "repos/" + pr["owner"] + "/" + pr["name"]

    def _environment(self):
        if self.host != "github.com" and os.environ.get("GH_TOKEN") and not os.environ.get("GH_ENTERPRISE_TOKEN"):
            return {**os.environ, "GH_ENTERPRISE_TOKEN": os.environ["GH_TOKEN"]}
        return None

    def _request(self, method, path, payload=None, *, raw=False):
        if method not in ("GET", "POST", "PATCH", "PUT", "DELETE"):
            raise ValueError("unsupported GitHub method")
        if (not isinstance(path, str) or not path or path.startswith(("/", "-"))
                or any(c in path for c in ("\0", "\r", "\n", "\\", "#", "%"))
                or urlsplit(path).scheme or "//" in path
                or any(p in (".", "..") for p in path.split("?", 1)[0].split("/"))):
            raise ValueError("relative GitHub API route required")
        args = ["gh", "api", "--hostname", self.host, "--method", method, path]
        data = None
        if payload is not None:
            data = canonical(payload)
            args += ["--input", "-"]
        observed = run(self.root, args, data, env=self._environment())
        if raw:
            return observed.decode("utf-8", errors="strict")
        return _decode(observed) if observed.strip() else {}

    def request(self, method, path, payload=None):
        return self._request(method, path, payload)

    def pages(self, path, *, key=None):
        parsed = urlsplit(path)
        query = dict(parse_qsl(parsed.query, keep_blank_values=True))
        if "page" in query or query.get("per_page", "100") != "100":
            raise ValueError("pagination is owned by the transport")
        query.pop("per_page", None)
        rows = []
        seen = set()
        total = None
        for page in range(1, MAX_PAGES + 1):
            route = parsed.path + "?" + urlencode(dict(query, per_page=100, page=page))
            value = self.request("GET", route)
            batch = value if key is None else value.get(key)
            if not isinstance(batch, list) or any(not isinstance(v, dict) for v in batch):
                raise ValueError("malformed GitHub pagination response")
            if key is not None and "total_count" in value:
                count = value["total_count"]
                if isinstance(count, bool) or not isinstance(count, int) or count < 0:
                    raise ValueError("invalid GitHub pagination count")
                if total is not None and total != count:
                    raise ValueError("GitHub collection changed during pagination")
                total = count
            for row in batch:
                key_value = row.get("id")
                if key_value is not None:
                    if key_value in seen:
                        raise ValueError("duplicate GitHub row across pages")
                    seen.add(key_value)
            rows.extend(batch)
            if len(batch) < 100:
                if total is not None and len(rows) != total:
                    raise ValueError("incomplete GitHub collection")
                return rows
        raise ValueError("GitHub collection exceeds bounded pagination")

    def capture(self, pr):
        prefix = self._bind(pr)
        route = prefix + "/pulls/" + str(pr["number"])
        metadata = self.request("GET", route)
        if (identity(metadata.get("html_url")) != pr
                or metadata.get("number") != pr["number"]):
            raise ValueError("GitHub returned another PR")
        viewer = self.request("GET", "user")
        _positive(viewer.get("id"), "viewer ID")
        _text(viewer.get("login"), "viewer login")
        result = {"metadata": metadata, "viewer": {"id": viewer["id"],
                                                     "login": viewer["login"]},
                  "comments": self.pages(prefix + "/issues/" + str(pr["number"]) + "/comments"),
                  "reviews": self.pages(route + "/reviews"),
                  "inline": self.pages(route + "/comments"), "threads": []}
        cursor = None
        seen = set()
        cursors = set()
        first_total = None
        query = ("query($owner:String!,$name:String!,$number:Int!,$cursor:String){"
                 "repository(owner:$owner,name:$name){pullRequest(number:$number){"
                 "reviewThreads(first:100,after:$cursor){totalCount "
                 "nodes{id isResolved isOutdated comments(first:1){nodes{id databaseId}}} "
                 "pageInfo{hasNextPage endCursor}}}}}")
        for _ in range(MAX_PAGES):
            response = self.request("POST", "graphql", {"query": query, "variables": {
                "owner": pr["owner"], "name": pr["name"], "number": pr["number"],
                "cursor": cursor}})
            if response.get("errors"):
                raise ValueError("complete review threads unavailable")
            try:
                threads = response["data"]["repository"]["pullRequest"]["reviewThreads"]
                nodes, info, total = threads["nodes"], threads["pageInfo"], threads["totalCount"]
                if (not isinstance(nodes, list) or not isinstance(total, int)
                        or isinstance(total, bool) or total < 0):
                    raise ValueError("malformed review thread enumeration")
                if first_total is not None and total != first_total:
                    raise ValueError("review thread collection changed during pagination")
                first_total = total
                for node in nodes:
                    key = _text(node.get("id"), "review thread ID")
                    if key in seen:
                        raise ValueError("duplicate review thread")
                    seen.add(key)
                result["threads"].extend(nodes)
                if info["hasNextPage"] is False:
                    if len(result["threads"]) != total:
                        raise ValueError("incomplete review thread enumeration")
                    break
                if info["hasNextPage"] is not True:
                    raise ValueError("malformed review thread page status")
                next_cursor = _text(info["endCursor"], "review thread cursor")
                if next_cursor in cursors:
                    raise ValueError("review thread cursor did not advance")
                cursors.add(next_cursor)
                cursor = next_cursor
            except (KeyError, TypeError) as error:
                raise ValueError("complete review thread enumeration unavailable") from error
        else:
            raise ValueError("review threads exceed bounded pagination")
        after = self.request("GET", route)
        before_pin = [metadata.get(k) for k in ("head", "base", "updated_at", "state")]
        after_pin = [after.get(k) for k in ("head", "base", "updated_at", "state")]
        if before_pin != after_pin:
            raise ValueError("PR changed during capture; retry this read")
        self.last_capture = result
        return result

    def observe_ci(self, pr, head):
        if not isinstance(head, str) or not SHA.fullmatch(head):
            raise ValueError("CI requires an immutable head")
        prefix = self._bind(pr)
        commit = prefix + "/commits/" + head
        checks = self.pages(commit + "/check-runs?filter=latest", key="check_runs")
        statuses = self.pages(commit + "/statuses")
        observed = []
        for check in checks:
            if check.get("head_sha") != head:
                raise ValueError("CI check belongs to another head")
            check_id = str(_positive(check.get("id"), "check ID"))
            row = {"id": check_id, "name": _text(check.get("name"), "check name"),
                   "status": check.get("status"), "conclusion": check.get("conclusion"),
                   "url": check.get("html_url") or "", "run_id": "", "job_id": "", "attempt": 0}
            link = urlsplit(check.get("details_url") or row["url"])
            parts = link.path.strip("/").split("/")
            if (link.scheme == "https" and link.netloc.lower() == pr["host"]
                    and len(parts) == 7 and [p.lower() for p in parts[:2]] == [pr["owner"], pr["name"]]
                    and parts[2:4] == ["actions", "runs"] and parts[4].isdigit()
                    and parts[5] == "job" and parts[6].isdigit()):
                job = self.request("GET", prefix + "/actions/jobs/" + parts[6])
                if (job.get("head_sha") != head or str(job.get("run_id")) != parts[4]
                        or str(job.get("id")) != parts[6]
                        or not str(job.get("check_run_url", "")).endswith("/check-runs/" + check_id)):
                    raise ValueError("Actions job does not match exact check/head")
                row.update(run_id=parts[4], job_id=parts[6],
                           attempt=_positive(job.get("run_attempt"), "Actions attempt"))
            observed.append(row)
        contexts = set()
        for status in statuses:
            name = _text(status.get("context"), "commit status context")
            if name in contexts:
                continue  # GitHub returns newest statuses first.
            contexts.add(name)
            state = status.get("state")
            observed.append({"id": "status:" + str(_positive(status.get("id"), "status ID")),
                             "name": name, "status": "pending" if state == "pending" else "completed",
                             "conclusion": state, "url": status.get("target_url") or "",
                             "run_id": "", "job_id": "", "attempt": 0})
        # Freshness includes the PR head itself, not merely a successful response
        # from the old commit's CI endpoint.
        metadata = self.request("GET", prefix + "/pulls/" + str(pr["number"]))
        if metadata.get("head", {}).get("sha") != head:
            raise ValueError("PR head changed while observing CI")
        result = {"head": head, "checks": sorted(observed, key=lambda c: c["id"]),
                  "evidence": digest({"head": head, "check_runs": checks, "statuses": statuses})}
        self.last_ci = {"observation": result, "check_runs": checks, "statuses": statuses}
        return result

    def failure_logs(self, pr, run_id, job_id, attempt):
        self._bind(pr)
        if (not str(run_id).isdigit() or not str(job_id).isdigit()
                or isinstance(attempt, bool) or not isinstance(attempt, int) or attempt < 1):
            raise ValueError("exact failed run/job/attempt required")
        return run(self.root, ["gh", "run", "view", str(run_id), "--job", str(job_id),
                               "--attempt", str(attempt), "--log-failed", "--repo",
                               pr["repository"]], env=self._environment()).decode("utf-8", errors="strict")


def _git(root, *args, raw=False):
    value = run(root, ["git", "-c", "core.quotePath=false", *args])
    return value if raw else value.decode("utf-8", errors="strict").strip()


def _owned_exclusions(root, plan):
    """Only omit state/generation directories carrying this application's marker."""
    exclusions = []
    for name in (STATE, GENERATED):
        path = root / name
        if not path.exists():
            continue
        marker = path / ".owned-by-portable-pr-fix"
        if (path.is_symlink() or not path.is_dir() or marker.is_symlink()
                or not marker.is_file() or marker.stat().st_size > 1024
                or bounded_json(marker.read_bytes()) != {"format": FORMAT}):
            raise ValueError("application directory lacks ownership marker: " + name)
        if _git(root, "ls-files", "-z", "--", name, raw=True):
            raise ValueError("application state namespace contains tracked files: " + name)
        exclusions.append(":(exclude)" + name)
    return exclusions


def _untracked(root, names):
    entries = {}
    remaining = MAX_FILE_BYTES
    for raw in names.split(b"\0"):
        if not raw:
            continue
        name = raw.decode("utf-8", errors="strict")
        if len(entries) >= MAX_FILES:
            raise ValueError("untracked snapshot exceeds 10000 files")
        path = root / name
        info = path.lstat()
        if stat.S_ISLNK(info.st_mode):
            value = os.readlink(path).encode("utf-8", errors="strict")
            remaining -= len(value)
            hashed = hashlib.sha256(value).hexdigest()
        elif stat.S_ISREG(info.st_mode):
            if info.st_size > remaining:
                raise ValueError("untracked snapshot exceeds 64 MiB")
            flags = os.O_RDONLY | getattr(os, "O_NOFOLLOW", 0) | getattr(os, "O_NONBLOCK", 0)
            fd = os.open(path, flags)
            with os.fdopen(fd, "rb") as stream:
                opened = os.fstat(stream.fileno())
                if (not stat.S_ISREG(opened.st_mode) or opened.st_dev != info.st_dev
                        or opened.st_ino != info.st_ino):
                    raise ValueError("untracked file changed before capture")
                hashed_value = hashlib.sha256()
                read = 0
                while True:
                    chunk = stream.read(min(65536, remaining + 1))
                    if not chunk:
                        break
                    read += len(chunk)
                    remaining -= len(chunk)
                    if remaining < 0:
                        raise ValueError("untracked snapshot exceeds 64 MiB")
                    hashed_value.update(chunk)
                final = os.fstat(stream.fileno())
                if (read != opened.st_size or final.st_size != opened.st_size
                        or final.st_mtime_ns != opened.st_mtime_ns):
                    raise ValueError("untracked file changed during capture")
                hashed = hashed_value.hexdigest()
        else:
            raise ValueError("untracked snapshot requires regular files or symlinks")
        if remaining < 0:
            raise ValueError("untracked snapshot exceeds 64 MiB")
        entries[name] = {"sha256": hashed, "mode": stat.S_IMODE(info.st_mode),
                         "symlink": stat.S_ISLNK(info.st_mode)}
    return entries


def capture(root, provider, plan, *, clean=False):
    root = Path(root).resolve(strict=True)
    pr = _identity(plan["pr"])
    observed = provider.capture(pr)
    metadata, viewer = observed["metadata"], observed["viewer"]
    head, target = metadata["head"]["sha"], metadata["base"]["sha"]
    if not SHA.fullmatch(head) or not SHA.fullmatch(target):
        raise ValueError("complete immutable PR and target heads required")
    if metadata.get("state") != "open" or metadata.get("merged"):
        raise ValueError("an open PR is required")
    author = metadata["user"]
    _positive(author.get("id"), "author ID")
    _positive(viewer.get("id"), "viewer ID")
    _text(author.get("login"), "author login")
    _text(viewer.get("login"), "viewer login")
    accounts = plan.get("options", {}).get("accounts", [])
    owned = author["id"] == viewer["id"] or any(
        a.get("host", "").lower() == pr["host"] and a.get("id") == author["id"] for a in accounts)
    if not owned:
        authority = plan.get("maintain_other") or plan.get("authorizations", {}).get("maintain_other")
        if not isinstance(authority, dict) or any(not authority.get(k) for k in ("request_id", "instruction", "source")):
            raise ValueError("maintaining another author's PR requires explicit authority")
    for ref in (metadata["head"]["ref"], metadata["base"]["ref"]):
        _git(root, "check-ref-format", "refs/heads/" + ref)
    head_repository = metadata["head"].get("repo", {}).get("full_name")
    if (not isinstance(head_repository, str) or len(head_repository.split("/")) != 2
            or any(not REPO_PART.fullmatch(p) or p in (".", "..") for p in head_repository.split("/"))):
        raise ValueError("complete head repository identity required")
    local_head = _git(root, "rev-parse", "--verify", "HEAD^{commit}")
    if local_head != head:
        # The application must have retained the exact authorized local commit;
        # a caller cannot merely set an unrestricted boolean to bypass this.
        if plan.get("allow_local_head") != local_head:
            raise ValueError("local HEAD differs from PR head without retained local commit")
        _git(root, "merge-base", "--is-ancestor", head, local_head)
    base = _git(root, "merge-base", target, local_head)
    trees = {name: _git(root, "rev-parse", "--verify", pin + "^{tree}")
             for name, pin in (("target_tree", target), ("base_tree", base), ("head_tree", head),
                               ("local_tree", local_head))}
    exclusions = _owned_exclusions(root, plan)
    status = _git(root, "status", "--porcelain=v1", "--untracked-files=all", "-z", "--", ".", *exclusions, raw=True)
    if clean and status:
        raise ValueError("initial PR capture requires a clean worktree")
    diff = _git(root, "diff", "--binary", "--no-ext-diff", "--no-renames", base,
                "--", ".", *exclusions, raw=True)
    tracked_paths = _git(root, "diff", "--name-only", "--no-renames", "-z", base,
                         "--", ".", *exclusions, raw=True)
    names = _git(root, "ls-files", "--others", "--exclude-standard", "-z",
                 "--", ".", *exclusions, raw=True)
    untracked = _untracked(root, names)
    history = _git(root, "log", "--format=raw", target + ".." + local_head, raw=True)
    paths = sorted(set([p.decode("utf-8", errors="strict") for p in tracked_paths.split(b"\0") if p] + list(untracked)))
    instruction_files = {}
    ancestors = {root}
    for changed in paths:
        ancestors.update((root / changed).parents)
    for directory in sorted(ancestors):
        if not directory.is_relative_to(root):
            continue
        if any(parent.is_symlink() for parent in (directory, *directory.parents)
               if parent.is_relative_to(root)):
            raise ValueError("repository instruction path traverses a symlink")
        path = directory / "AGENTS.md"
        if path.is_symlink():
            raise ValueError("repository instruction file must not be a symlink")
        if path.is_file():
            rel = path.relative_to(root).as_posix()
            if path.stat().st_size > 1 << 20:
                raise ValueError("repository instruction file exceeds 1 MiB")
            instruction_files[rel] = hashlib.sha256(path.read_bytes()).hexdigest()
    options = {k: v for k, v in plan.get("options", {}).items()
               if k not in ("tasks", "finalizers", "reviewers", "limits")}
    discussion = {"title": metadata.get("title"), "body": metadata.get("body"),
                  **{k: observed[k] for k in ("comments", "reviews", "inline", "threads")}}
    result = {"pr": pr, "head": head, "target": target, "base": base,
              "local_head": local_head, **trees, "author": author["login"],
              "author_id": author["id"], "viewer": viewer["login"], "actor_id": viewer["id"],
              "head_repository": pr["host"] + "/" + head_repository.lower(),
              "head_branch": metadata["head"]["ref"], "target_branch": metadata["base"]["ref"],
              "open": True, "draft": bool(metadata.get("draft")), "paths": paths,
              "diff": digest({"tracked": hashlib.sha256(diff).hexdigest(), "untracked": untracked}),
              "history": hashlib.sha256(history).hexdigest(), "discussion": digest(discussion),
              "instructions": digest({"request": plan["instructions"], "repository": instruction_files}),
              "settings": digest({"options": options, "settings": plan.get("settings", {})}),
              "resources": digest(plan.get("resources", {})),
              "definitions": digest(plan.get("definitions", {})),
              "resource_digests": _decode(canonical(plan.get("resources", {}))),
              "definition_digests": _decode(canonical(plan.get("definitions", {}))),
              "status_fingerprint": hashlib.sha256(status).hexdigest(),
              "dirty": bool(status),
              "evidence": digest({"pr": observed, "status": status.hex(), "untracked": untracked,
                                  "diff": hashlib.sha256(diff).hexdigest(), "history": hashlib.sha256(history).hexdigest()})}
    if status:
        result["local_tree"] = digest({"head_tree": trees["local_tree"], "diff": result["diff"],
                                      "status": status.hex()})
    return result


def fingerprint(snapshot, task):
    keys = ("pr", "author_id", "actor_id", "head_repository", "head_branch", "target_branch",
            "target_tree", "base_tree", "local_tree", "diff", "discussion", "instructions",
            "settings")
    selected = {k: snapshot[k] for k in keys}
    resources = task.get("resources", [])
    if any(path not in snapshot.get("resource_digests", {}) for path in resources):
        raise ValueError("task resource is absent from captured resource identities")
    selected["resources"] = {path: snapshot.get("resource_digests", {}).get(path)
                             for path in resources}
    selected["definition"] = task.get("definition_digest") or snapshot.get("definition_digests", {}).get(task.get("definition"))
    if task.get("history"):
        selected["history"] = snapshot["history"]
    if task.get("head_sensitive") or task.get("head-sensitive"):
        selected.update({k: snapshot[k] for k in ("head", "target", "local_head")})
    return digest({"snapshot": selected, "task": task})
