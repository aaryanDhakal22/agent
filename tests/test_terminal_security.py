"""The store-side security round (2026-11-12; contract/agent.md §2.10 "A staff login", §2.10s "PIN guard",
§2.10y "Pairing window" / "Admin mode").

  * H3: 200 wrong PINs at once over real HTTP are 5 PIN checks at most (the slot is taken before bcrypt)
  * H3: the daily ceiling locks every PIN check until a manager unlocks it (the Pi page)
  * C1b: a terminal's key alone reads none of the store's data and gets no Square sign-in
  * C1a: the first-setup window of a Pi that never paired a terminal
  * M7: the admin code checked by the Pi; a manager's PIN with nobody logged in for the device-level actions
  * M2: one approval per manager need
  * M1: a phone's "ok" with a transaction Square doesn't know stays unrecorded

Uses test_terminal.py's rig (a throwaway agent behind the fake main, fake printers only).
"""

from __future__ import annotations

import uuid
from concurrent.futures import ThreadPoolExecutor

import requests

from conftest import TermSession, provision
from test_phone_pay import _charge, _lookups, _phone
from test_terminal import pizza_line, place, rig  # noqa: F401  (rig is a fixture)


def _new_terminal(agent, name: str) -> TermSession:
    code = agent.http.post(f"{agent.base}/api/pairing/code", json={"kind": "terminal"}, timeout=5).json()["code"]
    r = requests.post(f"{agent.base}/api/pair", json={"code": code, "name": name, "kind": "terminal"}, timeout=5)
    assert r.status_code == 201, r.text
    t = TermSession()
    t.headers["X-Agent-Device-Key"] = r.json()["key"]
    return t


def test_parallel_wrong_pins_stop_at_the_limit(rig):  # noqa: F811
    agent = rig["agent"]
    base = agent.base
    till2 = _new_terminal(agent, "Till 2")
    key = till2.headers["X-Agent-Device-Key"]

    def guess(i: int) -> int:
        # One connection each, all at once: the throttle used to be read before bcrypt and counted after it.
        return requests.post(f"{base}/api/terminal/login", json={"pin": f"{7000 + i:04d}"},
                             headers={"X-Agent-Device-Key": key}, timeout=30).status_code

    with ThreadPoolExecutor(max_workers=100) as pool:
        codes = list(pool.map(guess, range(200)))
    checked, refused = codes.count(401), codes.count(429)
    assert checked + refused == 200, sorted(set(codes))
    assert 1 <= checked <= 5, f"{checked} PINs were checked at once (the limit is 5 per terminal)"
    guard = agent.http.get(f"{base}/api/pin-guard", timeout=5).json()
    assert guard["misses_24h"] == checked and guard["locked"] is False, guard
    # The right PIN is paused too now (the terminal's 5 misses).
    r = till2.post(f"{base}/api/terminal/login", json={"pin": "1234"}, timeout=5)
    assert r.status_code == 429 and int(r.headers["Retry-After"]) > 0, r.text


def test_daily_ceiling_locks_until_a_manager_unlocks(rig):  # noqa: F811
    agent, term = rig["agent"], rig["term"]
    base = agent.base
    assert agent.http.put(f"{base}/api/settings/pin-guard", json={"daily_ceiling": 9}, timeout=5).status_code == 400
    r = agent.http.put(f"{base}/api/settings/pin-guard", json={"daily_ceiling": 10}, timeout=5)
    assert r.status_code == 200 and r.json()["daily_ceiling"] == 10, r.text
    # Ten wrong PINs over three terminals, 4 + 4 + 2: nobody trips the per-terminal pause (5).
    tills = [_new_terminal(agent, f"Till x{i}") for i in range(3)]
    for i, t in enumerate([tills[0]] * 4 + [tills[1]] * 4 + [tills[2]] * 2):
        r = t.post(f"{base}/api/terminal/login", json={"pin": "0000"}, timeout=5)
        assert r.status_code == 401, (i, r.text)
    r = tills[0].post(f"{base}/api/terminal/login", json={"pin": "1234"}, timeout=5)
    assert r.status_code == 403 and r.json()["code"] == "pin_locked" and r.json()["locked_at"], r.text
    # The cashier already logged in keeps working; her manager PIN prompts are locked too.
    assert place(rig).status_code == 201
    r = term.post(f"{base}/api/terminal/manager-check", json={"pin": "9876"}, timeout=5)
    assert r.status_code == 403 and r.json()["code"] == "pin_locked", r.text
    guard = term.get(f"{base}/api/pin-guard", timeout=5).json()
    assert guard["locked"] and guard["misses_24h"] == 10 and guard["daily_ceiling"] == 10, guard
    assert "pin_locked" in agent.http.get(f"{base}/api/diag", timeout=5).json()["problems"]
    # A terminal can't unlock itself; the Pi page (the store manager login) does.
    assert term.post(f"{base}/api/pin-guard/unlock", json={}, timeout=5).json()["code"] == "terminal_only"
    r = agent.http.post(f"{base}/api/pin-guard/unlock", timeout=5)
    assert r.status_code == 200 and r.json()["locked"] is False and r.json()["misses_24h"] == 0, r.text
    assert tills[0].post(f"{base}/api/terminal/login", json={"pin": "1234"}, timeout=5).status_code == 200
    actions = [e["action"] for e in agent.http.get(f"{base}/api/terminal/journal?limit=100", timeout=5).json()]
    assert "pin.locked" in actions and "pin.unlocked" in actions


def test_a_terminal_key_alone_reads_nothing(rig):  # noqa: F811
    agent, term = rig["agent"], rig["term"]
    base = agent.base
    o = place(rig).json()["order"]["order"]
    bare = _new_terminal(agent, "Till bare")  # paired, nobody logged in
    for method, path in [("GET", "/api/terminal/customers?phone=410"), ("GET", "/api/terminal/orders"),
                         ("GET", f"/api/terminal/orders/{o['id']}"), ("GET", "/api/terminal/journal"),
                         ("GET", "/api/terminal/drafts"), ("GET", "/api/terminal/unsettled"), ("GET", "/api/terminal/pickups"),
                         ("GET", "/api/terminal/z"), ("GET", "/api/terminal/checks"), ("GET", "/api/terminal/reports/till"),
                         ("GET", "/api/terminal/square/session"),
                         ("POST", "/api/terminal/customers")]:
        r = bare.request(method, base + path, json={} if method == "POST" else None, timeout=5)
        assert r.status_code == 401 and r.json()["code"] == "login_required", (method, path, r.status_code, r.text)
    # What the locked terminal shows still answers.
    for path in ("/api/terminal/session", "/api/terminal/menu", "/api/terminal/status", "/api/terminal/setup"):
        assert bare.get(base + path, timeout=5).status_code == 200, path
    # Logged in, the same key reads them; the Pi page (the store manager login) too.
    assert bare.post(f"{base}/api/terminal/login", json={"pin": "1234"}, timeout=5).status_code == 200
    assert any(x["order"]["id"] == o["id"] for x in bare.get(f"{base}/api/terminal/orders", timeout=5).json())
    assert agent.http.get(f"{base}/api/terminal/journal?limit=5", timeout=5).status_code == 200
    # Logged out again (the terminal locked): closed again.
    bare.post(f"{base}/api/terminal/logout", timeout=5)
    assert bare.get(f"{base}/api/terminal/orders", timeout=5).status_code == 401


def test_first_setup_window(admin, main_store, spawn_agent):
    """A linked Pi that never paired a terminal: the admin code works until the first terminal pairs."""
    agent = spawn_agent()
    provision(agent, main_store)
    base = agent.base
    w = agent.http.get(f"{base}/api/pairing/window", timeout=5).json()
    assert w["open"] and w["reason"] == "first_setup" and w["default_code_ok"] and "until" not in w, w
    r = requests.post(f"{base}/api/pair/admin", json={"code": "1024", "name": "Counter 1"}, timeout=5)
    assert r.status_code == 201, r.text
    assert agent.http.get(f"{base}/api/pairing/window", timeout=5).json()["open"] is False
    r = requests.post(f"{base}/api/pair/admin", json={"code": "1024", "name": "Counter 2"}, timeout=5)
    assert r.status_code == 403 and r.json()["code"] == "pairing_closed", r.text
    # The first terminal revoked doesn't bring the first setup back.
    dev = next(d for d in agent.http.get(f"{base}/api/devices", timeout=5).json() if d["name"] == "Counter 1")
    assert agent.http.post(f"{base}/api/devices/{dev['id']}/revoke", timeout=5).status_code == 204
    assert requests.post(f"{base}/api/pair/admin", json={"code": "1024", "name": "Counter 2"}, timeout=5).json()["code"] == "pairing_closed"


def test_admin_code_check_and_device_level_manager_pin(rig):  # noqa: F811
    agent = rig["agent"]
    base = agent.base
    bare = _new_terminal(agent, "Till admin")  # nobody logged in: admin mode's state
    assert requests.post(f"{base}/api/admin-code/check", json={"code": "1024"}, timeout=5).status_code == 401
    assert bare.post(f"{base}/api/admin-code/check", json={"code": "1024"}, timeout=5).json() == {"ok": True}
    r = bare.post(f"{base}/api/admin-code/check", json={"code": "4321"}, timeout=5)
    assert r.status_code == 401 and r.json()["code"] == "wrong_admin_code", r.text
    assert agent.http.put(f"{base}/api/settings/admin-code", json={"code": "246810"}, timeout=5).status_code == 200
    assert bare.post(f"{base}/api/admin-code/check", json={"code": "1024"}, timeout=5).status_code == 401
    assert bare.post(f"{base}/api/admin-code/check", json={"code": "246810"}, timeout=5).status_code == 200
    # A manager's PIN with nobody logged in: only for admin mode's drawer test, Unpair and the pairing window.
    for action in ("admin_drawer_test", "unpair", "pairing_window"):
        r = bare.post(f"{base}/api/terminal/manager-check", json={"pin": "9876", "action": action}, timeout=5)
        assert r.status_code == 200 and r.json()["staff"]["role"] == "manager", (action, r.text)
    r = bare.post(f"{base}/api/terminal/manager-check", json={"pin": "1234", "action": "unpair"}, timeout=5)
    assert r.status_code == 401 and r.json()["code"] == "wrong_pin", r.text
    r = bare.post(f"{base}/api/terminal/manager-check", json={"pin": "9876", "action": "refund"}, timeout=5)
    assert r.status_code == 401 and r.json()["code"] == "login_required", r.text
    # The paired terminal opens the pairing window with that manager's approval.
    ap = bare.post(f"{base}/api/terminal/manager-check", json={"pin": "9876", "action": "pairing_window"}, timeout=5).json()["approval_id"]
    r = bare.post(f"{base}/api/terminal/pairing/window", json={"approvals": [ap]}, timeout=5)
    assert r.status_code == 401 and r.json()["code"] == "login_required", r.text  # the terminal's own route needs a login
    assert bare.post(f"{base}/api/terminal/login", json={"pin": "1234"}, timeout=5).status_code == 200
    r = bare.post(f"{base}/api/terminal/pairing/window", json={"approvals": [ap]}, timeout=5)
    assert r.status_code == 200 and r.json()["open"] and r.json()["reason"] == "manager", r.text
    assert requests.post(f"{base}/api/pair/admin", json={"code": "246810", "name": "Counter 9"}, timeout=5).status_code == 201


def test_one_approval_per_manager_need(rig):  # noqa: F811
    agent, term = rig["agent"], rig["term"]
    base = agent.base
    cart = {"lines": [{**pizza_line(), "price_cents": 1000}], "discount": {"pct": 30, "reason": "regular"}}

    def approval(action: str) -> str:
        r = term.post(f"{base}/api/terminal/manager-check", json={"pin": "9876", "action": action}, timeout=5)
        assert r.status_code == 200, r.text
        return r.json()["approval_id"]

    def send(approvals: list[str]) -> requests.Response:
        return term.post(f"{base}/api/terminal/orders", json={"client_key": f"k-{uuid.uuid4()}", "type": "counter", "cart": cart,
                                                               "approvals": approvals}, timeout=10)

    disc = approval("discount")
    r = send([disc])  # the discount's approval used to cover the price drop too
    assert r.status_code == 403 and r.json()["code"] == "manager_required" and r.json()["action"] == "price_change", r.text
    r = send([disc, approval("price_change")])  # nothing was used up by the refusal
    assert r.status_code == 201, r.text
    assert send([disc]).status_code == 403  # single use


def test_a_forged_phone_result_is_not_a_payment(rig):  # noqa: F811
    agent, term, fm = rig["agent"], rig["term"], rig["fm"]
    base = agent.base
    _lookups(fm, found=False)  # Square (asked through main) doesn't know the transaction
    phone = _phone(rig)
    rig["phone_id"] = phone.get(f"{base}/api/phone/me", timeout=5).json()["phone"]["id"]
    o = place(rig).json()
    oid, total = o["order"]["order"]["id"], o["order"]["order"]["money"]["total_cents"]
    c = _charge(rig, oid, total).json()["charge"]
    phone.post(f"{base}/api/phone/charges/{c['id']}/open", json={}, timeout=5)
    r = phone.post(f"{base}/api/phone/charges/{c['id']}/result",
                   json={"status": "ok", "transaction_id": "made-up-tx", "client_transaction_id": "made-up-ctx"}, timeout=20)
    assert r.status_code == 200 and r.json()["charge"]["status"] == "unrecorded", r.text
    view = term.get(f"{base}/api/terminal/orders/{oid}", timeout=5).json()
    assert view["payments"] == [] and not view["order"]["status"].get("paid"), view
    r = term.post(f"{base}/api/terminal/phone-charges/{c['id']}/record", json={}, timeout=15)
    assert r.status_code == 409 and r.json()["code"] == "not_found_in_square", r.text
