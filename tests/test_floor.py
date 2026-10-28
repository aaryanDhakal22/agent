"""The agent-owned floor flow on a throwaway agent with one fake printer:
tablet pairing + device-key auth, accept (kitchen number + prep estimate) →
ready → driver run → delivered, close batch, live SSE events, and the sync of
floor events to main.

Orders are fed through the web UI's Resend (the arrival pipeline) — they don't
exist in main, so main answers their floor events `not_found`; the test only
checks the sync round-trips (the backlog drains). The full vendor → main →
agent → back-to-main status path is covered by the root E2E suite.
"""

from __future__ import annotations

import json
import threading
import time
import uuid
from datetime import datetime, timezone

import pytest
import requests

from conftest import STORE_V1, FakePrinter, manager_session, provision, wait_until


@pytest.fixture
def floor(admin, main_store, spawn_agent, fakes):
    mgr = manager_session(admin, main_store)
    fakes["Front"] = FakePrinter()
    assert mgr.post(f"{STORE_V1}/me/printers", json={"name": "Front", "ip": fakes["Front"].addr}, timeout=5).status_code == 201
    a = spawn_agent()
    provision(a, main_store)
    wait_until(lambda: len(a.http.get(f"{a.base}/api/printers", timeout=5).json()) == 1, timeout=20, what="printer roster")
    a.store = main_store
    return a


def arrive(a, fulfillment: str = "pickup") -> str:
    oid = str(uuid.uuid4())
    snap = {
        "id": oid, "tenant_id": a.store.tenant_id, "location_id": a.store.location_id,
        "source": "online", "source_event_id": str(int(time.time() * 1000) % 10**7),
        "placed_at": datetime.now(timezone.utc).isoformat(),
        "customer": {"display_name": "Pytest Floor"}, "fulfillment": {"type": fulfillment},
        "money": {"subtotal_cents": 1000, "total_cents": 1000},
        "items": [{"id": str(uuid.uuid4()), "name": "Thing", "quantity": 1, "unit_price_cents": 1000, "line_total_cents": 1000}],
    }
    r = a.http.post(f"{a.base}/api/resend", json=snap, timeout=10)
    assert r.status_code == 200, r.text
    return oid


def post(a, path: str, body: dict | None = None, **kw) -> requests.Response:
    return a.http.post(f"{a.base}{path}", json=body, timeout=15, **kw)


def test_pairing_and_device_keys(floor) -> None:
    a = floor
    assert requests.post(f"{a.base}/api/pair", json={"code": "123456", "name": "Tab"}, timeout=5).status_code == 401
    code = post(a, "/api/pairing/code").json()["code"]
    assert len(code) == 6
    r = requests.post(f"{a.base}/api/pair", json={"code": code, "name": ""}, timeout=5)
    assert r.status_code == 400 and r.json()["code"] == "name_required"
    r = requests.post(f"{a.base}/api/pair", json={"code": code, "name": "Counter tablet"}, timeout=5)
    assert r.status_code == 201, r.text
    key = r.json()["key"]
    assert key.startswith("qagt_")
    # Single use.
    assert requests.post(f"{a.base}/api/pair", json={"code": code, "name": "Again"}, timeout=5).status_code == 401

    tablet = {"X-Agent-Device-Key": key}
    assert requests.get(f"{a.base}/api/floor", headers=tablet, timeout=5).status_code == 200
    assert requests.get(f"{a.base}/api/floor", timeout=5).status_code == 401
    # The key opens floor routes only, not the web-UI ones.
    assert requests.get(f"{a.base}/api/devices", headers=tablet, timeout=5).status_code == 401

    devs = a.http.get(f"{a.base}/api/devices", timeout=5).json()
    assert [d["name"] for d in devs] == ["Counter tablet"] and "key" not in devs[0]
    assert post(a, f"/api/devices/{devs[0]['id']}/revoke").status_code == 204
    assert requests.get(f"{a.base}/api/floor", headers=tablet, timeout=5).status_code == 401

    # A tablet can also unpair itself ("Unpair" in the app).
    code = post(a, "/api/pairing/code").json()["code"]
    key2 = requests.post(f"{a.base}/api/pair", json={"code": code, "name": "Tab 2"}, timeout=5).json()["key"]
    tab2 = {"X-Agent-Device-Key": key2}
    assert post(a, "/api/device/unpair").status_code == 400, "a web session isn't a device"
    assert requests.post(f"{a.base}/api/device/unpair", headers=tab2, timeout=5).status_code == 204
    assert requests.get(f"{a.base}/api/floor", headers=tab2, timeout=5).status_code == 401


def test_pickup_flow(floor, fakes) -> None:
    a = floor
    oid = arrive(a)
    assert [o["order"]["id"] for o in a.http.get(f"{a.base}/api/orders/arrived", timeout=5).json()] == [oid]

    assert post(a, f"/api/orders/{oid}/accept", {"prep_minutes": 999}).status_code == 400
    r = post(a, f"/api/orders/{oid}/accept", {"prep_minutes": 15})
    assert r.status_code == 200, r.text
    st = r.json()["status"]
    assert st["state"] == "accepted" and st["kitchen_number"] == 1100 and st["prep_minutes"] == 15
    eta = datetime.fromisoformat(st["estimated_ready_at"].replace("Z", "+00:00"))
    acc = datetime.fromisoformat(st["accepted_at"].replace("Z", "+00:00"))
    assert abs((eta - acc).total_seconds() - 900) < 2
    wait_until(lambda: fakes["Front"].jobs, what="ticket printed")
    assert "1100" in fakes["Front"].text(), "kitchen number on the ticket"
    again = post(a, f"/api/orders/{oid}/accept")
    assert again.status_code == 409 and again.json()["code"] == "already_accepted"

    floor_now = a.http.get(f"{a.base}/api/floor", timeout=5).json()
    assert floor_now["kitchen_next"] == 1101 and floor_now["last_prep_minutes"] == 15

    r = post(a, f"/api/orders/{oid}/ready")
    assert r.status_code == 200 and r.json()["status"]["state"] == "ready"
    assert post(a, f"/api/orders/{oid}/ready").json()["code"] == "invalid_state"
    assert post(a, "/api/orders/not-a-uuid/ready").status_code == 400
    assert post(a, f"/api/orders/{uuid.uuid4()}/ready").status_code == 404

    # The sync round-trips: main acknowledges every event (not_found here).
    wait_until(lambda: a.http.get(f"{a.base}/api/floor", timeout=5).json()["unsynced_events"] == 0,
               timeout=20, what="floor events synced to main")


def test_delivery_flow_and_driver_queue(floor) -> None:
    a = floor
    sam = post(a, "/api/drivers", {"name": "Sam"}).json()
    ana = post(a, "/api/drivers", {"name": "Ana"}).json()
    assert post(a, "/api/drivers", {"name": " "}).status_code == 400
    assert sam["status"] == "off", "new drivers start clocked out"
    for d in (sam, ana):
        r = post(a, f"/api/drivers/{d['id']}/clock-in")
        assert r.status_code == 200 and r.json()["status"] == "available" and r.json()["clocked_in_at"], r.text
    assert post(a, f"/api/drivers/{sam['id']}/clock-in").json()["code"] == "invalid_state"
    orders = [arrive(a, "delivery") for _ in range(2)]
    pickup = arrive(a, "pickup")
    for oid in orders + [pickup]:
        assert post(a, f"/api/orders/{oid}/accept").status_code == 200

    r = post(a, "/api/deliveries/assign", {"driver_id": sam["id"], "order_ids": orders})
    assert r.status_code == 409 and r.json()["code"] == "invalid_state", "not ready yet"
    for oid in orders + [pickup]:
        post(a, f"/api/orders/{oid}/ready")
    r = post(a, "/api/deliveries/assign", {"driver_id": sam["id"], "order_ids": [orders[0], pickup]})
    assert r.status_code == 409, "a pickup can't go out"
    assert post(a, "/api/deliveries/assign", {"driver_id": "nope", "order_ids": orders}).status_code == 404

    r = post(a, "/api/deliveries/assign", {"driver_id": sam["id"], "order_ids": orders})
    assert r.status_code == 200, r.text
    assert {o["status"]["state"] for o in r.json()} == {"on_delivery"}
    assert {o["status"]["driver_name"] for o in r.json()} == {"Sam"}
    ds = a.http.get(f"{a.base}/api/drivers", timeout=5).json()
    assert [(d["name"], d["status"], d["deliveries"]) for d in ds] == [("Sam", "out", 2), ("Ana", "available", 0)]

    r = post(a, f"/api/drivers/{sam['id']}/returned")
    assert r.status_code == 200 and {o["status"]["state"] for o in r.json()} == {"delivered"}
    ds = a.http.get(f"{a.base}/api/drivers", timeout=5).json()
    assert [d["name"] for d in ds] == ["Ana", "Sam"], "back of the queue"

    assert post(a, "/api/drivers/reorder", {"ids": [sam["id"]]}).status_code == 204
    assert [d["name"] for d in a.http.get(f"{a.base}/api/drivers", timeout=5).json()] == ["Sam", "Ana"]
    assert a.http.patch(f"{a.base}/api/drivers/{ana['id']}", json={"name": "Anna"}, timeout=5).status_code == 204
    assert a.http.delete(f"{a.base}/api/drivers/{sam['id']}", timeout=5).status_code == 204
    assert [d["name"] for d in a.http.get(f"{a.base}/api/drivers", timeout=5).json()] == ["Anna"]


def test_clock_out_and_shift_records(floor) -> None:
    a = floor
    d = post(a, "/api/drivers", {"name": "Deep"}).json()
    oid = arrive(a, "delivery")
    post(a, f"/api/orders/{oid}/accept")
    post(a, f"/api/orders/{oid}/ready")
    r = post(a, "/api/deliveries/assign", {"driver_id": d["id"], "order_ids": [oid]})
    assert r.status_code == 409 and r.json()["code"] == "driver_not_available", "clocked-out drivers can't take runs"
    post(a, f"/api/drivers/{d['id']}/clock-in")
    assert post(a, "/api/deliveries/assign", {"driver_id": d["id"], "order_ids": [oid]}).status_code == 200
    assert post(a, f"/api/drivers/{d['id']}/clock-out").json()["code"] == "driver_not_available", "can't clock out mid-run"
    post(a, f"/api/drivers/{d['id']}/returned")
    r = post(a, f"/api/drivers/{d['id']}/clock-out")
    assert r.status_code == 200 and r.json()["status"] == "off"
    shifts = a.http.get(f"{a.base}/api/drivers/shifts", timeout=5).json()
    assert len(shifts) == 1 and shifts[0]["driver_name"] == "Deep" and shifts[0]["clock_out"] and not shifts[0]["auto_out"]

    assert a.http.get(f"{a.base}/api/floor", timeout=5).json()["driver_reset_time"] == "10:00"
    assert a.http.put(f"{a.base}/api/settings/driver-reset", json={"time": "25:00"}, timeout=5).status_code == 400
    r = a.http.put(f"{a.base}/api/settings/driver-reset", json={"time": "04:30"}, timeout=5)
    assert r.status_code == 200 and r.json()["time"] == "04:30"


def test_close_batch(floor) -> None:
    a = floor
    first = a.http.get(f"{a.base}/api/floor", timeout=5).json()["batch"]["id"]
    done = arrive(a)
    post(a, f"/api/orders/{done}/accept")
    post(a, f"/api/orders/{done}/ready")
    waiting = arrive(a)

    r = post(a, "/api/batch/close", {"force": False})
    assert r.status_code == 409 and r.json()["code"] == "batch_has_leftovers"
    assert [o["order"]["id"] for o in r.json()["leftovers"]] == [waiting]

    r = post(a, "/api/batch/close", {"force": True})
    assert r.status_code == 200, r.text
    assert r.json()["closed"]["id"] == first and r.json()["opened"]["id"] > first
    assert [o["status"]["state"] for o in r.json()["auto_finished"]] == ["cancelled"], "never accepted → cancelled"
    assert r.json()["auto_finished"][0]["status"].get("kitchen_number") is None

    f = a.http.get(f"{a.base}/api/floor", timeout=5).json()
    assert f["batch"]["id"] > first and f["kitchen_next"] == 1100 and f["batch"]["order_count"] == 0
    assert a.http.get(f"{a.base}/api/orders", timeout=5).json() == [], "current batch is empty"
    assert len(a.http.get(f"{a.base}/api/orders", params={"batch": first}, timeout=5).json()) == 2
    assert len(a.http.get(f"{a.base}/api/orders", params={"batch": "all"}, timeout=5).json()) == 2
    batches = a.http.get(f"{a.base}/api/batches", timeout=5).json()
    assert batches[0].get("closed_at") is None and batches[1]["id"] == first and batches[1]["order_count"] == 2

    # Kitchen numbers restart.
    nxt = arrive(a)
    assert post(a, f"/api/orders/{nxt}/accept").json()["status"]["kitchen_number"] == 1100


def test_live_events_for_a_paired_tablet(floor) -> None:
    a = floor
    code = post(a, "/api/pairing/code").json()["code"]
    key = requests.post(f"{a.base}/api/pair", json={"code": code, "name": "Tab"}, timeout=5).json()["key"]
    events: list[tuple[str, dict]] = []
    stop = threading.Event()

    def listen() -> None:
        try:
            _listen()
        except requests.RequestException:
            pass  # the agent stops at teardown

    def _listen() -> None:
        with requests.get(f"{a.base}/api/events", headers={"X-Agent-Device-Key": key}, stream=True, timeout=30) as r:
            assert r.status_code == 200
            ev = None
            for line in r.iter_lines(decode_unicode=True):
                if stop.is_set():
                    return
                if line.startswith("event: "):
                    ev = line[7:]
                elif line.startswith("data: ") and ev:
                    events.append((ev, json.loads(line[6:])))

    t = threading.Thread(target=listen, daemon=True)
    t.start()
    time.sleep(0.5)
    oid = arrive(a)
    post(a, f"/api/orders/{oid}/accept")
    zed = post(a, "/api/drivers", {"name": "Zed"}).json()
    post(a, f"/api/drivers/{zed['id']}/clock-in")
    did = arrive(a, "delivery")
    post(a, f"/api/orders/{did}/accept")
    post(a, f"/api/orders/{did}/ready")
    post(a, "/api/deliveries/assign", {"driver_id": zed["id"], "order_ids": [did]})
    try:
        wait_until(lambda: {"order.arrived", "order.updated", "drivers.updated", "drivers.dispatched"} <= {e for e, _ in events},
                   timeout=10, what="arrived + updated + drivers + dispatched events")
    finally:
        stop.set()
    flash = next(d for e, d in events if e == "drivers.dispatched")
    assert flash["driver"]["name"] == "Zed" and [o["order"]["id"] for o in flash["orders"]] == [did]
    upd = next(d for e, d in events if e == "order.updated")
    assert upd["order"]["id"] == oid and upd["status"]["kitchen_number"] == 1100


def test_undo_ready(floor) -> None:
    """2.3.8: READY tapped by mistake goes back to cooking; a second undo is too late."""
    a = floor
    oid = arrive(a)
    assert post(a, f"/api/orders/{oid}/accept").status_code == 200
    assert post(a, f"/api/orders/{oid}/ready").status_code == 200
    r = post(a, f"/api/orders/{oid}/undo-ready")
    assert r.status_code == 200, r.text
    assert r.json()["status"]["state"] == "accepted" and "ready_at" not in r.json()["status"]
    r = post(a, f"/api/orders/{oid}/undo-ready")
    assert r.status_code == 409 and r.json()["code"] == "undo_expired"
    assert post(a, f"/api/orders/{oid}/ready").status_code == 200
