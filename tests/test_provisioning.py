"""The agent's provisioning state machine, driven end-to-end against real main:

    unconfigured → (paste token) → needs_password → (set password) → locked
                 → (login) → ready → (logout) → locked

plus password rotation, unlink (self-revoke + wipe) and the auto-wipe when an
admin revokes the device from main's dashboard.
"""

import pytest
import requests

from conftest import ADMIN_V1, wait_until


def _post(agent, path: str, body: dict | None = None, client: requests.Session | None = None):
    return (client or requests).post(f"{agent.base}{path}", json=body or {}, timeout=10)


def _provision(agent, main_store, password: str = "pw-1", hint: str = "first pet") -> dict:
    dev = main_store.mint()
    assert _post(agent, "/api/setup/token", {"token": dev["token"]}).status_code == 200
    r = _post(agent, "/api/setup/password", {"password": password, "confirm": password, "hint": hint})
    assert r.status_code == 200, r.text
    return dev


def test_full_lifecycle(blank_agent, main_store) -> None:
    a = blank_agent
    assert a.state() == "unconfigured"

    # -- token -------------------------------------------------------------
    r = _post(a, "/api/setup/token", {"token": ""})
    assert r.status_code == 400 and r.json()["code"] == "token_required"
    r = _post(a, "/api/setup/token", {"token": "qpos_not-a-real-token"})
    assert r.status_code == 401 and r.json()["code"] == "invalid_token"
    assert a.state() == "unconfigured"

    dev = main_store.mint()
    r = _post(a, "/api/setup/token", {"token": dev["token"]})
    assert r.status_code == 200, r.text
    assert a.state() == "needs_password"
    wait_until(lambda: a.session()["sse_state"] == "connected", timeout=20, what="SSE to connect after linking")

    r = _post(a, "/api/setup/token", {"token": dev["token"]})
    assert r.status_code == 409 and r.json()["code"] == "already_provisioned"

    # -- password ----------------------------------------------------------
    r = _post(a, "/api/setup/password", {"password": "", "confirm": ""})
    assert r.status_code == 400 and r.json()["code"] == "password_required"
    r = _post(a, "/api/setup/password", {"password": "abc", "confirm": "abd"})
    assert r.status_code == 400 and r.json()["code"] == "password_mismatch"
    r = _post(a, "/api/setup/password", {"password": "pw-1", "confirm": "pw-1", "hint": "first pet"})
    assert r.status_code == 200, r.text
    r = _post(a, "/api/setup/password", {"password": "x", "confirm": "x"})
    assert r.status_code == 409 and r.json()["code"] == "password_already_set"
    assert a.state() == "locked"
    assert a.session()["has_hint"] is True
    assert requests.get(f"{a.base}/api/hint", timeout=5).json()["hint"] == "first pet"

    # -- login / gated routes ------------------------------------------------
    r = _post(a, "/api/login", {"password": "wrong"})
    assert r.status_code == 401 and r.json()["code"] == "invalid_password"
    ui = requests.Session()
    assert _post(a, "/api/login", {"password": "pw-1"}, client=ui).status_code == 200
    assert a.state(ui) == "ready"
    sc = ui.get(f"{a.base}/api/store-config", timeout=10)  # proxied to main with the stored key
    assert sc.status_code == 200, sc.text
    assert sc.json()["location"]["id"] == main_store.location_id

    # -- rotate password -----------------------------------------------------
    r = _post(a, "/api/settings/password", {"current": "nope", "new": "pw-2", "confirm": "pw-2"}, client=ui)
    assert r.status_code == 401
    r = _post(a, "/api/settings/password", {"current": "pw-1", "new": "pw-2", "confirm": "pw-2"}, client=ui)
    assert r.status_code == 200, r.text
    assert _post(a, "/api/login", {"password": "pw-1"}).status_code == 401
    assert _post(a, "/api/login", {"password": "pw-2"}).status_code == 200

    # -- logout ---------------------------------------------------------------
    assert _post(a, "/api/logout", client=ui).status_code in (200, 204)
    assert a.state(ui) == "locked"
    assert ui.get(f"{a.base}/api/store-config", timeout=5).status_code == 401


def test_unlink_revokes_in_main_and_wipes(blank_agent, main_store) -> None:
    a = blank_agent
    dev = _provision(a, main_store)
    ui = requests.Session()
    assert _post(a, "/api/login", {"password": "pw-1"}, client=ui).status_code == 200

    r = _post(a, "/api/settings/unlink", client=ui)
    assert r.status_code == 200, r.text
    assert a.state() == "unconfigured"
    assert main_store.device(dev["device_id"]).get("revoked_at"), "unlink must revoke the device in main"


@pytest.mark.slow
def test_dashboard_revoke_auto_wipes_agent(blank_agent, main_store) -> None:
    """Admin revokes the device in main → the agent's next agent→main call (the
    ~30s printer-report heartbeat) gets 401 → it wipes back to `unconfigured`."""
    a = blank_agent
    dev = _provision(a, main_store)
    wait_until(lambda: a.session()["sse_state"] == "connected", timeout=20, what="SSE connected")

    r = main_store.admin.post(f"{ADMIN_V1}/devices/{dev['device_id']}/revoke", timeout=5)
    assert r.status_code == 204, r.text

    wait_until(lambda: a.state() == "unconfigured", timeout=45, interval=1,
               what=f"agent auto-wipe after revoke\n--- agent log ---\n{a.log_tail()}")
    s = a.session()
    assert s["sse_state"] == "unconfigured"
