"""Real-printer tests for the agent's KeepCheck + print dispatch.

These require TEST_REAL_PRINTERS=1 and a live printer network. They are
skipped by default so CI / dev runs don't burn paper.
"""

import os
import socket
import time

import pytest
import requests


pytestmark = pytest.mark.real_printer


def _dial(host: str, port: int, timeout: float = 2.0) -> bool:
    try:
        with socket.create_connection((host, port), timeout=timeout):
            return True
    except OSError:
        return False


def test_staging_printer_reachable() -> None:
    """The single-printer desktop (staging) should have PRINTER_IP reachable on :9100."""
    ip = os.getenv("PRINTER_IP")
    assert ip, "PRINTER_IP must be set for real_printer tests"
    assert _dial(ip, 9100), f"Printer at {ip}:9100 is not reachable"


def test_reprint_against_real_store(agent_session: requests.Session, agent_base_url: str) -> None:
    """If the agent holds at least one order, reprinting it should print its
    tickets (station tickets + receipt) and return 200."""
    http = agent_session
    r = http.get(f"{agent_base_url}/api/orders", params={"batch": "all"}, timeout=5)
    assert r.status_code == 200
    orders = r.json()
    if not orders:
        pytest.skip("the agent holds no orders — nothing to reprint")

    newest = orders[0]
    order_id = newest["order"]["id"]  # canonical UUID (the Snapshot's `id`)

    # Give the printer a moment between potential prior ops.
    time.sleep(1)
    resp = http.post(f"{agent_base_url}/api/orders/{order_id}/reprint", timeout=30)
    assert resp.status_code == 200, resp.text
    assert resp.json()["order"]["id"] == order_id  # reprint returns the stored order
