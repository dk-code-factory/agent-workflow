"""One shared wall-clock budget for CI evidence reads and classification."""
from contextlib import contextmanager
from contextvars import ContextVar
import math
import subprocess
import time


ACTIVE_BUDGET = ContextVar('ci_classification_budget', default=None)


def check_budget():
    """Cooperate with the active deadline during local report processing."""
    budget = ACTIVE_BUDGET.get()
    if budget is not None:
        budget.remaining()


class ClassificationTimeout(Exception):
    evidence_file = None


class Budget:
    def __init__(self, seconds=60, clock=time.monotonic, on_progress=None):
        if not math.isfinite(seconds) or seconds <= 0:
            raise ValueError('classification timeout must be positive and finite')
        self.seconds = seconds
        self.clock = clock
        self.started = clock()
        self.deadline = self.started + seconds
        self.phase = 'classification'
        self.on_progress = on_progress

    @contextmanager
    def activate(self):
        token = ACTIVE_BUDGET.set(self)
        try:
            yield
        finally:
            ACTIVE_BUDGET.reset(token)

    def remaining(self):
        remaining = self.deadline - self.clock()
        if remaining <= 0:
            raise ClassificationTimeout('CI classification deadline exceeded during ' + self.phase)
        return remaining

    def progress(self, phase, **details):
        self.phase = phase
        self.remaining()
        if self.on_progress:
            self.on_progress({'phase': phase, 'elapsed_seconds': round(self.clock() - self.started, 1),
                              'classification_timeout': self.seconds, **details})


class BoundedAPI:
    """Check all call boundaries; the production transport also caps each subprocess."""
    def __init__(self, api, budget):
        self.budget = budget
        self.api = api

    def __getattr__(self, name):
        target = getattr(self.api, name)
        if not callable(target):
            return target
        def call(*args, **kwargs):
            self.budget.remaining()
            try:
                with self.budget.activate():
                    result = target(*args, **kwargs)
            except subprocess.TimeoutExpired as exc:
                self.budget.remaining()
                raise RuntimeError(str(exc)) from exc
            self.budget.remaining()
            return result
        return call
