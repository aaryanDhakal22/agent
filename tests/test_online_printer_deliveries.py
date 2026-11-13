"""The Online printer prints only deliveries (agent 2.5.7, the owner 2026-11-13; contract §2.10ai).

Lombardi's receipt printer ("Online"; Front here) gets, of an order's own paper, only a delivery's tickets
(Merchant + Customer Copy), whatever the source: an online vendor order, a website order, the call center, a
terminal. A pickup / takeout's customer receipt prints only at its Cash Out on the terminal's own printer, so the
Pi prints nothing of a pickup on Front (its station tickets still print). The delivery ticket shows the $0
choices the driver hands over ("Can of Ice Tea", "Side of Fries", "Honey Mustard Dip"), never a cooking choice
("Add Mayo"), for a vendor order too (prod #1109 printed "15 Piece Chicken Tender" alone); every drink's choice
(a 2-liter's flavour, the half & half).

One throwaway agent behind the fake main (placing orders as a real Pi does), fake printers only (no paper).
"""
from __future__ import annotations

import time
import uuid
from datetime import datetime, timezone

import pytest

from conftest import wait_until
from gatemark import intentional_sleep
from test_callcenter import cc_config, relay
from test_terminal import make_rig, pizza_line

ADDR = {"line1": "1 Test St", "city": "Towson", "state": "MD", "zip": "21204"}


@pytest.fixture
def placed(admin, main_store, spawn_agent, fakes):
    # AGENT_TEST_ACCEPT_STEP=0: this Pi places orders from main as a real one does.
    for r in make_rig(admin, main_store, spawn_agent, fakes, AGENT_TEST_ACCEPT_STEP="0"):
        r["fm"].terminal_config = cc_config()
        r["fm"].rpc["terminal.relay.reply"] = lambda payload: (200, {})
        yield r


def from_main(rig, source: str, typ: str, last: str) -> None:
    """An order from main (the vendor's `online`, the website's `web`), as the Pi's web UI resends one."""
    agent = rig["agent"]
    kid = lambda name: {"id": str(uuid.uuid4()), "name": name, "quantity": 1, "unit_price_cents": 0, "line_total_cents": 0,
                        "notes": "add"}
    snap = {"id": str(uuid.uuid4()), "tenant_id": str(uuid.uuid4()), "location_id": str(uuid.uuid4()), "source": source,
            "source_event_id": str(int(time.time() * 1000) % 10**7), "placed_at": datetime.now(timezone.utc).isoformat(),
            "customer": {"display_name": f"Ada {last}", "phone": "4105550100"},
            "fulfillment": {"type": typ, **({"address": {"line1": ADDR["line1"], "city": ADDR["city"]}} if typ == "delivery" else {})},
            "money": {"subtotal_cents": 1899, "total_cents": 1899},
            "payments": [{"method": "card", "amount_cents": 1899, "last4": "1881"}],
            # Prod #1109's line: the vendor's $0 children, never stamped by the Pi.
            "items": [{"id": str(uuid.uuid4()), "name": "Chicken Tender", "size": "15 Piece", "quantity": 1,
                       "unit_price_cents": 1899, "line_total_cents": 1899, "printer_id": rig["pizza_id"], "routing": "matched",
                       "children": [kid("Honey Mustard Dip"), kid("Can of Ice Tea"), kid("Side of Fries"), kid("Add Mayo")]},
                      # Drinks (the owner: "drink choices are very important"): a 2-liter's flavour, the half & half.
                      {"id": str(uuid.uuid4()), "name": "2 Liter Soda", "quantity": 1, "unit_price_cents": 0, "line_total_cents": 0,
                       "routing": "none", "children": [kid("Diet Pepsi")]},
                      {"id": str(uuid.uuid4()), "name": "Fountain Drink", "size": "Large", "quantity": 1, "unit_price_cents": 0,
                       "line_total_cents": 0, "routing": "none", "children": [kid("Half & Half")]}]}
    r = agent.http.post(f"{agent.base}/api/resend", json=snap, timeout=10)
    assert r.status_code == 200, r.text


def counter(rig, typ: str, last: str) -> dict:
    cust = {"name": f"Ada {last}", "phone": "4105550101", **({"address": ADDR} if typ == "delivery" else {})}
    r = rig["term"].post(f"{rig['agent'].base}/api/terminal/orders", json={
        "client_key": f"k-{uuid.uuid4()}", "type": typ, "cart": {"lines": [pizza_line()]}, "customer": cust}, timeout=10)
    assert r.status_code == 201, r.text
    return r.json()


def callcenter(rig, typ: str, last: str) -> dict:
    cust = {"name": f"Ada {last}", "phone": "4105550102", **({"address": ADDR} if typ == "delivery" else {})}
    got = relay(rig, "POST", "orders", {"client_key": f"cc-{uuid.uuid4()}", "type": typ, "cart": {"lines": [pizza_line()]},
                                       "customer": cust})
    assert got["status"] == 201, got
    return got["body"]


def test_the_online_printer_prints_only_deliveries(placed):
    rig = placed
    fakes = rig["fakes"]
    front, pizza = fakes["Front"], fakes["Pizza"]

    # Pickups first, from every source: each one's station ticket prints, nothing on Front.
    pickups = {"online": "Onlinepick", "web": "Webpick", "callcenter": "Callpick", "counter": "Counterpick"}
    from_main(rig, "online", "pickup", pickups["online"])
    from_main(rig, "web", "pickup", pickups["web"])
    assert "delivery_tickets" not in callcenter(rig, "pickup", pickups["callcenter"])
    assert "delivery_tickets" not in counter(rig, "pickup", pickups["counter"])
    wait_until(lambda: all(n.upper() in pizza.text().upper() for n in pickups.values()), what="the pickups' station tickets")

    # Then a delivery from every source: its Merchant + Customer Copy on Front with its station ticket.
    deliveries = {"online": "Onlinedeliv", "web": "Webdeliv", "callcenter": "Calldeliv", "counter": "Counterdeliv"}
    from_main(rig, "online", "delivery", deliveries["online"])
    from_main(rig, "web", "delivery", deliveries["web"])
    assert callcenter(rig, "delivery", deliveries["callcenter"])["delivery_tickets"] == "pi"
    assert counter(rig, "delivery", deliveries["counter"])["delivery_tickets"] == "pi"  # the terminal prints none at SEND
    wait_until(lambda: all(n.upper() in front.text().upper() for n in deliveries.values()), what="every delivery's tickets on Front")
    wait_until(lambda: all(n.upper() in pizza.text().upper() for n in deliveries.values()), what="the deliveries' station tickets")
    intentional_sleep(1.0, "absence: a late pickup print would land on Front now")

    jobs = [j.decode("latin-1") for j in front.jobs]
    assert len(jobs) == 4, f"one job per delivery on Front, got {len(jobs)}"
    for j in jobs:
        assert j.count("Merchant Copy") == 1 and j.count("Customer Copy") == 1, j
    text = front.text().upper()
    for src, name in pickups.items():
        assert name.upper() not in text, f"the {src} pickup printed on the Online printer"

    # The driver's $0 choices on a vendor delivery: what he hands over, never a cooking choice.
    online = next(j for j in jobs if deliveries["online"].upper() in j.upper())
    for want in ("Honey Mustard Dip", "Can of Ice Tea", "Side of Fries", "Diet Pepsi", "Half & Half"):
        assert online.count(want) == 2, f"{want!r} not on both copies:\n{online}"
    assert "Add Mayo" not in online
    # The kitchen keeps every choice.
    assert "Add Mayo" in pizza.text()
