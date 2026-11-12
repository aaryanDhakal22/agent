"""The terminal's driver view (contract/agent.md §2.10b): /api/terminal/driver/*.

A throwaway agent behind test_terminal's FAKE MAIN (dev main for everything
else), fake printers only. A cashier places counter deliveries on one
terminal; drivers log in with their own PIN on others and claim (Dispatch:
deliveries nobody took once past their promised time become theirs,
delivered), see their tickets, tip, close and print their report. The helper
places deliveries quoted 0 minutes (prep_minutes 0) so they are past their
promised time at once; the clock moving past a later promised time is covered
by the Go tests (internal/application/terminal/driver_test.go). The batch auto-close (§2.10c) is
covered by the Go tests with an injected clock
(internal/application/order/autoclose_test.go).
"""

from __future__ import annotations

import threading
import uuid
from datetime import datetime

import pytest
import requests

from conftest import MAIN_BASE_URL, STORE_V1, FakePrinter, TermSession, manager_session, provision, wait_until
from test_terminal import FakeMain, counter_menu, pizza_line, staff_config

# bcrypt (cost 4) of the drivers' PINs, made with the agent's golang.org/x/crypto/bcrypt.
DRIVER_HASHES = {"5511": "$2a$04$YucRH1Wg.1UMR/8ccruyweBklSB5AW6T5DkOe9pnGX6aAN5HETHpm",
                 "6622": "$2a$04$TIEmQPTjtxNku3EImy1N4eN4VqYEl3cddctcFkazy.mN0vGacXlAa"}


def driver_config(version: int = 1) -> dict:
    cfg = staff_config(version=version)
    cfg["staff"] += [{"id": "00000000-0000-4000-8000-0000000000d1", "first_name": "Ali", "last_name": "Khan", "role": "driver", "pin_hash": DRIVER_HASHES["5511"]},
                     {"id": "00000000-0000-4000-8000-0000000000d2", "first_name": "Sam", "last_name": "", "role": "driver", "pin_hash": DRIVER_HASHES["6622"]}]
    return cfg


def pair_terminal(agent, name: str) -> requests.Session:
    code = agent.http.post(f"{agent.base}/api/pairing/code", timeout=5).json()["code"]
    r = requests.post(f"{agent.base}/api/pair", json={"code": code, "name": name, "kind": "terminal"}, timeout=5)
    assert r.status_code == 201, r.text
    s = TermSession()
    s.headers["X-Agent-Device-Key"] = r.json()["key"]
    return s


@pytest.fixture
def drig(admin, main_store, spawn_agent, fakes):
    """A linked agent behind the fake main; Front (receipt) + Pizza printers
    (fake); a cashier terminal and two driver terminals, logged in."""
    fm = FakeMain()
    mgr = manager_session(admin, main_store)
    fakes["Pizza"], fakes["Front"] = FakePrinter(), FakePrinter()
    r = mgr.post(f"{STORE_V1}/me/printers", json={"name": "Front", "ip": fakes["Front"].addr}, timeout=5)
    assert r.status_code == 201 and r.json()["is_receipt"], r.text
    r = mgr.post(f"{STORE_V1}/me/printers", json={"name": "Pizza", "ip": fakes["Pizza"].addr}, timeout=5)
    assert r.status_code == 201, r.text
    fm.counter_menu, fm.terminal_config = counter_menu(r.json()["id"]), driver_config()
    agent = spawn_agent(MAIN_SERVER_URL=fm.url)
    provision(agent, main_store)
    wait_until(lambda: len(agent.http.get(f"{agent.base}/api/diag", timeout=5).json()["printers"]) == 2, what="printers from store_config")
    till, ali, sam = pair_terminal(agent, "Till 1"), pair_terminal(agent, "Driver 1"), pair_terminal(agent, "Driver 2")
    wait_until(lambda: till.get(f"{agent.base}/api/terminal/menu", timeout=5).status_code == 200, timeout=30, what="counter menu")
    wait_until(lambda: till.post(f"{agent.base}/api/terminal/login", json={"pin": "1234"}, timeout=5).status_code == 200,
               timeout=30, what="terminal config + cashier login")
    logins = {}
    for s, pin in ((ali, "5511"), (sam, "6622")):
        r = s.post(f"{agent.base}/api/terminal/login", json={"pin": pin}, timeout=5)
        assert r.status_code == 200 and r.json()["role"] == "driver", r.text
        logins[pin] = r.json()
    yield {"agent": agent, "till": till, "ali": ali, "sam": sam, "fm": fm, "fakes": fakes, "logins": logins}
    fm.stop()


def delivery(drig, *, ready: bool = True, paid: bool = False, prep_minutes: int = 0) -> str:
    agent, till = drig["agent"], drig["till"]
    body = {"client_key": f"k-{uuid.uuid4()}", "type": "delivery", "cart": {"lines": [pizza_line()]}, "prep_minutes": prep_minutes,
            "customer": {"name": "Erik Davis", "phone": "4105550101",
                         "address": {"line1": "12 Main St, Apt 3", "city": "Towson", "zip": "21286", "notes": "ring twice"}}}
    if paid:
        q = till.post(f"{agent.base}/api/terminal/quote", json={"type": "delivery", "cart": body["cart"]}, timeout=5).json()
        body["payments"] = [{"tender": "square_app", "amount_cents": q["quote"]["total_cents"]}]
    r = till.post(f"{agent.base}/api/terminal/orders", json=body, timeout=10)
    assert r.status_code == 201, r.text
    oid = r.json()["order"]["order"]["id"]
    if ready:
        assert agent.http.post(f"{agent.base}/api/orders/{oid}/ready", json={}, timeout=5).status_code == 200
    return oid


def test_link_at_login_and_roles(drig):
    agent, till, ali = drig["agent"], drig["till"], drig["ali"]
    base = agent.base
    d1, d2 = drig["logins"]["5511"]["driver"], drig["logins"]["6622"]["driver"]
    assert d1["name"] == "Ali K." and d1["status"] == "off" and d2["name"] == "Sam" and d1["id"] != d2["id"]
    roster = {d["id"]: d["name"] for d in agent.http.get(f"{base}/api/drivers", timeout=5).json()}
    assert roster[d1["id"]] == "Ali K." and roster[d2["id"]] == "Sam"
    # Logging in again keeps the same Pi driver (keyed by staff id).
    again = ali.post(f"{base}/api/terminal/login", json={"pin": "5511"}, timeout=5).json()
    assert again["driver"]["id"] == d1["id"]
    # A cashier (or manager) gets 403 driver_only on every driver route.
    for method, path in (("GET", "available"), ("GET", "mine"), ("POST", "claim"), ("GET", "report"), ("POST", "report")):
        r = till.request(method, f"{base}/api/terminal/driver/{path}", json={"order_ids": [str(uuid.uuid4())]}, timeout=5)
        assert r.status_code == 403 and r.json()["code"] == "driver_only", (path, r.text)
    # Locked: 401.
    till.post(f"{base}/api/terminal/logout", timeout=5)
    assert till.get(f"{base}/api/terminal/driver/available", timeout=5).json()["code"] == "login_required"


def test_claim_mine_tip_close_report(drig):
    agent, ali, sam, fm, fakes = drig["agent"], drig["ali"], drig["sam"], drig["fm"], drig["fakes"]
    base = agent.base
    cooking = delivery(drig, ready=False)
    cash = delivery(drig)
    paid = delivery(drig, paid=True)
    me = drig["logins"]["5511"]["driver"]["id"]
    sam_id = drig["logins"]["6622"]["driver"]["id"]
    # Dispatch: every delivery nobody took, cooking too, longest waiting first.
    avail = ali.get(f"{base}/api/terminal/driver/available", timeout=5).json()
    assert [a["order_id"] for a in avail] == [cooking, cash, paid]
    assert [a["state"] for a in avail] == ["accepted", "ready", "ready"]
    assert avail[1]["payment"] == "cash_due" and avail[1]["cash_due_cents"] > 0 and avail[1]["street"] == "12 Main St"
    # times compared as times: the agent trims trailing zeros ("…07.3Z" vs "…07.345Z" sorts wrong as text)
    assert avail[2]["payment"] == "paid" and avail[2]["cash_due_cents"] == 0 and avail[0]["waiting_minutes"] >= 0 and \
        datetime.fromisoformat(avail[0]["since"]) <= datetime.fromisoformat(avail[1]["since"])
    # The manager sends "paid" out with Sam: it is taken, all or nothing.
    assert agent.http.post(f"{base}/api/drivers/{sam_id}/clock-in", timeout=5).status_code == 200
    r = agent.http.post(f"{base}/api/drivers/{sam_id}/send-out", json={"include_order_ids": [paid]}, timeout=10)
    assert r.status_code == 200, r.text
    wait_until(lambda: any(e.get("driver_id") == sam_id for e in fm.of_type("delivery.run")), what="Sam's run synced to main")
    r = ali.post(f"{base}/api/terminal/driver/claim", json={"order_ids": [cash, paid]}, timeout=10)
    assert r.status_code == 409 and r.json()["code"] == "taken" and r.json()["order_ids"] == [paid], r.text
    assert ali.post(f"{base}/api/terminal/driver/claim", json={"order_ids": []}, timeout=5).status_code == 400

    # Claim = his and delivered; not a send-out: he stays off, no run.
    runs = len(fm.of_type("delivery.run"))
    r = ali.post(f"{base}/api/terminal/driver/claim", json={"order_ids": [cash, cooking]}, timeout=10)
    assert r.status_code == 200, r.text
    res = r.json()
    assert res["driver"]["id"] == me and res["driver"]["status"] == "off"
    assert [o["order"]["id"] for o in res["orders"]] == [cash, cooking]
    assert all(o["status"]["state"] == "delivered" and o["status"]["driver_id"] == me and not o["status"].get("out_at")
               for o in res["orders"])
    drivers = {d["id"]: d for d in agent.http.get(f"{base}/api/drivers", timeout=5).json()}
    assert drivers[me]["status"] == "off"
    wait_until(lambda: {e["order_id"] for e in fm.of_type("order.status")
                        if e.get("status") == "delivered" and e.get("driver_name") == "Ali K."} >= {cash, cooking},
               what="order.status delivered by him to main")
    assert len(fm.of_type("delivery.run")) == runs
    # Gone from everyone's list; a second claim of the same order is taken.
    assert sam.get(f"{base}/api/terminal/driver/available", timeout=5).json() == []
    r = sam.post(f"{base}/api/terminal/driver/claim", json={"order_ids": [cash]}, timeout=5)
    assert r.status_code == 409 and r.json()["code"] == "taken"
    # No unassign route for a driver.
    assert ali.post(f"{base}/api/terminal/driver/orders/{cash}/unassign", timeout=5).status_code in (404, 405)

    mine = ali.get(f"{base}/api/terminal/driver/mine", timeout=5).json()
    assert mine["out"] == [] and {t["order_id"] for t in mine["delivered"]} == {cash, cooking}
    t = next(t for t in mine["delivered"] if t["order_id"] == cash)
    assert t["name"] == "Erik Davis" and t["phone"] == "4105550101" and t["address"]["line1"] == "12 Main St, Apt 3"
    assert t["address"]["notes"] == "ring twice" and t["payment"] == "cash_due" and t["tip_kind"] == "none" and "door_tip_cents" not in t and "geo" in t
    # Someone else's order: 403.
    r = sam.post(f"{base}/api/terminal/driver/orders/{cash}/tip", json={"tip_cents": 100}, timeout=5)
    assert r.status_code == 403 and r.json()["code"] == "not_your_order"
    # Tip: a counter (cash) order takes none: cash tips are kept out of the system (the owner, 2026-11-12).
    r = ali.post(f"{base}/api/terminal/driver/orders/{cash}/tip", json={"tip_cents": 400}, timeout=5)
    assert r.status_code == 409 and r.json()["code"] == "not_tippable", r.text
    assert ali.post(f"{base}/api/terminal/driver/orders/{cash}/tip", json={"tip_cents": -1}, timeout=5).status_code == 400
    # A claimed order is closed already.
    r = ali.post(f"{base}/api/terminal/driver/orders/{cash}/close", timeout=5)
    assert r.status_code == 409 and r.json()["code"] == "invalid_state"
    # Close still works for an order he was sent out with: Sam's last stop returns him.
    r = sam.post(f"{base}/api/terminal/driver/orders/{paid}/close", timeout=10).json()
    assert r["returned"] and r["driver"]["status"] == "available" and r["ticket"]["state"] == "delivered"

    rep = ali.get(f"{base}/api/terminal/driver/report", timeout=5).json()
    assert rep["driver_id"] == me and rep["deliveries"] == 2 and "door_tip_cents" not in rep and rep["tip_cents"] == 0 and rep["cash_cents"] > 0
    # Print: the driver-report slip on the receipt printer (fake).
    before = len(fakes["Front"].jobs)
    r = ali.post(f"{base}/api/terminal/driver/report", timeout=30)
    assert r.status_code == 200 and r.json()["printed_on"], r.text
    wait_until(lambda: len(fakes["Front"].jobs) > before, what="driver report on Front")

    # Journaled, and the journal reaches main.
    actions = [e["action"] for e in ali.get(f"{base}/api/terminal/journal", timeout=5).json()]
    for a in ("driver.linked", "driver.claim", "driver.report"):
        assert a in actions, a
    assert "driver.tip" not in actions  # the refused cash tip journals nothing
    wait_until(lambda: any(e["action"] == "driver.claim" and e["detail"]["delivered"] == [cash, cooking]
                           for ev in fm.of_type("journal.entries") for e in ev["entries"]),
               what="driver journal synced")


def test_claim_waits_for_the_promised_time(drig):
    """The claim rule (the owner, 2026-11-12): past the order's promised time on the Pi (SEND + the quoted
    minutes), not a fixed wait. A delivery quoted 45 minutes isn't listed or claimable now; one quoted 0 is."""
    agent, ali = drig["agent"], drig["ali"]
    base = agent.base
    later = delivery(drig, prep_minutes=45)
    now = delivery(drig)
    avail = ali.get(f"{base}/api/terminal/driver/available", timeout=5).json()
    assert [a["order_id"] for a in avail] == [now] and avail[0]["late_minutes"] >= 0 and avail[0]["promised_at"]
    r = ali.post(f"{base}/api/terminal/driver/claim", json={"order_ids": [later]}, timeout=5)
    assert r.status_code == 409 and r.json()["code"] == "taken" and r.json()["order_ids"] == [later], r.text
    assert ali.post(f"{base}/api/terminal/driver/claim", json={"order_ids": [now]}, timeout=5).status_code == 200


def test_claim_race_one_wins(drig):
    agent, ali, sam = drig["agent"], drig["ali"], drig["sam"]
    oid = delivery(drig)
    barrier = threading.Barrier(2)
    out: dict[str, requests.Response] = {}

    def claim(name, s):
        barrier.wait()
        out[name] = s.post(f"{agent.base}/api/terminal/driver/claim", json={"order_ids": [oid]}, timeout=15)

    threads = [threading.Thread(target=claim, args=(n, s)) for n, s in (("ali", ali), ("sam", sam))]
    for t in threads:
        t.start()
    for t in threads:
        t.join()
    codes = sorted(r.status_code for r in out.values())
    assert codes == [200, 409], {k: r.text for k, r in out.items()}
    lost = next(r for r in out.values() if r.status_code == 409)
    assert lost.json()["code"] == "taken" and lost.json()["order_ids"] == [oid]
    so = agent.http.get(f"{agent.base}/api/orders/{oid}", timeout=5).json()
    winner = next(r for r in out.values() if r.status_code == 200).json()["driver"]["id"]
    assert so["status"]["state"] == "delivered" and so["status"]["driver_id"] == winner


def test_live_main_terminal_settings_carry_auto_close(admin, main_store):
    """Dev main's terminal settings carry auto_close_batch_at (skips on an older main)."""
    token = main_store.mint()["token"]
    r = requests.get(f"{MAIN_BASE_URL}/store/v1/agent/terminal-config", headers={"X-STORE-API-KEY": token}, timeout=5)
    if r.status_code == 404 or "auto_close_batch_at" not in r.json().get("settings", {}):
        pytest.skip("dev main has no auto_close_batch_at yet")
    assert r.json()["settings"]["auto_close_batch_at"] == "04:00"
