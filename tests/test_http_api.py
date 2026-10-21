"""The agent's HTTP surface on a blank (unprovisioned) throwaway agent: public
routes, the device-gated floor routes, session-gated web-UI routes, CORS, and
the pre-provisioning guards."""

import requests


def test_version_is_public(blank_agent) -> None:
    body = requests.get(f"{blank_agent.base}/api/version", timeout=5).json()
    assert body["service"] == "quicc-agent" and "version" in body and "commit" in body


def test_blank_agent_session_state(blank_agent) -> None:
    s = blank_agent.session()
    assert s["state"] == "unconfigured"
    assert s["sse_state"] == "unconfigured"
    assert s["sse_connected"] is False
    assert s["has_hint"] is False


def test_ui_is_served_at_root(blank_agent) -> None:
    r = requests.get(f"{blank_agent.base}/", timeout=5)
    assert r.status_code == 200 and r.headers["Content-Type"].startswith("text/html")
    assert "Quicc POS" in r.text


def test_floor_routes_need_a_device_key_or_session(blank_agent) -> None:
    """Orders carry customer names/phones/addresses: no longer LAN-open."""
    for method, path in [("GET", "/api/orders"), ("GET", "/api/orders/arrived"), ("GET", "/api/printers"),
                         ("GET", "/api/floor"), ("GET", "/api/drivers"), ("GET", "/api/events"),
                         ("POST", "/api/batch/close"), ("POST", "/api/deliveries/assign"),
                         ("POST", "/api/orders/00000000-0000-0000-0000-000000000001/kitchen")]:
        r = requests.request(method, f"{blank_agent.base}{path}", timeout=5)
        assert r.status_code == 401 and r.json()["code"] == "device_required", (method, path, r.status_code)
    bad = requests.get(f"{blank_agent.base}/api/orders", headers={"X-Agent-Device-Key": "qagt_nope"}, timeout=5)
    assert bad.status_code == 401


def test_web_routes_require_session(blank_agent) -> None:
    for method, path in [("GET", "/api/history"), ("GET", "/api/store-config"),
                         ("GET", "/api/settings/auto-accept"), ("POST", "/api/print"),
                         ("POST", "/api/settings/unlink"), ("POST", "/api/pairing/code"), ("GET", "/api/devices")]:
        r = requests.request(method, f"{blank_agent.base}{path}", timeout=5)
        assert r.status_code == 401, (method, path, r.status_code)


def test_login_before_provisioning_409(blank_agent) -> None:
    r = requests.post(f"{blank_agent.base}/api/login", json={"password": "x"}, timeout=5)
    assert r.status_code == 409 and r.json()["code"] == "not_provisioned"


def test_set_password_before_token_409(blank_agent) -> None:
    r = requests.post(f"{blank_agent.base}/api/setup/password", json={"password": "a", "confirm": "a"}, timeout=5)
    assert r.status_code == 409 and r.json()["code"] == "not_provisioned"


def test_cors_preflight(blank_agent) -> None:
    r = requests.options(f"{blank_agent.base}/api/orders", timeout=5)
    assert r.status_code == 204
    assert r.headers.get("Access-Control-Allow-Origin") == "*"
    assert "GET" in (r.headers.get("Access-Control-Allow-Methods") or "")


def test_unknown_api_get_404(blank_agent) -> None:
    assert requests.get(f"{blank_agent.base}/api/orders/1/reprint", timeout=5).status_code == 404


def test_routes_table_is_served(blank_agent) -> None:
    body = requests.get(f"{blank_agent.base}/__routes", timeout=5).json()
    assert body["module"] == "agent"
    paths = {r["path"] for r in body["routes"]}
    assert {"/api/session", "/api/setup/token", "/api/orders"} <= paths, paths


def test_store_config_is_read_only(blank_agent) -> None:
    """The agent only displays store config; edits happen in main's /app/store."""
    r = requests.post(f"{blank_agent.base}/api/store-config", json={}, timeout=5)
    assert r.status_code in (404, 405)
