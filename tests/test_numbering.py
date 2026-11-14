"""Ticket numbers, the owner's final rule (2026-11-14; contract/agent.md §2.10ae).

- ONE sequence for every order from main's counter_start (default 100): a terminal order at SEND and an online /
  website order placed on arrival take the next number alike, so they interleave.
- Every Close batch restarts it at the start (a second close the same day too).
- A future order holds a random 5-digit placeholder (unique among the open future orders) until it is officially
  entered (its ring time / MAKE NOW); it carries the placeholder across a close, then takes the next number. The
  kitchen tickets and main's floor events show the real number from then on.

Each test here fails on the 2026-11-13 code (online numbers from 1100, the business-day restart, no placeholder).
"""
from __future__ import annotations

import time
import uuid
from datetime import datetime, timedelta, timezone

import pytest

from conftest import wait_until
from test_terminal import make_rig, place, staff_config


@pytest.fixture
def placed(admin, main_store, spawn_agent, fakes):
    # AGENT_TEST_ACCEPT_STEP=0: this Pi places orders from main on arrival as a real one does (§2.10aa).
    yield from make_rig(admin, main_store, spawn_agent, fakes, AGENT_TEST_ACCEPT_STEP="0")


def use_start(rig, start: int | None, version: int = 2) -> None:
    """main's terminal config with counter_start = start (None = not sent: the Pi's default, 100)."""
    agent, term, fm = rig["agent"], rig["term"], rig["fm"]
    fm.terminal_config = staff_config(version=version)
    if start is None:
        fm.terminal_config["settings"].pop("counter_start")
    else:
        fm.terminal_config["settings"]["counter_start"] = start
    fm.command("terminal.changed", {"version": version})
    wait_until(lambda: term.get(f"{agent.base}/api/terminal/session", timeout=5).json()["config_version"] == version,
               what=f"config v{version}")
    wait_until(lambda: term.post(f"{agent.base}/api/terminal/login", json={"pin": "1234"}, timeout=5).status_code == 200,
               what="cashier logged in again")


def online(rig, *, promised: datetime | None = None) -> str:
    """An online card pickup from 'main' (the web UI's resend: this Pi alone knows it), placed on arrival."""
    agent = rig["agent"]
    oid = str(uuid.uuid4())
    snap = {"id": oid, "tenant_id": str(uuid.uuid4()), "location_id": str(uuid.uuid4()), "source": "online",
            "source_event_id": str(int(time.time() * 1000) % 10**7), "placed_at": datetime.now(timezone.utc).isoformat(),
            "customer": {"display_name": "Pytest Numbers", "phone": "4105550100"},
            "fulfillment": {"type": "pickup", **({"promised_at": promised.isoformat()} if promised else {})},
            "money": {"subtotal_cents": 1500, "total_cents": 1500},
            "payments": [{"method": "card", "amount_cents": 1500, "last4": "1881"}],
            "items": [{"id": str(uuid.uuid4()), "name": "Thing", "quantity": 1, "unit_price_cents": 1500, "line_total_cents": 1500,
                       "printer_id": rig["pizza_id"], "routing": "matched"}]}
    r = agent.http.post(f"{agent.base}/api/resend", json=snap, timeout=10)
    assert r.status_code == 200, r.text
    return oid


def status(rig, oid: str) -> dict:
    agent = rig["agent"]
    return agent.http.get(f"{agent.base}/api/orders/{oid}", timeout=5).json()["status"]


def online_number(rig) -> int:
    oid = online(rig)
    st = wait_until(lambda: (lambda s: s if s["state"] == "accepted" and s.get("kitchen_number") else None)(status(rig, oid)),
                    what="the online order placed")
    return st["kitchen_number"]


def counter_number(rig) -> int:
    r = place(rig)
    assert r.status_code == 201, r.text
    return r.json()["number"]


def close(rig) -> None:
    agent = rig["agent"]
    r = agent.http.post(f"{agent.base}/api/batch/close", json={"force": True}, timeout=30)
    assert r.status_code == 200, r.text


def next_number(rig) -> int:
    agent = rig["agent"]
    return agent.http.get(f"{agent.base}/api/floor", timeout=5).json()["kitchen_next"]


def pizza_ticket(rig, number: int) -> None:
    """The kitchen ticket on the fake Pizza printer carries the number, big (GS ! 0x33)."""
    want = b"\x1d!3" + str(number).encode() + b"\n"
    wait_until(lambda: any(want in j for j in rig["fakes"]["Pizza"].jobs), what=f"a Pizza ticket #{number}")


def test_one_sequence_from_100_online_and_counter_interleave(placed):
    rig = placed
    rig["fm"].accept = {"order.payment", "order.changed", "order.status"}  # orders only this Pi has
    use_start(rig, None)  # main sends no counter_start: the Pi's default
    assert next_number(rig) == 100
    got = [counter_number(rig), online_number(rig), counter_number(rig), online_number(rig)]
    assert got == [100, 101, 102, 103], f"one sequence for counter and online orders from 100, got {got}"
    pizza_ticket(rig, 101)  # the online order's kitchen ticket: 101, not 1100
    pizza_ticket(rig, 103)
    assert next_number(rig) == 104


def test_every_close_restarts_at_the_start_a_second_close_too(placed):
    rig = placed
    rig["fm"].accept = {"order.payment", "order.changed", "order.status"}
    use_start(rig, 100)
    assert [counter_number(rig), online_number(rig)] == [100, 101]
    close(rig)
    assert next_number(rig) == 100, "Close batch restarts the numbers"
    assert [online_number(rig), counter_number(rig)] == [100, 101]
    close(rig)  # the same day, minutes later
    assert counter_number(rig) == 100, "a second close the same day restarts again"
    close(rig)
    close(rig)  # empty closes
    assert online_number(rig) == 100


def test_future_order_placeholder_across_a_close_then_the_next_number(placed):
    rig = placed
    agent, fm = rig["agent"], rig["fm"]
    fm.accept = {"order.payment", "order.changed", "order.status"}
    use_start(rig, 100)
    r = agent.http.put(f"{agent.base}/api/settings/features", json={"future_orders": True}, timeout=5)
    assert r.status_code == 200, r.text
    promised = datetime.now(timezone.utc) + timedelta(hours=3)
    a, b = online(rig, promised=promised), online(rig, promised=promised)
    sa = wait_until(lambda: (lambda s: s if s["state"] == "scheduled" and s.get("kitchen_number") else None)(status(rig, a)),
                    what="future order A scheduled with its placeholder")
    sb = wait_until(lambda: (lambda s: s if s["state"] == "scheduled" and s.get("kitchen_number") else None)(status(rig, b)),
                    what="future order B scheduled with its placeholder")
    pa, pb = sa["kitchen_number"], sb["kitchen_number"]
    assert 10_000 <= pa <= 99_999 and 10_000 <= pb <= 99_999 and sa["number_placeholder"] and sb["number_placeholder"], (sa, sb)
    assert pa != pb, "two open future orders never share a placeholder"
    # The placeholder is no sequence number: today's orders count from 100.
    assert counter_number(rig) == 100
    close(rig)
    st = status(rig, a)
    assert st["state"] == "scheduled" and st["kitchen_number"] == pa and st["number_placeholder"], "carried across the close"
    assert counter_number(rig) == 100, "the close restarted the numbers; the future order kept its placeholder"
    # MAKE NOW: officially entered → the next number, the placeholder dropped.
    r = agent.http.post(f"{agent.base}/api/orders/{a}/make-now", json={"prep_minutes": 15}, timeout=30)
    assert r.status_code == 200, r.text
    st = r.json()["status"]
    assert st["state"] == "accepted" and st["kitchen_number"] == 101 and not st.get("number_placeholder"), st
    pizza_ticket(rig, 101)
    assert counter_number(rig) == 102
    # main's mirror: the scheduled event carried the placeholder, the accepted one the real number.
    evs = wait_until(lambda: (lambda e: e if any(x["status"] == "accepted" for x in e) else None)(
        [x for x in fm.of_type("order.status") if x.get("order_id") == a]), what="A's floor events to main")
    sched = [x for x in evs if x["status"] == "scheduled"]
    acc = [x for x in evs if x["status"] == "accepted"]
    assert sched and sched[0]["kitchen_number"] == pa, sched
    assert acc[-1]["kitchen_number"] == 101, acc
    st = status(rig, b)
    assert st["kitchen_number"] == pb and st["number_placeholder"], "B still waits with its own placeholder"
