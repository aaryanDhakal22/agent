"""A driver made in main's Store › Staff logs in on a terminal as a driver (owner 2026-11-13).

The whole real path, no fake main: a throwaway store on dev main, its people added the way the owner (and
deploy/stores/add_staff.py) adds them (POST /store/v1/me/staff {first_name, last_name, role, pin}), a throwaway
agent linked to it that takes them from main's GET /store/v1/agent/terminal-config, a paired terminal. The
driver's PIN answers role driver (top level and staff.role, plus his roster driver), and the Pi refuses him
everything of the store's (403 driver_not_allowed: order taking, lists, customers) while his own routes answer;
a cashier on the same terminal after him is unchanged (contract §2.10b "A driver's login").
"""

from __future__ import annotations

import requests

from conftest import STORE_V1, TermSession, manager_session, provision, wait_until


def test_driver_from_main_staff_logs_in_as_a_driver(admin, main_store, spawn_agent):
    mgr = manager_session(admin, main_store)
    for body in ({"first_name": "Dara", "last_name": "Cash", "role": "cashier", "pin": "41"},
                 {"first_name": "Abdul", "last_name": "Drive", "role": "driver", "pin": "52"}):
        r = mgr.post(f"{STORE_V1}/me/staff", json=body, timeout=10)
        assert r.status_code == 201 and r.json()["role"] == body["role"], r.text

    agent = spawn_agent()
    provision(agent, main_store)
    code = agent.http.post(f"{agent.base}/api/pairing/code", timeout=5).json()["code"]
    r = requests.post(f"{agent.base}/api/pair", json={"code": code, "name": "Till 1", "kind": "terminal"}, timeout=5)
    assert r.status_code == 201, r.text
    term = TermSession()
    term.headers["X-Agent-Device-Key"] = r.json()["key"]
    base = agent.base

    # The Pi has main's staff once the driver's PIN logs in.
    login = {}

    def driver_in() -> bool:
        r = term.post(f"{base}/api/terminal/login", json={"pin": "52"}, timeout=5)
        if r.status_code == 200:
            login.update(r.json())
        return r.status_code == 200

    wait_until(driver_in, timeout=30, what="terminal config from main + the driver's login")
    assert login["role"] == "driver" and login["staff"]["role"] == "driver", login
    assert login["staff"]["first_name"] == "Abdul" and login["staff"]["name"] == "Abdul D.", login
    assert login.get("driver", {}).get("name") == "Abdul D.", "his roster driver comes with the login"
    assert term.get(f"{base}/api/terminal/session", timeout=5).json()["staff"]["role"] == "driver"

    # Nothing of the store's.
    for method, path, body in (("POST", "/api/terminal/quote", {"type": "pickup", "cart": {"lines": []}}),
                               ("POST", "/api/terminal/orders", {"client_key": "k-driver", "type": "counter", "cart": {"lines": []}}),
                               ("GET", "/api/terminal/pickups", None),
                               ("GET", "/api/terminal/drafts", None),
                               ("GET", "/api/terminal/checks", None),
                               ("GET", "/api/terminal/customers?phone=410", None),
                               ("POST", "/api/terminal/refunds", {"client_key": "k-r", "amount_cents": 100, "reason": "x"})):
        r = term.request(method, f"{base}{path}", json=body, timeout=5)
        assert r.status_code == 403 and r.json()["code"] == "driver_not_allowed", (method, path, r.status_code, r.text)
    # His own routes answer.
    r = term.get(f"{base}/api/terminal/driver/mine", timeout=5)
    assert r.status_code == 200 and r.json()["driver"]["name"] == "Abdul D.", r.text
    assert term.get(f"{base}/api/terminal/driver/report", timeout=5).status_code == 200

    # A cashier after him: the store's routes again.
    assert term.post(f"{base}/api/terminal/logout", timeout=5).status_code == 204
    r = term.post(f"{base}/api/terminal/login", json={"pin": "41"}, timeout=5)
    assert r.status_code == 200 and r.json()["role"] == "cashier", r.text
    r = term.get(f"{base}/api/terminal/pickups", timeout=5)
    assert r.status_code == 200, r.text
