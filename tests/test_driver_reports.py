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
from test_terminal import counter_menu, rig, staff_config  # noqa: F401  (rig is a fixture)


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
    # 2.5.2, the owner's settlement: hand in = cash - delivery pay x every delivery - card tips after 6 %
    # (the store's delivery_driver_cents, $3 unless the counter menu says otherwise).
    # 2.5.8, the owner's settlement (default: main's driver_pay_from_cash off): hand in = cash - card tips after
    # 6 %; the delivery pay is paid weekly, so it isn't on the report (only the count).
    assert (ab["driver_share_cents"], ab["driver_pay_cents"], ab["driver_pay_from_cash"]) == (0, 0, False), ab
    assert all(not ln.get("driver_share_cents") for ln in ab["lines"]), ab["lines"]
    assert (ab["deliveries"], ab["cash_cents"], ab["tip_cents"], ab["tip_after_tax_cents"], ab["owed_cents"]) == (3, 3599, 400, 376, 3223)
    assert ab["lines"][0]["customer"] == "Erik D." and ab["lines"][2]["cash"] is False
    assert (ab["lines"][0]["cash_cents"], ab["lines"][0]["tip_cents"]) == (2049, 0)
    assert ab["owed_rounded_cents"] == 3200  # whole dollars: 23 cents round down
    assert by["Carlos"]["owed_cents"] == -940  # card only: the store owes his tip after 6 %
    assert by["Carlos"]["owed_rounded_cents"] == -900
    assert by["Deep"]["deliveries"] == 0

    before = len(fakes["Front"].jobs)
    r = post(a, "/api/batch/close", {"force": True})
    assert r.status_code == 200, r.text
    closed = r.json()["closed"]["id"]
    wait_until(lambda: len(fakes["Front"].jobs) >= before + 3, timeout=20, what="3 driver reports printed after close")
    slips = [j.decode("latin-1") if isinstance(j, bytes) else j for j in fakes["Front"].jobs[before:before + 3]]
    abdul_slip = next(s for s in slips if "ABDUL" in s)
    for want in ["DRIVER REPORT", "CASH ORDERS (2)", "CARD ORDERS (1)", "Total: $20.49", "Cash collected", "$35.99",
                 "Card tips after 6%", "-$3.76", "HAND IN", "$32.00", "exact $32.23", "Tip: $4.00", "Total: $30.00"]:
        assert want in abdul_slip, want
    for not_there in ["Delivery pay", "($3.00 x 3)", "-$9.00"]:
        assert not_there not in abdul_slip, f"paid weekly: no delivery pay on the slip: {not_there}"
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


def test_driver_pay_from_cash_setting(rig, main_store) -> None:  # noqa: F811
    """main's driver_pay_from_cash (agent 2.5.8, Store › Settings › End of day; the owner 2026-11-13): off (missing,
    the default) = hand in = cash - card tips after 6 %, the delivery pay paid weekly (only the count); on = the
    store's delivery_driver_cents x every delivery comes off too (the 2.5.2 rule). Read at report time: the same
    batch counts again when it changes. The owner's sample: cash $40 + $25, a card order with a $5 tip, 3 deliveries."""
    a, fm, term, fakes = rig["agent"], rig["fm"], rig["term"], rig["fakes"]
    a.store = main_store
    menu = counter_menu(rig["pizza_id"], version=8)
    menu["pricing"]["settings"]["delivery_driver_cents"] = 300
    fm.counter_menu = menu
    fm.command("menu.changed", {"version": 8})
    wait_until(lambda: term.get(f"{a.base}/api/terminal/menu", timeout=5).json()["menu_version"] == 8, what="menu v8")

    def config(version: int, on: bool | None) -> None:
        cfg = staff_config(version)
        if on is not None:
            cfg["settings"]["driver_pay_from_cash"] = on
        fm.terminal_config = cfg
        fm.command("terminal.changed", {"version": version})
        wait_until(lambda: term.get(f"{a.base}/api/terminal/session", timeout=5).json().get("config_version") == version,
                   what=f"config v{version}")

    def report(did: str) -> dict:
        return next(r for r in a.http.get(f"{a.base}/api/batches/current/driver-reports", timeout=5).json() if r["driver_id"] == did)

    ali = driver(a, "Ali")
    run(a, ali, deliver_order(a, "Erik Davis", 0, 4000, True), deliver_order(a, "Ana Perez", 0, 2500, True),
        deliver_order(a, "Lee Min", 500, 3000, False))

    def fields(r: dict) -> tuple:
        return (r["deliveries"], r["cash_cents"], r["tip_cents"], r["tip_after_tax_cents"], r["driver_share_cents"],
                r["driver_pay_cents"], r["driver_pay_from_cash"], r["owed_cents"], r["owed_rounded_cents"])

    # Not sent (missing) = off: $65.00 - $4.70 = $60.30 -> HAND IN $60.
    assert fields(report(ali)) == (3, 6500, 500, 470, 0, 0, False, 6030, 6000)
    config(2, True)  # on: - $3.00 x 3 as well -> $51.30 -> $51
    r = report(ali)
    assert fields(r) == (3, 6500, 500, 470, 900, 300, True, 5130, 5100)
    assert [ln["driver_share_cents"] for ln in r["lines"]] == [300, 300, 300]
    n = len(fakes["Front"].jobs)
    assert post(a, f"/api/drivers/{ali}/report/print").status_code == 200
    wait_until(lambda: len(fakes["Front"].jobs) > n, what="Ali's slip (pay from the cash)")
    slip = fakes["Front"].jobs[-1].decode("latin-1")
    for want in ["Cash collected", "$65.00", "Delivery pay", "-$9.00", "($3.00 x 3)", "Card tips after 6%", "-$4.70", "HAND IN", "$51.00", "exact $51.30"]:
        assert want in slip, want
    config(3, False)  # off again: the same batch counts without the pay
    assert fields(report(ali)) == (3, 6500, 500, 470, 0, 0, False, 6030, 6000)
    n = len(fakes["Front"].jobs)
    assert post(a, f"/api/drivers/{ali}/report/print").status_code == 200
    wait_until(lambda: len(fakes["Front"].jobs) > n, what="Ali's slip (paid weekly)")
    slip = fakes["Front"].jobs[-1].decode("latin-1")
    for want in ["Deliveries", "Cash collected", "$65.00", "Card tips after 6%", "-$4.70", "HAND IN", "$60.00", "exact $60.30"]:
        assert want in slip, want
    assert "Delivery pay" not in slip and "x 3)" not in slip


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
