"""Kitchen printing on a real (throwaway) agent with FAKE network printers.

Each fake is a TCP listener on 127.0.0.1 that records the ESC/POS bytes of
every print job (main allows `host:port` printer addresses; the agent dials a
bare IP on :9100 and an explicit host:port as-is). Printers are created in
main's /app/store API and pushed to the agent via store_config; orders are fed
through the agent's Resend → Accept path with routing already stamped, the
way main delivers them.
"""

from __future__ import annotations

import time
import uuid
from datetime import datetime, timezone

import pytest
import requests

from gatemark import intentional_sleep
from conftest import ADMIN_V1, STORE_V1, FakePrinter, wait_until

@pytest.fixture
def kitchen(admin, main_store, spawn_agent, fakes):
    """main store with Front (receipt) + Pizza + Wings fake printers, and a
    provisioned, logged-in throwaway agent that has applied that roster."""
    user, pw = f"mgr{uuid.uuid4().hex[:6]}", "pw-kitchen"
    r = admin.patch(f"{ADMIN_V1}/tenants/{main_store.tenant_id}/credentials",
                    json={"username": user, "password": pw}, timeout=5)
    assert r.status_code == 204, r.text
    mgr = requests.Session()
    assert mgr.post(f"{STORE_V1}/session", json={"username": user, "password": pw}, timeout=5).status_code == 200

    ids = {}
    for name in ("Front", "Pizza", "Wings"):
        fakes[name] = FakePrinter()
        r = mgr.post(f"{STORE_V1}/me/printers", json={"name": name, "ip": fakes[name].addr}, timeout=5)
        assert r.status_code == 201, r.text
        ids[name] = r.json()["id"]

    agent = spawn_agent()
    token = main_store.mint()["token"]
    assert requests.post(f"{agent.base}/api/setup/token", json={"token": token}, timeout=10).status_code == 200
    assert requests.post(f"{agent.base}/api/setup/password", json={"password": "pw", "confirm": "pw"}, timeout=10).status_code == 200
    assert agent.http.post(f"{agent.base}/api/login", json={"password": "pw"}, timeout=10).status_code == 200
    # 2.2.1 auto-assign off: these tests build their pools by hand.
    assert agent.http.put(f"{agent.base}/api/settings/deliveries", json={"auto_assign": False}, timeout=10).status_code == 200
    wait_until(lambda: len(agent.http.get(f"{agent.base}/api/printers", timeout=5).json()) == 3,
               timeout=20, what="agent to apply the 3-printer roster")
    return {"agent": agent, "ids": ids, "main_store": main_store}


def _item(name: str, printer_id: str = "", routing: str = "matched", size: str = "") -> dict:
    return {"id": str(uuid.uuid4()), "name": name, "size": size, "quantity": 1, "unit_price_cents": 500,
            "line_total_cents": 500, "routing": routing, "printer_id": printer_id}


def _snap(store, items: list[dict]) -> dict:
    return {
        "id": str(uuid.uuid4()), "tenant_id": store.tenant_id, "location_id": store.location_id,
        "source": "online", "source_event_id": str(int(time.time() * 1000) % 10**7),
        "placed_at": datetime.now(timezone.utc).isoformat(),
        "customer": {"display_name": "Pytest Kitchen"},
        # A delivery: since agent 2.5.7 only a delivery's paper prints on the receipt printer (a pickup's
        # receipt prints at its Cash Out on a terminal: test_pickup_prints_only_the_station_tickets).
        "fulfillment": {"type": "delivery", "address": {"line1": "1 Test St", "city": "Towson"}},
        "money": {"subtotal_cents": 1500, "total_cents": 1500, "currency": "USD"},
        # Paid by card (a cash delivery follows its own switch: see
        # test_pickup_prints_only_the_station_tickets).
        "payments": [{"method": "card", "amount_cents": 1500}],
        "items": items,
    }


def _accept(agent, snap: dict) -> requests.Response:
    r = agent.http.post(f"{agent.base}/api/resend", json=snap, timeout=15)
    assert r.status_code == 200, r.text
    return agent.http.post(f"{agent.base}/api/orders/{snap['id']}/accept", timeout=30)


def _wait_jobs(fake: FakePrinter, n: int) -> None:
    wait_until(lambda: len(fake.jobs) >= n, timeout=10, what=f"{n} job(s) on fake printer")


def test_station_tickets_and_receipt(kitchen, fakes) -> None:
    a, ids = kitchen["agent"], kitchen["ids"]
    snap = _snap(kitchen["main_store"], [
        _item("Pepperoni Pizza", ids["Pizza"], size="Large"),
        _item("Hot Wings", ids["Wings"]),
        _item("Mystery Dish", routing="unmatched"),
    ])
    r = _accept(a, snap)
    assert r.status_code == 200, r.text + a.log_tail()
    for f in fakes.values():
        _wait_jobs(f, 1)

    pizza, wings, front = fakes["Pizza"].text(), fakes["Wings"].text(), fakes["Front"].text()
    # Pizza station (the 2026-11-11 kitchen ticket): the type + print time under
    # the rail, the kitchen number + customer, its own item in full (big, with
    # size), the unmatched item flagged, the rest of the order under "ITEMS TO
    # OTHER PRINTER" one size smaller (the owner, 2026-11-14).
    assert pizza.startswith("\x1b@\x1ba\x01\x1bE\x01\x1d!\x11DELIVERY\n") and "Printed " in pizza, pizza
    assert "\x1d!3100\n" in pizza and "KITCHEN, PYTEST" in pizza, pizza
    # 2.5.7: the item's choices indented under it (two past the name, which prints double wide);
    # 2026-11-14: three spaces after the quantity, so the choices start at column 10.
    assert "1   Pepperoni Pizza\n" in pizza and "\x1bE\x00          - Large\n" in pizza, pizza
    assert "1   Mystery Dish" in pizza and "UNMATCHED" in pizza
    assert "\x1b!\x00ITEMS TO OTHER PRINTER\n - 1   Hot Wings\n" in pizza, pizza
    assert "\x1dB\x01" not in pizza, "no reverse print on an order ticket"
    assert "$" not in pizza, "station tickets carry no prices"
    # Wings station mirrors it.
    assert "1   Hot Wings" in wings and "\n - 1   Pepperoni Pizza (Large)\n" in wings
    # Front = the customer receipt only, in SoftTouch's layout (2026-11-12):
    # quantity, the size and name in capitals, the price on the right.
    assert len(fakes["Front"].jobs) == 1 and "Total:" in front and "1   Large Pepperoni Pizza " in front, front
    assert "REROUTED" not in pizza + wings + front


def test_items_on_the_receipt_printer_print_only_on_the_receipt(kitchen, fakes) -> None:
    """Drinks/desserts routed to the receipt printer get no slip of their own:
    the pizza station leaves them off (2.4.0), and Front prints just the
    receipt."""
    a, ids = kitchen["agent"], kitchen["ids"]
    snap = _snap(kitchen["main_store"], [
        _item("Pepperoni Pizza", ids["Pizza"]),
        _item("Can Soda", ids["Front"]),
    ])
    r = _accept(a, snap)
    assert r.status_code == 200, r.text + a.log_tail()
    _wait_jobs(fakes["Pizza"], 1)
    _wait_jobs(fakes["Front"], 1)
    intentional_sleep(1.5, "absence: nothing else may print")
    assert len(fakes["Front"].jobs) == 1, "Front must print only the receipt"
    assert "Total:" in fakes["Front"].text() and "Can Soda" in fakes["Front"].text()
    pizza = fakes["Pizza"].text()
    # 2.4.0: a receipt-only item isn't listed for the cooks.
    assert "1   Pepperoni Pizza" in pizza and "Can Soda" not in pizza, pizza
    assert len(fakes["Wings"].jobs) == 0


def test_pickup_prints_only_the_station_tickets(kitchen, fakes) -> None:
    """Agent 2.5.7 (the owner, 2026-11-13): the receipt printer (Lombardi's "Online") prints only a
    delivery's paper. A pickup, card or cash, with the online_cash_pickup switch on or off, prints its
    station tickets and nothing on Front (its receipt prints at Cash Out on a terminal). A cash
    delivery still follows its own switch: off = the customer receipt only."""
    a, ids = kitchen["agent"], kitchen["ids"]

    def order(typ: str, cash: bool) -> dict:
        snap = _snap(kitchen["main_store"], [_item("Pepperoni Pizza", ids["Pizza"]), _item("Hot Wings", ids["Wings"])])
        snap["fulfillment"] = {"type": typ}
        if cash:
            snap.pop("payments")  # no payments = the whole total is cash
        return snap

    for switch in (True, False):
        assert a.http.put(f"{a.base}/api/settings/tickets", json={"online_cash_pickup": switch}, timeout=5).status_code == 200
        for cash in (True, False):
            _reset(fakes)
            r = _accept(a, order("pickup", cash))
            assert r.status_code == 200, r.text + a.log_tail()
            _wait_jobs(fakes["Pizza"], 1)
            _wait_jobs(fakes["Wings"], 1)
            intentional_sleep(1.0, "absence: nothing may print on Front")
            assert fakes["Front"].jobs == [], f"a pickup (cash={cash}, switch={switch}) printed on the receipt printer"

    _reset(fakes)
    assert a.http.put(f"{a.base}/api/settings/tickets", json={"online_cash_delivery": False}, timeout=5).status_code == 200
    d = order("delivery", True)
    d["fulfillment"]["address"] = {"line1": "1 Test St", "city": "Towson"}
    r = _accept(a, d)
    assert r.status_code == 200, r.text + a.log_tail()
    _wait_jobs(fakes["Front"], 1)
    intentional_sleep(1.5, "absence: nothing else may print")
    assert len(fakes["Front"].jobs) == 1 and "Total:" in fakes["Front"].text()
    assert len(fakes["Pizza"].jobs) == 0 and len(fakes["Wings"].jobs) == 0, "cash delivery, switch off: the receipt only"


def test_station_down_reroutes_to_receipt_printer(kitchen, fakes) -> None:
    a, ids = kitchen["agent"], kitchen["ids"]
    fakes["Wings"].stop()
    snap = _snap(kitchen["main_store"], [_item("Pepperoni Pizza", ids["Pizza"]), _item("Hot Wings", ids["Wings"])])
    r = _accept(a, snap)
    assert r.status_code == 200, r.text + a.log_tail()
    _wait_jobs(fakes["Front"], 2)
    front = fakes["Front"].text()
    assert "REROUTED FROM WINGS" in front and "1   Hot Wings" in front, front
    assert "Total:" in front, "the receipt still prints on Front"
    assert fakes["Wings"].jobs == []


def test_nothing_printed_keeps_order_arrived(kitchen, fakes) -> None:
    a, ids = kitchen["agent"], kitchen["ids"]
    for f in fakes.values():
        f.stop()
    snap = _snap(kitchen["main_store"], [_item("Pepperoni Pizza", ids["Pizza"])])
    r = _accept(a, snap)
    assert r.status_code == 502 and r.json()["code"] == "nothing_printed", r.text
    got = a.http.get(f"{a.base}/api/orders/{snap['id']}", timeout=5).json()
    assert got["status"]["state"] == "arrived", got


def test_legacy_order_prints_one_full_receipt(kitchen, fakes) -> None:
    a = kitchen["agent"]
    item = _item("Old Style Pizza")
    item.pop("routing"), item.pop("printer_id")
    r = _accept(a, _snap(kitchen["main_store"], [item]))
    assert r.status_code == 200, r.text
    _wait_jobs(fakes["Front"], 1)
    intentional_sleep(0.5, "absence: nothing else may print")
    assert fakes["Pizza"].jobs == [] and fakes["Wings"].jobs == []
    assert "Old Style Pizza" in fakes["Front"].text()


def _reset(fakes) -> None:
    for f in fakes.values():
        f.clear()


def test_reprint_prints_only_the_receipt(kitchen, fakes) -> None:
    """Reprint = the customer receipt again, on Front only."""
    a, ids = kitchen["agent"], kitchen["ids"]
    snap = _snap(kitchen["main_store"], [_item("Pepperoni Pizza", ids["Pizza"]), _item("Hot Wings", ids["Wings"])])
    assert _accept(a, snap).status_code == 200
    for f in fakes.values():
        _wait_jobs(f, 1)
    intentional_sleep(0.5, "every job lands before the next check")
    _reset(fakes)

    r = a.http.post(f"{a.base}/api/orders/{snap['id']}/reprint", timeout=30)
    assert r.status_code == 200 and r.json()["order"]["id"] == snap["id"], r.text
    _wait_jobs(fakes["Front"], 1)
    intentional_sleep(1.5, "absence: nothing else may print")
    assert len(fakes["Front"].jobs) == 1 and "Total:" in fakes["Front"].text()
    assert fakes["Pizza"].jobs == [] and fakes["Wings"].jobs == [], "reprint must not print kitchen tickets"

    # History's /api/print (a fetched snapshot) is receipt-only too.
    _reset(fakes)
    r = a.http.post(f"{a.base}/api/print", json=snap, timeout=30)
    assert r.status_code == 200, r.text
    _wait_jobs(fakes["Front"], 1)
    intentional_sleep(1.5, "absence: nothing else may print")
    assert len(fakes["Front"].jobs) == 1 and "Total:" in fakes["Front"].text()
    assert fakes["Pizza"].jobs == [] and fakes["Wings"].jobs == []


def test_resend_to_kitchen_prints_only_the_stations(kitchen, fakes) -> None:
    a, ids = kitchen["agent"], kitchen["ids"]
    snap = _snap(kitchen["main_store"], [_item("Pepperoni Pizza", ids["Pizza"]), _item("Hot Wings", ids["Wings"])])
    assert _accept(a, snap).status_code == 200
    for f in fakes.values():
        _wait_jobs(f, 1)
    intentional_sleep(0.5, "every job lands before the next check")
    _reset(fakes)

    r = a.http.post(f"{a.base}/api/orders/{snap['id']}/kitchen", timeout=30)
    assert r.status_code == 200, r.text + a.log_tail()
    body = r.json()
    assert body["order"]["order"]["id"] == snap["id"], body
    assert sorted(body["printed"]) == ["Pizza", "Wings"] and body["failed"] == [] and body["rerouted"] == [], body
    _wait_jobs(fakes["Pizza"], 1)
    _wait_jobs(fakes["Wings"], 1)
    intentional_sleep(1.5, "the rest of the tickets print")
    assert "1   Pepperoni Pizza" in fakes["Pizza"].text() and "1   Hot Wings" in fakes["Wings"].text()
    assert fakes["Front"].jobs == [], "a kitchen resend prints no receipt"
    got = a.http.get(f"{a.base}/api/orders/{snap['id']}", timeout=5).json()
    assert got["status"]["state"] == "accepted", got

    # A station down: rerouted to Front with the banner, reported in the body.
    _reset(fakes)
    fakes["Wings"].stop()
    r = a.http.post(f"{a.base}/api/orders/{snap['id']}/kitchen", timeout=30)
    assert r.status_code == 200, r.text
    assert r.json()["rerouted"] == [{"station": "Wings", "printed_on": "Front"}], r.text
    _wait_jobs(fakes["Front"], 1)
    assert "REROUTED FROM WINGS" in fakes["Front"].text() and "Total:" not in fakes["Front"].text()


def test_resend_to_kitchen_of_delivery_cash_prints_the_stations(kitchen, fakes) -> None:
    """Accept of a delivery paid in cash with its switch off prints only the
    receipt; a deliberate resend to the kitchen prints the station tickets
    anyway."""
    a, ids = kitchen["agent"], kitchen["ids"]
    assert a.http.put(f"{a.base}/api/settings/tickets", json={"online_cash_delivery": False}, timeout=5).status_code == 200
    snap = _snap(kitchen["main_store"], [_item("Pepperoni Pizza", ids["Pizza"]), _item("Hot Wings", ids["Wings"])])
    snap.pop("payments")
    assert _accept(a, snap).status_code == 200
    _wait_jobs(fakes["Front"], 1)
    intentional_sleep(1.5, "absence: nothing else may print")
    assert fakes["Pizza"].jobs == [] and fakes["Wings"].jobs == []
    _reset(fakes)

    r = a.http.post(f"{a.base}/api/orders/{snap['id']}/kitchen", timeout=30)
    assert r.status_code == 200 and sorted(r.json()["printed"]) == ["Pizza", "Wings"], r.text
    _wait_jobs(fakes["Pizza"], 1)
    _wait_jobs(fakes["Wings"], 1)
    intentional_sleep(1.5, "absence: nothing else may print")
    assert fakes["Front"].jobs == []


def test_resend_to_kitchen_refusals(kitchen, fakes) -> None:
    a, ids = kitchen["agent"], kitchen["ids"]
    # Waiting (not accepted): 409 not_accepted, nothing printed.
    waiting = _snap(kitchen["main_store"], [_item("Pepperoni Pizza", ids["Pizza"])])
    assert a.http.post(f"{a.base}/api/resend", json=waiting, timeout=15).status_code == 200
    r = a.http.post(f"{a.base}/api/orders/{waiting['id']}/kitchen", timeout=30)
    assert r.status_code == 409 and r.json()["code"] == "not_accepted", r.text

    # Only receipt-only items: 409 nothing_for_kitchen.
    soda = _snap(kitchen["main_store"], [_item("Can Soda", ids["Front"])])
    assert _accept(a, soda).status_code == 200
    _wait_jobs(fakes["Front"], 1)
    r = a.http.post(f"{a.base}/api/orders/{soda['id']}/kitchen", timeout=30)
    assert r.status_code == 409 and r.json()["code"] == "nothing_for_kitchen", r.text

    # Unknown id: 404.
    r = a.http.post(f"{a.base}/api/orders/{uuid.uuid4()}/kitchen", timeout=10)
    assert r.status_code == 404, r.text

    # Every printer down: 502 print_failed.
    accepted = _snap(kitchen["main_store"], [_item("Pepperoni Pizza", ids["Pizza"])])
    assert _accept(a, accepted).status_code == 200
    _wait_jobs(fakes["Pizza"], 1)
    intentional_sleep(0.5, "every job lands before the next check")
    before = {n: len(f.jobs) for n, f in fakes.items()}
    for f in fakes.values():
        f.stop()
    r = a.http.post(f"{a.base}/api/orders/{accepted['id']}/kitchen", timeout=60)
    assert r.status_code == 502 and r.json()["code"] == "print_failed", r.text
    intentional_sleep(0.5, "absence: nothing new prints")
    assert {n: len(f.jobs) for n, f in fakes.items()} == before, "nothing new printed"
