"""The store's customers on the Pi (TERMINAL-PLAN 1d, contract §2.10j), against dev main.
Synthetic customers only (410-555-01xx-style numbers, "Test" names).

  * main's admin import → main pushes customers.changed → the Pi pulls its change feed
  * lookup by the last 4 / the first digits / the full number (the Pi's copy, no internet needed)
  * a customer added at the terminal is saved on the Pi at once (pending) and reaches main
    as customer.upsert (main keeps it; an import never renames it); main's copy comes back
  * the customer's recent orders on the Pi; address suggestions are a later piece (501)

Uses test_terminal.py's rig (a throwaway agent behind the fake main, fake printers only).
"""

from __future__ import annotations

import secrets
import uuid

from conftest import ADMIN_V1, wait_until
from test_terminal import pizza_line, rig  # noqa: F401  (rig is a fixture)


def _phone() -> str:
    return "410555" + f"{secrets.randbelow(10000):04d}"


def _import(admin, location_id: str, recs: list[dict], dry_run: bool = False) -> dict:
    r = admin.post(f"{ADMIN_V1}/locations/{location_id}/customers/import", json={"customers": recs},
                   params={"dry_run": "1"} if dry_run else None, timeout=30)
    assert r.status_code == 200, r.text
    return r.json()


def _lookup(rig, digits: str) -> dict:
    r = rig["term"].get(f"{rig['agent'].base}/api/terminal/customers", params={"phone": digits}, timeout=5)
    assert r.status_code == 200, r.text
    return r.json()


def test_customers_sync_lookup_add_and_orders(rig, admin, main_store):  # noqa: F811
    agent, term = rig["agent"], rig["term"]
    base = agent.base
    # Three synthetic customers sharing a last 4 / a prefix.
    tail = f"{secrets.randbelow(10000):04d}"
    p1, p2, p3 = "410555" + tail, "443555" + tail, _phone()
    res = _import(admin, main_store.location_id, [
        {"phone": p1, "first_name": "Ada", "last_name": "Test", "notes": "side door",
         "addresses": [{"line1": "1 Test Street", "line2": "Apt 2", "city": "Towson", "zip": "21286"}]},
        {"phone": p2, "first_name": "Bo", "last_name": "Test"},
        {"phone": p3, "first_name": "Cy", "last_name": "Test"}])
    assert res["created"] == 3, res

    # main pushes customers.changed; the Pi pulls the feed.
    wait_until(lambda: len(_lookup(rig, tail)["customers"]) == 2, timeout=45, what="imported customers on the Pi")
    found = _lookup(rig, tail)
    assert {c["phone"] for c in found["customers"]} == {p1, p2} and found["count"] >= 3 and found["synced_at"]
    ada = next(c for c in found["customers"] if c["phone"] == p1)
    assert (ada["name"], ada["notes"], ada["source"], ada["pending"]) == ("Ada Test", "side door", "import", False)
    assert [(a["line1"], a["line2"], a["default"]) for a in ada["addresses"]] == [("1 Test Street", "Apt 2", True)]
    assert [c["phone"] for c in _lookup(rig, p1)["customers"]] == [p1]
    assert p1 in {c["phone"] for c in _lookup(rig, p1[:7])["customers"]}
    assert _lookup(rig, "41")["customers"] == []

    # Add one at the terminal: saved on the Pi at once, then main has it.
    p4 = _phone()
    while p4 in (p1, p2, p3):
        p4 = _phone()
    r = term.post(f"{base}/api/terminal/customers", json={"phone": f"({p4[:3]}) {p4[3:6]}-{p4[6:]}", "first_name": "Dee",
                                                          "last_name": "Test", "notes": "no onions",
                                                          "address": {"line1": "4 Test Road", "line2": "Unit 1", "city": "Towson",
                                                                      "zip": "21204"}}, timeout=5)
    assert r.status_code == 201, r.text
    dee = r.json()["customer"]
    assert r.json()["created"] is True and dee["pending"] is True and dee["phone"] == p4 and len(dee["addresses"]) == 1
    assert [c["id"] for c in _lookup(rig, p4[-4:])["customers"] if c["phone"] == p4] == [dee["id"]]
    bad = term.post(f"{base}/api/terminal/customers", json={"phone": p4, "address": {"line1": "x", "city": "y"}}, timeout=5)
    assert bad.status_code == 400 and bad.json()["code"] == "bad_request"

    # main took customer.upsert: an import of that phone keeps the counter's name.
    wait_until(lambda: rig["fm"].of_type("customer.upsert"), what="customer.upsert sent to main")
    wait_until(lambda: _import(admin, main_store.location_id, [{"phone": p4, "first_name": "Imp", "last_name": "Other"}],
                               dry_run=True)["created"] == 0, timeout=30, what="the counter's customer at main")
    dry = _import(admin, main_store.location_id, [{"phone": p4, "first_name": "Imp", "last_name": "Other"}], dry_run=True)
    assert dry["kept"] == {"name": 1}, dry
    # main's copy comes back through the feed: no longer pending, same ids.
    wait_until(lambda: not next(c for c in _lookup(rig, p4)["customers"])["pending"], timeout=45, what="main's copy back on the Pi")
    back = _lookup(rig, p4)["customers"][0]
    assert back["id"] == dee["id"] and back["source"] == "counter" and back["notes"] == "no onions"
    assert [a["id"] for a in back["addresses"]] == [a["id"] for a in dee["addresses"]]

    # Edit + an address; the phone never changes.
    r = term.patch(f"{base}/api/terminal/customers/{dee['id']}", json={"last_name": "Tested"}, timeout=5)
    assert r.status_code == 200 and r.json()["customer"]["name"] == "Dee Tested", r.text
    r = term.post(f"{base}/api/terminal/customers/{dee['id']}/addresses", json={"line1": "9 Work Ave", "city": "Towson"}, timeout=5)
    assert r.status_code == 200 and len(r.json()["customer"]["addresses"]) == 2, r.text
    r = term.patch(f"{base}/api/terminal/customers/{dee['id']}", json={"phone": _phone()}, timeout=5)
    assert r.status_code == 400
    assert term.patch(f"{base}/api/terminal/customers/{uuid.uuid4()}", json={"first_name": "X"}, timeout=5).status_code == 404

    # An order for the customer; it shows in their recent orders.
    r = term.post(f"{base}/api/terminal/orders", json={"client_key": f"k-{uuid.uuid4()}", "type": "pickup",
                                                       "cart": {"lines": [pizza_line()]},
                                                       "customer": {"id": dee["id"], "name": "Dee", "phone": p4}}, timeout=10)
    assert r.status_code == 201, r.text
    oid = r.json()["order"]["order"]["id"]
    orders = term.get(f"{base}/api/terminal/customers/{dee['id']}/orders", timeout=5).json()
    assert [o["order"]["id"] for o in orders["orders"]] == [oid] and orders["source"] == "pi"
    placed = wait_until(lambda: [e for e in rig["fm"].of_type("order.placed") if e["order_id"] == oid], what="order.placed")
    assert placed[0]["snapshot"]["customer_id"] == dee["id"]
    assert _lookup(rig, p4)["customers"][0]["last_order_at"]

    # Address suggestions: a later piece.
    r = term.get(f"{base}/api/terminal/addresses", params={"q": "4 Test"}, timeout=5)
    assert r.status_code == 501 and r.json()["code"] == "not_implemented"
    # A logged-out terminal neither writes nor looks up (the security round: its key alone doesn't read the customer book).
    term.post(f"{base}/api/terminal/logout", timeout=5)
    assert term.post(f"{base}/api/terminal/customers", json={"phone": _phone(), "first_name": "Eve"}, timeout=5).status_code == 401
    for path in (f"/api/terminal/customers?phone={p1}", f"/api/terminal/customers/{dee['id']}/orders"):
        r = term.get(base + path, timeout=5)
        assert r.status_code == 401 and r.json()["code"] == "login_required", (path, r.text)
