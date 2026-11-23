#!/usr/bin/env python3
"""Sanitized drafts and explicitly reviewed posting with durable uncertainty handling."""
import argparse
import fcntl
import hashlib
import json
import os
from pathlib import Path
import re
import subprocess
import sys
import tempfile

REPO = "dk-code-factory/agent-workflow"
FIELDS = ("reproduction", "expected", "actual", "versions", "platform", "evidence")


def sanitize(value):
    value = re.sub(r"(?i)(authorization\s*[:=]\s*)[^\r\n]+", r"\1[REDACTED]", value)
    value = re.sub(r"(?i)((?:[\w-]*(?:token|password|secret|api[_-]?key))\s*[:=]\s*)[^\s,;]+", r"\1[REDACTED]", value)
    value = re.sub(r"\b(?:gh[pousr]_[A-Za-z0-9_]+|github_pat_[A-Za-z0-9_]+|sk-[A-Za-z0-9_-]+)\b", "[REDACTED]", value)
    return re.sub(r"/(?:home|Users)/[^/\s]+", "<HOME>", value)


def draft(facts):
    if not all(isinstance(facts.get(field), str) and facts[field].strip() for field in ("title", *FIELDS)):
        raise ValueError("Supply nonempty title, reproduction, expected, actual, versions, platform and evidence strings.")
    title = sanitize(facts["title"]).replace("\n", " ").replace("\r", " ")
    body = "# " + title + "\n\n" + "\n\n".join("## " + field.capitalize() + "\n\n" + sanitize(facts[field]) for field in FIELDS) + "\n"
    marker = hashlib.sha256(body.encode()).hexdigest()
    return body + "\n<!-- agent-workflow-report:" + marker + " -->\n"


def search(query):
    result = subprocess.run(["gh", "issue", "list", "--repo", REPO, "--state", "all", "--search", query,
                             "--limit", "100", "--json", "number,url,title,body"], capture_output=True, text=True)
    if result.returncode:
        raise ValueError("Duplicate search failed; inspect existing GitHub access. No login was attempted.")
    return json.loads(result.stdout)


def save_journal(path, value):
    with tempfile.NamedTemporaryFile(mode="w", dir=path.parent, delete=False) as output:
        temporary = Path(output.name)
        try:
            json.dump(value, output, sort_keys=True)
            output.flush()
            os.fsync(output.fileno())
            os.replace(temporary, path)
            fd = os.open(path.parent, os.O_RDONLY)
            try:
                os.fsync(fd)
            finally:
                os.close(fd)
        finally:
            temporary.unlink(missing_ok=True)


def post(body_path, title, approved, journal_path):
    raw = body_path.read_bytes()
    body = raw.decode()
    checksum = hashlib.sha256(raw).hexdigest()
    if checksum != approved or not body.startswith("# " + title + "\n") or sanitize(title) != title:
        raise ValueError("Posting requires the exact reviewed body digest and sanitized draft title.")
    markers = re.findall(r"<!-- agent-workflow-report:([a-f0-9]{64}) -->", body)
    if len(markers) != 1:
        raise ValueError("Draft must contain exactly one report marker.")
    marker = "agent-workflow-report:" + markers[0]
    journal_path.parent.mkdir(mode=0o700, parents=True, exist_ok=True)
    lock = os.open(str(journal_path) + ".lock", os.O_CREAT | os.O_RDWR | os.O_NOFOLLOW, 0o600)
    try:
        fcntl.flock(lock, fcntl.LOCK_EX)
        if journal_path.is_symlink():
            raise ValueError("Posting journal must not be a symlink.")
        previous = json.loads(journal_path.read_text()) if journal_path.exists() else None
        if previous and (previous["sha256"] != checksum or previous["title"] != title):
            raise ValueError("Journal belongs to different reviewed content.")
        # Search even when a prior result exists; return only an observed issue.
        matches = [row for row in search('"' + marker + '" in:body') if marker in row["body"]]
        if len(matches) > 1:
            raise ValueError("Multiple matching issues; reconcile manually.")
        if matches:
            value = {"sha256": checksum, "title": title, "state": "created", "url": matches[0]["url"]}
            save_journal(journal_path, value)
            return value["url"]
        if previous:
            raise ValueError("Prior creation is uncertain or no longer visible. Reconcile manually; creation was not retried.")
        value = {"sha256": checksum, "title": title, "state": "uncertain", "marker": marker}
        save_journal(journal_path, value)
        # Copy reviewed bytes so a concurrently edited input cannot change the post.
        with tempfile.NamedTemporaryFile(mode="wb", dir=journal_path.parent) as reviewed:
            reviewed.write(raw)
            reviewed.flush()
            result = subprocess.run(["gh", "issue", "create", "--repo", REPO, "--title", title,
                                     "--body-file", reviewed.name], capture_output=True, text=True)
        url = result.stdout.strip()
        if result.returncode or not re.fullmatch(r"https://github\.com/dk-code-factory/agent-workflow/issues/\d+", url):
            raise ValueError("Issue creation result is uncertain. Keep the journal and reconcile before any retry.")
        value.update(state="created", url=url)
        save_journal(journal_path, value)
        return url
    finally:
        os.close(lock)


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    commands = parser.add_subparsers(dest="operation", required=True)
    create = commands.add_parser("draft")
    create.add_argument("--input", type=Path, required=True)
    create.add_argument("--output", type=Path, required=True)
    find = commands.add_parser("search")
    find.add_argument("--query", required=True)
    publish = commands.add_parser("post")
    publish.add_argument("--body", type=Path, required=True)
    publish.add_argument("--title", required=True)
    publish.add_argument("--approve-sha256", required=True)
    publish.add_argument("--journal", type=Path, required=True)
    args = parser.parse_args()
    try:
        if args.operation == "draft":
            body = draft(json.loads(args.input.read_text()))
            fd = os.open(args.output, os.O_CREAT | os.O_EXCL | os.O_WRONLY, 0o600)
            with os.fdopen(fd, "w", encoding="utf-8") as output:
                output.write(body)
            print(json.dumps({"body": str(args.output), "sha256": hashlib.sha256(args.output.read_bytes()).hexdigest()}))
        elif args.operation == "search":
            print(json.dumps(search(args.query)))
        else:
            print(post(args.body, args.title, args.approve_sha256, args.journal))
        return 0
    except (ValueError, OSError, KeyError) as error:
        print("agent-workflow report: " + str(error), file=sys.stderr)
        return 1


if __name__ == "__main__":
    sys.exit(main())
