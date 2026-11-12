"""Deliberate waits and phases, told to the release gate (scripts/grace/README.md §3 "Time").

    from gatemark import intentional, intentional_sleep, gate_phase

    intentional_sleep(1.5, "nothing else may print")       # a fixed wait on purpose (an absence check)
    with intentional("absence: no frame may arrive"):      # a block that waits on purpose
        assert s.wait_event("order", ..., timeout=10) is None
    gate_phase("gradle build")                             # a new phase of the gate step starts

Use these only for waits a test does ON PURPOSE (absence checks, a timer the test must sit out, chaos
pauses). Polling until the system reacts (wait_until) is real work: leave it alone. They sleep/run
exactly as before; inside a gate run (GATE_RUN + GATE_STEP set by scripts/gate.sh) they also append a
row to $GATE_RUN/events.tsv, so the dashboard counts the time as intentional, not effective.
Outside the gate they only sleep. The same file lives in tests/, main/tests/ and agent/tests/: keep
them identical.
"""
from __future__ import annotations

import os
import time
from contextlib import contextmanager


def _append(line: str) -> None:
    run = os.environ.get("GATE_RUN")
    if not run or not os.path.isdir(run):
        return
    try:
        fd = os.open(os.path.join(run, "events.tsv"), os.O_WRONLY | os.O_APPEND | os.O_CREAT, 0o644)
        try:
            os.write(fd, line.encode())
        finally:
            os.close(fd)
    except OSError:
        pass


def _clean(s: str) -> str:
    return s.replace("\t", " ").replace("\n", " ")


def _record(t0: float, t1: float, reason: str) -> None:
    step = os.environ.get("GATE_STEP")
    if not step:
        return
    prefix = os.environ.get("GATE_TEST_PREFIX", "")
    test = os.environ.get("PYTEST_CURRENT_TEST", "").rsplit(" (", 1)[0]
    worker = os.environ.get("PYTEST_XDIST_WORKER", "pytest")
    _append(f"{t0:.6f}\t{step}\twait\t{t1 - t0:.3f}\t{_clean(reason)}\t{_clean(prefix + test) if test else ''}\t{prefix}{worker}\n")


def intentional_sleep(seconds: float, reason: str) -> None:
    """time.sleep(seconds), counted as intentional time by the gate."""
    t0 = time.time()
    time.sleep(seconds)
    _record(t0, time.time(), reason)


@contextmanager
def intentional(reason: str):
    """Times the block as intentional (a wait whose length the test chose: an absence check's timeout)."""
    t0 = time.time()
    try:
        yield
    finally:
        _record(t0, time.time(), reason)


def gate_phase(name: str) -> None:
    """A new phase of the current gate step starts now (it lasts until the next phase or the step's end)."""
    step = os.environ.get("GATE_STEP")
    if step:
        _append(f"{time.time():.6f}\t{step}\tphase\t{_clean(name)}\n")
