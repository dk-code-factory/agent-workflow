"""Keep disposable Git repositories independent of contributor configuration."""
import os
from unittest.mock import patch


def environment():
    return {**os.environ, "GIT_CONFIG_NOSYSTEM": "1", "GIT_CONFIG_GLOBAL": os.devnull,
            "GIT_CONFIG_COUNT": "0", "GIT_CONFIG_PARAMETERS": "", "GIT_TEMPLATE_DIR": ""}


def command(*args):
    return ["git", "-c", "core.hooksPath=" + os.devnull, "-c", "init.templateDir=", *args]


def isolate(test):
    context = patch.dict(os.environ, environment())
    context.start()
    test.addCleanup(context.stop)
