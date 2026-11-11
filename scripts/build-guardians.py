#!/usr/bin/env python3
"""Build static Linux guardians for local container execution."""
import argparse
from pathlib import Path
from guardian_assets import build_guardians

parser = argparse.ArgumentParser(description=__doc__)
parser.add_argument("--output", default="bin/guardians")
args = parser.parse_args()
build_guardians(Path(__file__).resolve().parents[1], Path(args.output).resolve())
