#!/usr/bin/env python3
"""Produce current dependency vulnerability reports without running providers."""
import argparse
import json
import os
from pathlib import Path
import subprocess

root = Path(__file__).resolve().parents[1]
parser = argparse.ArgumentParser(description=__doc__)
parser.add_argument("--output", default=str(root / "dist"))
args = parser.parse_args()
out = Path(args.output).resolve()
out.mkdir(parents=True, exist_ok=True)
reports = []
commands = [
    ("go-vulnerabilities.json", ["go", "run", "golang.org/x/vuln/cmd/govulncheck@v1.9.0", "-json", "./..."]),
    ("npm-vulnerabilities.json", ["npm", "audit", "--prefix", "bridge/claude", "--package-lock-only", "--json"]),
    ("codex-image-vulnerabilities.json", ["npm", "audit", "--prefix", "images/codex", "--package-lock-only", "--json"]),
]
for filename, command in commands:
    with (out / filename).open("w") as stream:
        result = subprocess.run(command, cwd=root, stdout=stream, stderr=subprocess.PIPE, text=True)
    reports.append({"file": filename, "command": command, "exit_code": result.returncode, "diagnostics": result.stderr[-4096:]})
summary = {"go_toolchain": subprocess.check_output(["go", "version"], text=True).strip(), "reports": reports, "all_checks_passed": all(item["exit_code"] == 0 for item in reports)}
(out / "vulnerability-summary.json").write_text(json.dumps(summary, indent=2) + "\n")
print(json.dumps(summary))
raise SystemExit(0 if summary["all_checks_passed"] else 1)
