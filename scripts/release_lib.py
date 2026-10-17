"""Shared release identities. No provider calls or remote writes."""
import hashlib
from pathlib import Path
import subprocess

ROOT = Path(__file__).resolve().parents[1]

def git(*args, root=ROOT):
    return subprocess.check_output(['git', *args], cwd=root, text=True).strip()

def source_fingerprint(root=ROOT):
    """Hash every tracked input, its Git mode and current bytes, without exclusions."""
    value = hashlib.sha256()
    entries = subprocess.check_output(['git', 'ls-files', '--stage', '-z'], cwd=root)
    for entry in sorted(entries.split(b'\0')):
        if not entry:
            continue
        metadata, name = entry.split(b'\t', 1)
        mode, _, stage = metadata.split()
        if stage != b'0' or mode not in (b'100644', b'100755'):
            raise ValueError('unmerged, symlink or submodule build input: ' + name.decode())
        path = Path(root) / name.decode()
        executable = bool(path.stat().st_mode & 0o111)
        actual_mode = b'100755' if executable else b'100644'
        value.update(actual_mode + b'\0' + name + b'\0' + path.read_bytes() + b'\0')
    return value.hexdigest()

def sha256(path):
    value = hashlib.sha256()
    with Path(path).open('rb') as stream:
        for chunk in iter(lambda: stream.read(1024 * 1024), b''):
            value.update(chunk)
    return value.hexdigest()
