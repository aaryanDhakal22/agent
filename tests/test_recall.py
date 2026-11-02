"""2.4.1: bring a driver who is out back with his run (the tablet's ↺ beside
"I have returned"): his undelivered orders go back to his pool, ready, he is
next in the queue, and SEND OUT sends him again with any stops added."""

from __future__ import annotations

import requests

from test_driver_reports import deliver_order, driver
from test_floor import floor, post  # noqa: F401  (fixture re-export)
from test_v220 import pair


def _drivers(a) -> dict[str, dict]:
    return {d["id"]: d for d in a.http.get(f"{a.base}/api/drivers", timeout=5).json()}


def test_recall_driver_with_his_run(floor) -> None:  # noqa: F811
    a = floor
    tablet = pair(a, "Counter")
    abdul, bea = driver(a, "Abdul"), driver(a, "Bea")
    o1 = deliver_order(a, "Erik Davis", 0, 2249, True)
    o2 = deliver_order(a, "Ana Perez", 0, 1850, False)

    # Not out yet: nothing to bring back; an unknown driver is a 404.
    r = requests.post(f"{a.base}/api/drivers/{abdul}/recall", headers=tablet, timeout=5)
    assert r.status_code == 409 and r.json()["code"] == "driver_not_out", r.text
    assert requests.post(f"{a.base}/api/drivers/nope/recall", headers=tablet, timeout=5).status_code == 404

    assert post(a, "/api/deliveries/pool", {"order_ids": [o1, o2], "driver_id": abdul}).status_code == 200
    r = post(a, f"/api/drivers/{abdul}/send-out", {})
    assert r.status_code == 200 and {o["status"]["state"] for o in r.json()} == {"on_delivery"}, r.text

    r = requests.post(f"{a.base}/api/drivers/{abdul}/recall", headers=tablet, timeout=5)
    assert r.status_code == 200, r.text
    back = r.json()
    assert {o["order"]["id"] for o in back} == {o1, o2}
    for o in back:
        st = o["status"]
        assert st["state"] == "ready" and st["pool_driver_id"] == abdul and "driver_id" not in st
    ds = _drivers(a)
    assert ds[abdul]["status"] == "available" and ds[abdul]["position"] < ds[bea]["position"]

    # One more stop, then out again with all three.
    o3 = deliver_order(a, "Lee Min", 0, 3000, True)
    assert post(a, "/api/deliveries/pool", {"order_ids": [o3], "driver_id": abdul}).status_code == 200
    r = post(a, f"/api/drivers/{abdul}/send-out", {})
    assert r.status_code == 200 and {o["order"]["id"] for o in r.json()} == {o1, o2, o3}, r.text
    assert _drivers(a)[abdul]["status"] == "out"
