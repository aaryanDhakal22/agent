"""2.1.0 store hub surface on a throwaway agent with fake printers: pairing an
integration key + route guarding, POS orders (upsert / unchanged / changed /
void, local only, the POS ticket switches, Reprint + Resend), the hub feed
(cursor + long-poll), driver numbers + the terminal driver mirror, the online
cash-pickup rule x the hub's live insert, the hub status in /api/floor, close
batch wiping POS customers, and the future-order schedule.

The scheduled re-ring, AUTO exclusion, geo check / address.confirmed and the
anonymous delivery.run are covered by the Go tests
(application/order/integration_test.go, repositories/integration_repo_test.go).
"""

from __future__ import annotations

import threading
import time
import uuid
from datetime import datetime, timedelta, timezone

import requests

from conftest import wait_until
from test_kitchen_printing import _accept, _item, _snap, kitchen  # noqa: F401  (kitchen is a fixture)


def _pair(a, kind: str, name: str) -> dict:
    code = a.http.post(f"{a.base}/api/pairing/code", timeout=5).json()["code"]
    r = requests.post(f"{a.base}/api/pair", json={"code": code, "name": name, "kind": kind}, timeout=5)
    assert r.status_code == 201, r.text
    return {"X-Agent-Device-Key": r.json()["device_key"]}


def _hub(k) -> dict:
    return _pair(k["agent"], "integration", "Store hub")


def _pos(ids: dict, ftype: str = "pickup", hash_: str = "h1", **extra) -> dict:
    now = datetime.now(timezone.utc).isoformat()
    body = {
        "source": "softtouch", "hash": hash_, "display_number": 99, "check_type": ftype, "sent_at": now, "placed_at": now,
        "customer": {"display_name": "Pat Phone", "phone": "4105550100"},
        "items": [{"name": "14\" Cheese Pizza", "quantity": 1, "unit_price_cents": 1199, "line_total_cents": 1199,
                   "printer_id": ids["Pizza"], "children": [{"name": "Pineapple", "quantity": 1, "unit_price_cents": 150, "line_total_cents": 150}]},
                  {"name": "Can Soda", "quantity": 1, "unit_price_cents": 150, "line_total_cents": 150, "printer_id": ""}],
        "notes": "", "money": {"subtotal_cents": 1499, "tax_cents": 90, "tip_cents": 0, "delivery_fee_cents": 0, "total_cents": 1589},
        "paid": False, "cash_due_cents": 1589, "tip_cents": 0, "board_hidden": False, "driver_pos_number": None,
    }
    if ftype == "delivery":
        body["address"] = {"line1": "931 Taylor Ave", "city": "Towson", "state": "MD", "postal_code": "21286"}
    body.update(extra)
    return body


def _put(a, hub: dict, ref: str, body: dict) -> requests.Response:
    return requests.put(f"{a.base}/api/integration/orders/{ref}", json=body, headers=hub, timeout=15)


def _order(a, oid: str) -> dict:
    return a.http.get(f"{a.base}/api/orders/{oid}", timeout=5).json()


def _feed(a, hub: dict, after: int = 0, wait: int = 0) -> dict:
    r = requests.get(f"{a.base}/api/integration/events", params={"after": after, "wait": wait}, headers=hub, timeout=wait + 10)
    assert r.status_code == 200, r.text
    return r.json()


def _reset(fakes) -> None:
    for f in fakes.values():
        f.jobs.clear()


def test_integration_key_guarding(kitchen) -> None:  # noqa: F811
    a = kitchen["agent"]
    hub, tablet = _hub(kitchen), _pair(a, "tablet", "Counter")
    r = requests.get(f"{a.base}/api/integration/config", headers=hub, timeout=5)
    assert r.status_code == 200, r.text
    cfg = r.json()
    assert {p["id"] for p in cfg["printers"]} == set(kitchen["ids"].values())
    assert cfg["features"]["pos_pull"] is True and cfg["tickets"]["pos_delivery"] is False
    assert cfg["times"] == {"default_pickup_minutes": 15, "default_delivery_minutes": 35}
    assert cfg["location"]["id"] == kitchen["main_store"].location_id and cfg["agent_version"]
    for path in ("/api/integration/config", "/api/integration/events"):
        r = requests.get(f"{a.base}{path}", headers=tablet, timeout=5)
        assert r.status_code == 403 and r.json()["code"] == "integration_required", path
        assert a.http.get(f"{a.base}{path}", timeout=5).status_code == 403, "web session"
    for path in ("/api/floor", "/api/drivers", "/api/settings/features", "/api/settings/times"):
        assert requests.get(f"{a.base}{path}", headers=hub, timeout=5).status_code == 200, path
    for method, path in (("GET", "/api/orders"), ("POST", "/api/drivers"), ("PUT", "/api/settings/tickets"), ("POST", "/api/batch/close")):
        r = requests.request(method, f"{a.base}{path}", json={}, headers=hub, timeout=5)
        assert r.status_code == 403 and r.json()["code"] == "integration_only", path
    devs = a.http.get(f"{a.base}/api/devices", timeout=5).json()
    assert {d["name"]: d["kind"] for d in devs} == {"Store hub": "integration", "Counter": "tablet"}


def test_pos_order_lifecycle_and_printing(kitchen, fakes) -> None:  # noqa: F811
    a, ids = kitchen["agent"], kitchen["ids"]
    hub = _hub(kitchen)
    # Switches off (default): stored accepted, nothing printed.
    r = _put(a, hub, "1276237", _pos(ids, "pickup"))
    assert r.status_code == 200, r.text
    first = r.json()
    assert first["created"] is True and first["changed"] is False
    so = _order(a, first["order_id"])
    st = so["status"]
    assert st["state"] == "accepted" and st["display_number"] == 99 and st["local_only"] is True
    assert st["ext_source"] == "softtouch" and st["ext_ref"] == "1276237" and "kitchen_number" not in st
    assert st["prep_minutes"] == 15 and st["cash_due_cents"] == 1589 and so["order"]["source"] == "pos"
    time.sleep(1)
    assert not any(f.jobs for f in fakes.values()), "POS switches are off"

    # Same hash: nothing. Changed: updated, never reprinted.
    assert _put(a, hub, "1276237", _pos(ids, "pickup")).json() == {"order_id": first["order_id"], "created": False, "changed": False}
    r = _put(a, hub, "1276237", _pos(ids, "pickup", "h2", paid=True, cash_due_cents=0))
    assert r.json()["changed"] is True and _order(a, first["order_id"])["status"]["paid"] is True

    # Takeout switch on: a new takeout prints its station ticket — never a receipt.
    r = a.http.put(f"{a.base}/api/settings/tickets", json={"pos_takeout": True}, timeout=5)
    assert r.status_code == 200 and r.json()["pos_takeout"] is True and r.json()["pos_delivery"] is False
    second = _put(a, hub, "1276238", _pos(ids, "pickup")).json()
    wait_until(lambda: len(fakes["Pizza"].jobs) == 1, timeout=10, what="POS station ticket")
    time.sleep(1)
    assert not fakes["Front"].jobs and not fakes["Wings"].jobs, "no receipt, no soda ticket"
    assert "#99" in fakes["Pizza"].text() and "Cheese Pizza" in fakes["Pizza"].text()
    _put(a, hub, "1276238", _pos(ids, "pickup", "h3"))
    time.sleep(1)
    assert len(fakes["Pizza"].jobs) == 1, "a changed check doesn't reprint"

    # Reprint = the receipt; Resend = the kitchen tickets.
    _reset(fakes)
    assert a.http.post(f"{a.base}/api/orders/{second['order_id']}/reprint", timeout=30).status_code == 200
    wait_until(lambda: len(fakes["Front"].jobs) == 1, timeout=10, what="POS receipt")
    r = a.http.post(f"{a.base}/api/orders/{second['order_id']}/kitchen", timeout=30)
    assert r.status_code == 200 and r.json()["printed"] == ["Pizza"], r.text

    # Void: cancelled.
    r = requests.post(f"{a.base}/api/integration/orders/1276238/void", headers=hub, timeout=10)
    assert r.status_code == 200 and r.json()["status"]["state"] == "cancelled"
    assert requests.post(f"{a.base}/api/integration/orders/nope/void", headers=hub, timeout=10).status_code == 404

    # Pull-in off: 409.
    assert a.http.put(f"{a.base}/api/settings/features", json={"pos_pull": False}, timeout=5).json()["pos_pull"] is False
    r = _put(a, hub, "1276239", _pos(ids))
    assert r.status_code == 409 and r.json()["code"] == "pos_pull_off"
    r = _put(a, hub, "1276239", {**_pos(ids), "check_type": "drive_thru"})
    assert r.status_code in (400, 409)

    # Never reached main: the floor backlog drains with no POS order in it
    # (main would answer not_found for an id it never had - none are sent).
    events = _feed(a, hub)["events"]
    assert [e["type"] for e in events] == ["settings.changed", "order.cancelled", "settings.changed"]


def test_feed_long_poll(kitchen) -> None:  # noqa: F811
    a = kitchen["agent"]
    hub = _hub(kitchen)
    page = _feed(a, hub)
    assert page == {"events": [], "next": 0}
    got: dict = {}

    def poll() -> None:
        t0 = time.time()
        got["page"] = _feed(a, hub, 0, 15)
        got["took"] = time.time() - t0

    th = threading.Thread(target=poll)
    th.start()
    time.sleep(0.5)
    snap = _snap(kitchen["main_store"], [_item("Pepperoni", kitchen["ids"]["Pizza"])])
    assert _accept(a, snap).status_code == 200
    th.join(timeout=20)
    assert got["took"] < 10, f"long-poll didn't wake ({got['took']:.1f}s)"
    evs = got["page"]["events"]
    assert [e["type"] for e in evs] == ["order.accepted"] and evs[0]["data"]["order"]["order"]["id"] == snap["id"]
    assert got["page"]["next"] == evs[0]["seq"]
    # The cursor moves on; an idle poll waits, then answers empty.
    t0 = time.time()
    page = _feed(a, hub, got["page"]["next"], 2)
    assert page["events"] == [] and page["next"] == got["page"]["next"] and time.time() - t0 >= 1.5
    r = requests.get(f"{a.base}/api/integration/events", params={"after": "x"}, headers=hub, timeout=5)
    assert r.status_code == 400


def test_driver_numbers_and_terminal_mirror(kitchen) -> None:  # noqa: F811
    a, ids = kitchen["agent"], kitchen["ids"]
    hub = _hub(kitchen)
    r = a.http.post(f"{a.base}/api/drivers", json={"name": "Ann", "pos_number": "12"}, timeout=5)
    assert r.status_code == 201 and r.json()["pos_number"] == "12", r.text
    ann = r.json()
    r = a.http.post(f"{a.base}/api/drivers", json={"name": "Bob", "pos_number": "12"}, timeout=5)
    assert r.status_code == 409 and r.json()["code"] == "pos_number_taken"
    bob = a.http.post(f"{a.base}/api/drivers", json={"name": "Bob"}, timeout=5).json()
    assert a.http.patch(f"{a.base}/api/drivers/{bob['id']}", json={"pos_number": "13"}, timeout=5).status_code == 204
    r = a.http.patch(f"{a.base}/api/drivers/{bob['id']}", json={"pos_number": "12"}, timeout=5)
    assert r.status_code == 409 and r.json()["code"] == "pos_number_taken"
    assert a.http.patch(f"{a.base}/api/drivers/{bob['id']}", json={"pos_number": "1 2"}, timeout=5).status_code == 400
    cfg = requests.get(f"{a.base}/api/integration/config", headers=hub, timeout=5).json()
    assert {d["name"]: d["pos_number"] for d in cfg["drivers"]} == {"Ann": "12", "Bob": "13"}
    assert a.http.post(f"{a.base}/api/drivers/{ann['id']}/clock-in", timeout=5).status_code == 200

    # The cashier puts Ann (#12) on a POS delivery: it goes out with her.
    r = _put(a, hub, "5001", _pos(ids, "delivery", driver_pos_number="12"))
    assert r.status_code == 200, r.text
    st = _order(a, r.json()["order_id"])["status"]
    assert st["state"] == "on_delivery" and st["driver_id"] == ann["id"]
    feed = _feed(a, hub)["events"]
    sent = [e for e in feed if e["type"] == "run.sent"]
    assert len(sent) == 1 and sent[0]["data"]["driver"]["pos_number"] == "12"
    assert sent[0]["data"]["orders"] == [{"order_id": r.json()["order_id"], "source": "pos", "ext_ref": "5001"}]
    # Bob (#13) is off: a note, the order stays.
    r = _put(a, hub, "5002", _pos(ids, "delivery", driver_pos_number="13"))
    st = _order(a, r.json()["order_id"])["status"]
    assert st["state"] == "accepted" and st["pos_driver_note"].startswith("POS says #13")


def test_online_cash_pickup_follows_the_hub_insert(kitchen, fakes) -> None:  # noqa: F811
    a, ids = kitchen["agent"], kitchen["ids"]
    hub = _hub(kitchen)

    def cash_pickup() -> str:
        snap = _snap(kitchen["main_store"], [_item("Pepperoni", ids["Pizza"])])
        snap.pop("payments")  # no payments = a cash order
        assert _accept(a, snap).status_code == 200
        return snap["id"]

    def insert_expected(oid: str) -> bool:
        # order.accepted tells the hub whether to insert (decided with the print).
        acc = [e["data"] for e in _feed(a, hub)["events"]
               if e["type"] == "order.accepted" and e["data"]["order"]["order"]["id"] == oid]
        assert len(acc) == 1, acc
        return acc[0]["insert_expected"]

    def live(mode: str) -> None:
        r = requests.put(f"{a.base}/api/integration/status", json={"ok": True, "insert_mode": mode, "operation_day": "2026-10-25",
                                                                   "problems": [], "version": "0.1.0"}, headers=hub, timeout=5)
        assert r.status_code == 204, r.text

    first = cash_pickup()  # default: receipt only (staff key it into the POS)
    wait_until(lambda: len(fakes["Front"].jobs) == 1, timeout=10, what="receipt")
    assert not fakes["Pizza"].jobs
    assert insert_expected(first) is False
    a.http.put(f"{a.base}/api/settings/features", json={"order_insert": True}, timeout=5)
    live("live")
    integ = a.http.get(f"{a.base}/api/floor", timeout=5).json()["integration"]
    assert integ["connected"] is True and integ["insert_live"] is True and integ["insert_mode"] == "live"
    _reset(fakes)
    inserted = cash_pickup()  # the hub inserts it live: kitchen tickets + receipt
    wait_until(lambda: len(fakes["Pizza"].jobs) == 1 and len(fakes["Front"].jobs) == 1, timeout=10, what="ticket + receipt")
    assert insert_expected(inserted) is True
    live("shadow")
    _reset(fakes)
    shadow = cash_pickup()
    wait_until(lambda: len(fakes["Front"].jobs) == 1, timeout=10, what="receipt")
    time.sleep(0.5)
    assert not fakes["Pizza"].jobs, "shadow insert: receipt only again"
    assert insert_expected(shadow) is False
    r = requests.put(f"{a.base}/api/integration/status", json={"insert_mode": "turbo"}, headers=hub, timeout=5)
    assert r.status_code == 400


def test_close_batch_wipes_pos_customers(kitchen) -> None:  # noqa: F811
    a, ids = kitchen["agent"], kitchen["ids"]
    hub = _hub(kitchen)
    oid = _put(a, hub, "7001", _pos(ids, "delivery")).json()["order_id"]
    promised = (datetime.now(timezone.utc) + timedelta(hours=3)).isoformat()
    later = _snap(kitchen["main_store"], [_item("Pepperoni", ids["Pizza"])])
    later["fulfillment"] = {"type": "pickup", "promised_at": promised}
    assert a.http.post(f"{a.base}/api/resend", json=later, timeout=15).status_code == 200
    assert _order(a, later["id"])["status"]["future"] is True
    r = a.http.post(f"{a.base}/api/orders/{later['id']}/schedule", json={"lead_minutes": 20}, timeout=5)
    assert r.status_code == 200 and r.json()["status"]["state"] == "scheduled", r.text
    # A future order still ringing for its first confirm waits too.
    ringing = _snap(kitchen["main_store"], [_item("Pepperoni", ids["Pizza"])])
    ringing["fulfillment"] = {"type": "pickup", "promised_at": promised}
    assert a.http.post(f"{a.base}/api/resend", json=ringing, timeout=15).status_code == 200
    assert _order(a, ringing["id"])["status"]["state"] == "arrived"

    # Neither the open POS check nor the future orders block a plain close.
    r = a.http.post(f"{a.base}/api/batch/close", json={"force": False}, timeout=30)
    assert r.status_code == 200, r.text
    so = _order(a, oid)
    assert so["status"]["state"] == "cancelled" and so["status"]["display_number"] == 99
    assert so["order"]["customer"] == {} and "address" not in so["order"]["fulfillment"]
    sched = _order(a, later["id"])["status"]
    assert sched["state"] == "scheduled" and sched["batch_id"] == r.json()["opened"]["id"]
    st = _order(a, ringing["id"])["status"]
    assert st["state"] == "arrived" and st["batch_id"] == r.json()["opened"]["id"] and st["future"] is True
    assert [e["type"] for e in _feed(a, hub)["events"]][-1] == "batch.closed"


def test_future_orders_schedule(kitchen) -> None:  # noqa: F811
    a, ids = kitchen["agent"], kitchen["ids"]
    asap = _snap(kitchen["main_store"], [_item("Pepperoni", ids["Pizza"])])
    assert a.http.post(f"{a.base}/api/resend", json=asap, timeout=15).status_code == 200
    r = a.http.post(f"{a.base}/api/orders/{asap['id']}/schedule", json={"lead_minutes": 20}, timeout=5)
    assert r.status_code == 409 and r.json()["code"] == "not_future"
    promised = datetime.now(timezone.utc) + timedelta(hours=2)
    later = _snap(kitchen["main_store"], [_item("Pepperoni", ids["Pizza"])])
    later["fulfillment"] = {"type": "pickup", "promised_at": promised.isoformat()}
    assert a.http.post(f"{a.base}/api/resend", json=later, timeout=15).status_code == 200
    r = a.http.post(f"{a.base}/api/orders/{later['id']}/schedule", json={"lead_minutes": 0}, timeout=5)
    assert r.status_code == 400
    r = a.http.post(f"{a.base}/api/orders/{later['id']}/schedule", json={"lead_minutes": 25}, timeout=5)
    st = r.json()["status"]
    assert r.status_code == 200 and st["state"] == "scheduled" and st["scheduled_lead_minutes"] == 25 and st["future"] is True
    ring = datetime.fromisoformat(st["ring_at"].replace("Z", "+00:00"))
    assert abs((promised - timedelta(minutes=25) - ring).total_seconds()) < 1
    assert a.http.get(f"{a.base}/api/orders/arrived", timeout=5).json() == [] or all(
        o["order"]["id"] != later["id"] for o in a.http.get(f"{a.base}/api/orders/arrived", timeout=5).json())
    # Default times feed the future rule (X = pickup).
    r = a.http.put(f"{a.base}/api/settings/times", json={"default_pickup_minutes": 241}, timeout=5)
    assert r.status_code == 400
    r = a.http.put(f"{a.base}/api/settings/times", json={"default_pickup_minutes": 20}, timeout=5)
    assert r.json() == {"default_pickup_minutes": 20, "default_delivery_minutes": 35}
    assert uuid.UUID(later["id"])
