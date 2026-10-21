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
        "fulfillment": {"type": "pickup"},
        "money": {"subtotal_cents": 1500, "total_cents": 1500, "currency": "USD"},
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
    # Pizza station: the kitchen number + customer, its own item in full (big,
    # with size), the unmatched item flagged, the rest of the order listed small
    # after the rule.
    assert "#1100\n" in pizza and "KITCHEN, PYTEST" in pizza, pizza
    assert "1 Pepperoni Pizza\n  (Large)" in pizza, pizza
    assert "1 Mystery Dish" in pizza and "UNMATCHED" in pizza
    assert "\x1b!\x00\x1bE\x01ITEMS TO OTHER PRINTERS" in pizza and "\n1 Hot Wings\n" in pizza
    assert "$" not in pizza, "station tickets carry no prices"
    # Wings station mirrors it.
    assert "1 Hot Wings" in wings and "\n1 Pepperoni Pizza (Large)\n" in wings
    # Front = the full customer receipt only (v1 layout: size on the price
    # line, the item name under it).
    assert len(fakes["Front"].jobs) == 1 and "Total:" in front and "1   Large" in front and "    Pepperoni Pizza\n" in front
    assert "REROUTED" not in pizza + wings + front


def test_items_on_the_receipt_printer_print_only_on_the_receipt(kitchen, fakes) -> None:
    """Drinks/desserts routed to the receipt printer get no slip of their own:
    the pizza station lists them small, and Front prints just the receipt."""
    a, ids = kitchen["agent"], kitchen["ids"]
    snap = _snap(kitchen["main_store"], [
        _item("Pepperoni Pizza", ids["Pizza"]),
        _item("Can Soda", ids["Front"]),
    ])
    r = _accept(a, snap)
    assert r.status_code == 200, r.text + a.log_tail()
    _wait_jobs(fakes["Pizza"], 1)
    _wait_jobs(fakes["Front"], 1)
    time.sleep(1.5)  # nothing else may follow
    assert len(fakes["Front"].jobs) == 1, "Front must print only the receipt"
    assert "Total:" in fakes["Front"].text() and "Can Soda" in fakes["Front"].text()
    pizza = fakes["Pizza"].text()
    assert "1 Pepperoni Pizza" in pizza and "ITEMS TO OTHER PRINTERS" in pizza and "\n1 Can Soda\n" in pizza, pizza
    assert len(fakes["Wings"].jobs) == 0


def test_station_down_reroutes_to_receipt_printer(kitchen, fakes) -> None:
    a, ids = kitchen["agent"], kitchen["ids"]
    fakes["Wings"].stop()
    snap = _snap(kitchen["main_store"], [_item("Pepperoni Pizza", ids["Pizza"]), _item("Hot Wings", ids["Wings"])])
    r = _accept(a, snap)
    assert r.status_code == 200, r.text + a.log_tail()
    _wait_jobs(fakes["Front"], 2)
    front = fakes["Front"].text()
    assert "REROUTED from WINGS" in front and "1 Hot Wings" in front, front
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
    time.sleep(0.5)
    assert fakes["Pizza"].jobs == [] and fakes["Wings"].jobs == []
    assert "Old Style Pizza" in fakes["Front"].text()
