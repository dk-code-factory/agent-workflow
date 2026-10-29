#!/usr/bin/env python3
"""Regenerate embedded CO-RE objects without loading any kernel program."""
import hashlib
from pathlib import Path
import subprocess

root = Path(__file__).resolve().parents[1]
package = root / "internal/socketfence"
digest = hashlib.sha256((package / "bpf/fence.c").read_bytes()).hexdigest()
subprocess.run([
    "go", "run", "github.com/cilium/ebpf/cmd/bpf2go", "-target", "bpfel,bpfeb",
    "-tags", "linux", "-cc", "clang", "-no-strip", "fence", "bpf/fence.c", "--",
    "-O2", "-g", "-Wall", "-Werror", f'-DFENCE_SOURCE_SHA="{digest}"',
    f"-fdebug-prefix-map={root}=.",
], cwd=package, check=True)
