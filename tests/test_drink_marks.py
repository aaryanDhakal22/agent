"""Drink marks (the owner 2026-11-14, contract/agent.md §2.1k): SoftTouch ended every drink line of a delivery
ticket in "#####" and the drivers scan for them to know what to carry. The Pi stamps each drink when it stores the
order (an item of a drink category, a choice of a drink group, a vendor line by its exact name) and a delivery's
Merchant + Customer Copy print the marks; a pickup's receipt and every kitchen ticket never do.

One throwaway agent behind the fake main, fake printers only (no paper)."""

from __future__ import annotations

import copy
import time
import uuid
from datetime import datetime, timezone

from conftest import wait_until
from test_online_printer_deliveries import placed  # noqa: F401  (a fixture: this Pi places orders from main)
from test_terminal import SODA, counter_menu, rig  # noqa: F401  (rig is a fixture)

WINGS = "00000000-0000-4000-8000-00000000d001"
SODA_GROUP = "00000000-0000-4000-8000-00000000d002"
CAN_COKE = "00000000-0000-4000-8000-00000000d003"
BBQ = "00000000-0000-4000-8000-00000000d004"
ADDR = {"line1": "1 Test St", "city": "Towson", "state": "MD", "zip": "21204"}
MARK = "#####"


def drinks_menu(rig, version: int) -> None:
    """The case menu with Lombardi's shapes: the soda as "2Lt Pepsi" in a Beverages category, "8 Wings & Fries" in
    Party Wings with the "Whole Wings Soda (Counter)" group (a can, and a sauce sold in it). The build carries the
    categories and groups the Pi's drink rule reads."""
    agent, term, fm = rig["agent"], rig["term"], rig["fm"]
    menu = counter_menu(rig["pizza_id"], version=version)
    ctx = menu["pricing"]
    m = ctx["menu"]
    pizza = m["categories"][0]
    soda = next(i for i in pizza["items"] if i["id"] == SODA)
    pizza["items"] = [i for i in pizza["items"] if i["id"] != SODA]
    soda = {**soda, "name": "2Lt Pepsi", "price_cents": 365}
    wings = {**copy.deepcopy(soda), "id": WINGS, "name": "8 Wings & Fries", "price_cents": 1299,
             "anchor": {"item_ids": [WINGS]}, "modifier_group_ids": [SODA_GROUP]}
    m["categories"] += [{**{k: v for k, v in pizza.items() if k != "items"}, "id": str(uuid.uuid4()), "name": "Party Wings",
                         "items": [wings]},
                        {**{k: v for k, v in pizza.items() if k != "items"}, "id": str(uuid.uuid4()), "name": "Beverages",
                         "items": [soda]}]
    group = copy.deepcopy(m["modifier_groups"][0])
    opt = {k: v for k, v in group["options"][0].items() if k != "prices"}
    group.update({"id": SODA_GROUP, "name": "Whole Wings Soda (Counter)", "max_select": 2,
                  "options": [{**opt, "id": CAN_COKE, "name": "W Can Coke", "price_cents": 0, "prices": []},
                              {**opt, "id": BBQ, "name": "Extra BBq Sauce", "price_cents": 50, "prices": []}]})
    m["modifier_groups"].append(group)
    ctx["items"][WINGS] = dict(ctx["items"][SODA])
    menu["build"].update({"categories": m["categories"], "modifier_groups": m["modifier_groups"]})
    menu["routes"][WINGS] = {"printer_id": rig["pizza_id"], "name": "Pizza"}
    fm.counter_menu = menu
    fm.command("menu.changed", {"version": version})
    wait_until(lambda: term.get(f"{agent.base}/api/terminal/menu", timeout=5).json()["menu_version"] == version,
               what=f"menu v{version}")


def lines() -> list[dict]:
    return [{"item_id": SODA, "qty": 1},
            {"item_id": WINGS, "qty": 1, "options": [{"group_id": SODA_GROUP, "option_id": CAN_COKE},
                                                     {"group_id": SODA_GROUP, "option_id": BBQ}]}]


def place(rig, typ: str, last: str) -> dict:
    cust = {"name": f"Ada {last}", "phone": "4105550103", **({"address": ADDR} if typ == "delivery" else {})}
    r = rig["term"].post(f"{rig['agent'].base}/api/terminal/orders", json={
        "client_key": f"k-{uuid.uuid4()}", "type": typ, "cart": {"lines": lines()}, "customer": cust}, timeout=10)
    assert r.status_code == 201, r.text
    return r.json()


def test_a_delivery_prints_the_drink_marks(rig):
    fakes = rig["fakes"]
    front, pizza = fakes["Front"], fakes["Pizza"]
    drinks_menu(rig, 31)

    res = place(rig, "delivery", "Drinkdeliv")
    items = res["order"]["order"]["items"]
    two_lt, wings = items[0], items[1]
    assert two_lt["name"] == "2Lt Pepsi" and two_lt.get("drink") is True, two_lt
    assert "drink" not in wings, wings
    kids = {c["name"]: c for c in wings["children"]}
    assert kids["W Can Coke"].get("drink") is True and "drink" not in kids["Extra BBq Sauce"], kids
    assert res["delivery_tickets"] == "pi"

    wait_until(lambda: "DRINKDELIV" in front.text().upper() and "Customer Copy" in front.text(),
               what="the delivery's Merchant + Customer Copy on Front")
    slip = front.text()
    assert slip.count("2Lt Pepsi " + MARK) == 2, slip
    assert slip.count("W Can Coke " + MARK) == 2, slip
    assert slip.count(MARK) == 4, slip  # never on the sauce
    wait_until(lambda: "WINGS" in pizza.text().upper(), what="the wings' station ticket")
    assert MARK not in pizza.text(), pizza.text()  # kitchen tickets never

    # A pickup with a 2 liter: stamped the same, but its receipt (a reprint on the Pi) carries no marks.
    pick = place(rig, "pickup", "Drinkpick")
    assert pick["order"]["order"]["items"][0].get("drink") is True
    front.clear()
    r = rig["term"].post(f"{rig['agent'].base}/api/terminal/orders/{pick['order']['order']['id']}/reprint", json={}, timeout=15)
    assert r.status_code == 200, r.text
    wait_until(lambda: "DRINKPICK" in front.text().upper() and "Customer Copy" in front.text(), what="the pickup's reprint on Front")
    assert "2Lt Pepsi" in front.text() and MARK not in front.text(), front.text()


def test_an_online_vendor_line_is_a_drink_by_its_exact_name(placed):
    """A vendor order has no bank item: its line is a drink only when its name is a drink category item's name
    (case, spaces and punctuation aside), a choice when it is an option of a drink group."""
    rig = placed
    agent, front = rig["agent"], rig["fakes"]["Front"]
    drinks_menu(rig, 32)
    kid = lambda name: {"id": str(uuid.uuid4()), "name": name, "quantity": 1, "unit_price_cents": 0, "line_total_cents": 0}
    line = lambda name, children=(): {"id": str(uuid.uuid4()), "name": name, "quantity": 1, "unit_price_cents": 300,
                                      "line_total_cents": 300, "routing": "none", "children": list(children)}
    snap = {"id": str(uuid.uuid4()), "tenant_id": str(uuid.uuid4()), "location_id": str(uuid.uuid4()), "source": "online",
            "source_event_id": str(int(time.time() * 1000) % 10**7), "placed_at": datetime.now(timezone.utc).isoformat(),
            "customer": {"display_name": "Ada Vendordeliv", "phone": "4105550100"},
            "fulfillment": {"type": "delivery", "address": {"line1": ADDR["line1"], "city": ADDR["city"]}},
            "money": {"subtotal_cents": 900, "total_cents": 900},
            "payments": [{"method": "card", "amount_cents": 900, "last4": "1881"}],
            "items": [line("2 LT. PEPSI"), line("2 Liter Pepsi"), line("Chicken Tender", [kid("W Can Coke"), kid("Can Coke")])]}
    r = agent.http.post(f"{agent.base}/api/resend", json=snap, timeout=10)
    assert r.status_code == 200, r.text
    wait_until(lambda: "VENDORDELIV" in front.text().upper() and "Customer Copy" in front.text(),
               what="the vendor delivery's copies on Front")
    slip = front.text()
    assert slip.count("2 Lt. Pepsi " + MARK) + slip.count("2 LT. PEPSI " + MARK) == 2, slip  # exact name
    assert "2 Liter Pepsi " + MARK not in slip  # not a name of the menu: nothing fuzzy
    assert slip.count("W Can Coke " + MARK) == 2 and "Can Coke " + MARK not in slip.replace("W Can Coke " + MARK, ""), slip
