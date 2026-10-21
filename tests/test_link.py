"""Manager-login link (headless SD-card recovery) on a throwaway agent: the
store manager login links a blank agent (no admin token), pairs the calling
tablet, and from then on unlocks the web UI; the local-password routes step
aside. The full mid-shift swap (restore) is in the root E2E suite."""

from __future__ import annotations

import secrets

import requests

from conftest import ADMIN_V1, wait_until


def _manager(admin, store) -> tuple[str, str]:
    user, pw = f"mgr{secrets.token_hex(3)}", "pw-link"
    r = admin.patch(f"{ADMIN_V1}/tenants/{store.tenant_id}/credentials", json={"username": user, "password": pw}, timeout=5)
    assert r.status_code == 204, r.text
    return user, pw


def test_link_with_manager_login(admin, main_store, blank_agent) -> None:
    a = blank_agent
    user, pw = _manager(admin, main_store)
    s = a.session()
    assert s["state"] == "unconfigured" and s["link_available"] is True and s["manager_login"] is False

    r = requests.post(f"{a.base}/api/setup/link", json={"username": user, "password": ""}, timeout=10)
    assert r.status_code == 400 and r.json()["code"] == "credentials_required"
    r = requests.post(f"{a.base}/api/setup/link", json={"username": user, "password": "nope"}, timeout=30)
    assert r.status_code == 401 and r.json()["code"] == "invalid_credentials"
    assert a.session()["state"] == "unconfigured", "a failed link stores nothing"

    r = requests.post(f"{a.base}/api/setup/link", json={"username": user, "password": pw, "tablet_name": "Counter tablet"},
                      timeout=60)
    assert r.status_code == 200, r.text
    body = r.json()
    assert body["device_key"].startswith("qagt_") and body["device_name"] == "Counter tablet"
    assert body["restored"]["orders"] == 0 and body["restored"]["kitchen_next"] == 1100
    tablet = {"X-Agent-Device-Key": body["device_key"]}
    assert requests.get(f"{a.base}/api/floor", headers=tablet, timeout=5).status_code == 200

    wait_until(lambda: a.session()["sse_state"] == "connected", timeout=20, what="SSE connected with the minted key")
    s = a.session()
    assert s["state"] == "locked" and s["manager_login"] is True, "linked from a tablet: the web UI is still locked"

    # Already linked: neither setup path runs again.
    r = requests.post(f"{a.base}/api/setup/link", json={"username": user, "password": pw}, timeout=10)
    assert r.status_code == 409 and r.json()["code"] == "already_provisioned"
    assert requests.post(f"{a.base}/api/setup/token", json={"token": "x"}, timeout=10).status_code == 409

    # The web UI unlocks with the manager login (username checked, case-insensitive).
    assert a.http.post(f"{a.base}/api/login", json={"username": "other", "password": pw}, timeout=10).status_code == 401
    assert a.http.post(f"{a.base}/api/login", json={"username": user.upper(), "password": pw}, timeout=10).status_code == 200
    assert a.state(a.http) == "ready"
    r = a.http.post(f"{a.base}/api/settings/password", json={"current": pw, "new": "x", "confirm": "x"}, timeout=10)
    assert r.status_code == 409 and r.json()["code"] == "manager_login"

    # A changed manager password in main reaches the agent on the next login.
    pw2 = "pw-link-2"
    assert admin.patch(f"{ADMIN_V1}/tenants/{main_store.tenant_id}/credentials",
                       json={"username": user, "password": pw2}, timeout=5).status_code == 204
    fresh = requests.Session()
    assert fresh.post(f"{a.base}/api/login", json={"username": user, "password": pw2}, timeout=15).status_code == 200


def test_second_link_revokes_the_first_agent(admin, main_store, spawn_agent) -> None:
    user, pw = _manager(admin, main_store)
    first, second = spawn_agent(), spawn_agent()
    assert requests.post(f"{first.base}/api/setup/link", json={"username": user, "password": pw}, timeout=60).status_code == 200
    wait_until(lambda: first.session()["sse_state"] == "connected", timeout=20, what="first agent connected")
    r = requests.post(f"{second.base}/api/setup/link", json={"username": user, "password": pw}, timeout=60)
    assert r.status_code == 200 and r.json()["revoked_agents"] == 1, r.text
    # The replaced agent notices (its stream is dropped, reconnect → 401) and wipes itself.
    wait_until(lambda: first.session()["state"] == "unconfigured", timeout=45, what="old agent wiped itself")
