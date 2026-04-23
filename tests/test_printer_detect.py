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


def test_reprint_against_real_store(
    http: requests.Session, agent_base_url: str, agent_ready: None
) -> None:
    """If there's at least one order in the 24h store, we should be able to
    reprint it and receive a 200. (On the home-network desktop there must
    have been recent activity for this to be meaningful.)"""
    r = http.get(f"{agent_base_url}/api/orders", timeout=5)
    assert r.status_code == 200
    orders = r.json()
    if not orders:
        pytest.skip("no orders in the 24h store — nothing to reprint")

    newest = orders[0]
    order_id = newest["order"]["order_id"]

    # Give the printer a moment between potential prior ops.
    time.sleep(1)
    resp = http.post(f"{agent_base_url}/api/orders/{order_id}/reprint", timeout=30)
    assert resp.status_code == 200, resp.text
    assert resp.json().get("status") == "reprinted"
