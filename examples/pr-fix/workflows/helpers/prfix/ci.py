"""One-shot CI observation and exact-attempt exception verification.

Polling, repair selection and repetition live in YAML. Unknown failures remain
blocking; an agent's classification is a candidate, never authority by itself.
"""

from __future__ import annotations

from collections import defaultdict
from datetime import datetime
import json
import re
import time
from urllib.parse import urlsplit

from .core import bounded_json, digest, text


SUCCESS = {"success", "neutral", "skipped"}
TIMESTAMP = re.compile(r"^\d{4}-\d\d-\d\dT\d\d:\d\d:\d\d(?:\.\d+)?Z\s")
NAMED_FAILURE = re.compile(r"^(?:--- FAIL: |FAILED )([^\s(]+)")
GO_SUMMARY = re.compile(r"^(?:FAIL(?:\s+\S+(?:\s+[0-9.]+s)?)?|PASS|ok\s+\S+\s+(?:[0-9.]+s|\(cached\))|\?\s+\S+\s+\[no test files\]|exit status [1-9][0-9]*)$")
MAKE_FAILURE = re.compile(r"^(?:make(?:\[[0-9]+\])?: \*\*\* \[[^\]]+: test\] Error [1-9][0-9]*|(?:##\[error\])?(?:Error: )?Process completed with exit code [1-9][0-9]*\.)$")
GO_LOCATION = re.compile(r"^\s+[^\s]+\.go:[0-9]+:\s*")


def _integer(value, label):
    if isinstance(value, bool) or not isinstance(value, int) or value < 1:
        raise ValueError(label + " requires a positive integer")
    return value


def _tuple(value):
    return tuple(str(value.get(k, "")) for k in ("head", "check_id", "run_id", "job_id", "attempt"))


def classify(observation, exceptions=None):
    """Classify an exact inventory using independently verified proof objects.

    ``exceptions`` must come from ``verify_exceptions`` at this boundary, rather
    than from a leaf's JSON. Proofs retain the precise source and complete failed
    step inventory; callers persist that original evidence for checkpoint use.
    """
    head = text(observation.get("head"), "CI head")
    checks = observation.get("checks")
    if not isinstance(checks, list):
        raise ValueError("CI check inventory must be an array")
    proofs = {_tuple(proof): proof for proof in (exceptions or [])
              if isinstance(proof, dict) and proof.get("verified") is True
              and proof.get("exempt") is True and proof.get("evidence")}
    seen = set()
    pending, failed = not checks, False
    used = []
    for check in checks:
        check_id = text(check.get("id"), "CI check ID")
        if check_id in seen:
            raise ValueError("duplicate CI check identity")
        seen.add(check_id)
        if check.get("status") != "completed":
            pending = True
            continue
        if check.get("conclusion") in SUCCESS:
            continue
        pin = {"head": head, "check_id": check_id, "run_id": check.get("run_id", ""),
               "job_id": check.get("job_id", ""), "attempt": check.get("attempt", 0)}
        proof = proofs.get(_tuple(pin))
        if proof is None:
            failed = True
        else:
            used.append(proof)
    status = "failed" if failed else "pending" if pending else "success"
    return {"head": head, "status": status, "checks": checks,
            "evidence": observation.get("evidence"), "classifications": used}


def _test_command(line):
    return (not any(c in line for c in (";", "|", "&", "`", "$("))
            and (line == "make test" or line.startswith("go test ")))


def _test_inventory(log, candidates):
    signatures, tests = set(), set()
    for candidate in candidates:
        test = text(candidate.get("test"), "test exception name")
        tests.add(test)
        signature = candidate.get("signature")
        if signature:
            if any(c in signature for c in ("\r", "\n")):
                raise ValueError("test exception signature must name one diagnostic line")
            signatures.add(signature.strip())
    failures = 0
    preamble = False
    for number, original in enumerate(log.splitlines(), 1):
        line = TIMESTAMP.sub("", original)
        value = line.strip()
        if not value:
            continue
        if value.startswith("##[group]Run "):
            if not _test_command(value[len("##[group]Run "):]):
                raise ValueError("unsupported failed-step command envelope")
            preamble = True
            continue
        if value == "##[endgroup]":
            preamble = False
            continue
        if preamble and (value.startswith("shell: ") or value == "env:"
                         or re.match(r"^\s+[A-Za-z_][A-Za-z0-9_]*: ", line)):
            continue
        if _test_command(value) or MAKE_FAILURE.fullmatch(value):
            continue
        match = NAMED_FAILURE.match(value)
        if match:
            if match[1] not in tests:
                raise ValueError("uncovered failing test: " + match[1])
            failures += 1
            continue
        if GO_SUMMARY.fullmatch(value) or value.startswith(("=== RUN   ", "=== PAUSE ",
                "=== CONT  ", "=== NAME  ", "--- PASS: ", "--- SKIP: ")):
            continue
        if value in signatures or GO_LOCATION.sub("", line).strip() in signatures:
            continue
        raise ValueError("unclassified failed-step log line " + str(number))
    if not failures:
        raise ValueError("test exception cannot exempt an unknown failure")


def _failed_logs(log, job):
    expected = []
    for step in job.get("steps", []):
        if step.get("status") != "completed":
            raise ValueError("CI exception requires complete exact-attempt steps")
        if step.get("conclusion") in SUCCESS:
            continue
        name = text(step.get("name"), "failed step name")
        _integer(step.get("number"), "failed step number")
        if name in expected:
            raise ValueError("ambiguous failed step names")
        expected.append(name)
    if not expected:
        raise ValueError("failed step metadata is unavailable")
    parts = defaultdict(list)
    for line in log.splitlines():
        if not line.strip():
            continue
        row = line.split("\t", 2)
        if len(row) != 3 or row[0] != job["name"] or row[1] not in expected:
            raise ValueError("failed-step log identity is missing or mismatched")
        parts[row[1]].append(row[2])
    if any(not parts[name] for name in expected):
        raise ValueError("failed-step log inventory is incomplete")
    return ["\n".join(parts[name]) for name in expected]


def _affirmative_issue(issue, candidate):
    if issue.get("state") != "open" or issue.get("pull_request"):
        return False
    content = issue.get("title", "") + "\n" + issue.get("body", "")
    keys = ("workflow", "job", "test", "signature")
    if any(not isinstance(candidate.get(k), str) or not candidate[k].strip() for k in keys):
        return False
    for block in re.findall(r"```dk-flaky\s*\n(.*?)\n```", content, re.S):
        try:
            declaration = bounded_json(block)
        except (ValueError, TypeError):
            continue
        if all(declaration.get(k) == candidate[k] for k in keys):
            return declaration.get("flaky") is True
    if any("`" + candidate[k] + "`" not in content for k in keys):
        return False
    return (not re.search(r"\b(?:not|never|no|non)[ -]?(?:a )?(?:flaky|intermittent)", content, re.I)
            and not re.search(r"\b(?:maybe|possibly|suspect|could|might|whether|is it)\b|\?", content, re.I)
            and bool(re.search(r"^\s*(?:confirmed flaky test|known flaky test|this test is flaky)[.: ]", content, re.I | re.M)))


def verify_exceptions(provider, pr, observation, candidates, rules=None):
    """Read exact run/job/log/issue evidence before permitting any exemption."""
    head = observation["head"]
    rule_map = {rule["id"]: rule for rule in (rules or [])}
    grouped = defaultdict(list)
    checks = {check["id"]: check for check in observation["checks"]}
    for candidate in candidates:
        if not isinstance(candidate, dict) or candidate.get("check_id") not in checks:
            raise ValueError("classification names an unknown CI check")
        grouped[candidate["check_id"]].append(candidate)
    proofs = []
    prefix = "repos/" + pr["owner"] + "/" + pr["name"]
    for check_id, group in grouped.items():
        check = checks[check_id]
        if check.get("status") != "completed" or check.get("conclusion") in SUCCESS:
            raise ValueError("exceptions cannot replace pending or successful checks")
        first = group[0]
        for key in ("run_id", "job_id"):
            if not str(first.get(key, "")).isdigit() or int(first[key]) < 1:
                raise ValueError("exact Actions run/job required")
        attempt = _integer(first.get("attempt"), "Actions attempt")
        if any(str(first.get(k)) != str(check.get(k)) for k in ("run_id", "job_id", "attempt")):
            raise ValueError("classification differs from observed Actions attempt")
        run = provider.request("GET", prefix + "/actions/runs/" + str(first["run_id"]))
        job = provider.request("GET", prefix + "/actions/jobs/" + str(first["job_id"]))
        if (str(run.get("id")) != str(first["run_id"])
                or str(job.get("id")) != str(first["job_id"])
                or run.get("head_sha") != head or job.get("head_sha") != head
                or run.get("run_attempt") != attempt
                or str(job.get("run_id")) != str(first["run_id"])
                or job.get("run_attempt") != attempt or job.get("conclusion") != "failure"
                or job.get("status") != "completed"
                or not str(job.get("check_run_url", "")).endswith("/check-runs/" + check_id)):
            raise ValueError("CI exception evidence does not bind exact failed head/check/run/job/attempt")
        logs = provider.failure_logs(pr, first["run_id"], first["job_id"], attempt)
        workflow = run.get("path", "").split("@", 1)[0]
        evidence = [{"kind": "actions", "sha256": digest({"run": run, "job": job, "logs": logs})}]
        pipeline = False
        for candidate in group:
            if (any(str(candidate.get(k)) != str(first.get(k)) for k in ("check_id", "run_id", "job_id", "attempt"))
                    or candidate.get("workflow") != workflow or candidate.get("job") != job.get("name")):
                raise ValueError("classification scope differs from exact workflow/job")
            kind = candidate.get("kind")
            if kind == "exception":
                rule = rule_map.get(candidate.get("rule"))
                if (not rule or rule.get("workflow") != workflow
                        or rule.get("job") and rule["job"] != job.get("name")
                        or not rule.get("reason")):
                    raise ValueError("CI exception is absent from scoped settings")
                if rule.get("kind") == "pipeline":
                    pipeline = True
                    continue
                if (rule.get("kind") != "test" or rule.get("test") != candidate.get("test")
                        or rule.get("signature") != candidate.get("signature")):
                    raise ValueError("test exemption differs from saved policy")
            elif kind == "flake":
                link = urlsplit(text(candidate.get("issue"), "flake issue URL"))
                parts = link.path.strip("/").split("/")
                if (link.scheme != "https" or link.netloc != pr["host"] or link.query or link.fragment
                        or "%" in link.path or len(parts) != 4 or parts[:2] != [pr["owner"], pr["name"]]
                        or parts[2] != "issues" or not parts[3].isdigit() or int(parts[3]) < 1):
                    raise ValueError("flake requires a canonical issue in the PR repository")
                issue = provider.request("GET", prefix + "/issues/" + parts[3])
                if issue.get("html_url", "").lower() != candidate["issue"].lower() or not _affirmative_issue(issue, candidate):
                    raise ValueError("issue does not affirm this exact test/signature as flaky")
                evidence.append({"kind": "open-flake-issue", "url": candidate["issue"], "sha256": digest(issue)})
            else:
                raise ValueError("unsupported CI classification kind")
            test = text(candidate.get("test"), "classified test")
            signature = candidate.get("signature")
            if test not in logs or signature and signature not in logs:
                raise ValueError("classified test/signature is absent from exact logs")
        if not pipeline:
            for part in _failed_logs(logs, job):
                _test_inventory(part, group)
        proofs.append({"head": head, "check_id": check_id, "run_id": str(first["run_id"]),
                       "job_id": str(first["job_id"]), "attempt": attempt, "verified": True,
                       "exempt": True, "classifications": group, "evidence": evidence})
    return proofs


def _timestamp(value):
    if isinstance(value, dict):
        value = value.get("value", value.get("deadline"))
    if isinstance(value, str):
        return datetime.fromisoformat(value.replace("Z", "+00:00")).timestamp()
    if isinstance(value, bool) or not isinstance(value, (int, float)):
        raise ValueError("retained CI deadline is invalid")
    return value


def observe(provider, plan, ledger, operation):
    """Observe once. A new process or head never silently renews the clock."""
    options = plan.get("options", {})
    selected = options.get("ci", {})
    timeout = selected.get("timeout_seconds", options.get("ci_timeout_seconds", 1800))
    if isinstance(timeout, bool) or not isinstance(timeout, int) or not 1 <= timeout <= 86400:
        raise ValueError("CI timeout must be an integer in 1..86400 seconds")
    text(operation, "CI observation operation")
    deadline = _timestamp(ledger.deadline("ci:" + plan["logical_run"], timeout))
    if time.time() >= deadline:
        raise TimeoutError("retained CI deadline exhausted")
    evidence = provider.capture(plan["pr"])
    head = evidence["metadata"]["head"]["sha"]
    observation = provider.observe_ci(plan["pr"], head)
    if observation.get("head") != head:
        raise ValueError("CI observer returned another head")
    candidates = selected.get("classifications", options.get("ci_classifications", []))
    rules = selected.get("exceptions", options.get("ci_exceptions", []))
    if isinstance(rules, dict):
        rules = rules.get("entries", [])
    proofs = verify_exceptions(provider, plan["pr"], observation, candidates, rules) if candidates else []
    result = classify(observation, proofs)
    if time.time() >= deadline:
        raise TimeoutError("retained CI deadline exhausted during observation")
    result.update(deadline=deadline, operation=operation,
                  ready=result["status"] == "success", pending=result["status"] == "pending",
                  failed=result["status"] == "failed", repair=False)
    return result


def reserve_repair(ledger, cycle, operation, limit=5):
    if isinstance(limit, bool) or not isinstance(limit, int) or not 0 <= limit <= 5:
        raise ValueError("CI repair allowance must be in 0..5")
    return ledger.reserve("ci-repair:" + text(cycle, "review cycle"), text(operation, "repair operation"), limit)


def reserve_rerun(ledger, head, operation, limit=3):
    if isinstance(limit, bool) or not isinstance(limit, int) or not 0 <= limit <= 3:
        raise ValueError("CI rerun allowance must be in 0..3")
    return ledger.reserve("ci-reruns:" + text(head, "CI head"), text(operation, "rerun operation"), limit)
