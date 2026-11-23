"""Disposable subjects for manual native Codex acceptance; no provider calls."""
import hashlib
from pathlib import Path

SUBJECTS = {
    "plugin-parallel-audit": {
        "request": "Build me a workflow to audit this small repository with multiple reviewers.",
        "answers": "Audit fixture/ only for correctness and security. Two independent native Codex agents must overlap and return JSON objects with findings arrays; combine deduplicated findings into a JSON object. No fixture edits. At most four turns per agent, host parallelism two, read-only nested agents, no network tools or publication. Validate, then run once after I approve the complete plan. Verify overlap from durable attempt timestamps and unchanged fixture hashes.",
    },
    "plugin-bounded-fix": {
        "request": "Build me a workflow to fix the little Python bug here and check the result.",
        "answers": "Repair fixture/calculator.py only; tests and other fixture files are immutable. Implement, run python3 -m unittest discover -s fixture/tests, then independently review. At most two implementation iterations, four turns per agent and host parallelism two. Nested agents may edit this disposable workspace, but no network tools, publication or commits. Completion requires passing existing tests and a clean review. Validate first; wait for my complete-plan approval before writing files, then execute once when requested.",
    },
    "plugin-gated-report-recovery": {
        "request": "Build me a workflow to process this CSV and make a report after I approve it.",
        "answers": "Use fixture/data.csv and fixture/process.py to validate integer nonnegative amounts and nonempty unique names. Process once to prepared.json with a stable SHA-256 evidence identity; fixture/processed.count proves no repeated processing. Wait for JSON string approve or decline; only approve permits fixture/render.py to create report.txt. No agents are needed in this workflow. No fixture edits, remote calls or publication. The harness will restart its owned controller while waiting, reject a stale evidence answer, then approve the same gate/run. Preserve preparation and all limits. Validate first and wait for explicit complete-plan approval before implementation.",
    },
}


def seed(workspace, scenario):
    workspace = Path(workspace)
    fixture = workspace / "fixture"
    fixture.mkdir(parents=True)
    if scenario == "plugin-parallel-audit":
        (fixture / "service.py").write_text('''import subprocess
def average(values):
    return sum(values) / len(values)
def lookup(name):
    return subprocess.check_output("echo " + name, shell=True)
''')
    elif scenario == "plugin-bounded-fix":
        (fixture / "calculator.py").write_text("def total(values):\n    return sum(values) + 1\n")
        tests = fixture / "tests"
        tests.mkdir()
        (tests / "test_calculator.py").write_text('''import unittest
from fixture.calculator import total
class Totals(unittest.TestCase):
    def test_empty(self): self.assertEqual(total([]), 0)
    def test_values(self): self.assertEqual(total([2, 3]), 5)
    def test_negative(self): self.assertEqual(total([-2, 2]), 0)
''')
    elif scenario == "plugin-gated-report-recovery":
        (fixture / "data.csv").write_text("name,amount\nalpha,2\nbeta,3\n")
        (fixture / "process.py").write_text('''import csv, hashlib, json
from pathlib import Path
root = Path(__file__).resolve().parent
rows = list(csv.DictReader((root / "data.csv").open()))
names = set()
for row in rows:
    if not row["name"] or row["name"] in names or int(row["amount"]) < 0:
        raise ValueError("invalid CSV row")
    names.add(row["name"])
data = json.dumps({"total": sum(int(row["amount"]) for row in rows)}, sort_keys=True)
(root / "prepared.json").write_text(data)
count = root / "processed.count"
count.write_text(str(int(count.read_text()) + 1 if count.exists() else 1))
print(hashlib.sha256(data.encode()).hexdigest())
''')
        (fixture / "render.py").write_text('''import json
from pathlib import Path
root = Path(__file__).resolve().parent
data = json.loads((root / "prepared.json").read_text())
(root / "report.txt").write_text("Total: " + str(data["total"]) + "\\n")
''')
    else:
        raise ValueError("Unknown acceptance subject")
    return snapshot(workspace)


def snapshot(workspace):
    return {path.relative_to(workspace).as_posix(): hashlib.sha256(path.read_bytes()).hexdigest()
            for path in sorted(Path(workspace).rglob("*")) if path.is_file()
            and not any(part in {".git", ".agent-workflow", "__pycache__"} for part in path.relative_to(workspace).parts)}
