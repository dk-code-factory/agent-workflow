"""Build and verify the two static Linux workload guardians (no live services)."""
import hashlib
import json
import os
from pathlib import Path
import struct
import subprocess

ARCHITECTURES = {"amd64": 62, "arm64": 183}

def verify_guardians(directory):
    directory = Path(directory)
    checksums = json.loads((directory / "checksums.json").read_text())
    if set(checksums) != {"linux-" + arch for arch in ARCHITECTURES}:
        raise ValueError("guardian platform set mismatch")
    for arch, machine in ARCHITECTURES.items():
        path = directory / ("linux-" + arch)
        data = path.read_bytes()
        if path.is_symlink() or not path.is_file() or not path.stat().st_mode & 0o111:
            raise ValueError("guardian must be an executable regular file")
        if hashlib.sha256(data).hexdigest() != checksums[path.name]:
            raise ValueError("guardian checksum mismatch")
        if len(data) < 64 or data[:6] != b"\x7fELF\x02\x01" or struct.unpack_from("<H", data, 18)[0] != machine:
            raise ValueError("guardian platform mismatch")
        offset = struct.unpack_from("<Q", data, 32)[0]
        size, count = struct.unpack_from("<HH", data, 54)
        for index in range(count):
            if struct.unpack_from("<I", data, offset + size * index)[0] == 3:
                raise ValueError("guardian must be statically linked")
    return checksums

def build_guardians(root, directory, ldflags="-s -w"):
    directory = Path(directory)
    directory.mkdir(parents=True, exist_ok=True)
    checksums = {}
    for arch in ARCHITECTURES:
        path = directory / ("linux-" + arch)
        subprocess.run(["go", "build", "-trimpath", "-buildvcs=false", "-ldflags", ldflags,
                        "-o", str(path), "./cmd/agent-workflow-guardian"], cwd=root,
                       env=dict(os.environ, CGO_ENABLED="0", GOOS="linux", GOARCH=arch), check=True)
        checksums[path.name] = hashlib.sha256(path.read_bytes()).hexdigest()
    (directory / "checksums.json").write_text(json.dumps(checksums, sort_keys=True, indent=2) + "\n")
    verify_guardians(directory)
