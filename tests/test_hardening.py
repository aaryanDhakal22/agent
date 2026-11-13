"""The hardening round on a throwaway Pi (contract/agent.md §2.10s, HARDENING-PLAN A1 / A2):

  * closing the day: a manual close without force is refused over an unpaid counter check (409
    unsettled_checks saying what force will do, also from the Pi page), an unpaid pickup is carried (still
    on the Pickup list, payable after the close, counted as earlier_checks on the next batch's Z), the
    Unsettled Check List route
  * a forced close follows the store's close_unpaid_counter_action (the owner, 2026-11-13): waste (the
    default) writes each unpaid counter check off as Void waste "Unpaid at close" (order.void kind waste,
    the Z's Waste section, the UNPAID AT CLOSE list on the receipt printer), a partly paid one is
    carried; carry keeps them open like pickups
  * Void waste with the store's reasons; a void of a paid order answers refund_first
  * idempotency: client_key required on payments, the same key with another body = key_reused
  * pairing codes bound to their minter: a tablet's code pairs tablets only (another kind burns it)
  * main's floor events: what main refuses is never marked synced

Uses test_terminal.py's rig (a throwaway agent behind the fake main, fake printers only).
"""

from __future__ import annotations

import time
import uuid

import requests

from conftest import wait_until
from gatemark import intentional
from test_terminal import pizza_line, rig  # noqa: F401  (rig is a fixture)


def _place(rig, typ="counter", payments=None) -> tuple[str, int]:
    body = {"client_key": f"k-{uuid.uuid4()}", "type": typ, "cart": {"lines": [pizza_line()]}}
    if typ == "pickup":
        body["customer"] = {"name": "Sam", "phone": "4105550100"}
    if payments is not None:
        body["payments"] = payments
    r = rig["term"].post(f"{rig['agent'].base}/api/terminal/orders", json=body, timeout=10)
    assert r.status_code == 201, r.text
    so = r.json()["order"]["order"]
    return so["id"], so["money"]["total_cents"]


def test_close_refused_over_an_unpaid_counter_check_and_pickups_carried(rig):  # noqa: F811
    agent, term, fm = rig["agent"], rig["term"], rig["fm"]
    base = agent.base
    counter_id, counter_total = _place(rig)
    pickup_id, pickup_total = _place(rig, "pickup")

    lst = term.get(f"{base}/api/terminal/unsettled", timeout=5).json()
    assert lst["count"] == 2 and lst["blocking"] == 1 and lst["due_cents"] == counter_total + pickup_total
    by = {c["order_id"]: c for c in lst["checks"]}
    assert by[counter_id]["blocks_close"] and by[counter_id]["type"] == "counter" and by[counter_id]["on_force"] == "waste"
    assert lst["force_action"] == "waste" and lst["force"]["waste_count"] == 1 and lst["force"]["waste_cents"] == counter_total
    assert not by[pickup_id]["blocks_close"] and by[pickup_id]["customer"]["name"] == "Sam" and by[pickup_id]["items"]

    # The terminal's close (a manager) and the Pi page's: refused without force; the answer says what
    # force would do (the store's setting: waste by default).
    r = term.post(f"{base}/api/terminal/close", json={"force": False, "manager_pin": "9876"}, timeout=30)
    assert r.status_code == 409 and r.json()["code"] == "unsettled_checks", r.text
    assert [c["order_id"] for c in r.json()["checks"]] == [counter_id]
    assert r.json()["force_action"] == "waste" and r.json()["checks"][0]["on_force"] == "waste"
    assert r.json()["force"] == {"waste_count": 1, "waste_cents": counter_total, "carry_count": 0, "carry_due_cents": 0}, r.text
    r = agent.http.post(f"{base}/api/batch/close", json={"force": False}, timeout=30)
    assert r.status_code == 409 and r.json()["code"] == "unsettled_checks" and r.json()["force_action"] == "waste", r.text

    # Pay the counter check: the close goes through and carries the pickup.
    r = term.post(f"{base}/api/terminal/orders/{counter_id}/payments", json={"tender": "cash", "amount_cents": counter_total}, timeout=5)
    assert r.status_code == 200, r.text
    r = term.post(f"{base}/api/terminal/close", json={"force": True, "manager_pin": "9876"}, timeout=30)
    assert r.status_code == 200, r.text
    closed_id = r.json()["closed"]["id"]
    un = r.json()["unsettled"]
    assert un["count"] == 1 and un["checks"][0]["order_id"] == pickup_id and un["checks"][0]["carried"]
    journal = term.get(f"{base}/api/terminal/journal", timeout=5).json()
    assert any(e["action"] == "batch.unsettled" and e["detail"]["count"] == 1 for e in journal)

    # Still on the Pickup list and payable after the close.
    picks = term.get(f"{base}/api/terminal/pickups", timeout=5).json()["orders"]
    assert pickup_id in [o["order"]["id"] for o in picks]
    assert [o for o in picks if o["order"]["id"] == pickup_id][0]["status"]["carried"]
    r = term.post(f"{base}/api/terminal/orders/{pickup_id}/payments", json={"tender": "cash", "amount_cents": pickup_total}, timeout=5)
    assert r.status_code == 200 and r.json()["balance_cents"] == 0, r.text
    assert term.post(f"{base}/api/terminal/orders/{pickup_id}/picked-up", timeout=5).status_code == 200
    assert pickup_id not in [o["order"]["id"] for o in term.get(f"{base}/api/terminal/pickups", timeout=5).json()["orders"]]

    # The closed batch's Z counts it unpaid; the next batch's Z counts the money as earlier_checks.
    wait_until(lambda: term.get(f"{base}/api/terminal/z?batch={closed_id}", timeout=10).status_code == 200, what="Z built")
    z1 = term.get(f"{base}/api/terminal/z?batch={closed_id}", timeout=10).json()["report"]
    assert z1["unpaid"]["cents"] == pickup_total and z1["reconcile"]["difference_cents"] == 0 and not z1["flagged"]
    r = agent.http.post(f"{base}/api/batch/close", json={"force": True}, timeout=30)
    assert r.status_code == 200, r.text
    second = r.json()["closed"]["id"]
    wait_until(lambda: term.get(f"{base}/api/terminal/z?batch={second}", timeout=10).status_code == 200, what="second Z built")
    z2 = term.get(f"{base}/api/terminal/z?batch={second}", timeout=10).json()["report"]
    assert z2["earlier_checks"]["cents"] == pickup_total and z2["reconcile"]["difference_cents"] == 0, z2
    wait_until(lambda: fm.of_type("z.report"), what="z.report synced")


CARA, KHAWAR = "00000000-0000-4000-8000-0000000000c1", "00000000-0000-4000-8000-0000000000e1"


def _set_close_action(rig, action: str, version: int) -> None:
    """main's terminal setting close_unpaid_counter_action, pushed like main does (terminal.changed)."""
    fm, term, base = rig["fm"], rig["term"], rig["agent"].base
    fm.terminal_config = dict(fm.terminal_config, version=version,
                              settings=dict(fm.terminal_config["settings"], close_unpaid_counter_action=action))
    fm.command("terminal.changed", {"version": version})
    wait_until(lambda: term.get(f"{base}/api/terminal/session", timeout=5).json()["config_version"] == version,
               what=f"terminal config v{version}")


def test_forced_close_writes_unpaid_counter_checks_off(rig):  # noqa: F811
    agent, term, fm, fakes = rig["agent"], rig["term"], rig["fm"], rig["fakes"]
    base = agent.base
    unpaid_id, unpaid_total = _place(rig)
    part_id, part_total = _place(rig, payments=[{"tender": "cash", "amount_cents": 500}])
    pickup_id, pickup_total = _place(rig, "pickup")
    wait_until(lambda: sum(b"COUNTER" in j for j in fakes["Pizza"].jobs) >= 2 and any(b"PICKUP" in j for j in fakes["Pizza"].jobs),
               what="the station tickets")
    station_jobs = len(fakes["Pizza"].jobs)

    r = term.post(f"{base}/api/terminal/close", json={"force": False, "manager_pin": "9876"}, timeout=30)
    assert r.status_code == 409 and r.json()["code"] == "unsettled_checks", r.text
    on = {c["order_id"]: c["on_force"] for c in r.json()["checks"]}
    assert on == {unpaid_id: "waste", part_id: "carry"}, r.text  # a partly paid check is always carried
    assert r.json()["force"] == {"waste_count": 1, "waste_cents": unpaid_total, "carry_count": 1,
                                 "carry_due_cents": part_total - 500}, r.text

    # Forced by the cashier with the manager's PIN: written off in her name, approved by him.
    r = term.post(f"{base}/api/terminal/close", json={"force": True, "manager_pin": "9876"}, timeout=30)
    assert r.status_code == 200, r.text
    closed_id = r.json()["closed"]["id"]
    u = r.json()["unpaid_at_close"]
    assert u["count"] == 1 and u["total_cents"] == unpaid_total and u["reason"] == "Unpaid at close" and u["printed_by"] == "pi", u
    assert u["checks"][0]["order_id"] == unpaid_id and u["checks"][0]["items"], u
    assert u["closed_by"]["staff_id"] == CARA and u["closed_by"]["approved_by"] == KHAWAR and u["closed_by"]["trigger"] == "terminal", u
    un = r.json()["unsettled"]
    assert {c["order_id"] for c in un["checks"]} == {part_id, pickup_id} and all(c["carried"] for c in un["checks"]), un

    # The UNPAID AT CLOSE list on the receipt printer (the driver reports' printer); nothing in the kitchen.
    slip = wait_until(lambda: [j for j in fakes["Front"].jobs if b"UNPAID" in j and b"AT CLOSE" in j], what="the Unpaid at close list")[-1]
    text = slip.decode("latin-1")
    assert f"${unpaid_total / 100:.2f}" in text and "Unpaid at close" in text and "Cara" in text, text
    assert len(fakes["Pizza"].jobs) == station_jobs

    # The order is cancelled as waste; the partly paid one is still open and payable.
    got = term.get(f"{base}/api/terminal/checks/{unpaid_id}", timeout=5).json()
    assert got["order"]["status"]["state"] == "cancelled", got["order"]["status"]
    picks = {o["order"]["id"]: o for o in term.get(f"{base}/api/terminal/pickups", timeout=5).json()["orders"]}
    assert unpaid_id not in picks and picks[part_id]["status"]["carried"], list(picks)

    # main: order.void {kind: waste, waste_reason}, no money moved.
    wait_until(lambda: [e for e in fm.of_type("order.void") if e.get("order_id") == unpaid_id], what="order.void to main")
    ev = [e for e in fm.of_type("order.void") if e.get("order_id") == unpaid_id][0]
    assert ev["kind"] == "waste" and ev["waste_reason"] == "Unpaid at close" and ev["staff_id"] == CARA and ev["approved_by"] == KHAWAR, ev
    assert not [e for e in fm.of_type("order.payment") if e.get("order_id") == unpaid_id]
    j = [e for e in term.get(f"{base}/api/terminal/journal", timeout=5).json() if e["action"] == "void.waste" and e.get("order_id") == unpaid_id]
    assert len(j) == 1 and j[0]["detail"]["at_close"] and j[0]["detail"]["reason"] == "Unpaid at close", j

    # The Z: the write-off on the Waste section, the carried checks unpaid, balanced; no driver reports.
    wait_until(lambda: term.get(f"{base}/api/terminal/z?batch={closed_id}", timeout=10).status_code == 200, what="Z built")
    z = term.get(f"{base}/api/terminal/z?batch={closed_id}", timeout=10).json()["report"]
    assert z["waste"]["count"] == 1 and z["waste"]["cents"] == unpaid_total, z["waste"]
    assert z["waste"]["reasons"] == [{"reason": "Unpaid at close", "count": 1, "cents": unpaid_total}], z["waste"]
    assert z["unpaid"]["cents"] == part_total - 500 + pickup_total and z["reconcile"]["difference_cents"] == 0, z
    assert z["sales"]["total_cents"] == part_total + pickup_total, z["sales"]
    reps = agent.http.get(f"{base}/api/batches/{closed_id}/driver-reports", timeout=10)
    assert reps.status_code == 200 and unpaid_id not in reps.text, reps.text  # a counter check is on no driver's report


def test_forced_close_carry_setting_then_a_terminal_prints_it(rig):  # noqa: F811
    agent, term, fakes = rig["agent"], rig["term"], rig["fakes"]
    base = agent.base
    _set_close_action(rig, "carry", 2)
    counter_id, counter_total = _place(rig)

    r = agent.http.post(f"{base}/api/batch/close", json={"force": False}, timeout=30)
    assert r.status_code == 409 and r.json()["force_action"] == "carry" and r.json()["checks"][0]["on_force"] == "carry", r.text
    assert r.json()["force"] == {"waste_count": 0, "waste_cents": 0, "carry_count": 1, "carry_due_cents": counter_total}, r.text
    # Forced from the Pi page: carried like a pickup, still payable / voidable.
    r = agent.http.post(f"{base}/api/batch/close", json={"force": True}, timeout=30)
    assert r.status_code == 200 and r.json()["unpaid_at_close"] is None, r.text
    assert [c["order_id"] for c in r.json()["unsettled"]["checks"]] == [counter_id] and r.json()["unsettled"]["checks"][0]["carried"]
    picks = {o["order"]["id"]: o for o in term.get(f"{base}/api/terminal/pickups", timeout=5).json()["orders"]}
    assert picks[counter_id]["status"]["carried"]

    # Back to waste: the carried counter check still blocks; a terminal that prints the list itself closes.
    _set_close_action(rig, "waste", 3)
    r = term.post(f"{base}/api/terminal/close", json={"force": False, "manager_pin": "9876"}, timeout=30)
    assert r.status_code == 409 and r.json()["checks"][0]["carried"] and r.json()["checks"][0]["on_force"] == "waste", r.text
    front = len(fakes["Front"].jobs)
    r = term.post(f"{base}/api/terminal/close", json={"force": True, "manager_pin": "9876", "print_unpaid_here": True}, timeout=30)
    assert r.status_code == 200, r.text
    u = r.json()["unpaid_at_close"]
    assert u["printed_by"] == "terminal" and [c["order_id"] for c in u["checks"]] == [counter_id] and u["checks"][0]["carried"], u
    assert term.get(f"{base}/api/terminal/checks/{counter_id}", timeout=5).json()["order"]["status"]["state"] == "cancelled"
    assert counter_id not in {o["order"]["id"] for o in term.get(f"{base}/api/terminal/pickups", timeout=5).json()["orders"]}
    with intentional("absence check: the Pi must not print the list the terminal prints"):
        time.sleep(2)
    assert not [j for j in fakes["Front"].jobs[front:] if b"UNPAID" in j], "the Pi printed the terminal's list"


def test_void_refund_first_and_waste(rig):  # noqa: F811
    agent, term, fm, fakes = rig["agent"], rig["term"], rig["fm"], rig["fakes"]
    base = agent.base
    paid_id, total = _place(rig, payments=[{"tender": "cash", "amount_cents": 500}])
    r = term.post(f"{base}/api/terminal/orders/{paid_id}/void", json={"reason": "left"}, timeout=5)
    assert r.status_code == 409 and r.json()["code"] == "refund_first" and r.json()["paid_cents"] == 500

    made_id, made_total = _place(rig, "pickup")
    # Station tickets print after the commit, in the background: wait for this pickup's (and the paid
    # counter order's, which may land later under load) before counting.
    wait_until(lambda: any(b"PICKUP" in j for j in fakes["Pizza"].jobs) and any(b"COUNTER" in j for j in fakes["Pizza"].jobs),
               what="the counter order's and the pickup's station tickets")
    r = term.post(f"{base}/api/terminal/orders/{made_id}/waste", json={"reason": "nope"}, timeout=5)
    assert r.status_code == 400 and r.json()["code"] == "bad_reason" and "Never picked up" in r.json()["reasons"]
    jobs = len(fakes["Pizza"].jobs)
    # A waste is a void of a sent order: a manager's (the owner, 2026-11-12 evening).
    r = term.post(f"{base}/api/terminal/orders/{made_id}/waste", json={"reason": "Never picked up"}, timeout=5)
    assert r.status_code == 403 and r.json()["code"] == "manager_required" and r.json()["action"] == "void", r.text
    r = term.post(f"{base}/api/terminal/orders/{made_id}/waste", json={"reason": "Never picked up", "manager_pin": "9876"}, timeout=5)
    assert r.status_code == 200 and r.json()["order"]["status"]["state"] == "cancelled", r.text
    wait_until(lambda: any(e.get("kind") == "waste" for e in fm.of_type("order.void")), what="order.void kind waste")
    ev = [e for e in fm.of_type("order.void") if e.get("kind") == "waste"][0]
    assert ev["waste_reason"] == "Never picked up" and ev["order_id"] == made_id
    assert len(fakes["Pizza"].jobs) == jobs  # nothing printed
    till = term.get(f"{base}/api/terminal/reports/till?batch=current", timeout=10).json()["report"]
    assert till["detail"]["waste"]["count"] == 1 and till["detail"]["waste"]["cents"] == made_total
    # The settings carry the waste reasons (main's list, else the defaults).
    login = term.post(f"{base}/api/terminal/login", json={"pin": "1234"}, timeout=5).json()
    assert login["settings"]["waste_reasons"][0] == "Never picked up"


def test_payment_keys(rig):  # noqa: F811
    term, base = rig["term"], rig["agent"].base
    oid, total = _place(rig)
    r = term.post(f"{base}/api/terminal/orders/{oid}/payments", json={"tender": "cash", "amount_cents": 100, "client_key": ""}, timeout=5)
    assert r.status_code == 400 and r.json()["code"] == "client_key_required"
    body = {"tender": "cash", "amount_cents": 1000, "tendered_cents": 2000, "client_key": "pay-hard-0001"}
    first = term.post(f"{base}/api/terminal/orders/{oid}/payments", json=body, timeout=5).json()
    again = term.post(f"{base}/api/terminal/orders/{oid}/payments", json=body, timeout=5).json()
    assert again["duplicate"] and again["payment"]["id"] == first["payment"]["id"] and again["balance_cents"] == total - 1000
    r = term.post(f"{base}/api/terminal/orders/{oid}/payments", json=body | {"amount_cents": 999}, timeout=5)
    assert r.status_code == 409 and r.json()["code"] == "key_reused"
    pays = term.get(f"{base}/api/terminal/orders/{oid}", timeout=5).json()["payments"]
    assert len(pays) == 1
    # The cart change needs its key too.
    r = term.put(f"{base}/api/terminal/orders/{oid}/cart", json={"cart": {"lines": [pizza_line()]}, "client_key": ""}, timeout=5)
    assert r.status_code == 400 and r.json()["code"] == "client_key_required"


def test_tablet_codes_pair_tablets_only(rig):  # noqa: F811
    agent = rig["agent"]
    base = agent.base
    code = agent.http.post(f"{base}/api/pairing/code", json={"kind": "tablet"}, timeout=5).json()["code"]
    r = requests.post(f"{base}/api/pair", json={"code": code, "name": "Tab A", "kind": "tablet"}, timeout=5)
    assert r.status_code == 201, r.text
    tab = requests.Session()
    tab.headers["X-Agent-Device-Key"] = r.json()["key"]
    # A tablet asking for a terminal code: refused.
    r = tab.post(f"{base}/api/pairing/code", json={"kind": "terminal"}, timeout=5)
    assert r.status_code == 403 and r.json()["code"] == "kind_not_allowed"
    # A tablet's code used for a terminal: 403, and the code is burned.
    code = tab.post(f"{base}/api/pairing/code", json={"kind": "tablet"}, timeout=5).json()["code"]
    r = requests.post(f"{base}/api/pair", json={"code": code, "name": "Sneaky", "kind": "terminal"}, timeout=5)
    assert r.status_code == 403 and r.json()["code"] == "kind_not_allowed"
    r = requests.post(f"{base}/api/pair", json={"code": code, "name": "Tab B", "kind": "tablet"}, timeout=5)
    assert r.status_code == 401
    # At most 5 codes per device per 15 min (it has minted 1: the refused ask doesn't count).
    for _ in range(4):
        assert tab.post(f"{base}/api/pairing/code", json={}, timeout=5).status_code == 200
    r = tab.post(f"{base}/api/pairing/code", json={}, timeout=5)
    assert r.status_code == 429 and r.json()["code"] == "too_many_codes" and r.headers.get("Retry-After")
    journal = rig["term"].get(f"{base}/api/terminal/journal", timeout=5).json()
    assert sum(1 for e in journal if e["action"] == "pairing.code") >= 5


def test_refused_money_events_stay_queued(rig):  # noqa: F811
    """M-2: main refusing order.placed keeps it (and the payment behind it) queued; nothing is marked synced."""
    agent, term, fm = rig["agent"], rig["term"], rig["fm"]
    fm.refuse = {"order.placed"}
    oid, total = _place(rig)
    wait_until(lambda: any(e["order_id"] == oid for e in fm.refused), what="main refused order.placed")
    r = term.post(f"{agent.base}/api/terminal/orders/{oid}/payments", json={"tender": "cash", "amount_cents": total}, timeout=5)
    assert r.status_code == 200
    # Through a few flushes the payment stays behind its refused order.
    with intentional("the payment must not go out while its order is refused (absence check)"):
        time.sleep(3)
    assert not [e for e in fm.of_type("order.payment") if e["order_id"] == oid], "the payment waits behind its refused order"
    fm.refuse = set()
    wait_until(lambda: [e for e in fm.of_type("order.payment") if e["order_id"] == oid], timeout=60, what="payment after the order")
    placed = [e for e in fm.of_type("order.placed") if e["order_id"] == oid]
    assert placed, "order.placed applied after main took it"
