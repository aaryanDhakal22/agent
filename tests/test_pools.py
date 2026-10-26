"""2.0.9 deliveries on a throwaway agent: driver pools (pool / move / unassign,
the rules), Send Out (not_ready vs mark_ready, include_order_ids), pools
cleared on clock-out with the SSE order.updated, AUTO plan (no_drivers, no
location), delivery settings, the store map route, the geo proxies and the Pi
web page.

Orders here are never located (main doesn't know them), so AUTO's runs and
the geo upsert are covered by the Go tests (application/order/pools_test.go,
repositories/pool_repo_test.go).
"""

from __future__ import annotations

import json
import threading
import time
import uuid

import requests

from conftest import wait_until
from test_floor import arrive, floor, post  # noqa: F401  (floor is a fixture)


def drivers(a, *names: str, clock_in: bool = True) -> list[dict]:
    out = []
    for n in names:
        d = post(a, "/api/drivers", {"name": n}).json()
        if clock_in:
            assert post(a, f"/api/drivers/{d['id']}/clock-in").status_code == 200
        out.append(d)
    return out


def accepted_delivery(a, ready: bool = False) -> str:
    oid = arrive(a, "delivery")
    assert post(a, f"/api/orders/{oid}/accept").status_code == 200
    if ready:
        assert post(a, f"/api/orders/{oid}/ready").status_code == 200
    return oid


def status_of(a, oid: str) -> dict:
    return a.http.get(f"{a.base}/api/orders/{oid}", timeout=5).json()["status"]


def test_pool_rules_and_send_out(floor) -> None:  # noqa: F811
    a = floor
    ann, bob = drivers(a, "Ann", "Bob")
    (cat,) = drivers(a, "Cat", clock_in=False)
    d1, d2 = accepted_delivery(a), accepted_delivery(a)
    d3 = accepted_delivery(a, ready=True)
    pickup = arrive(a, "pickup")
    post(a, f"/api/orders/{pickup}/accept")
    waiting = arrive(a, "delivery")

    pool = lambda ids, drv: post(a, "/api/deliveries/pool", {"order_ids": ids, "driver_id": drv})  # noqa: E731
    r = pool([], ann["id"])
    assert r.status_code == 400 and r.json()["code"] == "bad_request"
    r = pool([str(uuid.uuid4())], ann["id"])
    assert r.status_code == 404 and r.json()["code"] == "not_found"
    r = pool([d1], "nope")
    assert r.status_code == 404 and r.json()["code"] == "driver_not_found"
    r = pool([d1], cat["id"])
    assert r.status_code == 409 and r.json()["code"] == "driver_not_available", "clocked-out driver"
    for bad in (pickup, waiting):
        r = pool([d1, bad], ann["id"])
        assert r.status_code == 409 and r.json()["code"] == "invalid_state", bad
    assert "pool_driver_id" not in status_of(a, d1), "all or nothing"

    r = pool([d1, d2], ann["id"])
    assert r.status_code == 200, r.text
    assert {o["status"]["pool_driver_id"] for o in r.json()} == {ann["id"]}
    assert all(o["status"]["pooled_at"] and o["status"]["state"] == "accepted" for o in r.json())
    # Stop numbers: no drive times here, so placed order (d1 placed first).
    assert {o["order"]["id"]: o["status"]["pool_seq"] for o in r.json()} == {d1: 1, d2: 2}, r.text
    assert [status_of(a, i)["pool_seq"] for i in (d1, d2)] == [1, 2]
    r = pool([d2], bob["id"])
    assert r.status_code == 200 and r.json()[0]["status"]["pool_driver_id"] == bob["id"], "driver -> driver"
    assert r.json()[0]["status"]["pool_seq"] == 1 and status_of(a, d1)["pool_seq"] == 1, "both pools renumbered"
    r = pool([d2], None)
    assert r.status_code == 200 and "pool_driver_id" not in r.json()[0]["status"], "unassign"
    assert "pool_seq" not in r.json()[0]["status"]

    r = post(a, f"/api/drivers/{ann['id']}/send-out", {"mark_ready": False})
    assert r.status_code == 409 and r.json()["code"] == "not_ready" and r.json()["order_ids"] == [d1], r.text
    assert status_of(a, d1)["state"] == "accepted", "refused send-out changes nothing"
    r = post(a, f"/api/drivers/{bob['id']}/send-out", {"mark_ready": True})
    assert r.status_code == 400 and r.json()["code"] == "bad_request", "empty pool"

    r = post(a, f"/api/drivers/{ann['id']}/send-out", {"mark_ready": True, "include_order_ids": [d3]})
    assert r.status_code == 200, r.text
    assert sorted(o["order"]["id"] for o in r.json()) == sorted([d1, d3])
    assert {o["status"]["state"] for o in r.json()} == {"on_delivery"}
    assert all("pool_driver_id" not in o["status"] for o in r.json())
    ds = {d["name"]: d for d in a.http.get(f"{a.base}/api/drivers", timeout=5).json()}
    assert ds["Ann"]["status"] == "out" and ds["Ann"]["deliveries"] == 2
    r = post(a, f"/api/drivers/{ann['id']}/send-out", {"mark_ready": True})
    assert r.status_code == 409 and r.json()["code"] == "driver_not_available"
    r = pool([d2], ann["id"])
    assert r.status_code == 409 and r.json()["code"] == "driver_not_available", "out driver"

    # The old assign still works and takes the order out of its pool.
    post(a, f"/api/orders/{d2}/ready")
    assert pool([d2], bob["id"]).status_code == 200
    (dee,) = drivers(a, "Dee")
    r = post(a, "/api/deliveries/assign", {"driver_id": dee["id"], "order_ids": [d2]})
    assert r.status_code == 200 and "pool_driver_id" not in r.json()[0]["status"]


def test_pool_events_and_clock_out_clears(floor) -> None:  # noqa: F811
    a = floor
    code = post(a, "/api/pairing/code").json()["code"]
    key = requests.post(f"{a.base}/api/pair", json={"code": code, "name": "Tab"}, timeout=5).json()["key"]
    tablet = {"X-Agent-Device-Key": key}
    events: list[tuple[str, dict]] = []
    stop = threading.Event()

    def listen() -> None:
        try:
            with requests.get(f"{a.base}/api/events", headers=tablet, stream=True, timeout=30) as r:
                ev = None
                for line in r.iter_lines(decode_unicode=True):
                    if stop.is_set():
                        return
                    if line.startswith("event: "):
                        ev = line[7:]
                    elif line.startswith("data: ") and ev:
                        events.append((ev, json.loads(line[6:])))
        except requests.RequestException:
            pass

    (zed,) = drivers(a, "Zed")
    oid = accepted_delivery(a, ready=True)
    threading.Thread(target=listen, daemon=True).start()
    time.sleep(0.5)
    try:
        # The tablet's own key drives the pool.
        r = requests.post(f"{a.base}/api/deliveries/pool", json={"order_ids": [oid], "driver_id": zed["id"]}, headers=tablet, timeout=5)
        assert r.status_code == 200, r.text
        wait_until(lambda: any(e == "order.updated" and d["order"]["id"] == oid and d["status"].get("pool_driver_id") == zed["id"]
                               for e, d in events), timeout=10, what="order.updated with the pool")
        assert post(a, f"/api/drivers/{zed['id']}/clock-out").status_code == 200
        wait_until(lambda: any(e == "order.updated" and d["order"]["id"] == oid and "pool_driver_id" not in d["status"]
                               for e, d in events), timeout=10, what="order.updated back to unassigned")
    finally:
        stop.set()
    assert "pool_driver_id" not in status_of(a, oid)
    # Every pool change is synced to main as a floor event.
    wait_until(lambda: a.http.get(f"{a.base}/api/floor", timeout=5).json()["unsynced_events"] == 0,
               timeout=20, what="pool floor events synced")


def test_auto_plan_without_drivers_or_locations(floor) -> None:  # noqa: F811
    a = floor
    r = post(a, "/api/deliveries/auto-plan", {})
    assert r.status_code == 409 and r.json()["code"] == "no_drivers"
    drivers(a, "Ann")
    r = post(a, "/api/deliveries/auto-plan", {})
    assert r.status_code == 200, r.text
    assert r.json()["runs"] == [] and r.json()["unassigned"] == [], "nothing to plan"

    ids = [accepted_delivery(a, ready=True) for _ in range(2)]
    r = post(a, "/api/deliveries/auto-plan", {})
    assert r.status_code == 200, r.text
    plan = r.json()
    assert plan["plan_id"] and plan["created_at"] and plan["runs"] == []
    assert {u["order_id"]: u["reason"] for u in plan["unassigned"]} == {i: "no_location" for i in ids}
    for k in ("estimated", "fallback", "solve_ms"):
        assert k in plan
    assert all("pool_driver_id" not in status_of(a, i) for i in ids), "planning changes nothing"

    r = post(a, f"/api/deliveries/auto-plan/{plan['plan_id']}/confirm")
    assert r.status_code == 200 and r.json() == {"applied": [], "skipped": []}, r.text
    r = post(a, f"/api/deliveries/auto-plan/{plan['plan_id']}/confirm", {"order_ids": ids[:1]})
    assert r.status_code == 200 and r.json() == {"applied": [], "skipped": []}, r.text
    r = post(a, f"/api/deliveries/auto-plan/{uuid.uuid4()}/confirm")
    assert r.status_code == 404 and r.json()["code"] == "not_found"


def test_geo_state_without_main_geo(floor) -> None:  # noqa: F811
    a = floor
    did = arrive(a, "delivery")
    pid = arrive(a, "pickup")
    got = {o["order"]["id"]: o for o in a.http.get(f"{a.base}/api/orders", timeout=5).json()}
    assert got[did]["geo"] == {"state": "pending"}, got[did].get("geo")
    assert "geo" not in got[pid], "pickups carry no geo"
    assert a.http.get(f"{a.base}/api/orders/{did}", timeout=5).json()["geo"]["state"] == "pending"


def test_delivery_settings_and_floor_fields(floor) -> None:  # noqa: F811
    a = floor
    f = a.http.get(f"{a.base}/api/floor", timeout=5).json()
    assert f["soon_ready_minutes"] == 5 and f["delivery_target_minutes"] == 45
    assert "store" in f and "map" in f and f["map"] is None, "no map downloaded on a test agent"
    for bad in ({"soon_ready_minutes": -1, "delivery_target_minutes": 45}, {"soon_ready_minutes": 31, "delivery_target_minutes": 45},
                {"soon_ready_minutes": 5, "delivery_target_minutes": 14}, {"soon_ready_minutes": 5, "delivery_target_minutes": 121}):
        r = a.http.put(f"{a.base}/api/settings/deliveries", json=bad, timeout=5)
        assert r.status_code == 400 and r.json()["code"] == "bad_request", bad
    # 2.2.0: any subset; the answer is the full settings object.
    r = a.http.put(f"{a.base}/api/settings/deliveries", json={"soon_ready_minutes": 5}, timeout=5)
    assert r.status_code == 200 and r.json()["delivery_target_minutes"] == 45 and r.json()["nearby_rule"] == "detour", r.text
    # A paired tablet may change them too (device auth).
    code = post(a, "/api/pairing/code").json()["code"]
    key = requests.post(f"{a.base}/api/pair", json={"code": code, "name": "Tab"}, timeout=5).json()["key"]
    body = {"soon_ready_minutes": 0, "delivery_target_minutes": 120}
    r = requests.put(f"{a.base}/api/settings/deliveries", json=body, headers={"X-Agent-Device-Key": key}, timeout=5)
    assert r.status_code == 200 and body.items() <= r.json().items(), r.text
    f = a.http.get(f"{a.base}/api/floor", timeout=5).json()
    assert f["soon_ready_minutes"] == 0 and f["delivery_target_minutes"] == 120
    assert requests.put(f"{a.base}/api/settings/deliveries", json=body, timeout=5).status_code == 401


def test_map_file_and_geo_proxies(floor) -> None:  # noqa: F811
    a = floor
    r = requests.get(f"{a.base}/api/map.pmtiles", timeout=5)  # LAN-open
    assert r.status_code == 404 and r.json()["code"] == "not_found"

    (ann,) = drivers(a, "Ann")
    o1, o2 = accepted_delivery(a), accepted_delivery(a)
    assert post(a, "/api/deliveries/pool", {"order_ids": [o1, o2], "driver_id": ann["id"]}).status_code == 200
    r = a.http.get(f"{a.base}/api/drivers/{ann['id']}/pool/trip", timeout=20)
    assert r.status_code == 200, r.text
    trip = r.json()
    assert sorted(trip["order_ids"]) == sorted([o1, o2])
    for k in ("polyline", "distance_m", "duration_s", "approximate"):
        assert k in trip
    r = a.http.get(f"{a.base}/api/drivers/{ann['id']}/pool/trip", params={"order_ids": o1}, timeout=20)
    assert r.status_code == 200 and r.json()["order_ids"] == [o1]
    assert a.http.get(f"{a.base}/api/drivers/nope/pool/trip", timeout=5).json()["code"] == "driver_not_found"

    r = post(a, f"/api/orders/{o1}/pin", {"lat": 95, "lng": 0})
    assert r.status_code == 400
    r = post(a, f"/api/orders/{uuid.uuid4()}/pin", {"lat": 40.66, "lng": -74.11})
    assert r.status_code == 404
    # The pin reaches main as geo.pin: these test orders were fed through
    # Resend, so main doesn't hold them and answers not_found (404); a main
    # without geo.pin yet gives 503 main_offline.
    r = post(a, f"/api/orders/{o1}/pin", {"lat": 40.66, "lng": -74.11})
    assert (r.status_code, r.json().get("code")) in ((202, None), (404, "not_found"), (503, "main_offline")), r.text


def test_pi_web_page_shows_pools_read_only(floor) -> None:  # noqa: F811
    a = floor
    html = requests.get(f"{a.base}/", timeout=5).text
    assert "Driver pools" in html and "Deliveries (AUTO)" in html
    assert "SELECT ALL" not in html, "no assign controls on the Pi web any more"
    js = requests.get(f"{a.base}/static/app.js", timeout=5).text
    assert "driverPools" in js and "/api/settings/deliveries" in js and "/api/deliveries/assign" not in js
