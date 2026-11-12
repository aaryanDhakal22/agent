"""The owner's 2026-11-12 Pi round for the terminal (contract §2.10u, §2.10v).

  * prices don't move under an order being taken: a cart line's rung_cents
    keeps its price when main's menu moves; a line whose item left the menu
    becomes an open-price custom line at that price (never unknown_item)
  * a staff member main switched off loses their live session at once:
    SSE terminal.session.ended, then 401 login_required
  * the Dispatch / Pickup bars: clocked-in drivers, send-out, driver back,
    an order's ready time moved; the checks' new fields

Uses test_terminal.py's rig (a throwaway agent behind the fake main, fake printers only).
"""

from __future__ import annotations

import copy
import json
import threading
import time
import uuid
from datetime import datetime, timedelta, timezone

from conftest import wait_until
from test_terminal import CHEESE, counter_menu, pizza_line, rig, staff_config  # noqa: F401  (rig is a fixture)


def _listen(term, base: str, want: str, out: list, stop: threading.Event) -> None:
    """Collect the data of every SSE frame of type want."""
    with term.get(f"{base}/api/events", stream=True, timeout=(5, 60)) as s:
        typ = ""
        for line in s.iter_lines(decode_unicode=True):
            if stop.is_set():
                return
            if line.startswith("event:"):
                typ = line.split(":", 1)[1].strip()
            elif line.startswith("data:") and typ == want:
                out.append(json.loads(line.split(":", 1)[1]))


def _new_menu(rig, version: int, edit) -> None:
    agent, term, fm = rig["agent"], rig["term"], rig["fm"]
    m = counter_menu(rig["pizza_id"], version=version)
    edit(m["pricing"]["menu"])
    fm.counter_menu = m
    fm.command("menu.changed", {"version": version})
    wait_until(lambda: term.get(f"{agent.base}/api/terminal/menu", timeout=5).json()["menu_version"] == version, what=f"menu v{version}")


def _raise_pizzas(menu: dict) -> None:
    """+$3 on every pizza size (a half and half is priced from its dearest half)."""
    for cat in menu["categories"]:
        for it in cat["items"]:
            for sz in it.get("sizes") or []:
                sz["price_cents"] += 300


def _drop_cheese(menu: dict) -> None:
    for cat in menu["categories"]:
        cat["items"] = [it for it in cat["items"] if it["id"] != CHEESE]


def test_prices_dont_move_under_an_order(rig):  # noqa: F811
    agent, term, fm = rig["agent"], rig["term"], rig["fm"]
    base = agent.base
    first = term.post(f"{base}/api/terminal/quote", json={"type": "counter", "cart": {"lines": [pizza_line()]}}, timeout=5).json()
    ql = first["quote"]["lines"][0]
    rung = {**pizza_line(), "rung_cents": ql["unit_cents"], "rung_name": ql["name"], "rung_mods": ql["mods"]}

    _new_menu(rig, 8, _raise_pizzas)
    moved = term.post(f"{base}/api/terminal/quote", json={"cart": {"lines": [pizza_line()]}}, timeout=5).json()
    assert moved["quote"]["lines"][0]["unit_cents"] == ql["unit_cents"] + 300
    kept = term.post(f"{base}/api/terminal/quote", json={"cart": {"lines": [rung]}}, timeout=5).json()
    assert kept["quote"]["lines"][0]["unit_cents"] == ql["unit_cents"] and not kept["needs_manager"]
    assert kept["kept_prices"] == [{"line": 0, "item_id": CHEESE, "name": ql["name"], "menu_cents": ql["unit_cents"] + 300,
                                    "rung_cents": ql["unit_cents"], "from": "terminal"}]
    assert "price_changes" not in kept

    # The item leaves the menu: without rung_cents a 422 as before, with it a custom line.
    _new_menu(rig, 9, _drop_cheese)
    bare = term.post(f"{base}/api/terminal/orders", json={"client_key": f"k-{uuid.uuid4()}", "type": "counter",
                                                        "cart": {"lines": [pizza_line()]}}, timeout=10)
    assert bare.status_code == 422 and bare.json()["errors"][0]["code"] == "unknown_item"
    q = term.post(f"{base}/api/terminal/quote", json={"cart": {"lines": [rung]}}, timeout=5).json()
    assert q["quote"]["errors"] == [] and q["quote"]["lines"][0]["custom"] and q["quote"]["lines"][0]["unit_cents"] == ql["unit_cents"]
    assert q["menu_gone"][0]["reason"] == "unknown_item" and q["menu_gone"][0]["item_id"] == CHEESE
    r = term.post(f"{base}/api/terminal/orders", json={"client_key": f"k-{uuid.uuid4()}", "type": "counter", "cart": {"lines": [rung]}}, timeout=10)
    assert r.status_code == 201, r.text
    line = r.json()["order"]["order"]["items"][0]
    assert line["sku"] == "custom" and line["unit_price_cents"] == ql["unit_cents"] and line["name"] == ql["name"]
    # The gone line still reaches the kitchen (every station, its mods in the note) and main.
    wait_until(lambda: rig["fakes"]["Pizza"].jobs, what="station ticket")
    assert "CHEESE PIZZA" in rig["fakes"]["Pizza"].text().upper()
    wait_until(lambda: any(e["action"] == "menu_gone_line" for ev in fm.of_type("journal.entries") for e in ev["entries"]),
               what="menu_gone_line journaled to main")
    wait_until(lambda: fm.of_type("order.placed"), what="order.placed")


def test_switched_off_staff_is_logged_out_at_once(rig):  # noqa: F811
    agent, term, fm = rig["agent"], rig["term"], rig["fm"]
    base = agent.base
    ended: list[dict] = []
    stop = threading.Event()
    threading.Thread(target=_listen, args=(term, base, "terminal.session.ended", ended, stop), daemon=True).start()
    time.sleep(0.5)  # the stream is open (a missed frame would only fail this test)
    assert term.get(f"{base}/api/terminal/session", timeout=5).json()["staff"]["first_name"] == "Cara"
    cfg = copy.deepcopy(staff_config(version=2))
    cfg["staff"][0]["active"] = False  # the cashier, switched off in main
    fm.terminal_config = cfg
    fm.command("terminal.changed", {"version": 2})
    wait_until(lambda: ended, what="terminal.session.ended")
    assert ended[0]["reason"] == "deactivated" and ended[0]["staff_id"] == cfg["staff"][0]["id"] and ended[0]["device_id"]
    r = term.post(f"{base}/api/terminal/orders", json={"client_key": f"k-{uuid.uuid4()}", "type": "counter",
                                                     "cart": {"lines": [pizza_line()]}}, timeout=10)
    assert r.status_code == 401 and r.json()["code"] == "login_required"
    assert term.post(f"{base}/api/terminal/login", json={"pin": "1234"}, timeout=5).status_code == 401
    # The manager's session goes on.
    assert term.post(f"{base}/api/terminal/login", json={"pin": "9876"}, timeout=5).status_code == 200
    stop.set()


def test_dispatch_and_pickup_bars(rig):  # noqa: F811
    agent, term, fm = rig["agent"], rig["term"], rig["fm"]
    base = agent.base
    agent.http.put(f"{base}/api/settings/deliveries", json={"auto_assign": False}, timeout=5)
    d = agent.http.post(f"{base}/api/drivers", json={"name": "Dana"}, timeout=5).json()
    off = agent.http.post(f"{base}/api/drivers", json={"name": "Eli"}, timeout=5).json()
    assert agent.http.post(f"{base}/api/drivers/{d['id']}/clock-in", timeout=5).status_code == 200
    drivers = term.get(f"{base}/api/terminal/drivers", timeout=5).json()["drivers"]
    assert [x["id"] for x in drivers] == [d["id"]] and off["id"] not in [x["id"] for x in drivers]

    body = {"client_key": f"k-{uuid.uuid4()}", "type": "delivery", "cart": {"lines": [pizza_line()]},
            "customer": {"name": "Erik Davis", "phone": "4105550101",
                         "address": {"line1": "12 Main St", "line2": "Apt 3", "city": "Towson", "zip": "21286"}}}
    q = term.post(f"{base}/api/terminal/quote", json={"type": "delivery", "cart": body["cart"]}, timeout=5).json()
    body["payments"] = [{"tender": "square_app", "amount_cents": q["quote"]["total_cents"]}]
    r = term.post(f"{base}/api/terminal/orders", json=body, timeout=10)
    assert r.status_code == 201, r.text
    oid = r.json()["order"]["order"]["id"]

    # Move its time: 40 minutes from now.
    at = (datetime.now(timezone.utc) + timedelta(minutes=40)).replace(microsecond=0)
    t = term.post(f"{base}/api/terminal/orders/{oid}/time", json={"ready_at": at.isoformat().replace("+00:00", "Z")}, timeout=5)
    assert t.status_code == 200, t.text
    assert t.json()["from"] and datetime.fromisoformat(t.json()["to"].replace("Z", "+00:00")) == at
    assert term.post(f"{base}/api/terminal/orders/{oid}/time", json={}, timeout=5).status_code == 400
    wait_until(lambda: any(e["order_id"] == oid and e["estimated_ready_at"].startswith(at.strftime("%Y-%m-%dT%H:%M"))
                           for e in fm.of_type("order.status")), what="order.status with the new time")

    # The checks show the address, payments and the promised time.
    page = term.get(f"{base}/api/terminal/checks", params={"type": "delivery"}, timeout=5).json()
    row = next(c for c in page["checks"] if c["order_id"] == oid)
    assert row["address"]["line1"] == "12 Main St" and row["address"]["line2"] == "Apt 3" and row["address"]["city"] == "Towson"
    assert row["payments_count"] == 1 and row["promised_at"] and row["driver_id"] == ""

    # Send it out (still cooking: mark_ready), then the driver is back.
    so = term.post(f"{base}/api/terminal/deliveries/send-out", json={"driver_id": d["id"], "order_ids": [oid]}, timeout=10)
    assert so.status_code == 409 and so.json()["code"] == "not_ready"
    so = term.post(f"{base}/api/terminal/deliveries/send-out", json={"driver_id": d["id"], "order_ids": [oid], "mark_ready": True}, timeout=10)
    assert so.status_code == 200, so.text
    assert [o["order"]["id"] for o in so.json()["orders"]] == [oid] and so.json()["orders"][0]["status"]["state"] == "on_delivery"
    row = next(c for c in term.get(f"{base}/api/terminal/checks", timeout=5).json()["checks"] if c["order_id"] == oid)
    assert row["driver_id"] == d["id"] and row["driver_name"] == "Dana"
    back = term.post(f"{base}/api/terminal/drivers/{d['id']}/returned", timeout=10)
    assert back.status_code == 200 and back.json()["orders"][0]["status"]["state"] == "delivered", back.text
    journal = [e["action"] for e in term.get(f"{base}/api/terminal/journal", timeout=5).json()]
    assert {"delivery.send_out", "driver.returned", "order.time"} <= set(journal)

    # Locked terminal: 401 on every one of them.
    assert term.post(f"{base}/api/terminal/logout", timeout=5).status_code == 204
    assert term.get(f"{base}/api/terminal/drivers", timeout=5).json()["code"] == "login_required"
    assert term.post(f"{base}/api/terminal/drivers/{d['id']}/returned", timeout=5).status_code == 401
