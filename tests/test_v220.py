"""Agent 2.2.0 on throwaway agents (fake printers only):

  * the tablet's auto-accept toggle (device key) and auto_accepted on orders
    (SSE order.updated with auto_accepted, never order.arrived);
  * GET /api/dev/events (the developer-mode feed, no customer data);
  * X-Quibase-Version recorded per tablet: /api/diag tablets, /api/devices;
  * delivery settings: the nearby rule fields, GET /api/deliveries/next-check,
    delivery_radius_m through main (RPC location.radius);
  * GET/PUT /api/settings/auto-update and the auto-update after Close batch
    (AGENT_UPDATE_ENABLED + a fake manifest: the test plays the root updater).

The nearby rules themselves need located orders (main's order.geo) and are
covered by the Go tests (application/order/v220_test.go).
"""

from __future__ import annotations

import json
import threading

import requests

from conftest import STORE_V1, manager_session, provision, wait_until
from test_floor import arrive, floor, post  # noqa: F401  (floor is a fixture)
from test_updates import utility  # noqa: F401  (fixture)

QUIBASE = "1.7.0+1761480000"

NEW_ROUTES = {
    ("GET", "/api/deliveries/next-check"): "device",
    ("GET", "/api/settings/deliveries"): "device",
    ("PUT", "/api/settings/deliveries"): "device",
    ("GET", "/api/settings/auto-accept"): "device",
    ("PUT", "/api/settings/auto-accept"): "device",
    ("GET", "/api/settings/auto-update"): "device",
    ("PUT", "/api/settings/auto-update"): "device",
    ("GET", "/api/dev/events"): "device",
}


def pair(a, name: str = "Counter") -> dict:
    code = post(a, "/api/pairing/code").json()["code"]
    r = requests.post(f"{a.base}/api/pair", json={"code": code, "name": name}, timeout=5)
    assert r.status_code == 201, r.text
    return {"X-Agent-Device-Key": r.json()["key"], "X-Quibase-Version": QUIBASE}


class Listener:
    """A tablet on /api/events, collecting (event, data)."""

    def __init__(self, a, headers: dict) -> None:
        self.events: list[tuple[str, dict]] = []
        self._stop = threading.Event()
        opened = threading.Event()

        def run() -> None:
            try:
                with requests.get(f"{a.base}/api/events", headers=headers, stream=True, timeout=60) as r:
                    opened.set()
                    ev = None
                    for line in r.iter_lines(decode_unicode=True):
                        if self._stop.is_set():
                            return
                        if line.startswith("event: "):
                            ev = line[7:]
                        elif line.startswith("data: ") and ev:
                            self.events.append((ev, json.loads(line[6:])))
            except requests.RequestException:
                pass

        threading.Thread(target=run, daemon=True).start()
        assert opened.wait(10), "the tablet's /api/events never opened"
        # Subscribed once the agent counts the tablet as on the stream.
        wait_until(lambda: any(t["connected"] for t in
                               requests.get(f"{a.base}/api/diag", headers=headers, timeout=10).json()["tablets"]),
                   timeout=10, interval=0.05, what="the tablet on /api/events")

    def of(self, kind: str, oid: str) -> list[dict]:
        out = []
        for ev, d in self.events:
            if ev != kind:
                continue
            o = d.get("order") or {}
            if o.get("id") == oid:
                out.append(d)
        return out

    def stop(self) -> None:
        self._stop.set()


def test_new_routes_listed(blank_agent) -> None:
    routes = requests.get(f"{blank_agent.base}/__routes", timeout=5).json()["routes"]
    got = {(r["method"], r["path"]): r["auth"] for r in routes}
    for k, auth in NEW_ROUTES.items():
        assert got.get(k) == auth, (k, got.get(k))
    for method, path in NEW_ROUTES:
        assert requests.request(method, f"{blank_agent.base}{path}", json={}, timeout=5).status_code == 401, path


def test_tablet_auto_accept_marks_orders(floor, fakes) -> None:  # noqa: F811
    a = floor
    tablet = pair(a)
    r = requests.put(f"{a.base}/api/settings/auto-accept", json={"auto_accept": True}, headers=tablet, timeout=5)
    assert r.status_code == 200 and r.json() == {"auto_accept": True}, r.text
    assert requests.get(f"{a.base}/api/settings/auto-accept", headers=tablet, timeout=5).json() == {"auto_accept": True}

    sse = Listener(a, tablet)
    try:
        jobs = len(fakes["Front"].jobs)
        oid = arrive(a, "delivery")
        wait_until(lambda: any(d["status"].get("auto_accepted") for d in sse.of("order.updated", oid)), timeout=15,
                   what="order.updated with auto_accepted")
        assert not sse.of("order.arrived", oid), "an auto-accepted order rang"
        st = a.http.get(f"{a.base}/api/orders/{oid}", timeout=5).json()["status"]
        assert st["state"] == "accepted" and st["auto_accepted"] is True and st["kitchen_number"] >= 1100
        wait_until(lambda: len(fakes["Front"].jobs) > jobs, timeout=15, what="the fake printer to get the ticket")
        assert any(o["status"].get("auto_accepted") for o in a.http.get(f"{a.base}/api/orders?batch=current", timeout=5).json())

        # Off again from the tablet: the next order rings, and a staff accept isn't marked.
        assert requests.put(f"{a.base}/api/settings/auto-accept", json={"auto_accept": False}, headers=tablet,
                            timeout=5).status_code == 200
        oid2 = arrive(a, "pickup")
        wait_until(lambda: sse.of("order.arrived", oid2), timeout=10, what="order.arrived")
        r = requests.post(f"{a.base}/api/orders/{oid2}/accept", json={"prep_minutes": 15}, headers=tablet, timeout=15)
        assert r.status_code == 200 and "auto_accepted" not in r.json()["status"], r.text
    finally:
        sse.stop()

    # The developer-mode feed has the story, by kitchen number, no customer.
    r = requests.get(f"{a.base}/api/dev/events?since=0", headers=tablet, timeout=5)
    assert r.status_code == 200, r.text
    body = r.json()
    texts = [e["text"] for e in body["events"]]
    assert body["last_seq"] >= len(body["events"]) > 0
    assert all(set(e) == {"seq", "at", "level", "kind", "text"} for e in body["events"])
    assert [e["seq"] for e in body["events"]] == sorted(e["seq"] for e in body["events"])
    assert any("auto-accepted" in t for t in texts), texts
    assert any("Auto-accept: off → on (tablet Counter)" == t for t in texts), texts
    assert any(t.startswith("Order ") and "arrived (delivery)" in t for t in texts), texts
    assert any("printed on Front" in t for t in texts), texts
    assert not any("Pytest Floor" in t for t in texts), "a customer name in the dev events"
    more = requests.get(f"{a.base}/api/dev/events?since={body['last_seq']}", headers=tablet, timeout=5).json()
    assert all(e["seq"] > body["last_seq"] for e in more["events"])


def test_tablet_versions(floor) -> None:  # noqa: F811
    a = floor
    tablet = pair(a, "Front tablet")
    assert requests.get(f"{a.base}/api/floor", headers=tablet, timeout=5).status_code == 200
    sse = Listener(a, tablet)
    try:
        d = requests.get(f"{a.base}/api/diag", headers=tablet, timeout=10).json()
        assert len(d["tablets"]) == 1, d["tablets"]
        t = d["tablets"][0]
        assert t["name"] == "Front tablet" and t["app_version"] == QUIBASE and t["connected"] is True and t["device_id"]
        devs = a.http.get(f"{a.base}/api/devices", timeout=5).json()
        assert devs[0]["app_version"] == QUIBASE
    finally:
        sse.stop()
    # A newer Quibase is recorded at once.
    newer = dict(tablet, **{"X-Quibase-Version": "1.7.1+1761490000"})
    requests.get(f"{a.base}/api/floor", headers=newer, timeout=5)
    assert a.http.get(f"{a.base}/api/devices", timeout=5).json()[0]["app_version"] == "1.7.1+1761490000"


def test_delivery_settings_next_check_and_radius(admin, floor) -> None:  # noqa: F811
    a = floor
    tablet = pair(a)
    put = lambda body: requests.put(f"{a.base}/api/settings/deliveries", json=body, headers=tablet, timeout=15)  # noqa: E731

    f = requests.get(f"{a.base}/api/floor", headers=tablet, timeout=5).json()
    assert (f["nearby_rule"], f["nearby_detour_minutes"], f["nearby_distance_mi"]) == ("detour", 5, 1.0)

    r = put({"nearby_rule": "distance", "nearby_distance_mi": 0.5})
    assert r.status_code == 200, r.text
    s = r.json()
    assert s["nearby_rule"] == "distance" and s["nearby_distance_mi"] == 0.5 and s["soon_ready_minutes"] == 5
    assert s["delivery_target_minutes"] == 45 and s["nearby_detour_minutes"] == 5 and "delivery_radius_m" in s
    for bad in ({"nearby_rule": "close"}, {"nearby_detour_minutes": 0}, {"nearby_distance_mi": 11},
                {"delivery_radius_m": 400}, {"soon_ready_minutes": 40}):
        r = put(bad)
        assert r.status_code == 400 and r.json()["code"] == "bad_request", (bad, r.text)
    assert requests.get(f"{a.base}/api/settings/deliveries", headers=tablet, timeout=5).json()["nearby_rule"] == "distance"

    # next-check: nobody clocked in -> null; a driver with an (unlocated) pool -> nothing nearby.
    r = requests.get(f"{a.base}/api/deliveries/next-check", headers=tablet, timeout=5)
    assert r.status_code == 200 and r.json() == {"driver_id": None, "rule": "distance", "nearby_order_ids": [], "reason": ""}
    d = post(a, "/api/drivers", {"name": "Juma"}).json()
    post(a, f"/api/drivers/{d['id']}/clock-in")
    o1, o2 = arrive(a, "delivery"), arrive(a, "delivery")
    for o in (o1, o2):
        assert post(a, f"/api/orders/{o}/accept").status_code == 200
        assert post(a, f"/api/orders/{o}/ready").status_code == 200
    assert post(a, "/api/deliveries/pool", {"order_ids": [o1], "driver_id": d["id"]}).status_code == 200
    r = requests.get(f"{a.base}/api/deliveries/next-check", headers=tablet, timeout=5).json()
    assert r["driver_id"] == d["id"] and r["nearby_order_ids"] == [] and r["rule"] == "distance"

    # The radius goes to main (location.radius) and comes back in store_config.
    r = put({"delivery_radius_m": 6500, "nearby_rule": "detour"})
    assert r.status_code == 200, r.text
    assert r.json()["delivery_radius_m"] == 6500 and r.json()["nearby_rule"] == "detour"
    mgr = manager_session(admin, a.store)
    assert mgr.get(f"{STORE_V1}/me/location", timeout=5).json()["delivery_radius_m"] == 6500
    wait_until(lambda: a.http.get(f"{a.base}/api/store-config", timeout=5).json()["location"]["delivery_radius_m"] == 6500,
               timeout=10, what="store_config with the new radius")


def _after_close_runs(a) -> int:
    """Background after-close runs finished (the driver-report print that the
    auto-update check follows), from the agent's log."""
    log = (a.dir / "agent.log").read_text(errors="replace")
    return log.count("Driver reports printed after close") + log.count("driver reports after close failed")


def test_auto_update_after_close(main_store, spawn_agent, tmp_path, utility) -> None:  # noqa: F811
    upd = tmp_path / "upd"
    upd.mkdir()
    a = spawn_agent(AGENT_UPDATE_ENABLED="1", AGENT_UPDATE_DIR=str(upd), UTILITY_URL=utility)
    provision(a, main_store)
    req = upd / "update.request"

    assert a.http.get(f"{a.base}/api/settings/auto-update", timeout=5).json() == {"auto_update": True}
    r = a.http.put(f"{a.base}/api/settings/auto-update", json={"auto_update": False}, timeout=5)
    assert r.status_code == 200 and r.json() == {"auto_update": False}
    assert a.http.put(f"{a.base}/api/settings/auto-update", json={}, timeout=5).status_code == 400

    # Off: Close batch updates nothing. The auto-update check runs right after
    # the background driver-report print, so wait for that line in the log.
    assert a.http.post(f"{a.base}/api/batch/close", json={}, timeout=15).status_code == 200
    wait_until(lambda: _after_close_runs(a) >= 1, timeout=15, interval=0.1, what="the after-close driver reports")
    assert not req.exists(), "auto-update ran while off"

    # On: right after Close batch the newer manifest (9.9.9) is requested as "auto".
    assert a.http.put(f"{a.base}/api/settings/auto-update", json={"auto_update": True}, timeout=5).status_code == 200
    assert a.http.post(f"{a.base}/api/batch/close", json={}, timeout=15).status_code == 200
    wait_until(req.exists, timeout=20, what="update.request after Close batch")
    body = json.loads(req.read_text())
    assert body["action"] == "update" and body["requested_by"] == "auto" and body["id"]
    st = a.http.get(f"{a.base}/api/update/status", timeout=5).json()
    assert st["state"] == "requested" and st["pending"]["requested_by"] == "auto"
    ev = a.http.get(f"{a.base}/api/dev/events", timeout=5).json()["events"]
    assert any("Auto-update after Close batch requested" in e["text"] and e["kind"] == "update" for e in ev), ev
