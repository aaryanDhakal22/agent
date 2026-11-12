"""Online orders = called-in orders (agent contract §2.10aa, the owner 2026-11-12): an order from main is placed on
arrival (no accept step, no ring, the store's default prep), prints its station tickets and a delivery's Merchant +
Customer Copy at once, and is edited at a terminal like ours: an added line leaves a balance, a removed one of a
paid order owes the customer a refund (refund_due, no money moved). Tips are card only (400 tip_card_only)."""
from __future__ import annotations

import uuid
import time
from datetime import datetime, timezone

import pytest
import requests

from conftest import wait_until
from test_terminal import make_rig


@pytest.fixture
def placed(admin, main_store, spawn_agent, fakes):
    # AGENT_TEST_ACCEPT_STEP=0: this Pi places orders from main as a real one does.
    yield from make_rig(admin, main_store, spawn_agent, fakes, AGENT_TEST_ACCEPT_STEP="0")


def resend(rig, *, delivery: bool, card: bool) -> str:
    agent = rig["agent"]
    oid = str(uuid.uuid4())
    snap = {"id": oid, "tenant_id": str(uuid.uuid4()), "location_id": str(uuid.uuid4()), "source": "online",
            "source_event_id": str(int(time.time() * 1000) % 10**7), "placed_at": datetime.now(timezone.utc).isoformat(),
            "customer": {"display_name": "Pytest Online", "phone": "4105550100"},
            "fulfillment": {"type": "delivery" if delivery else "pickup",
                            **({"address": {"line1": "1 Test St", "city": "Towson"}} if delivery else {})},
            "money": {"subtotal_cents": 1500, "total_cents": 1500},
            "items": [{"id": str(uuid.uuid4()), "name": "Thing", "quantity": 1, "unit_price_cents": 1500, "line_total_cents": 1500,
                       "printer_id": rig["pizza_id"], "routing": "matched"}]}
    if card:
        snap["payments"] = [{"method": "card", "amount_cents": 1500, "last4": "1881"}]
    r = agent.http.post(f"{agent.base}/api/resend", json=snap, timeout=10)
    assert r.status_code == 200, r.text
    return oid


def test_online_delivery_placed_printed_and_edited(placed):
    rig = placed
    agent, term, fm, fakes = rig["agent"], rig["term"], rig["fm"], rig["fakes"]
    base = agent.base
    fm.accept = {"order.payment", "order.changed"}  # main never saw this order (resent through the web UI)
    oid = resend(rig, delivery=True, card=True)
    so = wait_until(lambda: (lambda o: o if o["status"]["state"] == "accepted" else None)(
        agent.http.get(f"{base}/api/orders/{oid}", timeout=5).json()), what="placed on arrival")
    assert so["status"].get("placed_direct") is True and so["status"].get("prep_minutes") is not None, so["status"]
    wait_until(lambda: any(b"Merchant Copy" in j and b"Customer Copy" in j for j in fakes["Front"].jobs), what="the delivery copies")
    wait_until(lambda: fakes["Pizza"].jobs, what="the station ticket")
    assert not any(b"Collect Cash" in j for j in fakes["Front"].jobs), "prepaid: no Collect Cash"

    # Edit Order: the order's own line comes as a custom line; remove it, add a pizza → refund due, balance.
    view = term.get(f"{base}/api/terminal/orders/{oid}", timeout=5).json()
    cart = view["order"]["order"]["source_metadata"]["terminal"]["cart"]
    assert cart["lines"][0]["custom"] and cart["lines"][0]["price_cents"] == 1500, cart
    swap = {"cart": {"lines": [{"custom": True, "name": "Fries", "price_cents": 500, "qty": 1}]}, "client_key": f"k-{uuid.uuid4()}"}
    r = term.put(f"{base}/api/terminal/orders/{oid}/cart", json=swap, timeout=10)  # a sent line off, the check smaller: a manager's
    assert r.status_code == 403 and r.json()["code"] == "manager_required" and r.json()["action"] == "void", r.text
    r = term.put(f"{base}/api/terminal/orders/{oid}/cart", json=swap | {"manager_pin": "9876"}, timeout=10)
    assert r.status_code == 200, r.text
    ch = r.json()
    total = ch["order"]["order"]["money"]["total_cents"]
    assert ch["refund_due"]["cents"] == 1500 - total and ch["refund_due"]["lines"][0]["cents"] == 1500, ch["refund_due"]
    assert ch["order"]["status"]["refund_owed_cents"] == ch["refund_due"]["total_owed_cents"], ch["order"]["status"]
    # Tips are card only.
    r = term.post(f"{base}/api/terminal/orders/{oid}/payments", json={"tender": "cash", "amount_cents": 100, "tip_cents": 50,
                                                                   "client_key": f"k-{uuid.uuid4()}"}, timeout=10)
    assert r.status_code == 400 and r.json()["code"] == "tip_card_only", r.text
