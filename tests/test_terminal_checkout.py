"""The Pi routes the terminal's checkout (Stage 1c) assumed (contract §2.10i).

  * GET /api/terminal/pickups: every pickup source (counter, online), paid / cash due
  * an online cash pickup paid at the terminal, then POST …/picked-up (order.status to main)
  * shared drafts: two terminals racing for one draft (one wins, 409 open_elsewhere), TAKE OVER
  * a cashier comps without a manager (the owner, 2026-11-12: comp was manager-only)
  * the Employee meal at the Pi's default 20 % by a cashier (cashier_discount_pct is 10)

Uses test_terminal.py's rig (a throwaway agent behind the fake main, fake printers only).
"""

from __future__ import annotations

import json
import threading
import time
import uuid
from datetime import datetime, timezone

import requests

from conftest import wait_until
from test_terminal import pizza_line, rig  # noqa: F401  (rig is a fixture)
from test_terminal_driver import pair_terminal

SODA = "00000000-0000-0000-0000-0000000000a6"


class Events:
    """A terminal on /api/events, collecting (event, data)."""

    def __init__(self, base: str, sess: requests.Session) -> None:
        self.events: list[tuple[str, dict]] = []
        self._resp = None

        def run() -> None:
            try:
                with sess.get(f"{base}/api/events", stream=True, timeout=(5, 60)) as r:
                    self._resp = r
                    ev = None
                    for line in r.iter_lines(decode_unicode=True):
                        if line.startswith("event: "):
                            ev = line[7:]
                        elif line.startswith("data: ") and ev:
                            self.events.append((ev, json.loads(line[6:])))
            except (requests.RequestException, AttributeError, ValueError):
                pass

        threading.Thread(target=run, daemon=True).start()

    def of(self, kind: str) -> list[dict]:
        return [d for e, d in list(self.events) if e == kind]

    def close(self) -> None:
        r = self._resp
        if r is not None:
            try:
                r.raw._fp.fp.raw._sock.shutdown(2)  # don't wait for the next keepalive
            except (AttributeError, OSError):
                pass
            r.close()


def arrive_online(rig, fulfillment: str = "pickup") -> str:
    """An online order through the web UI's Resend (no payments = cash due), accepted."""
    agent = rig["agent"]
    oid = str(uuid.uuid4())
    snap = {"id": oid, "tenant_id": str(uuid.uuid4()), "location_id": str(uuid.uuid4()), "source": "online",
            "source_event_id": str(int(time.time() * 1000) % 10**7), "placed_at": datetime.now(timezone.utc).isoformat(),
            "customer": {"display_name": "Pytest Pickup"}, "fulfillment": {"type": fulfillment},
            "money": {"subtotal_cents": 1500, "total_cents": 1500},
            "items": [{"id": str(uuid.uuid4()), "name": "Thing", "quantity": 1, "unit_price_cents": 1500, "line_total_cents": 1500}]}
    r = agent.http.post(f"{agent.base}/api/resend", json=snap, timeout=10)
    assert r.status_code == 200, r.text
    r = agent.http.post(f"{agent.base}/api/orders/{oid}/accept", json={"prep_minutes": 15}, timeout=30)
    assert r.status_code == 200, r.text
    return oid


def test_online_cash_pickup_paid_at_the_terminal(rig):  # noqa: F811
    agent, term, fm = rig["agent"], rig["term"], rig["fm"]
    base = agent.base
    ev = Events(base, term)
    # The online order exists only on this Pi (resent through the web UI):
    # dev main would answer its payment not_found, which the Pi now keeps and
    # holds the order's later events behind (contract 2.10s §3).
    fm.accept = {"order.payment"}
    try:
        oid = arrive_online(rig)
        delivery = arrive_online(rig, "delivery")
        counter = term.post(f"{base}/api/terminal/orders", json={"client_key": f"k-{uuid.uuid4()}", "type": "pickup",
                            "cart": {"lines": [{"item_id": SODA, "qty": 1}]}, "customer": {"name": "Ann"}}, timeout=10)
        assert counter.status_code == 201, counter.text
        cid = counter.json()["order"]["order"]["id"]

        pickups = {o["order"]["id"]: o for o in term.get(f"{base}/api/terminal/pickups", timeout=5).json()["orders"]}
        assert set(pickups) == {oid, cid}, "every pickup source, no delivery"
        assert pickups[oid]["order"]["source"] == "online" and pickups[cid]["order"]["source"] == "counter"
        assert pickups[oid]["status"]["cash_due_cents"] == 1500 and not pickups[oid]["status"].get("paid")

        r = term.post(f"{base}/api/terminal/orders/{oid}/picked-up", timeout=5)
        assert r.status_code == 409 and r.json()["code"] == "cash_due" and r.json()["balance_cents"] == 1500
        r = term.post(f"{base}/api/terminal/orders/{delivery}/picked-up", timeout=5)
        assert r.status_code == 409 and r.json()["code"] == "not_pickable"

        # Pay at pickup: the terminal reads it, then takes the cash.
        view = term.get(f"{base}/api/terminal/orders/{oid}", timeout=5)
        assert view.status_code == 200 and view.json()["payments"] == []
        r = term.post(f"{base}/api/terminal/orders/{oid}/payments",
                      json={"tender": "cash", "amount_cents": 1500, "tendered_cents": 2000, "client_key": "pay-online-0001"}, timeout=5)
        assert r.status_code == 200, r.text
        assert r.json()["balance_cents"] == 0 and r.json()["order"]["status"]["paid"] and r.json()["payment"]["change_cents"] == 500
        assert term.get(f"{base}/api/terminal/orders/{oid}", timeout=5).json()["payments"][0]["tender"] == "cash"
        wait_until(lambda: any(e["order_id"] == oid for e in fm.of_type("order.payment")), what="order.payment to main")
        # Fully paid = off the Pickup list at once (the owner, 2026-11-12: no Picked Up needed); picked-up still works.
        ids = [o["order"]["id"] for o in term.get(f"{base}/api/terminal/pickups", timeout=5).json()["orders"]]
        assert oid not in ids and cid in ids

        r = term.post(f"{base}/api/terminal/orders/{oid}/picked-up", timeout=5)
        assert r.status_code == 200, r.text
        st = r.json()["order"]["status"]
        assert st["finished_at"] and st["finished_by"] == "terminal" and st["state"] == "ready" and not r.json()["already"]
        assert term.post(f"{base}/api/terminal/orders/{oid}/picked-up", timeout=5).json()["already"]
        ids = [o["order"]["id"] for o in term.get(f"{base}/api/terminal/pickups", timeout=5).json()["orders"]]
        assert oid not in ids and cid in ids
        wait_until(lambda: any(e["order_id"] == oid and e.get("agent_status", {}).get("finished_at")
                               for e in fm.of_type("order.status")), what="order.status with finished_at to main")
        wait_until(lambda: any(d["order"]["order"]["id"] == oid for d in ev.of("terminal.order.picked_up")),
                   what="terminal.order.picked_up on /api/events")
        actions = [e["action"] for e in term.get(f"{base}/api/terminal/journal", timeout=5).json()]
        assert "order.picked_up" in actions and "payment" in actions
    finally:
        ev.close()


def test_drafts_race_and_take_over(rig):  # noqa: F811
    agent, t1 = rig["agent"], rig["term"]
    base = agent.base
    t2 = pair_terminal(agent, "Till 2")
    assert t2.post(f"{base}/api/terminal/login", json={"pin": "9876"}, timeout=5).status_code == 200
    ev = Events(base, t1)
    try:
        did = f"d-{uuid.uuid4().hex[:12]}"
        body = {"type": "counter", "name": "Sam", "held": True, "draft": {"type": "counter", "lines": [{"item_id": SODA}]}}
        # SUSPEND on till 1: held, under Cara, nobody holding it.
        r = t1.put(f"{base}/api/terminal/drafts/{did}", json=body, timeout=5)
        assert r.status_code == 200, r.text
        d = r.json()["draft"]
        assert d["held"] and d["holder"] is None and d["staff_name"] == "Cara S." and d["draft"]["lines"]
        wait_until(lambda: ev.of("terminal.draft.changed") or
                   t1.put(f"{base}/api/terminal/drafts/{did}", json=body, timeout=5) and False,
                   what="terminal.draft.changed on /api/events")
        assert [x["id"] for x in t2.get(f"{base}/api/terminal/drafts", timeout=5).json()["drafts"]] == [did]

        # Both terminals RESUME it at once: one wins, the other is told who has it.
        open_body = {**body, "held": False}
        out: dict[str, requests.Response] = {}
        go = threading.Barrier(2)

        def resume(name: str, s: requests.Session) -> None:
            go.wait()
            out[name] = s.put(f"{base}/api/terminal/drafts/{did}", json=open_body, timeout=10)

        ths = [threading.Thread(target=resume, args=(n, s)) for n, s in (("t1", t1), ("t2", t2))]
        for th in ths:
            th.start()
        for th in ths:
            th.join()
        codes = sorted(r.status_code for r in out.values())
        assert codes == [200, 409], {k: v.text for k, v in out.items()}
        winner = next(k for k, v in out.items() if v.status_code == 200)
        loser = "t2" if winner == "t1" else "t1"
        sess = {"t1": t1, "t2": t2}
        conflict = out[loser].json()
        holder = out[winner].json()["draft"]["holder"]
        assert conflict["code"] == "open_elsewhere" and conflict["holder"]["device_id"] == holder["device_id"]
        assert conflict["holder"]["device_name"] == ("Till 1" if winner == "t1" else "Till 2")

        # The loser can't delete it either; TAKE OVER moves it and tells the old holder.
        r = sess[loser].delete(f"{base}/api/terminal/drafts/{did}", timeout=5)
        assert r.status_code == 409 and r.json()["code"] == "open_elsewhere"
        r = sess[loser].put(f"{base}/api/terminal/drafts/{did}", json={**open_body, "take_over": True}, timeout=5)
        assert r.status_code == 200 and r.json()["draft"]["holder"]["device_id"] != holder["device_id"]
        wait_until(lambda: any(x["id"] == did and x["by"]["device_id"] != holder["device_id"] for x in ev.of("terminal.draft.taken")),
                   what="terminal.draft.taken")
        r = sess[winner].put(f"{base}/api/terminal/drafts/{did}", json=open_body, timeout=5)
        assert r.status_code == 409, "the old holder's next mirror is refused"

        # Sent: the holder deletes it; Close batch clears the rest.
        assert sess[loser].delete(f"{base}/api/terminal/drafts/{did}", timeout=5).status_code == 204
        wait_until(lambda: any(x["id"] == did for x in ev.of("terminal.draft.removed")), what="terminal.draft.removed")
        assert t1.put(f"{base}/api/terminal/drafts/{did}-2", json=body, timeout=5).status_code == 200
        assert agent.http.post(f"{base}/api/batch/close", json={"force": True}, timeout=30).status_code == 200
        assert t1.get(f"{base}/api/terminal/drafts", timeout=5).json()["drafts"] == []
        wait_until(lambda: any(x["id"] == f"{did}-2" for x in ev.of("terminal.draft.removed")), what="cleared at Close batch")
    finally:
        ev.close()


def test_comp_by_a_cashier_needs_no_manager(rig):  # noqa: F811
    agent, term = rig["agent"], rig["term"]
    base = agent.base
    r = term.post(f"{base}/api/terminal/orders", json={"client_key": f"k-{uuid.uuid4()}", "type": "counter",
                  "cart": {"lines": [{"item_id": SODA, "qty": 1}]}}, timeout=10)
    assert r.status_code == 201, r.text
    oid, total = r.json()["order"]["order"]["id"], r.json()["order"]["order"]["money"]["total_cents"]
    r = term.post(f"{base}/api/terminal/orders/{oid}/payments", json={"tender": "comp", "amount_cents": total}, timeout=5)
    assert r.status_code == 200 and r.json()["payment"].get("approved_by") is None and r.json()["balance_cents"] == 0, r.text
    # A comp line ($0 on a set-price item) too.
    cart = {"lines": [{"item_id": SODA, "qty": 1, "price_cents": 0}]}
    q = term.post(f"{base}/api/terminal/quote", json={"cart": cart}, timeout=5).json()
    assert not q["needs_manager"] and q["price_changes"][0]["comp"]
    r = term.post(f"{base}/api/terminal/orders", json={"client_key": f"k-{uuid.uuid4()}", "type": "counter", "cart": cart}, timeout=10)
    assert r.status_code == 201, r.text
    entries = term.get(f"{base}/api/terminal/journal", timeout=5).json()
    assert any(e["action"] == "comp" and e["staff_id"] == "00000000-0000-4000-8000-0000000000c1" for e in entries)
    assert not any(e["action"] == "override" for e in entries)


def test_employee_meal_at_20_by_a_cashier(rig):  # noqa: F811
    agent, term = rig["agent"], rig["term"]
    base = agent.base
    meal = {"pct": 20, "reason": "Employee meal", "kind": "employee_meal"}
    q = term.post(f"{base}/api/terminal/quote", json={"cart": {"lines": [pizza_line()], "discount": meal}}, timeout=5).json()
    assert not q["needs_manager"] and q["discount_pct"] == 20
    assert [d["label"] for d in q["quote"]["discounts"]] == ["Employee meal 20%"]
    r = term.post(f"{base}/api/terminal/orders", json={"client_key": f"k-{uuid.uuid4()}", "type": "counter",
                  "cart": {"lines": [pizza_line()], "discount": meal}}, timeout=10)
    assert r.status_code == 201, r.text
    # Over the Pi's default 20 % (main sends no employee_meal_pct here) it needs a manager; a typed 50 %
    # follows the cashier's 10 %.
    r = term.post(f"{base}/api/terminal/orders", json={"client_key": f"k-{uuid.uuid4()}", "type": "counter",
                  "cart": {"lines": [pizza_line()], "discount": {**meal, "pct": 30}}}, timeout=10)
    assert r.status_code == 403 and r.json()["action"] == "employee_meal" and r.json()["limit"] == 20
    r = term.post(f"{base}/api/terminal/orders", json={"client_key": f"k-{uuid.uuid4()}", "type": "counter",
                  "cart": {"lines": [pizza_line()], "discount": {"pct": 50, "reason": "friend"}}}, timeout=10)
    assert r.status_code == 403 and r.json()["action"] == "discount"
    entries = term.get(f"{base}/api/terminal/journal", timeout=5).json()
    assert any(e["action"] == "employee_meal" and e["detail"]["pct"] == 20 for e in entries)
