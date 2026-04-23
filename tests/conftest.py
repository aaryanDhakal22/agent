"""Shared fixtures and config for agent/ pytest.

Env vars respected (all injected via `doppler run --`):

    APP_ENV                 — dev / staging / prod
    AGENT_BASE_URL          — defaults to http://localhost:${HTTP_PORT:-8080}
    HTTP_PORT               — agent HTTP port
    TEST_REAL_PRINTERS      — set to 1 to include real_printer-marked tests
"""

from __future__ import annotations

import os
import socket
import threading
import time
from contextlib import closing
from typing import Callable, Iterator

import pytest
import requests

APP_ENV = os.getenv("APP_ENV", "dev")
HTTP_PORT = os.getenv("HTTP_PORT", "8080")
AGENT_BASE_URL = os.getenv("AGENT_BASE_URL", f"http://localhost:{HTTP_PORT}")
REAL_PRINTERS = os.getenv("TEST_REAL_PRINTERS") == "1"


def pytest_collection_modifyitems(config, items):
    """Skip real_printer-marked tests unless TEST_REAL_PRINTERS=1."""
    if REAL_PRINTERS:
        return
    skip_real = pytest.mark.skip(reason="TEST_REAL_PRINTERS is not set to 1")
    for item in items:
        if "real_printer" in item.keywords:
            item.add_marker(skip_real)


@pytest.fixture(scope="session")
def http() -> requests.Session:
    """Session HTTP client for agent API."""
    s = requests.Session()
    s.headers.update({"Content-Type": "application/json"})
    return s


@pytest.fixture(scope="session")
def agent_base_url() -> str:
    return AGENT_BASE_URL


@pytest.fixture(scope="session")
def agent_ready(http: requests.Session, agent_base_url: str) -> None:
    """Guard: skip the suite if the agent isn't reachable."""
    deadline = time.time() + 5
    last_err: Exception | None = None
    while time.time() < deadline:
        try:
            r = http.get(f"{agent_base_url}/api/orders", timeout=1)
            if r.status_code == 200:
                return
        except requests.RequestException as exc:
            last_err = exc
        time.sleep(0.25)
    pytest.skip(f"agent not reachable at {agent_base_url}: {last_err}")


# ---------------------------------------------------------------------------
# Printer TCP-sink fixture (used when TEST_REAL_PRINTERS is NOT set).
# ---------------------------------------------------------------------------


def _free_port() -> int:
    with closing(socket.socket(socket.AF_INET, socket.SOCK_STREAM)) as s:
        s.bind(("127.0.0.1", 0))
        return s.getsockname()[1]


class FakePrinter:
    """A tiny TCP listener that accepts connections on a port and records bytes.

    Agent code dials the printer on port 9100 but escpos.New accepts an explicit
    host:port; here we only use this sink when running the agent as a goroutine
    inside the same process (advanced) — for simple HTTP API tests it's unused.
    """

    def __init__(self) -> None:
        self.port = _free_port()
        self.received: list[bytes] = []
        self._srv: socket.socket | None = None
        self._stop = threading.Event()
        self._thread: threading.Thread | None = None

    def start(self) -> None:
        self._srv = socket.socket(socket.AF_INET, socket.SOCK_STREAM)
        self._srv.setsockopt(socket.SOL_SOCKET, socket.SO_REUSEADDR, 1)
        self._srv.bind(("127.0.0.1", self.port))
        self._srv.listen(5)
        self._srv.settimeout(0.25)

        def loop() -> None:
            while not self._stop.is_set():
                try:
                    conn, _ = self._srv.accept()
                except socket.timeout:
                    continue
                try:
                    conn.settimeout(1.0)
                    data = conn.recv(65536)
                    if data:
                        self.received.append(data)
                finally:
                    conn.close()

        self._thread = threading.Thread(target=loop, daemon=True)
        self._thread.start()

    def stop(self) -> None:
        self._stop.set()
        if self._thread:
            self._thread.join(timeout=2)
        if self._srv:
            self._srv.close()


@pytest.fixture
def fake_printer() -> Iterator[FakePrinter]:
    fp = FakePrinter()
    fp.start()
    try:
        yield fp
    finally:
        fp.stop()
