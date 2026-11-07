"""Check the complete Go dependency locks without modifying the checkout."""
import difflib
import os
from pathlib import Path
import subprocess
import sys
import tempfile

ROOT = Path(__file__).resolve().parents[1]


def check_go_locks(root=ROOT):
    root = Path(root)
    locks = {name: (root / name).read_bytes() for name in ('go.mod', 'go.sum')}
    with tempfile.TemporaryDirectory(prefix='agent-workflow-go-locks-') as directory:
        temporary = Path(directory)
        for name, contents in locks.items():
            (temporary / name).write_bytes(contents)
        # Do not inherit a parent go.work or let downloads repair the source tree.
        subprocess.run(['go', 'mod', 'download', 'all'], cwd=temporary, check=True,
                       env=dict(os.environ, GOWORK='off'))
        differences = []
        for name, before in locks.items():
            after = (temporary / name).read_bytes()
            if before != after:
                differences.extend(difflib.unified_diff(
                    before.decode().splitlines(keepends=True), after.decode().splitlines(keepends=True),
                    fromfile=name, tofile=name + ' (complete)'))
    if differences:
        print('Go dependency locks are incomplete. Run make go-deps and commit go.mod/go.sum.',
              file=sys.stderr)
        sys.stderr.writelines(differences)
        return False
    return True


if __name__ == '__main__':
    try:
        sys.exit(0 if check_go_locks() else 1)
    except subprocess.CalledProcessError as error:
        sys.exit(error.returncode)
