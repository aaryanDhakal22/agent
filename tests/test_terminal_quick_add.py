"""Quick add (owner 2026-11-13, contract/agent.md §2.10a): a Quicc Add (custom) line prints where main's
Quick add item routes (the counter menu's `quick_add_item_id` + its route), like any item: here "no kitchen
ticket" = receipt only, then the Pizza station. A counter menu without the field (an older main) keeps the old
rule (every station): test_terminal_gaps covers that. Printing goes to fake printers only.
"""

from __future__ import annotations

from conftest import wait_until
from gatemark import intentional_sleep
from test_terminal import counter_menu, pizza_line, place, rig  # noqa: F401  (rig is a fixture)

QUICK_ADD = "00000000-0000-4000-8000-000000000a01"


def _menu(rig, version: int, route: dict) -> None:
    agent, term, fm = rig["agent"], rig["term"], rig["fm"]
    menu = counter_menu(rig["pizza_id"], version=version)
    menu["quick_add_item_id"] = QUICK_ADD
    menu["routes"][QUICK_ADD] = route
    fm.counter_menu = menu
    fm.command("menu.changed", {"version": version})
    wait_until(lambda: term.get(f"{agent.base}/api/terminal/menu", timeout=5).json()["menu_version"] == version,
               what=f"menu v{version}")


def test_quick_add_line_prints_where_main_routes_it(rig):
    fm, fakes = rig["fm"], rig["fakes"]
    custom = {"custom": True, "name": "Side of ranch", "price_cents": 75, "qty": 3}

    # Receipt only: the line is on the order, off the station ticket (and off "Also on this order").
    _menu(rig, 11, {"printer_id": "", "no_kitchen": True})
    r = place(rig, cart={"lines": [pizza_line(), custom]})
    assert r.status_code == 201, r.text
    so = r.json()["order"]["order"]
    cu = so["items"][1]
    assert (cu["sku"], cu["routing"], cu.get("menu_item_id"), cu.get("printer_id", "")) == ("custom", "none", QUICK_ADD, ""), cu
    wait_until(lambda: fakes["Pizza"].jobs, what="pizza station ticket")
    assert "RANCH" not in fakes["Pizza"].text().upper(), fakes["Pizza"].text()

    def sent(order_id):
        for ev in fm.of_type("order.placed"):
            if ev.get("order_id") == order_id:
                return ev["snapshot"]["items"][1]
        return None
    wait_until(lambda: sent(so["id"]) is not None, what="order.placed to main")
    assert sent(so["id"])["routing"] == "none" and sent(so["id"])["menu_item_id"] == QUICK_ADD

    # On the Pizza printer: matched there.
    _menu(rig, 12, {"printer_id": rig["pizza_id"], "name": "Pizza"})
    fakes["Pizza"].clear()
    r = place(rig, cart={"lines": [custom]})
    assert r.status_code == 201, r.text
    cu = r.json()["order"]["order"]["items"][0]
    assert (cu["routing"], cu["printer_id"], cu["menu_item_id"]) == ("matched", rig["pizza_id"], QUICK_ADD), cu
    wait_until(lambda: "RANCH" in fakes["Pizza"].text().upper(), what="the Quick add line on the Pizza station")


def test_quick_add_prints_only_on_the_printer_the_cashier_picked(rig):
    """The owner, 2026-11-14 (#17; contract §2.1i / §2.10an): a Quicc Add carrying printer_id prints there alone;
    printer_id null prints NOWHERE, even when main's Quick add item has a printer (renaming it changes nothing).
    Fake printers only: zero jobs on every printer for an order of a Quicc Add sent nowhere."""
    fm, fakes = rig["fm"], rig["fakes"]
    _menu(rig, 21, {"printer_id": rig["pizza_id"], "name": "Pizza"})  # the old rule would print it on Pizza
    for f in fakes.values():
        f.clear()
    nowhere = {"custom": True, "name": "Pizza dough", "price_cents": 300, "qty": 1, "printer_id": None}
    r = place(rig, cart={"lines": [nowhere]})
    assert r.status_code == 201, r.text
    cu = r.json()["order"]["order"]["items"][0]
    assert (cu["routing"], cu.get("printer_id", "")) == ("none", ""), cu
    intentional_sleep(1.5, "absence: a Quicc Add sent nowhere may print on no printer")
    assert all(not f.jobs for f in fakes.values()), {k: len(f.jobs) for k, f in fakes.items()}

    # Picked: Pizza alone, by the cashier's printer_id (renamed, still there).
    picked = dict(nowhere, name="Dough ball", printer_id=rig["pizza_id"])
    r = place(rig, cart={"lines": [picked]})
    assert r.status_code == 201, r.text
    cu = r.json()["order"]["order"]["items"][0]
    assert (cu["routing"], cu["printer_id"]) == ("matched", rig["pizza_id"]), cu
    wait_until(lambda: "DOUGH BALL" in fakes["Pizza"].text().upper(), what="the Quicc Add on the Pizza station")
    intentional_sleep(1.0, "absence: nothing on the other printers")
    assert not fakes["Front"].jobs, fakes["Front"].text()
