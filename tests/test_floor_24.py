"""Agent 2.4.0 floor release on throwaway agents (fake printers only):

  * MARK DELIVERED -> Unassigned (who, from the paired tablet), Back to
    deliveries, moving it to a driver and back, and Close batch printing the
    UNASSIGNED slip after the driver reports;
  * READY held on the tablet (callout false) rides the hub feed;
  * a future order's early tickets (the two switches): the MAKE AT / READY
    band on the stations, FUTURE on the receipt, and the CONFIRM without the receipt;
  * kitchen tickets list no receipt-only items under "other printers", and
    say the order type above the number.
"""

from __future__ import annotations

import uuid
from datetime import datetime, timedelta, timezone

import requests

from conftest import wait_until
from test_driver_reports import deliver_order, driver
from test_floor import floor, post  # noqa: F401  (fixture re-export)
from test_integration import _feed, _hub
from test_kitchen_printing import _accept, _item, _snap, kitchen  # noqa: F401  (kitchen is a fixture)
from test_v220 import pair


def test_unassigned_flow_and_report(floor, fakes) -> None:  # noqa: F811
    a = floor
    tablet = pair(a, "Counter")
    abdul = driver(a, "Abdul")
    o1 = deliver_order(a, "Erik Davis", 200, 2249, True)
    o2 = deliver_order(a, "Ana Perez", 300, 1850, False)

    r = requests.post(f"{a.base}/api/orders/{o1}/delivered", headers=tablet, timeout=5)
    assert r.status_code == 200, r.text
    st = r.json()["status"]
    assert st["state"] == "delivered" and "driver_id" not in st
    md = st["marked_delivered"]
    assert md["by"]["kind"] == "tablet" and md["by"]["name"] == "Counter" and md["available"] == ["Abdul"]
    assert md["prev_state"] == "ready"
    assert requests.post(f"{a.base}/api/orders/{o1}/delivered", headers=tablet, timeout=5).status_code == 409

    # Back to deliveries, and marked again.
    r = requests.post(f"{a.base}/api/orders/{o1}/undeliver", headers=tablet, timeout=5)
    assert r.status_code == 200 and r.json()["status"]["state"] == "ready", r.text
    assert requests.post(f"{a.base}/api/orders/{o1}/undeliver", headers=tablet, timeout=5).json()["code"] == "not_unassigned"
    assert requests.post(f"{a.base}/api/orders/{o1}/delivered", headers=tablet, timeout=5).status_code == 200
    assert requests.post(f"{a.base}/api/orders/{o2}/delivered", headers=tablet, timeout=5).status_code == 200

    # o2 goes to Abdul (his report), and back to Unassigned is allowed too.
    r = requests.post(f"{a.base}/api/deliveries/reassign", json={"order_ids": [o2], "driver_id": abdul}, headers=tablet, timeout=5)
    assert r.status_code == 200 and r.json()[0]["status"]["driver_name"] == "Abdul", r.text
    reps = {x["driver_name"]: x for x in a.http.get(f"{a.base}/api/batches/current/driver-reports", timeout=5).json()}
    assert reps["Abdul"]["deliveries"] == 1
    r = requests.post(f"{a.base}/api/deliveries/reassign", json={"order_ids": [o2], "driver_id": ""}, headers=tablet, timeout=5)
    assert r.status_code == 200 and r.json()[0]["status"]["marked_delivered"]["from_driver"] == "Abdul"
    r = requests.post(f"{a.base}/api/deliveries/reassign", json={"order_ids": [o2], "driver_id": abdul}, headers=tablet, timeout=5)
    assert r.status_code == 200

    # A clocked-out driver takes nothing.
    gone = driver(a, "Gone")
    assert post(a, f"/api/drivers/{gone}/clock-out").status_code == 200
    r = requests.post(f"{a.base}/api/deliveries/reassign", json={"order_ids": [o2], "driver_id": gone}, headers=tablet, timeout=5)
    assert r.status_code == 409 and r.json()["code"] == "driver_not_available"

    # Close batch: Abdul's report, Gone's, then the UNASSIGNED slip (o1).
    before = len(fakes["Front"].jobs)
    assert post(a, "/api/batch/close", {"force": True}).status_code == 200
    wait_until(lambda: any("UNASSIGNED" in j for j in _texts(fakes["Front"].jobs[before:])), timeout=20,
               what="the UNASSIGNED slip after the driver reports")
    # Gone's clock-out report (the open batch, "- open") prints asynchronously
    # and can land after `before` under load: only the Close batch slips count.
    slips = [s for s in _texts(fakes["Front"].jobs[before:]) if " - open\n" not in s]
    un = next(s for s in slips if "UNASSIGNED" in s)
    for want in ["DELIVERED - NO DRIVER", "12 Main St", "CASH", "Marked by tablet Counter", "In store then: Abdul", "WHO TOOK THESE?"]:
        assert want in un, want
    assert slips.index(un) == len(slips) - 1, "printed last"


def _texts(jobs) -> list[str]:
    return [j.decode("latin-1") if isinstance(j, bytes) else j for j in jobs]


def _order(a, oid: str) -> dict:
    return a.http.get(f"{a.base}/api/orders/{oid}", timeout=5).json()


def test_ready_held_on_the_feed(kitchen) -> None:  # noqa: F811
    a, ids = kitchen["agent"], kitchen["ids"]
    hub = _hub(kitchen)
    counter = _snap(kitchen["main_store"], [_item("Pepperoni", ids["Pizza"])])
    assert _accept(a, counter).status_code == 200
    r = a.http.post(f"{a.base}/api/orders/{counter['id']}/ready", json={"callout": False}, timeout=10)
    assert r.status_code == 200, r.text
    ready = [e for e in _feed(a, hub)["events"] if e["type"] == "order.ready"]
    assert ready[-1]["data"]["callout"] is False and ready[-1]["data"]["order_id"] == counter["id"]
    # A tap (no body) leaves callout out: the hub calls it out as before.
    other = _snap(kitchen["main_store"], [_item("Pepperoni", ids["Pizza"])])
    assert _accept(a, other).status_code == 200
    assert a.http.post(f"{a.base}/api/orders/{other['id']}/ready", timeout=10).status_code == 200
    assert "callout" not in [e for e in _feed(a, hub)["events"] if e["type"] == "order.ready"][-1]["data"]


def test_future_early_tickets(kitchen, fakes) -> None:  # noqa: F811
    a, ids = kitchen["agent"], kitchen["ids"]
    r = a.http.put(f"{a.base}/api/settings/tickets", json={"future_kitchen": True, "future_receipt": True}, timeout=5)
    assert r.status_code == 200, r.text
    promised = datetime.now(timezone.utc) + timedelta(hours=2)
    later = _snap(kitchen["main_store"], [_item("Pepperoni", ids["Pizza"]), _item("Can Coke", ids["Front"])])
    # 2.5.7: a future pickup's early print is the kitchen's only (its receipt prints at its Cash Out).
    pick = _snap(kitchen["main_store"], [_item("Pepperoni", ids["Pizza"])])
    pick["fulfillment"] = {"type": "pickup", "promised_at": promised.isoformat()}
    assert a.http.post(f"{a.base}/api/resend", json=pick, timeout=15).status_code == 200
    assert a.http.post(f"{a.base}/api/orders/{pick['id']}/schedule", json={"lead_minutes": 20}, timeout=5).status_code == 200
    wait_until(lambda: fakes["Pizza"].jobs, timeout=10, what="the pickup's early kitchen ticket")
    wait_until(lambda: _order(a, pick["id"])["status"].get("early_kitchen") is True, timeout=5, what="early kitchen flag")
    assert not fakes["Front"].jobs and not _order(a, pick["id"])["status"].get("early_receipt"), "no early receipt for a pickup"
    for f in fakes.values():
        f.clear()
    # A future delivery: the early kitchen ticket and its receipt with FUTURE on the receipt printer.
    later["fulfillment"] = {"type": "delivery", "promised_at": promised.isoformat(), "address": {"line1": "1 Main St"}}
    assert a.http.post(f"{a.base}/api/resend", json=later, timeout=15).status_code == 200
    r = a.http.post(f"{a.base}/api/orders/{later['id']}/schedule", json={"lead_minutes": 20}, timeout=5)
    assert r.status_code == 200, r.text
    wait_until(lambda: fakes["Pizza"].jobs and fakes["Front"].jobs, timeout=10, what="the early tickets")
    pizza = fakes["Pizza"].text()
    assert pizza.count("MAKE AT ") == 1 and " READY " in pizza and "\x1d!\x11DELIVERY\n" in pizza
    assert "Can Coke" not in pizza, "a receipt-only item isn't listed for the cooks"
    assert "FUTURE" in fakes["Front"].text()
    wait_until(lambda: _order(a, later["id"])["status"].get("early_receipt") is True, timeout=5, what="early flags")
    # A new time: nothing prints again.
    n = len(fakes["Pizza"].jobs) + len(fakes["Front"].jobs)
    assert a.http.post(f"{a.base}/api/orders/{later['id']}/schedule", json={"lead_minutes": 30}, timeout=5).status_code == 200
    assert len(fakes["Pizza"].jobs) + len(fakes["Front"].jobs) == n

    # The TEST PRINTs.
    for kind, want in [("future_kitchen", "MAKE AT "), ("future_receipt", "FUTURE")]:
        r = a.http.post(f"{a.base}/api/printers/sample", json={"printer_id": ids["Front"], "kind": kind}, timeout=15)
        assert r.status_code == 200 and r.json()["results"][0]["ok"], r.text
        wait_until(lambda: want in fakes["Front"].text(), timeout=5, what=f"the {kind} test print")
    assert a.http.post(f"{a.base}/api/printers/sample", json={"kind": "nope"}, timeout=5).status_code == 400


def test_kitchen_ticket_header_and_others(kitchen, fakes) -> None:  # noqa: F811
    a, ids = kitchen["agent"], kitchen["ids"]
    snap = _snap(kitchen["main_store"], [_item("Pepperoni", ids["Pizza"]), _item("Wings", ids["Wings"]), _item("Can Coke", ids["Front"])])
    snap["fulfillment"] = {"type": "delivery", "address": {"line1": "1 Main St"}}
    assert a.http.post(f"{a.base}/api/resend", json=snap, timeout=15).status_code == 200
    assert a.http.post(f"{a.base}/api/orders/{snap['id']}/accept", json={"prep_minutes": 20}, timeout=30).status_code == 200
    wait_until(lambda: fakes["Pizza"].jobs and fakes["Front"].jobs, timeout=10, what="tickets")
    pizza = fakes["Pizza"].text()
    assert pizza.index("DELIVERY") < pizza.index("\x1d!3100\n"), "the type above the number"
    assert "Wings" in pizza and "Can Coke" not in pizza
    receipt = fakes["Front"].text()
    # the type is a picture since 2026-11-14 (DELIVERY + its car, one paper-wide GS v 0 under the number;
    # the header's logo, when there is one, is a picture above it)
    num = receipt.index("Order #")
    assert receipt.find("\x1dv0", num) > num, "the receipt: number, then the type"
    assert uuid.UUID(snap["id"])
