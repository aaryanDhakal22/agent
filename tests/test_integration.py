"""The store hub surface (the callout box since H3: SoftTouch is gone) on a
throwaway agent with fake printers: pairing an integration key + route
guarding, the trimmed config, the hub feed (cursor + long-poll), the online
cash-pickup switch, the hub status in /api/floor, and the future-order
schedule + actions.

The scheduled re-ring, AUTO exclusion and the geo check are covered by the Go
tests (application/order/integration_test.go, repositories/integration_repo_test.go).
"""

from __future__ import annotations

import threading
import time
import uuid
from datetime import datetime, timedelta, timezone

import requests

from conftest import wait_until
from gatemark import intentional_sleep
from test_kitchen_printing import _accept, _item, _snap, kitchen  # noqa: F401  (kitchen is a fixture)


def _pair(a, kind: str, name: str) -> dict:
    code = a.http.post(f"{a.base}/api/pairing/code", timeout=5).json()["code"]
    r = requests.post(f"{a.base}/api/pair", json={"code": code, "name": name, "kind": kind}, timeout=5)
    assert r.status_code == 201, r.text
    return {"X-Agent-Device-Key": r.json()["device_key"]}


def _hub(k) -> dict:
    return _pair(k["agent"], "integration", "Store hub")


def _feed(a, hub: dict, after: int = 0, wait: int = 0) -> dict:
    r = requests.get(f"{a.base}/api/integration/events", params={"after": after, "wait": wait}, headers=hub, timeout=wait + 10)
    assert r.status_code == 200, r.text
    return r.json()


def test_integration_key_guarding(kitchen) -> None:  # noqa: F811
    a = kitchen["agent"]
    hub, tablet = _hub(kitchen), _pair(a, "tablet", "Counter")
    r = requests.get(f"{a.base}/api/integration/config", headers=hub, timeout=5)
    assert r.status_code == 200, r.text
    cfg = r.json()
    assert set(cfg) == {"location", "callout", "agent_version"}, cfg  # H3: no SoftTouch keys
    assert cfg["location"]["id"] == kitchen["main_store"].location_id and cfg["agent_version"] and cfg["callout"]["volume"] >= 0
    # The SoftTouch routes are gone.
    for method, path in (("PUT", "/api/integration/orders/1276237"), ("POST", "/api/integration/orders/1276237/void"),
                         ("PATCH", f"/api/integration/online-orders/{uuid.uuid4()}"), ("PUT", "/api/integration/driver-ledger")):
        r = requests.request(method, f"{a.base}{path}", json={}, headers=hub, timeout=5)
        assert r.status_code in (404, 405), (path, r.status_code, r.text)
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
    assert {d["name"]: d["kind"] for d in devs} == {"Store hub": "integration", "Counter": "tablet", "Call center": "callcenter"}


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
    assert _accept(a, snap).status_code == 200  # an accept writes nothing on the feed since H3
    assert a.http.post(f"{a.base}/api/orders/{snap['id']}/ready", json={}, timeout=5).status_code == 200
    th.join(timeout=20)
    assert got["took"] < 10, f"long-poll didn't wake ({got['took']:.1f}s)"
    evs = got["page"]["events"]
    assert [e["type"] for e in evs] == ["order.ready"] and evs[0]["data"]["order_id"] == snap["id"]
    assert "ext_ref" not in evs[0]["data"]
    assert got["page"]["next"] == evs[0]["seq"]
    # The cursor moves on; an idle poll waits, then answers empty.
    t0 = time.time()
    page = _feed(a, hub, got["page"]["next"], 2)
    assert page["events"] == [] and page["next"] == got["page"]["next"] and time.time() - t0 >= 1.5
    r = requests.get(f"{a.base}/api/integration/events", params={"after": "x"}, headers=hub, timeout=5)
    assert r.status_code == 400


def test_online_cash_pickup_follows_its_switch(kitchen, fakes) -> None:  # noqa: F811
    """H3: an online cash pickup prints its kitchen tickets by its own switch
    (no "only while the hub inserts it into the POS" rule); the hub's status
    still shows in /api/floor."""
    a, ids = kitchen["agent"], kitchen["ids"]
    hub = _hub(kitchen)

    def cash_pickup() -> str:
        snap = _snap(kitchen["main_store"], [_item("Pepperoni", ids["Pizza"])])
        snap.pop("payments")  # no payments = a cash order
        assert _accept(a, snap).status_code == 200
        return snap["id"]

    def clear() -> None:
        for f in fakes.values():
            f.clear()

    cash_pickup()  # default switch on: kitchen ticket + receipt
    wait_until(lambda: len(fakes["Pizza"].jobs) == 1 and len(fakes["Front"].jobs) == 1, timeout=10, what="ticket + receipt")
    r = a.http.put(f"{a.base}/api/settings/tickets", json={"online_cash_pickup": False}, timeout=5)
    assert r.status_code == 200 and r.json()["online_cash_pickup"] is False and "pos_takeout" not in r.json()
    clear()
    cash_pickup()
    wait_until(lambda: len(fakes["Front"].jobs) == 1, timeout=10, what="receipt")
    intentional_sleep(0.5, "absence: the switch is off, no kitchen ticket may follow")
    assert not fakes["Pizza"].jobs, "switch off: receipt only"

    r = requests.put(f"{a.base}/api/integration/status", json={"ok": True, "insert_mode": "off", "problems": [], "version": "0.4.0"},
                     headers=hub, timeout=5)
    assert r.status_code == 204, r.text
    integ = a.http.get(f"{a.base}/api/floor", timeout=5).json()["integration"]
    assert integ["connected"] is True and integ["version"] == "0.4.0" and "insert_live" not in integ
    r = requests.put(f"{a.base}/api/integration/status", json={"insert_mode": "turbo"}, headers=hub, timeout=5)
    assert r.status_code == 400


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


def test_future_order_actions(kitchen, fakes) -> None:  # noqa: F811
    """2.4.3: MAKE NOW / CHANGE DUE TIME / PRINT NOW / CANCEL on a waiting
    future order, end to end on fake printers."""
    a, ids = kitchen["agent"], kitchen["ids"]

    def future(hours: float) -> dict:
        snap = _snap(kitchen["main_store"], [_item("Pepperoni", ids["Pizza"])])
        snap["fulfillment"] = {"type": "pickup",
                               "promised_at": (datetime.now(timezone.utc) + timedelta(hours=hours)).isoformat()}
        assert a.http.post(f"{a.base}/api/resend", json=snap, timeout=15).status_code == 200
        r = a.http.post(f"{a.base}/api/orders/{snap['id']}/schedule", json={"lead_minutes": 30}, timeout=5)
        assert r.status_code == 200, r.text
        return snap

    # PRINT NOW: the kitchen's early ticket only (its MAKE AT / READY band).
    one = future(20)
    r = a.http.post(f"{a.base}/api/orders/{one['id']}/print-now", json={"kitchen": True, "receipt": False}, timeout=30)
    assert r.status_code == 200, r.text
    assert r.json()["printed"] == ["Pizza"] and r.json()["order"]["status"]["early_kitchen"] is True
    wait_until(lambda: len(fakes["Pizza"].jobs) >= 1, timeout=10, what="the early kitchen ticket")
    assert b"MAKE AT " in fakes["Pizza"].jobs[-1]
    assert a.http.post(f"{a.base}/api/orders/{one['id']}/print-now", json={}, timeout=5).status_code == 400

    # CHANGE DUE TIME: the ring time follows.
    due = datetime.now(timezone.utc) + timedelta(hours=3)
    r = a.http.post(f"{a.base}/api/orders/{one['id']}/due", json={"promised_at": due.isoformat()}, timeout=5)
    st = r.json()["status"]
    assert r.status_code == 200 and st["due_moved_from"] and st["state"] == "scheduled"
    ring = datetime.fromisoformat(st["ring_at"].replace("Z", "+00:00"))
    assert abs((due - timedelta(minutes=30) - ring).total_seconds()) < 1
    past = (datetime.now(timezone.utc) - timedelta(minutes=5)).isoformat()
    assert a.http.post(f"{a.base}/api/orders/{one['id']}/due", json={"promised_at": past}, timeout=5).status_code == 400

    # MAKE NOW: accepted, a kitchen number, printed with no FUTURE.
    pizza_before = len(fakes["Pizza"].jobs)
    r = a.http.post(f"{a.base}/api/orders/{one['id']}/make-now", json={"prep_minutes": 20}, timeout=30)
    assert r.status_code == 200, r.text
    st = r.json()["status"]
    assert st["state"] == "accepted" and st["kitchen_number"] and st.get("made_now") is True and not st.get("future")
    wait_until(lambda: len(fakes["Pizza"].jobs) > pizza_before, timeout=10, what="the make-now kitchen ticket")
    made = fakes["Pizza"].jobs[pizza_before]
    assert b"FUTURE" not in made and b"MAKE AT" not in made and b"READY " not in made
    r = a.http.post(f"{a.base}/api/orders/{one['id']}/make-now", json={"prep_minutes": 20}, timeout=5)
    assert r.status_code == 409

    # CANCEL a second one.
    two = future(30)
    r = a.http.post(f"{a.base}/api/orders/{two['id']}/cancel", timeout=5)
    assert r.status_code == 200 and r.json()["status"]["state"] == "cancelled"
    assert a.http.post(f"{a.base}/api/orders/{two['id']}/cancel", timeout=5).status_code == 409

    # The store's colours.
    look = a.http.get(f"{a.base}/api/settings/look", timeout=5).json()
    assert look == {"accent": "#AC94F4", "tickets": {"delivery": "#3B82F6", "pickup": "#3B82F6", "counter": "#3B82F6"},
                    "chime": "bell"}
    r = a.http.put(f"{a.base}/api/settings/look", json={"tickets": {"delivery": "#7C3AED"}}, timeout=5)
    assert r.status_code == 200 and r.json()["tickets"]["delivery"] == "#7C3AED" and r.json()["tickets"]["pickup"] == "#3B82F6"
    assert a.http.put(f"{a.base}/api/settings/look", json={"accent": "blue"}, timeout=5).status_code == 400
    assert a.http.get(f"{a.base}/api/floor", timeout=5).json()["settings"]["look"]["tickets"]["delivery"] == "#7C3AED"
    # The store's one-time chime (2026-11-13): saved alone, the colours kept; not a name -> 400.
    r = a.http.put(f"{a.base}/api/settings/look", json={"chime": "beeps"}, timeout=5)
    assert r.status_code == 200 and r.json()["chime"] == "beeps" and r.json()["tickets"]["delivery"] == "#7C3AED"
    assert a.http.put(f"{a.base}/api/settings/look", json={"chime": "Big Bell!"}, timeout=5).status_code == 400
    assert a.http.get(f"{a.base}/api/floor", timeout=5).json()["settings"]["look"]["chime"] == "beeps"
