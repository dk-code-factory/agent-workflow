"""One location resolver; compatibility addresses preserve existing state and locks."""
import os
from pathlib import Path


LOCATIONS = {
    'tasks': ('STATE', '.local/state', 'dk-workflows/v2'),
    'config': ('CONFIG', '.config', 'dk-workflows/v2'),
    'operations': ('STATE', '.local/state', 'dk-pr-skills/v2/operations'),
    'ci': ('STATE', '.local/state', 'dk-pr-skills/v2/ci'),
    'policies': ('DATA', '.local/share', 'codex-repo-rules'),
    'completion': ('DATA', '.local/share', 'dk-pr-iter/completion-prompts'),
    'legacy_runs': ('DATA', '.local/share', 'dk-pr-iter'),
}


def location(name):
    kind, default, relative = LOCATIONS[name]
    variable = 'XDG_' + kind + '_HOME'
    base = Path(os.environ.get(variable) or Path.home()/default)
    if not base.is_absolute():
        raise ValueError(variable + ' must be absolute')
    return base/relative
