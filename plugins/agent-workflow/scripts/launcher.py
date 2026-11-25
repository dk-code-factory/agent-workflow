"""Built-in plugin adapter for the shared exact-tag engine bootstrap."""
from pathlib import Path

from engine_bootstrap import main as bootstrap_main

PLUGIN = Path(__file__).resolve().parents[1]


def main(argv=None):
    return bootstrap_main(PLUGIN, argv, builtin=True)
