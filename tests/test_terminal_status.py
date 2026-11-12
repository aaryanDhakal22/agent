"""The Pi's answers to the terminal app's gaps (TERMINAL-PLAN 2026-11-11, contract §2.10e–g).

  * a terminal key reads printer health: GET /api/printers and GET /api/terminal/status
    (printers, the link to main, the snapshot versions)
  * main's terminal-app settings (default_tab, button_font, card tips, …) reach the terminal
    untouched (login answer + GET /api/terminal/menu)
  * a counter delivery's fee carries the driver's share (main's delivery fee split) to main

Uses test_terminal.py's rig (a throwaway agent behind the fake main, fake printers only).
"""

from __future__ import annotations

import copy
import uuid

from conftest import wait_until
from test_terminal import counter_menu, pizza_line, rig, staff_config  # noqa: F401  (rig is a fixture)

APP = {"default_tab": "takeout", "show_history": True, "button_font": "barlow_condensed", "cashier_price_drop_pct": 20,
       "click_sound": True, "prices_on_buttons": False, "out_of_area_margin_mi": 1.5, "name_on_paid_counter": False,
       "card_tips": True, "card_tip_percents": [15, 18, 20], "employee_meal_pct": 50}


def test_terminal_reads_printer_health(rig):  # noqa: F811
    agent, term = rig["agent"], rig["term"]
    printers = term.get(f"{agent.base}/api/printers", timeout=5)
    assert printers.status_code == 200, printers.text
    assert {p["name"] for p in printers.json()} == {"Front", "Pizza"}
    assert term.post(f"{agent.base}/api/printers/test", json={}, timeout=5).json()["code"] == "terminal_only"
    st = term.get(f"{agent.base}/api/terminal/status", timeout=5).json()
    assert {p["name"] for p in st["printers"]} == {"Front", "Pizza"} and st["printers_down"] == 0
    assert st["main"]["connected"] is True and st["main"]["state"] == "connected"
    assert st["menu_version"] == 7 and st["config_version"] == 1 and st["menu_fetched_at"]
    # A printer that stops answering shows as down (the fake printer closes its port).
    rig["fakes"]["Pizza"].stop()
    wait_until(lambda: term.get(f"{agent.base}/api/terminal/status", timeout=5).json()["printers_down"] == 1,
               timeout=60, what="Pizza down on the terminal status")


def test_app_settings_pass_through(rig):  # noqa: F811
    agent, term, fm = rig["agent"], rig["term"], rig["fm"]
    cfg = staff_config(version=2)
    cfg["settings"].update(APP)
    fm.terminal_config = cfg
    fm.command("terminal.changed", {"version": 2})
    wait_until(lambda: term.get(f"{agent.base}/api/terminal/session", timeout=5).json()["config_version"] == 2, what="config v2")
    login = term.post(f"{agent.base}/api/terminal/login", json={"pin": "1234"}, timeout=5).json()
    assert {k: login["settings"][k] for k in APP} == APP
    menu = term.get(f"{agent.base}/api/terminal/menu", timeout=5).json()
    assert {k: menu["settings"][k] for k in APP} == APP


def test_counter_delivery_fee_driver_share(rig):  # noqa: F811
    agent, term, fm = rig["agent"], rig["term"], rig["fm"]
    menu = copy.deepcopy(counter_menu(rig["pizza_id"], version=9))
    menu["pricing"]["settings"].update({"delivery_fee_cents": 400, "delivery_driver_cents": 300})
    fm.counter_menu = menu
    fm.command("menu.changed", {"version": 9})
    wait_until(lambda: term.get(f"{agent.base}/api/terminal/menu", timeout=5).json()["menu_version"] == 9, what="menu v9")
    r = term.post(f"{agent.base}/api/terminal/orders", json={
        "client_key": f"k-{uuid.uuid4()}", "type": "delivery", "cart": {"lines": [pizza_line()]},
        "customer": {"name": "Sam Lee", "phone": "4105550100", "address": {"line1": "12 Main St", "city": "Towson"}}}, timeout=10)
    assert r.status_code == 201, r.text
    fees = r.json()["order"]["order"]["money"]["fees"]
    assert {"type": "delivery", "label": "Delivery fee", "amount_cents": 400, "driver_cents": 300} in fees
    oid = r.json()["order"]["order"]["id"]
    wait_until(lambda: any(e.get("order_id") == oid for e in fm.of_type("order.placed")), what="order.placed to main")
    snap = next(e for e in fm.of_type("order.placed") if e.get("order_id") == oid)["snapshot"]
    assert any(f.get("driver_cents") == 300 for f in snap["money"]["fees"])
