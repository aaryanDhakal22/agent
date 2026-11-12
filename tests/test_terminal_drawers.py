"""Terminal Stage 2 on the Pi, end to end through a real dev main:

  * cash drawers per terminal + cashier shift (contract §2.10k): count in, a cash sale, pay-in,
    pay-out over the cashier's limit (manager PIN), no sale, count out (over / short), another
    drawer left open for Close batch
  * the one close = the Z report (§2.10l): Close batch closes the open drawer (flagged), the Pi
    builds the Z, it balances, and main stores drawers + Z (GET /store/v1/me/drawers, /me/reports/z)
  * the sync-stall rule (§2.4e): a type main refuses for good is parked, the rest keeps syncing,
    the web page's route lists it and a retry sends it once main takes it
  * prep_minutes on a placed pickup

Uses test_terminal.py's rig (a throwaway agent behind the fake main, fake printers only).
"""

from __future__ import annotations

import time
import uuid

from conftest import STORE_V1, wait_until
from test_terminal import pizza_line, rig  # noqa: F401  (rig is a fixture)
from test_terminal_driver import pair_terminal

SODA = "00000000-0000-0000-0000-0000000000a6"


def quote(rig, cart: dict) -> int:
    r = rig["term"].post(f"{rig['agent'].base}/api/terminal/quote", json={"cart": cart}, timeout=10)
    assert r.status_code == 200, r.text
    return r.json()["quote"]["total_cents"]


def test_drawers_and_z_report_reach_main(rig):  # noqa: F811
    agent, term, fm, mgr = rig["agent"], rig["term"], rig["fm"], rig["mgr"]
    base = agent.base
    # Count in on Till 1 (the cashier is logged in): $100 in twenties + $20 in ones.
    r = term.post(f"{base}/api/terminal/drawer/open", json={"denominations": [{"value_cents": 2000, "count": 5},
                                                                              {"value_cents": 100, "count": 20}]}, timeout=5)
    assert r.status_code == 201, r.text
    d1 = r.json()["drawer"]
    assert d1["start_cents"] == 12000 and d1["state"] == "open" and d1["staff_id"] == "00000000-0000-4000-8000-0000000000c1"
    r = term.post(f"{base}/api/terminal/drawer/open", json={"start_cents": 1}, timeout=5)
    assert r.status_code == 409 and r.json()["code"] == "drawer_open"
    # A cash sale with change goes in the drawer (a tip on cash is refused: tips are card only); a card sale
    # (its tip on the card) doesn't.
    cart = {"lines": [pizza_line()]}
    total = quote(rig, cart)
    r = term.post(f"{base}/api/terminal/orders", json={"client_key": f"z-{uuid.uuid4()}", "type": "counter", "cart": cart,
                                                         "payments": [{"tender": "cash", "amount_cents": total, "tip_cents": 100,
                                                                       "tendered_cents": total + 2000}]}, timeout=10)
    assert r.status_code == 400 and r.json()["code"] == "tip_card_only", r.text
    r = term.post(f"{base}/api/terminal/orders", json={"client_key": f"z-{uuid.uuid4()}", "type": "counter", "cart": cart,
                                                         "payments": [{"tender": "cash", "amount_cents": total,
                                                                       "tendered_cents": total + 2000}]}, timeout=10)
    assert r.status_code == 201, r.text
    r = term.post(f"{base}/api/terminal/orders", json={"client_key": f"z-{uuid.uuid4()}", "type": "pickup", "cart": cart,
                                                         "payments": [{"tender": "card", "amount_cents": total}]}, timeout=10)
    assert r.status_code == 201, r.text
    # Pay-in, a pay-out over the cashier's $20 (a manager's PIN), a no sale.
    r = term.post(f"{base}/api/terminal/drawer/pay-in", json={"amount_cents": 500, "reason": "change", "client_key": "mv-pytest-1"}, timeout=5)
    assert r.status_code == 200 and r.json()["movement"]["kind"] == "pay_in", r.text
    r = term.post(f"{base}/api/terminal/drawer/pay-out", json={"amount_cents": 2500, "reason": "ice"}, timeout=5)
    assert r.status_code == 403 and r.json()["code"] == "manager_required" and r.json()["limit"] == 2000
    r = term.post(f"{base}/api/terminal/drawer/pay-out", json={"amount_cents": 2500, "reason": "ice", "manager_pin": "9876"}, timeout=5)
    assert r.status_code == 200 and r.json()["movement"]["approved_by"] == "00000000-0000-4000-8000-0000000000e1", r.text
    r = term.post(f"{base}/api/terminal/journal", json={"action": "no_sale"}, timeout=5)
    assert r.status_code == 201 and r.json()["detail"]["drawer_id"] == d1["id"], r.text
    expected = 12000 + total + 500 - 2500
    v = term.get(f"{base}/api/terminal/drawer", timeout=5).json()
    assert v["drawer"]["expected_cents"] == expected and v["drawer"]["no_sales"] == 1 and len(v["drawer"]["movements"]) == 2
    # Count out, $1 short.
    r = term.post(f"{base}/api/terminal/drawer/close", json={"counted_cents": expected - 100}, timeout=5)
    assert r.status_code == 200, r.text
    closed = r.json()["drawer"]
    assert closed["over_short_cents"] == -100 and closed["close_reason"] == "counted" and not closed["flagged"]
    # Till 2: the manager opens a drawer and leaves it open for Close batch.
    till2 = pair_terminal(agent, "Till 2")
    wait_until(lambda: till2.post(f"{base}/api/terminal/login", json={"pin": "9876"}, timeout=5).status_code == 200, what="till 2 login")
    r = till2.post(f"{base}/api/terminal/drawer/open", json={"start_cents": 5000}, timeout=5)
    assert r.status_code == 201, r.text
    d2 = r.json()["drawer"]
    batch = agent.http.get(f"{base}/api/floor", timeout=5).json()["batch"]["id"]

    r = agent.http.post(f"{base}/api/batch/close", json={"force": True}, timeout=30)
    assert r.status_code == 200, r.text
    # The Pi builds the Z right after; it balances.
    z = wait_until(lambda: (lambda r: r.json()["report"] if r.status_code == 200 else None)(
        agent.http.get(f"{base}/api/batches/{batch}/z", timeout=5)), what="the Pi's Z report")
    assert z["batch_id"] == batch and z["close_kind"] == "manual" and z["version"] == 1
    assert z["reconcile"]["difference_cents"] == 0 and z["reconcile"]["drawers_difference_cents"] == 0, z["reconcile"]
    assert z["counts"]["counter"] == 1 and z["counts"]["pickup"] == 1 and z["sales"]["total_cents"] == 2 * total
    tenders = {t["tender"]: t for t in z["tenders"]}
    assert tenders["cash"]["collected_cents"] == total and tenders["card"]["collected_cents"] == total
    assert z["tips"]["cash_cents"] == 0 and z["over_short_cents"] == -100 and z["no_sales"] == 1
    drawers = {d["id"]: d for d in z["drawers"]}
    assert drawers[d1["id"]]["counted_cents"] == expected - 100
    assert drawers[d2["id"]]["close_reason"] == "batch_close" and drawers[d2["id"]]["flagged"] and drawers[d2["id"]]["counted_cents"] is None
    assert term.get(f"{base}/api/terminal/drawer", timeout=5).json()["drawer"] is None
    # main stores them (synced as drawer.* + z.report through the fake main).
    wait_until(lambda: fm.of_type("z.report"), what="z.report synced")
    assert len(fm.of_type("drawer.opened")) == 2 and len(fm.of_type("drawer.movement")) == 2 and len(fm.of_type("drawer.closed")) == 2
    day = z["business_date"]

    def main_z():
        r = mgr.get(f"{STORE_V1}/me/reports/z", params={"date": day}, timeout=5)
        assert r.status_code == 200, r.text
        return [x for x in r.json()["reports"] if x["batch_id"] == batch]
    rows = wait_until(main_z, what="main's Z report")
    assert rows[0]["id"] == z["id"] and rows[0]["difference_cents"] == 0 and rows[0]["over_short_cents"] == -100
    full = mgr.get(f"{STORE_V1}/me/reports/z/{z['id']}", timeout=5).json()["report"]
    assert full["reconcile"] == z["reconcile"] and full["tenders"] == z["tenders"]

    def main_drawer(did):
        r = mgr.get(f"{STORE_V1}/me/drawers/{did}", timeout=5)
        return r.json() if r.status_code == 200 and r.json()["drawer"].get("closed_at") else None
    got = wait_until(lambda: main_drawer(d1["id"]), what="main's drawer")
    assert got["drawer"]["over_short_cents"] == -100 and [m["kind"] for m in got["movements"]] == ["pay_in", "pay_out"]
    got2 = wait_until(lambda: main_drawer(d2["id"]), what="main's batch-closed drawer")
    assert got2["drawer"]["close_reason"] == "batch_close" and got2["drawer"]["counted_cents"] is None


def test_refused_events_park_and_retry(rig):  # noqa: F811
    agent, term, fm = rig["agent"], rig["term"], rig["fm"]
    base = agent.base
    r = agent.http.put(f"{base}/api/settings/sync", json={"refusals": 1}, timeout=5)
    assert r.status_code == 200 and r.json() == {"refusals": 1, "hours": 2}, r.text
    assert agent.http.put(f"{base}/api/settings/sync", json={"hours": 0}, timeout=5).status_code == 400
    fm.refuse = {"customer.upsert"}  # main refuses these for good
    phone = "410555" + str(int(time.time() * 1000))[-4:]
    r = term.post(f"{base}/api/terminal/customers", json={"phone": phone, "first_name": "Park"}, timeout=5)
    assert r.status_code == 201, r.text
    cid = r.json()["customer"]["id"]
    v = wait_until(lambda: (lambda v: v if v["count"] == 1 else None)(agent.http.get(f"{base}/api/sync/parked", timeout=5).json()),
                   what="the refused customer parked")
    assert v["parked"][0]["type"] == "customer.upsert" and v["parked"][0]["subject"] == f"customer:{cid}" and v["parked"][0]["result"] == "invalid"
    # A later edit of the same customer waits behind it; a counter order still syncs.
    r = term.patch(f"{base}/api/terminal/customers/{cid}", json={"notes": "side door"}, timeout=5)
    assert r.status_code == 200, r.text
    r = term.post(f"{base}/api/terminal/orders", json={"client_key": f"p-{uuid.uuid4()}", "type": "counter",
                                                         "cart": {"lines": [pizza_line()]}}, timeout=10)
    oid = r.json()["order"]["order"]["id"]
    wait_until(lambda: any(e.get("order_id") == oid for e in fm.of_type("order.placed")), what="the order sent past the parked event")

    def customers_parked():
        return [p for p in agent.http.get(f"{base}/api/sync/parked", timeout=5).json()["parked"] if p["type"] == "customer.upsert"]
    # (This rig's staff ids aren't uuids, so dev main refuses its order.placed too: with refusals = 1 it
    # parks as well. Only the customer's events matter here.)
    v = agent.http.get(f"{base}/api/sync/parked", timeout=5).json()
    assert len(customers_parked()) == 1 and v["held"] == 1, v
    assert not fm.of_type("customer.upsert")
    journal = [e["action"] for e in term.get(f"{base}/api/terminal/journal", timeout=5).json()]
    assert "sync.parked" in journal
    # main takes them now: retry the customer's from the page's route; it and the held edit go, in order.
    fm.refuse = set()
    parked_id = customers_parked()[0]["event_id"]
    r = agent.http.post(f"{base}/api/sync/parked/{parked_id}/retry", timeout=5)
    assert r.status_code == 200 and r.json()["retried"] == 1, r.text
    wait_until(lambda: len(fm.of_type("customer.upsert")) == 2, what="parked + held customer events synced")
    ups = fm.of_type("customer.upsert")
    assert ups[0]["customer"]["notes"] == "" and ups[1]["customer"]["notes"] == "side door", ups  # in their old order
    wait_until(lambda: not customers_parked() and agent.http.get(f"{base}/api/sync/parked", timeout=5).json()["held"] == 0,
               what="nothing of the customer parked or held")
    assert agent.http.post(f"{base}/api/sync/parked/{parked_id}/retry", timeout=5).status_code == 404


def test_place_prep_minutes(rig):  # noqa: F811
    base, term = rig["agent"].base, rig["term"]
    cart = {"lines": [{"item_id": SODA, "qty": 1}]}
    r = term.post(f"{base}/api/terminal/orders", json={"client_key": f"prep-{uuid.uuid4()}", "type": "pickup", "cart": cart,
                                                         "prep_minutes": 30}, timeout=10)
    assert r.status_code == 201, r.text
    st = r.json()["order"]["status"]
    assert st["prep_minutes"] == 30, st
    r = term.post(f"{base}/api/terminal/orders", json={"client_key": f"prep-{uuid.uuid4()}", "type": "pickup", "cart": cart,
                                                         "prep_minutes": 500}, timeout=10)
    assert r.status_code == 400 and r.json()["code"] == "bad_request"
