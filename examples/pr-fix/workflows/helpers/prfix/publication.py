"""One guarded publication operation at a time; YAML owns all orchestration.

The intent is committed before dispatch. A failed acknowledgement leaves its
state uncertain. Reconciliation only observes the service and never changes the
ledger; only an execute invocation may commit the resulting receipt or absence
proof. Creation absence is deliberately never a proof that a write did not run.
"""
from __future__ import annotations

import copy
import json
import re
from pathlib import Path
from typing import Callable

from .core import auth, canonical, digest, text


class PublicationError(RuntimeError):
    pass


class UncertainEffect(PublicationError):
    pass


def repository_merge_requirements(provider, pr, metadata, head):
    if metadata.get("mergeable") is not True or metadata.get("mergeable_state") != "clean" or metadata.get("draft"):
        raise PublicationError("repository merge requirements are not currently satisfied")
    query = "query($owner:String!,$name:String!,$number:Int!){repository(owner:$owner,name:$name){pullRequest(number:$number){headRefOid isDraft mergeable mergeStateStatus reviewDecision}}}"
    response = provider.request("POST", "graphql", {"query": query, "variables": {
        "owner": pr["owner"], "name": pr["name"], "number": pr["number"]}})
    if response.get("errors"):
        raise PublicationError("repository merge requirements unavailable")
    actual = response.get("data", {}).get("repository", {}).get("pullRequest", {})
    if actual.get("headRefOid") != head or actual.get("isDraft") is not False or actual.get("mergeable") != "MERGEABLE" or actual.get("mergeStateStatus") != "CLEAN" or actual.get("reviewDecision") == "CHANGES_REQUESTED":
        raise PublicationError("repository merge requirements changed or remain unsatisfied")
    return actual


KINDS = {
    "push", "rewrite", "edit_pr", "reply", "reply_inline", "resolve_thread",
    "split_push", "split_pr", "request_review", "ci_rerun", "merge",
}
PERMISSIONS = {
    "push": "push_commits", "rewrite": "rewrite_commits",
    "edit_pr": "edit_pr", "reply": "reply_to_feedback",
    "reply_inline": "reply_to_feedback", "resolve_thread": "resolve_threads",
    "split_push": "create_draft_prs", "split_pr": "create_draft_prs",
    "request_review": "request_review", "ci_rerun": "push_commits",
}
_SHA = re.compile(r"^[0-9a-f]{40}(?:[0-9a-f]{24})?$")
_ID = re.compile(r"^[A-Za-z0-9_.:-]{1,128}$")
_BRANCH = re.compile(r"^[A-Za-z0-9_][A-Za-z0-9_./-]{0,240}$")


def _sha(value, label):
    if not isinstance(value, str) or not _SHA.fullmatch(value):
        raise PublicationError(f"{label} requires a full lowercase object ID")
    return value


def _positive(value, label):
    if isinstance(value, bool) or not isinstance(value, int) or value <= 0:
        raise PublicationError(f"{label} requires a positive integer")
    return value


def _strings(value, label):
    if not isinstance(value, list) or not value or any(
        not isinstance(v, str) or not v.strip() or "\x00" in v for v in value
    ):
        raise PublicationError(f"{label} requires nonempty text references")
    return value


def _branch(value):
    if not isinstance(value, str) or not _BRANCH.fullmatch(value):
        raise PublicationError("invalid exact branch")
    if ".." in value or "//" in value or "@{" in value or value.endswith(("/", ".", ".lock")):
        raise PublicationError("invalid exact branch")
    return value


def _json(raw):
    return json.loads(raw) if isinstance(raw, (str, bytes)) else raw


def _row(row):
    if row is None:
        raise PublicationError("unknown publication intent")
    return {"id": row[0], "fingerprint": row[1], "intent": _json(row[2]),
            "state": row[3], "receipt": _json(row[4]) if row[4] else None}


def _discussion(ev):
    return digest({key: ev.get(key, []) for key in ("comments", "reviews", "inline", "threads")})


def original_discussion(ledger, plan, discussion):
    """Remove only exact, reconciled effects created by this logical run."""
    result = copy.deepcopy(discussion)
    for row in ledger.db.execute("SELECT id,fingerprint,value,state,receipt FROM publication_intents WHERE state='completed' ORDER BY rowid DESC"):
        saved = _row(row)
        intent, receipt = saved["intent"], saved["receipt"]
        if (saved["fingerprint"] != digest(intent) or intent.get("run") != plan["logical_run"]
                or intent.get("pr") != plan["pr"] or not isinstance(receipt, dict)
                or receipt.get("id") != saved["id"] or receipt.get("fingerprint") != saved["fingerprint"]
                or receipt.get("actor_id") != intent.get("actor_id")):
            raise PublicationError("retained discussion effect identity differs")
        kind, payload, observed = intent["kind"], intent["payload"], receipt.get("result")
        if kind in {"reply", "reply_inline"} and not intent["before"].get("satisfied_comment_id"):
            key = "comments" if kind == "reply" else "inline"
            if (isinstance(observed, dict) and observed.get("body") == payload["body"]
                    and observed.get("user", {}).get("id") == intent["actor_id"]):
                result[key] = [item for item in result[key] if item != observed]
        elif kind == "edit_pr" and observed == payload and all(result.get(key) == value for key, value in payload.items()):
            result.update(intent["before"])
        elif kind == "resolve_thread" and isinstance(observed, dict):
            before = intent["before"].get("thread")
            if before is not None:
                result["threads"] = [copy.deepcopy(before) if thread == observed else thread for thread in result["threads"]]
    return result


class Publication:
    """Application-owned publication policy with a narrow fakeable transport.

    ``readiness`` must recapture accepted application review/verification and CI
    for the supplied exact intent. No callback means merge is unavailable.
    ``source_validator`` validates original accepted report references, including
    their current artifact bytes. Python never reads the engine database.
    """

    def __init__(self, ledger, provider, root: Path, plan: dict,
                 readiness: Callable | None = None, source_validator: Callable | None = None):
        self.ledger = ledger
        self.provider = provider
        self.root = Path(root)
        self.plan = plan
        self.readiness = readiness
        self.source_validator = source_validator
        self.pr = plan["pr"]
        self.repo = f"repos/{self.pr['owner']}/{self.pr['name']}"
        self.pr_path = f"{self.repo}/pulls/{self.pr['number']}"
        self.issue_path = f"{self.repo}/issues/{self.pr['number']}"

    def _request(self, method, path, payload=None):
        return self.provider.request(method, path, payload)

    def _capture(self):
        ev = self.provider.capture(self.pr)
        meta = ev.get("metadata", {})
        if meta.get("html_url", "").lower() != self.pr["url"].lower() or meta.get("number") != self.pr["number"]:
            raise PublicationError("provider returned a different PR")
        viewer = ev.get("viewer", {})
        if not isinstance(viewer, dict):
            viewer = {"login": viewer, "id": ev.get("viewer_id")}
            ev = dict(ev, viewer=viewer)
        _positive(viewer.get("id"), "authenticated actor")
        text(viewer.get("login"), "authenticated actor login")
        return ev

    def _authorization(self, kind):
        values = self.plan.get("authorizations", {})
        value = values.get(kind)
        if value is None and kind != "merge":
            value = values.get("iterate")
        if value is None:
            raise PublicationError(f"explicit user authorization required for {kind}")
        auth(value)
        return value

    def _permission(self, kind):
        if kind == "merge":
            return
        permissions = self.plan.get("options", {}).get("permissions", {})
        if permissions.get(PERMISSIONS[kind]) is not True:
            raise PublicationError(f"configured permission forbids {kind}")

    def _load(self, ident):
        if not isinstance(ident, str) or not _ID.fullmatch(ident):
            raise PublicationError("invalid publication operation ID")
        row = self.ledger.db.execute(
            "SELECT id,fingerprint,value,state,receipt FROM publication_intents WHERE id=?", (ident,)
        ).fetchone()
        if row is None:
            alias = self.ledger.db.execute(
                "SELECT canonical_id FROM publication_aliases WHERE id=?", (ident,)
            ).fetchone()
            if alias:
                row = self.ledger.db.execute(
                    "SELECT id,fingerprint,value,state,receipt FROM publication_intents WHERE id=?", (alias[0],)
                ).fetchone()
        value = _row(row)
        if value["fingerprint"] != digest(value["intent"]):
            raise PublicationError("retained publication intent changed")
        if value["intent"].get("run") != self.plan["logical_run"] or value["intent"].get("pr") != self.pr:
            raise PublicationError("publication intent belongs to another run or PR")
        return value

    def _live_binding(self, intent, ev, *, discussion=True):
        meta = ev["metadata"]
        if ev["viewer"]["id"] != intent["actor_id"]:
            raise PublicationError("authenticated actor changed")
        if meta.get("state") != "open" or meta.get("merged") or meta.get("draft") and intent["kind"] == "merge":
            raise PublicationError("publication requires the expected open PR")
        head, base = meta.get("head", {}), meta.get("base", {})
        expected = (intent["lease"], intent["target"], intent["branch"], intent["head_repository"])
        actual = (head.get("sha"), base.get("sha"), head.get("ref"),
                  str(head.get("repo", {}).get("full_name", "")).lower())
        if actual != expected:
            raise PublicationError("PR head, target, branch or repository lease changed")
        if discussion and _discussion(ev) != intent["discussion"]:
            raise PublicationError("incoming discussion changed after preparation")
        if intent["kind"] == "reply_inline":
            destination = intent["payload"]["comment_id"]
            if sum(1 for c in ev.get("inline", []) if c.get("id") == destination) != 1:
                raise PublicationError("inline destination is not present in the exact PR")
        source = intent.get("source")
        if source:
            kind = source.get("kind")
            key = {"comment": "comments", "inline": "inline", "review": "reviews"}.get(kind)
            if key is None:
                raise PublicationError("invalid original feedback kind")
            matches = [c for c in ev.get(key, []) if c.get("id") == source.get("id")]
            if len(matches) != 1 or digest(matches[0]) != source.get("fingerprint"):
                raise PublicationError("original feedback changed or disappeared")
            if intent["kind"] == "reply_inline" and source["id"] != intent["payload"]["comment_id"]:
                raise PublicationError("inline destination differs from original feedback")

    def _payload(self, kind, payload):
        if not isinstance(payload, dict):
            raise PublicationError("publication payload requires an object")
        exact = {
            "push": {"new_head"}, "rewrite": {"new_head"},
            "reply": {"body"}, "reply_inline": {"body", "comment_id"},
            "resolve_thread": {"thread_id", "reply_operation"},
            "ci_rerun": {"run_id", "attempt", "reason"},
            "merge": {"merge_method"},
            "split_push": {"branch", "new_head", "paths", "reason"},
            "split_pr": {"title", "body", "head", "base", "draft", "push_operation"},
        }
        if kind in exact:
            expected = exact[kind]
            if not (set(payload) == expected or kind == "split_pr" and set(payload) == expected | {"head_repo"}):
                raise PublicationError(f"{kind} requires exactly {sorted(expected)}")
        if kind == "edit_pr" and (not payload or not set(payload) <= {"title", "body"}):
            raise PublicationError("PR edits accept title/body only")
        if kind == "request_review" and (not payload or not set(payload) <= {"reviewers", "team_reviewers"}):
            raise PublicationError("review request requires explicit user/team lists")
        for key in ("body", "title", "reason"):
            if key in payload:
                if kind == "edit_pr" and key == "body":
                    if not isinstance(payload[key], str) or "\x00" in payload[key]:
                        raise PublicationError("PR body must be text without NUL")
                    payload[key].encode("utf-8", "strict")
                else:
                    text(payload[key], key)
        if kind in ("push", "rewrite", "split_push"):
            _sha(payload["new_head"], "published head")
        if kind == "reply_inline":
            _positive(payload["comment_id"], "inline comment ID")
        if kind == "resolve_thread":
            text(payload["thread_id"], "thread ID")
            text(payload["reply_operation"], "answered inline operation")
        if kind == "request_review":
            if not any(payload.values()):
                raise PublicationError("empty reviewer selection")
            for key, values in payload.items():
                _strings(values, key)
                if len(set(values)) != len(values) or any(not re.fullmatch(r"[A-Za-z0-9_-]+", v) for v in values):
                    raise PublicationError("invalid or duplicate reviewer name")
        if kind == "ci_rerun":
            _positive(payload["run_id"], "Actions run ID")
            _positive(payload["attempt"], "Actions attempt")
        if kind == "merge" and payload["merge_method"] not in ("merge", "squash"):
            raise PublicationError("merge receipt supports merge/squash with exact target parents")
        if kind == "split_push":
            _branch(payload["branch"])
            _strings(payload["paths"], "accepted split paths")
            if any(v.startswith("/") or ".." in v.split("/") or "\x00" in v for v in payload["paths"]):
                raise PublicationError("split paths must stay in the checkout")
        if kind == "split_pr":
            if payload["draft"] is not True:
                raise PublicationError("split PR must be a draft")
            _branch(payload["base"])
            if not re.fullmatch(r"[A-Za-z0-9_-]+:[A-Za-z0-9_][A-Za-z0-9_./-]*", payload["head"]):
                raise PublicationError("split PR head requires exact owner:branch")
            _branch(payload["head"].split(":", 1)[1])
            text(payload["push_operation"], "reconciled split push")
            if "head_repo" in payload and not re.fullmatch(r"[A-Za-z0-9_.-]+", payload["head_repo"]):
                raise PublicationError("invalid exact split head repository")
        return copy.deepcopy(payload)

    def prepare(self, proposal):
        if not isinstance(proposal, dict):
            raise PublicationError("publication proposal requires an object")
        unknown = set(proposal) - {"id", "kind", "head", "lease", "target", "payload", "sources", "source"}
        if unknown:
            raise PublicationError(f"unsupported publication proposal fields: {sorted(unknown)}")
        kind = proposal.get("kind")
        if kind not in KINDS:
            raise PublicationError("unsupported publication operation")
        authority = self._authorization(kind)
        self._permission(kind)
        payload = self._payload(kind, proposal.get("payload", {}))
        sources = _strings(proposal.get("sources"), "original report sources")
        if self.source_validator is None:
            raise PublicationError("accepted publication-source validation unavailable")
        for source in sources:
            if self.source_validator(source) is not True:
                raise PublicationError("publication source is not current accepted evidence")
        head = _sha(proposal.get("head"), "intent head")
        lease = _sha(proposal.get("lease", head), "head lease")
        target = _sha(proposal.get("target"), "target")
        semantic = {"kind": kind, "head": head, "lease": lease, "target": target,
                    "payload": payload, "sources": list(sources), "source": proposal.get("source")}
        requested_id = proposal.get("id")
        if requested_id is not None:
            if not isinstance(requested_id, str) or not _ID.fullmatch(requested_id):
                raise PublicationError("invalid publication operation ID")
            # A retained effect is immutable even after a push or merge changes
            # the live head. Return its identity; execute/checkpoint validation
            # verifies the effect without preparing a replacement publication.
            old = self.ledger.db.execute(
                "SELECT id,fingerprint,value,state,receipt FROM publication_intents WHERE id=?", (requested_id,)
            ).fetchone()
            alias = self.ledger.db.execute(
                "SELECT canonical_id,fingerprint FROM publication_aliases WHERE id=?", (requested_id,)
            ).fetchone()
            if old:
                saved = self._load(requested_id)
                original = saved["intent"]
                expected = {key: original.get(key) for key in semantic}
                if expected != semantic or original["authorization"] != authority:
                    raise PublicationError("conflicting publication operation identity")
                return self._public(saved)
            if alias:
                if alias[1] != digest(semantic):
                    raise PublicationError("conflicting publication operation identity")
                saved = self._load(alias[0])
                if saved["intent"]["authorization"] != authority:
                    raise PublicationError("retained publication authority changed")
                return self._public(saved)
        ev = self._capture()
        meta = ev["metadata"]
        intent = {
            "run": self.plan["logical_run"], "pr": self.pr, "kind": kind,
            "head": head, "lease": lease, "target": target,
            "branch": _branch(meta.get("head", {}).get("ref")),
            "head_repository": str(meta.get("head", {}).get("repo", {}).get("full_name", "")).lower(),
            "actor_id": ev["viewer"]["id"], "payload": payload,
            "authorization": authority, "sources": list(sources),
            "discussion": _discussion(ev), "before": {},
        }
        if proposal.get("source") is not None:
            intent["source"] = copy.deepcopy(proposal["source"])
        if kind not in ("push", "rewrite", "split_push") and head != lease:
            raise PublicationError("non-Git publication requires the current published head")
        self._live_binding(intent, ev)
        if kind in ("push", "rewrite") and payload["new_head"] != head:
            raise PublicationError("push intent head must equal its exact desired head")
        if kind in ("reply", "reply_inline"):
            key = "inline" if kind == "reply_inline" else "comments"
            intent["before"]["comment_ids"] = [_positive(c.get("id"), "comment ID") for c in ev.get(key, [])]
            matches = self._matching_replies(intent, ev)
            if len(matches) > 1:
                raise PublicationError("multiple identical replies require explicit recovery")
            if matches:
                intent["before"]["satisfied_comment_id"] = matches[0]["id"]
        elif kind == "edit_pr":
            intent["before"] = {key: meta.get(key) for key in payload}
        elif kind == "resolve_thread":
            threads = [thread for thread in ev.get("threads", []) if thread.get("id") == payload["thread_id"]]
            if len(threads) == 1:
                intent["before"]["thread"] = copy.deepcopy(threads[0])
        elif kind == "split_pr":
            pushed = self._load(payload["push_operation"])
            if pushed["state"] != "completed" or pushed["intent"]["kind"] != "split_push" or not self.validate(pushed["id"], pushed["receipt"]):
                raise PublicationError("split PR requires its current reconciled branch publication")
            split = pushed["intent"]
            owner = split["head_repository"].split("/", 1)[0]
            if payload["head"] != owner + ":" + split["payload"]["branch"]:
                raise PublicationError("split PR head differs from its reconciled push")
            head_name = split["head_repository"].split("/", 1)[1]
            if "head_repo" in payload and payload["head_repo"] != head_name:
                raise PublicationError("split head_repo differs from its pinned source repository")
            if owner == self.pr["owner"] and head_name != self.pr["name"] and payload.get("head_repo") != head_name:
                raise PublicationError("same-owner fork split requires exact head_repo")
            if payload["base"] != meta.get("base", {}).get("ref"):
                raise PublicationError("split PR target branch differs")
            intent["created_head"] = split["payload"]["new_head"]
            prs = self._pages(f"{self.repo}/pulls?state=all&per_page=100")
            intent["before"]["pr_numbers"] = [_positive(v.get("number"), "PR baseline number") for v in prs]
            if self.pr["number"] not in intent["before"]["pr_numbers"]:
                raise PublicationError("split PR baseline omitted the source PR")
        ident = proposal.get("id") or digest(intent)
        if not isinstance(ident, str) or not _ID.fullmatch(ident):
            raise PublicationError("invalid publication operation ID")
        fingerprint = digest(intent)
        with self.ledger.transaction():
            existing = self.ledger.db.execute(
                "SELECT id,fingerprint,value,state,receipt FROM publication_intents WHERE id=?", (ident,)
            ).fetchone()
            alias = self.ledger.db.execute(
                "SELECT canonical_id,fingerprint FROM publication_aliases WHERE id=?", (ident,)
            ).fetchone()
            if existing:
                saved = _row(existing)
                # Replaying an already prepared operation uses its original
                # baseline. No observation is allowed to rewrite that intent.
                self._assert_replay(saved["intent"], intent)
                return self._public(saved)
            if alias:
                saved = self._load(alias[0])
                self._assert_replay(saved["intent"], intent)
                return self._public(saved)
            if kind in ("reply", "reply_inline"):
                rows = self.ledger.db.execute(
                    "SELECT id,fingerprint,value,state,receipt FROM publication_intents ORDER BY rowid"
                ).fetchall()
                key = self._reply_key(intent)
                for row in rows:
                    saved = _row(row)
                    if saved["intent"].get("kind") in ("reply", "reply_inline") and self._reply_key(saved["intent"]) == key:
                        self.ledger.db.execute(
                            "INSERT INTO publication_aliases(id,canonical_id,fingerprint) VALUES(?,?,?)",
                            (ident, saved["id"], digest(semantic)),
                        )
                        return self._public(saved)
            self.ledger.db.execute(
                "INSERT INTO publication_intents(id,fingerprint,value,state,receipt) VALUES(?,?,?,'prepared','')",
                (ident, fingerprint, canonical(intent).decode()),
            )
        return {"id": ident, "fingerprint": fingerprint, "intent": intent, "state": "prepared"}

    @staticmethod
    def _assert_replay(saved, current):
        left, right = copy.deepcopy(saved), copy.deepcopy(current)
        for value in (left, right):
            value.pop("discussion", None)
            value.pop("before", None)
        if left != right:
            raise PublicationError("conflicting publication operation identity")

    @staticmethod
    def _public(value):
        return {key: value[key] for key in ("id", "fingerprint", "intent", "state")}

    @staticmethod
    def _reply_key(intent):
        return digest({key: intent.get(key) for key in ("run", "pr", "kind", "actor_id", "payload", "authorization", "source")})

    def _matching_replies(self, intent, ev):
        key = "inline" if intent["kind"] == "reply_inline" else "comments"
        payload = intent["payload"]
        return [c for c in ev.get(key, [])
                if isinstance(c.get("id"), int) and not isinstance(c.get("id"), bool)
                and c["id"] > 0 and c.get("body") == payload["body"]
                and c.get("user", {}).get("id") == intent["actor_id"]
                and (intent["kind"] != "reply_inline" or c.get("in_reply_to_id") == payload["comment_id"])]

    def _pages(self, path):
        # The transport owns complete API pagination, not an application loop.
        pages = getattr(self.provider, "pages", None)
        if pages is None:
            raise PublicationError("complete provider pagination is unavailable")
        result = pages(path)
        if not isinstance(result, list):
            raise PublicationError("invalid complete API inventory")
        return result

    def _git(self, *args, input=None):
        from .capture import run
        try:
            output = run(self.root, ["git", "-c", "credential.helper=", "-c",
                "credential.helper=!gh auth git-credential", "-c", "credential.interactive=false", *args], input)
        except RuntimeError as error:
            raise PublicationError("Git publication command failed") from error
        return output.decode("utf-8", "strict").strip()

    def _git_remote(self, intent):
        # Never trust a mutable local remote URL for publication.
        repo = intent["head_repository"]
        if not re.fullmatch(r"[A-Za-z0-9_.-]+/[A-Za-z0-9_.-]+", repo):
            raise PublicationError("invalid pinned head repository")
        return f"https://{self.pr['host']}/{repo}.git"

    def _git_observation(self, intent):
        branch = intent["payload"].get("branch", intent["branch"])
        ref = "refs/heads/" + _branch(branch)
        value = self._git("ls-remote", "--refs", self._git_remote(intent), ref)
        if not value:
            return None
        parts = value.split()
        if len(parts) != 2 or parts[1] != ref:
            raise PublicationError("ambiguous remote branch observation")
        return _sha(parts[0], "observed remote head")

    def _check_git(self, intent):
        kind, payload = intent["kind"], intent["payload"]
        wanted = payload["new_head"]
        if self._git("rev-parse", "--verify", wanted + "^{commit}") != wanted:
            raise PublicationError("exact publication commit unavailable")
        if kind in ("push", "rewrite"):
            if self._git("rev-parse", "HEAD") != wanted:
                raise PublicationError("checked local head changed before push")
            from .capture import _owned_exclusions
            exclusions = _owned_exclusions(self.root, self.plan)
            if self._git("status", "--porcelain=v1", "--untracked-files=all", "--", ".", *exclusions):
                raise PublicationError("publish only the clean accepted local head")
            current = self._git_observation(intent)
            if current != intent["lease"]:
                raise PublicationError("remote branch changed before push")
            if kind == "push":
                # merge-base --is-ancestor returns status one for a rewrite.
                self._git("merge-base", "--is-ancestor", intent["lease"], wanted)
        else:
            if payload["branch"] == intent["branch"] or self._git_observation(intent) is not None:
                raise PublicationError("split publication requires a new branch absence lease")
            changed = self._git("diff", "--name-only", "--no-renames", "-z", intent["target"], wanted)
            paths = [p for p in changed.split("\x00") if p]
            if not set(paths) <= set(payload["paths"]):
                raise PublicationError("split commit contains paths outside its accepted boundary")

    def _check_resolution(self, intent, ev):
        payload = intent["payload"]
        reply = self._load(payload["reply_operation"])
        if reply["state"] != "completed" or reply["intent"]["kind"] != "reply_inline":
            raise PublicationError("thread resolution requires a completed inline answer")
        receipt = reply["receipt"]
        if not receipt or not self._validate_receipt(reply, receipt, ev):
            raise PublicationError("completed inline answer is unavailable")
        threads = [t for t in ev.get("threads", []) if t.get("id") == payload["thread_id"]]
        if len(threads) != 1:
            raise PublicationError("selected review thread unavailable")
        thread = threads[0]
        comments = thread.get("comments", {})
        comments = comments.get("nodes", []) if isinstance(comments, dict) else comments
        root_id = reply["intent"]["payload"]["comment_id"]
        if not any(c.get("databaseId", c.get("id")) == root_id for c in comments):
            raise PublicationError("inline answer belongs to a different thread")
        return thread

    def _merge_gate(self, intent, ev):
        if self.readiness is None:
            raise PublicationError("live application merge-readiness verification unavailable")
        evidence = self.readiness(intent)
        if not isinstance(evidence, dict) or evidence.get("ready") is not True:
            raise PublicationError("selected application work or live PR requirements incomplete")
        if evidence.get("head") != intent["head"] or evidence.get("target") != intent["target"]:
            raise PublicationError("merge-readiness evidence belongs to another head or target")
        if evidence.get("clean_review") is not True or evidence.get("verified") is not True:
            raise PublicationError("current clean review and independent verification required")
        ci = evidence.get("ci", {})
        if ci.get("head") != intent["head"] or ci.get("success") is not True:
            raise PublicationError("merge requires fresh exact-head successful CI")
        _strings(evidence.get("evidence"), "live readiness evidence")
        repository_merge_requirements(self.provider, self.pr, ev["metadata"], intent["head"])

    def _gate(self, intent, ev, audience):
        if self._authorization(intent["kind"]) != intent["authorization"]:
            raise PublicationError("retained user authority changed")
        self._permission(intent["kind"])
        if not isinstance(audience, dict) or audience.get("suitable") is not True or audience.get("fingerprint") != digest(intent):
            raise PublicationError("accepted audience review must name the exact payload fingerprint")
        _strings(audience.get("evidence"), "audience review evidence")
        text(audience.get("audience"), "intended audience")
        if self.source_validator is None:
            raise PublicationError("accepted publication-source validation unavailable")
        for source in intent["sources"] + audience["evidence"]:
            if self.source_validator(source) is not True:
                raise PublicationError("publication evidence is stale or unaccepted")
        self._live_binding(intent, ev)
        kind = intent["kind"]
        if kind in ("push", "rewrite", "split_push"):
            self._check_git(intent)
        elif kind == "resolve_thread":
            self._check_resolution(intent, ev)
        elif kind == "split_pr":
            pushed = self._load(intent["payload"]["push_operation"])
            if pushed["state"] != "completed" or not self.validate(pushed["id"], pushed["receipt"]):
                raise PublicationError("split branch changed before draft PR dispatch")
        elif kind == "ci_rerun":
            payload = intent["payload"]
            run = self._request("GET", f"{self.repo}/actions/runs/{payload['run_id']}")
            if run.get("head_sha") != intent["head"] or run.get("run_attempt") != payload["attempt"] or run.get("status") != "completed" or run.get("conclusion") in ("success", "neutral", "skipped"):
                raise PublicationError("rerun diagnosis no longer matches the failed exact attempt")
        elif kind == "merge":
            self._merge_gate(intent, ev)

    def execute(self, ident, audience):
        saved = self._load(ident)
        if saved["state"] == "completed":
            if not self.validate(saved["id"], saved["receipt"]):
                raise PublicationError("retained publication receipt no longer validates")
            return saved["receipt"]
        if saved["state"] == "uncertain":
            observed = self.reconcile(saved["id"])
            if observed["outcome"] == "completed":
                self._complete(saved, observed["result"])
                return observed["result"]
            if observed["outcome"] != "no_effect":
                raise UncertainEffect("publication may have taken effect; reconcile before retry")
            with self.ledger.transaction():
                self.ledger.db.execute("UPDATE publication_intents SET state='prepared',receipt=? WHERE id=? AND state='uncertain'",
                                       (canonical({"no_effect": observed["proof"]}).decode(), saved["id"]))
            saved = self._load(saved["id"])
        if saved["state"] != "prepared":
            raise PublicationError("publication intent is not dispatchable")
        intent = saved["intent"]
        ev = self._capture()
        self._gate(intent, ev, audience)
        # Exact pre-existing effects do not dispatch again. This is particularly
        # useful for distinct IDs proposing the same original reply.
        observed = self._observe(saved, ev)
        if observed["outcome"] == "completed":
            self._complete(saved, observed["result"])
            return observed["result"]
        if intent["kind"] == "ci_rerun":
            limit = self.plan.get("options", {}).get("limits", {}).get("ci_reruns_per_head", 3)
            if isinstance(limit, bool) or not isinstance(limit, int) or not 0 <= limit <= 3:
                raise PublicationError("invalid retained per-head CI rerun allowance")
            try:
                self.ledger.reserve("ci-reruns:" + intent["head"], saved["id"], limit)
            except ValueError as error:
                raise PublicationError("retained per-head CI rerun allowance exhausted or changed") from error
        with self.ledger.transaction():
            unresolved = self.ledger.db.execute(
                "SELECT id FROM publication_intents WHERE state='uncertain' AND id<>? LIMIT 1", (saved["id"],)
            ).fetchone()
            if unresolved:
                raise UncertainEffect("another publication operation needs reconciliation")
            cursor = self.ledger.db.execute(
                "UPDATE publication_intents SET state='uncertain',receipt='' WHERE id=? AND state='prepared'", (saved["id"],)
            )
            if cursor.rowcount != 1:
                raise UncertainEffect("publication acquired by another invocation")
        response = self._dispatch(intent)
        # Keep the exact acknowledged ID before observation. If observation is
        # temporarily unavailable, the next reconciliation can use this receipt.
        with self.ledger.transaction():
            self.ledger.db.execute("UPDATE publication_intents SET receipt=? WHERE id=? AND state='uncertain'",
                                   (canonical({"dispatch_response": response}).decode(), saved["id"]))
        observed = self.reconcile(saved["id"])
        if observed["outcome"] != "completed":
            raise UncertainEffect("publication response retained; its effect is not yet verified")
        self._complete(saved, observed["result"])
        return observed["result"]

    def _complete(self, saved, receipt):
        with self.ledger.transaction():
            changed = self.ledger.db.execute("UPDATE publication_intents SET state='completed',receipt=? WHERE id=? AND fingerprint=?",
                                   (canonical(receipt).decode(), saved["id"], saved["fingerprint"]))
            if changed.rowcount != 1:
                raise PublicationError("publication intent disappeared before receipt commit")

    def _dispatch(self, intent):
        kind, payload = intent["kind"], copy.deepcopy(intent["payload"])
        if kind in ("push", "rewrite", "split_push"):
            branch = payload.get("branch", intent["branch"])
            lease = "" if kind == "split_push" else intent["lease"]
            self._git("push", "--porcelain", f"--force-with-lease=refs/heads/{branch}:{lease}",
                      self._git_remote(intent), f"{payload['new_head']}:refs/heads/{branch}")
            return {"branch": branch, "head": payload["new_head"]}
        if kind == "edit_pr":
            return self._request("PATCH", self.pr_path, payload)
        if kind == "reply":
            return self._request("POST", self.issue_path + "/comments", payload)
        if kind == "reply_inline":
            return self._request("POST", f"{self.pr_path}/comments/{payload['comment_id']}/replies", {"body": payload["body"]})
        if kind == "resolve_thread":
            result = self._request("POST", "graphql", {"query": "mutation($id:ID!){resolveReviewThread(input:{threadId:$id}){thread{id isResolved}}}", "variables": {"id": payload["thread_id"]}})
            if result.get("errors"):
                raise UncertainEffect("thread mutation returned GraphQL errors")
            return result
        if kind == "request_review":
            return self._request("POST", self.pr_path + "/requested_reviewers", payload)
        if kind == "ci_rerun":
            return self._request("POST", f"{self.repo}/actions/runs/{payload['run_id']}/rerun-failed-jobs")
        if kind == "merge":
            response = self._request("PUT", self.pr_path + "/merge", {**payload, "sha": intent["head"]})
            if response.get("merged") is not True:
                raise UncertainEffect("merge response did not confirm a merge")
            return response
        if kind == "split_pr":
            payload.pop("push_operation")
            return self._request("POST", self.repo + "/pulls", payload)
        raise PublicationError("unsupported publication dispatch")

    def _receipt(self, saved, result):
        intent = saved["intent"]
        return {"id": saved["id"], "fingerprint": saved["fingerprint"], "run": intent["run"],
                "kind": intent["kind"], "pr": self.pr["url"], "head": intent["head"],
                "actor_id": intent["actor_id"], "result": result,
                "evidence": ["read-only exact service reconciliation"]}

    def _observe(self, saved, ev):
        intent = saved["intent"]
        kind, payload, meta = intent["kind"], intent["payload"], ev["metadata"]
        if ev["viewer"]["id"] != intent["actor_id"]:
            return {"outcome": "unknown", "proof": {"reason": "authenticated actor differs"}}
        found, absent, result = False, False, None
        if kind in ("push", "rewrite", "split_push"):
            current = self._git_observation(intent)
            found = current == payload["new_head"]
            absent = current is None if kind == "split_push" else current == intent["lease"]
            # An absent branch after an uncertain creation can have been deleted;
            # only the pre-dispatch fresh check may use an absence lease.
            if kind == "split_push" and saved["state"] == "uncertain":
                absent = False
            result = {"head": current, "branch": payload.get("branch", intent["branch"])}
        elif kind == "edit_pr":
            found = all(meta.get(k) == v for k, v in payload.items())
            absent = (meta.get("head", {}).get("sha") == intent["lease"] and
                      all(meta.get(k) == v for k, v in intent["before"].items()))
            result = {k: meta.get(k) for k in payload}
        elif kind in ("reply", "reply_inline"):
            wanted = intent["before"].get("satisfied_comment_id")
            retained = saved.get("receipt") or {}
            response = retained.get("dispatch_response", retained.get("result", {}))
            if response:
                wanted = _positive(response.get("id"), "acknowledged reply ID")
            matches = self._matching_replies(intent, ev)
            if wanted:
                matches = [v for v in matches if v["id"] == wanted]
            else:
                matches = [v for v in matches if v["id"] not in intent["before"]["comment_ids"]]
            if len(matches) == 1:
                found, result = True, matches[0]
            # Missing and ambiguous creations remain unknown, never no_effect.
        elif kind == "resolve_thread":
            thread = self._check_resolution(intent, ev)
            found = thread.get("isResolved") is True
            absent = thread.get("isResolved") is False
            result = thread
        elif kind == "request_review":
            result = self._request("GET", self.pr_path + "/requested_reviewers")
            actual = {"reviewers": {v.get("login") for v in result.get("users", [])},
                      "team_reviewers": {v.get("slug") for v in result.get("teams", [])}}
            found = all(set(values) <= actual[key] for key, values in payload.items())
            # Missing requests may have been satisfied, withdrawn or dismissed.
        elif kind == "ci_rerun":
            result = self._request("GET", f"{self.repo}/actions/runs/{payload['run_id']}")
            found = result.get("head_sha") == intent["head"] and result.get("run_attempt") == payload["attempt"] + 1
            # Same-attempt state can be a delayed request; no automatic replay.
        elif kind == "merge":
            merged = meta.get("merged") is True and meta.get("state") == "closed"
            merge_sha = meta.get("merge_commit_sha")
            if merged and meta.get("head", {}).get("sha") == intent["head"] and meta.get("merged_by", {}).get("id") == intent["actor_id"] and isinstance(merge_sha, str) and _SHA.fullmatch(merge_sha):
                commit = self._request("GET", f"{self.repo}/git/commits/{merge_sha}")
                wanted = [intent["target"]] + ([intent["head"]] if payload["merge_method"] == "merge" else [])
                found = commit.get("sha") == merge_sha and [p.get("sha") for p in commit.get("parents", [])] == wanted
                result = {"pr": meta, "commit": commit}
            absent = (not merged and meta.get("state") == "open" and
                      meta.get("head", {}).get("sha") == intent["head"] and meta.get("base", {}).get("sha") == intent["target"])
        elif kind == "split_pr":
            retained = saved.get("receipt") or {}
            response = retained.get("dispatch_response", retained.get("result", {}))
            wanted_number = response.get("number")
            matches = []
            for pr in self._pages(f"{self.repo}/pulls?state=all&per_page=100"):
                if wanted_number is not None and pr.get("number") != wanted_number:
                    continue
                if wanted_number is None and pr.get("number") in intent["before"]["pr_numbers"]:
                    continue
                matches.append(pr)
            owner, branch = payload["head"].split(":", 1)
            matches = [v for v in matches if v.get("user", {}).get("id") == intent["actor_id"]
                       and v.get("state") == "open" and v.get("draft") is True
                       and v.get("title") == payload["title"] and v.get("body") == payload["body"]
                       and v.get("head", {}).get("sha") == intent["created_head"]
                       and v.get("head", {}).get("ref") == branch
                       and v.get("head", {}).get("repo", {}).get("full_name", "").lower() == intent["head_repository"]
                       and intent["head_repository"].split("/", 1)[0] == owner.lower()
                       and v.get("base", {}).get("sha") == intent["target"]
                       and v.get("base", {}).get("ref") == payload["base"]
                       and v.get("base", {}).get("repo", {}).get("full_name", "").lower() == f"{self.pr['owner']}/{self.pr['name']}".lower()]
            if len(matches) == 1:
                found, result = True, matches[0]
        proof = {"intent": saved["fingerprint"], "observation": digest(result), "kind": kind}
        if found:
            receipt = self._receipt(saved, result)
            return {"outcome": "completed", "proof": proof, "result": receipt}
        return {"outcome": "no_effect" if absent else "unknown", "proof": proof}

    def reconcile(self, ident):
        """Read-only: return an observation without advancing local state."""
        saved = self._load(ident)
        try:
            return self._observe(saved, self._capture())
        except Exception as error:
            return {"outcome": "unknown", "proof": {"intent": saved["fingerprint"], "reason": str(error)}}

    def _validate_receipt(self, saved, receipt, ev):
        expected = self._receipt(saved, receipt.get("result"))
        if receipt != expected:
            return False
        observed = self._observe(dict(saved, receipt={"dispatch_response": receipt.get("result", {})}), ev)
        return observed.get("outcome") == "completed" and observed.get("result") == receipt

    def validate(self, ident, receipt):
        """Read-only current exact-effect validation for action checkpoints."""
        try:
            saved = self._load(ident)
            return isinstance(receipt, dict) and self._validate_receipt(saved, receipt, self._capture())
        except Exception:
            return False
