"""Tests the agent's public HTTP API — assumes the agent is already running.

These tests don't need a printer. They check the behavior exposed to the
mobile app and anything else that talks to the agent over HTTP.
"""

import requests


def test_get_orders_returns_list(http: requests.Session, agent_base_url: str, agent_ready: None) -> None:
    r = http.get(f"{agent_base_url}/api/orders", timeout=5)
    assert r.status_code == 200
    assert r.headers.get("Content-Type", "").startswith("application/json")
    data = r.json()
    assert isinstance(data, list)


def test_cors_headers_present(http: requests.Session, agent_base_url: str, agent_ready: None) -> None:
    r = http.options(f"{agent_base_url}/api/orders", timeout=5)
    # 204 from our CORS preflight handler
    assert r.status_code == 204
    assert r.headers.get("Access-Control-Allow-Origin") == "*"
    assert "GET" in (r.headers.get("Access-Control-Allow-Methods") or "")


def test_reprint_unknown_id_returns_404(
    http: requests.Session, agent_base_url: str, agent_ready: None
) -> None:
    # IDs we never print: reasonably high number, unlikely to collide with the 24h store.
    r = http.post(f"{agent_base_url}/api/orders/999999999/reprint", timeout=5)
    assert r.status_code == 404


def test_reprint_bad_id_returns_400(
    http: requests.Session, agent_base_url: str, agent_ready: None
) -> None:
    r = http.post(f"{agent_base_url}/api/orders/not-a-number/reprint", timeout=5)
    assert r.status_code == 400


def test_reprint_wrong_method_returns_405(
    http: requests.Session, agent_base_url: str, agent_ready: None
) -> None:
    r = http.get(f"{agent_base_url}/api/orders/1/reprint", timeout=5)
    assert r.status_code == 405


def test_unknown_path_returns_404(
    http: requests.Session, agent_base_url: str, agent_ready: None
) -> None:
    r = http.get(f"{agent_base_url}/api/orders/123/notarealaction", timeout=5)
    assert r.status_code == 404
