"""Driver reports (one slip per driver: deliveries, cash owed, tips after tip
tax, owed to store), undo-return, the floor settings and the Quibase release
cache — on a throwaway agent with one fake printer.
"""

from __future__ import annotations

import hashlib
import secrets
import time
import uuid
from datetime import datetime, timezone

import requests

from conftest import ADMIN_V1, wait_until
from test_floor import floor, post  # noqa: F401  (fixture re-export)


def deliver_order(a, name: str, tip: int, total: int, cash: bool) -> str:
    oid = str(uuid.uuid4())
    pay = {"method": "cash", "amount_cents": total} if cash else {"method": "card", "amount_cents": total, "last4": "4242"}
    snap = {
        "id": oid, "tenant_id": a.store.tenant_id, "location_id": a.store.location_id,
        "source": "online", "source_event_id": str(int(time.time() * 1000) % 10**7),
        "placed_at": datetime.now(timezone.utc).isoformat(),
        "customer": {"display_name": name},
        "fulfillment": {"type": "delivery", "address": {"line1": "12 Main St", "city": "Bayonne"}},
        "money": {"subtotal_cents": total - tip, "tip_cents": tip, "total_cents": total},
        "payments": [pay],
        "items": [{"id": str(uuid.uuid4()), "name": "Plain Cheese", "quantity": 1, "unit_price_cents": total, "line_total_cents": total}],
    }
    assert a.http.post(f"{a.base}/api/resend", json=snap, timeout=10).status_code == 200
    assert post(a, f"/api/orders/{oid}/accept", {"prep_minutes": 15}).status_code == 200
    assert post(a, f"/api/orders/{oid}/ready").status_code == 200
    time.sleep(0.01)
    return oid


def driver(a, name: str) -> str:
    d = post(a, "/api/drivers", {"name": name}).json()
    assert post(a, f"/api/drivers/{d['id']}/clock-in").status_code == 200
    return d["id"]


def run(a, driver_id: str, *orders: str) -> None:
    assert post(a, "/api/deliveries/assign", {"driver_id": driver_id, "order_ids": list(orders)}).status_code == 200
    assert post(a, f"/api/drivers/{driver_id}/returned").status_code == 200


def test_driver_reports_print_on_close(floor, fakes) -> None:
    a = floor
    abdul, carlos, _deep = driver(a, "Abdul"), driver(a, "Carlos"), driver(a, "Deep")  # Deep drives nothing: an empty report
    run(a, abdul, deliver_order(a, "Erik Davis", 200, 2249, True), deliver_order(a, "Ana Perez", 300, 1850, True))
    run(a, abdul, deliver_order(a, "Lee Min", 400, 3000, False))
    run(a, carlos, deliver_order(a, "Sam Tan", 1000, 2500, False))

    reps = a.http.get(f"{a.base}/api/batches/current/driver-reports", timeout=5).json()
    by = {r["driver_name"]: r for r in reps}
    assert set(by) == {"Abdul", "Carlos", "Deep"}
    ab = by["Abdul"]
    # 2.1.0: a cash order's tip is the driver's already (cash - tip, no tip
    # listed); only card tips count.
    assert (ab["deliveries"], ab["cash_cents"], ab["tip_cents"], ab["tip_after_tax_cents"], ab["owed_cents"]) == (3, 3599, 400, 376, 3223)
    assert ab["lines"][0]["customer"] == "Erik D." and ab["lines"][2]["cash"] is False
    assert (ab["lines"][0]["cash_cents"], ab["lines"][0]["tip_cents"]) == (2049, 0)
    assert ab["owed_rounded_cents"] == 3200  # whole dollars: 23 cents round down
    assert by["Carlos"]["owed_cents"] == -940  # card only: the store owes the tip
    assert by["Carlos"]["owed_rounded_cents"] == -900
    assert by["Deep"]["deliveries"] == 0

    before = len(fakes["Front"].jobs)
    r = post(a, "/api/batch/close", {"force": True})
    assert r.status_code == 200, r.text
    closed = r.json()["closed"]["id"]
    wait_until(lambda: len(fakes["Front"].jobs) >= before + 3, timeout=20, what="3 driver reports printed after close")
    slips = [j.decode("latin-1") if isinstance(j, bytes) else j for j in fakes["Front"].jobs[before:before + 3]]
    abdul_slip = next(s for s in slips if "ABDUL" in s)
    for want in ["DRIVER REPORT", "CASH ORDERS (2)", "CARD ORDERS (1)", "Total: $20.49", "Total cash owed", "$35.99", "$3.76",
                 "OWED TO STORE", "$32.00", "exact $32.23", "Tip: $4.00", "Total: $30.00"]:
        assert want in abdul_slip, want
    for not_there in ["Erik", "Ana P", "Lee M"]:
        assert not_there not in abdul_slip, f"no customer names on the driver report: {not_there}"
    assert "STORE OWES DRIVER" in next(s for s in slips if "CARLOS" in s)
    assert "No deliveries this batch" in next(s for s in slips if "DEEP" in s)

    # Reprint the closed batch from the tablet / web UI.
    n = len(fakes["Front"].jobs)
    r = post(a, f"/api/batches/{closed}/driver-reports/print")
    assert r.status_code == 200 and len(r.json()["printed"]) == 3, r.text
    wait_until(lambda: len(fakes["Front"].jobs) >= n + 3, what="reprinted reports")
    assert post(a, "/api/batches/99999/driver-reports/print").status_code == 404
    assert post(a, "/api/batches/abc/driver-reports/print").status_code == 400

    # Printer down → 502, nothing printed.
    fakes["Front"].stop()
    r = post(a, f"/api/batches/{closed}/driver-reports/print")
    assert r.status_code == 502 and r.json()["code"] == "nothing_printed"


def test_undo_return(floor) -> None:
    a = floor
    abdul, carlos = driver(a, "Abdul"), driver(a, "Carlos")
    run(a, abdul, deliver_order(a, "Erik Davis", 200, 2249, True))
    assert post(a, f"/api/drivers/{carlos}/undo-return").json()["code"] == "invalid_state"
    r = post(a, f"/api/drivers/{abdul}/undo-return")
    assert r.status_code == 200 and r.json()["status"] == "out"
    assert post(a, f"/api/drivers/{abdul}/undo-return").status_code == 409
    queue = [d["name"] for d in a.http.get(f"{a.base}/api/drivers", timeout=5).json()]
    assert queue[0] == "Abdul"
    assert post(a, f"/api/drivers/{abdul}/returned").status_code == 200


def test_floor_settings(floor) -> None:
    a = floor
    f = a.http.get(f"{a.base}/api/floor", timeout=5).json()
    assert f["green_till_minutes"] == 20 and f["tip_tax_percent"] == 6
    r = a.http.put(f"{a.base}/api/settings/floor", json={"green_till_minutes": 25, "tip_tax_percent": 6.5}, timeout=5)
    assert r.status_code == 200, r.text
    f = a.http.get(f"{a.base}/api/floor", timeout=5).json()
    assert f["green_till_minutes"] == 25 and f["tip_tax_percent"] == 6.5
    assert a.http.put(f"{a.base}/api/settings/floor", json={"green_till_minutes": 0, "tip_tax_percent": 6}, timeout=5).status_code == 400
    # Web-UI only (a paired tablet can't change it).
    assert requests.put(f"{a.base}/api/settings/floor", json={"green_till_minutes": 30, "tip_tax_percent": 6}, timeout=5).status_code == 401


def test_app_release_cache(floor, admin) -> None:
    """A release uploaded to main reaches the agent's cache (checked at start,
    on every SSE reconnect and every 5 min — the test restarts the agent)."""
    a = floor
    apk = b"PK\x03\x04" + secrets.token_bytes(1024)
    code = int(time.time())  # version codes are epoch seconds (console releases too)
    r = admin.post(f"{ADMIN_V1}/app-releases", params={"app": "quibase", "version_code": code, "version_name": "0.0.test"},
                   data=apk, timeout=30)
    assert r.status_code == 201, r.text
    a.restart()
    wait_until(lambda: a.http.get(f"{a.base}/api/app/latest", timeout=5).status_code == 200, timeout=30, what="release cached")
    latest = a.http.get(f"{a.base}/api/app/latest", timeout=5).json()
    assert latest["version_code"] == code and latest["sha256"] == hashlib.sha256(apk).hexdigest()
    got = a.http.get(f"{a.base}/api/app/apk", timeout=10)
    assert got.status_code == 200 and got.content == apk
    assert requests.get(f"{a.base}/api/app/apk", timeout=5).status_code == 401
    # Don't leave a fake build on dev main for real tablets.
    assert admin.delete(f"{ADMIN_V1}/app-releases/{code}", params={"app": "quibase"}, timeout=5).status_code == 204
