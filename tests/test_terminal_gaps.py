"""The order screen's gaps on the Pi (2026-11-11, contract/agent.md §2.10a):

  * double / triple toppings: the same pick again in an allow_multiple
    group, capped by main's topping_max_count; since 2026-11-14 each pick its own child / ticket line
  * Quicc Add: a custom line {custom, name, price_cents, qty, note?} on
    quote / place / PUT …/cart, journaled with who added it, synced to main
    as a line with sku "custom", printed on every station
  * a missed required group: a quote warning (line_warnings), the order
    still goes through

The menu is the shared pricing case 45 behind the fake main of
test_terminal; printing goes to fake printers only.
"""

from __future__ import annotations

import json
import uuid
from pathlib import Path

from conftest import wait_until
from test_terminal import rig, staff_config  # noqa: F401  (rig is a fixture)

CASE = Path(__file__).resolve().parent.parent / "internal/domain/pricing/testdata/cases/45_doubles.json"
PIZZA = "00000000-0000-0000-0000-0000000000a1"
CLUB = "00000000-0000-0000-0000-0000000000a3"
TOPPINGS = "00000000-0000-0000-0000-0000000000c1"
PEP = "00000000-0000-0000-0000-0000000000d1"
MUSH = "00000000-0000-0000-0000-0000000000d2"
XL = "00000000-0000-0000-0000-0000000000b2"


def _use_gap_menu(rig, max_count: int = 2) -> None:
    agent, term, fm = rig["agent"], rig["term"], rig["fm"]
    ctx = json.loads(CASE.read_text())["context"]
    ctx.pop("counter", None)
    ctx.pop("max_repeat", None)  # the Pi sets both itself
    fm.counter_menu = {"menu_version": 45, "build": {"view": {"kind": "counter", "name": "Counter"}, "layout": {"tabs": []}},
                       "pricing": ctx, "routes": {PIZZA: {"printer_id": rig["pizza_id"], "name": "Pizza"}}}
    fm.command("menu.changed", {"version": 45})
    wait_until(lambda: term.get(f"{agent.base}/api/terminal/menu", timeout=5).json()["menu_version"] == 45, what="menu v45")
    cfg = staff_config(version=45)
    cfg["settings"]["topping_max_count"] = max_count
    fm.terminal_config = cfg
    fm.command("terminal.changed", {"version": 45})
    wait_until(lambda: term.get(f"{agent.base}/api/terminal/session", timeout=5).json()["config_version"] == 45, what="config v45")
    wait_until(lambda: term.post(f"{agent.base}/api/terminal/login", json={"pin": "1234"}, timeout=5).status_code == 200, what="login")


def _pizza(*picks: tuple[str, str]) -> dict:
    return {"item_id": PIZZA, "size_id": XL, "qty": 1,
            "options": [{"group_id": TOPPINGS, "option_id": o, "portion": p} for o, p in picks]}


def _quote(rig, *lines: dict) -> dict:
    r = rig["term"].post(f"{rig['agent'].base}/api/terminal/quote", json={"cart": {"lines": list(lines)}}, timeout=5)
    assert r.status_code == 200, r.text
    return r.json()["quote"]


def test_doubles_custom_line_and_required_warning(rig):
    agent, term, fm, fakes = rig["agent"], rig["term"], rig["fm"], rig["fakes"]
    base = agent.base
    _use_gap_menu(rig, max_count=2)

    # topping_max_count 2: a 3rd Pepperoni is refused; 2× Pepperoni + 2× Mushrooms (1st half) price per pick.
    q = _quote(rig, _pizza((PEP, ""), (PEP, ""), (PEP, "")))
    assert [e["message"] for e in q["errors"]] == ["Cheese Pizza: up to 2× Pepperoni."]
    double = _pizza((PEP, ""), (PEP, ""), (MUSH, "first"), (MUSH, "first"))
    q = _quote(rig, double)
    # Mods never stack (the owner, 2026-11-14; contract 2.10an): each pick its own mod line.
    assert not q["errors"] and q["lines"][0]["mods"][1:5] == ["Pepperoni", "Pepperoni", "Mushrooms (1st half)", "Mushrooms (1st half)"]
    assert q["lines"][0]["unit_cents"] == 1299 + 2 * 225 + 2 * 113

    # A Club Sandwich with no side / dips: warnings, not errors.
    club = {"item_id": CLUB, "qty": 1}
    q = _quote(rig, club)
    assert not q["errors"] and [w["line"] for w in q["line_warnings"]] == [0, 0]
    assert q["warnings"] == ["Club Sandwich: choose a side.", "Club Sandwich: choose 2 dips."]

    # A custom line: priced at the store rate, placed, printed on every station, journaled, synced.
    custom = {"custom": True, "name": "Extra dough ball", "price_cents": 250, "qty": 2, "note": "on the side"}
    body = {"client_key": f"k-{uuid.uuid4()}", "type": "counter", "cart": {"lines": [double, club, custom]}}
    r = term.post(f"{base}/api/terminal/orders", json=body, timeout=10)
    assert r.status_code == 201, r.text
    so = r.json()["order"]["order"]
    pz, cl, cu = so["items"]
    assert [c["name"] for c in pz["children"]][:4] == ["Pepperoni", "Pepperoni", "Mushrooms (1st half)", "Mushrooms (1st half)"]
    assert [c["unit_price_cents"] for c in pz["children"]][:4] == [225, 225, 113, 113]
    assert (cu["name"], cu["sku"], cu["quantity"], cu["line_total_cents"], cu["routing"]) == ("Extra dough ball", "custom", 2, 500, "unmatched")
    wait_until(lambda: "Extra Dough Ball" in fakes["Pizza"].text(), what="the custom line on the Pizza station (unrouted: every station)")
    wait_until(lambda: fakes["Pizza"].text().upper().count("PEPPERONI") >= 2, what="each Pepperoni its own line on the Pizza station")
    journal = term.get(f"{base}/api/terminal/journal", timeout=5).json()
    rows = [e for e in journal if e["action"] == "custom_line"]
    assert len(rows) == 1 and rows[0]["staff_id"] == "00000000-0000-4000-8000-0000000000c1" and rows[0]["detail"]["name"] == "Extra dough ball"
    assert rows[0]["detail"]["price_cents"] == 250 and rows[0]["detail"]["qty"] == 2

    def synced():
        for ev in fm.of_type("order.placed"):
            if ev.get("order_id") == so["id"]:
                return ev
        return None
    wait_until(lambda: synced() is not None, what="order.placed to main")
    sent = synced()["snapshot"]["items"][2]
    assert sent["sku"] == "custom" and "menu_item_id" not in sent

    # A kitchen change adding a second custom line journals only the new one.
    body2 = {"client_key": f"k-{uuid.uuid4()}", "cart": {"lines": [double, club, custom, {"custom": True, "name": "", "price_cents": 0, "qty": 1}]}}
    r = term.put(f"{base}/api/terminal/orders/{so['id']}/cart", json=body2, timeout=10)
    assert r.status_code == 200, r.text
    journal = term.get(f"{base}/api/terminal/journal", timeout=5).json()
    assert sorted(e["detail"]["name"] for e in journal if e["action"] == "custom_line") == ["Extra dough ball", "Open item"]
