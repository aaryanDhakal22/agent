"""What printed, the card's brand and who took it reach main (agent 2.5.5, contract §2.10ag; main §2.5zn).

Through a real dev main (test_terminal.py's rig: a throwaway agent behind the fake main's proxy, a Pizza station +
a Front receipt printer, both fake, a terminal "Till 1" logged in as the cashier). The Pizza printer is down: the
counter order's station ticket comes out on Front with the REROUTED banner, and main's order detail shows

  * prints: one `kitchen` job, the Pizza station ticket `rerouted` to Front (`by` auto, 2 printers tried);
  * the card payment's brand (the terminal sent "VISA") as `card_brand` visa;
  * who took it: source_metadata device_name "Till 1" + staff_name (the cashier as the Pi had them).

A reprint afterwards is a second job.
"""

from __future__ import annotations

import uuid

from conftest import STORE_V1, wait_until
from test_terminal import pizza_line, rig  # noqa: F401  (rig is a fixture)


def test_print_results_brand_and_taken_by_reach_main(rig):  # noqa: F811
    agent, term, fakes, mgr = rig["agent"], rig["term"], rig["fakes"], rig["mgr"]
    base = agent.base
    fakes["Pizza"].stop()  # the station is down: its ticket fails over to Front
    cart = {"lines": [pizza_line()]}
    q = term.post(f"{base}/api/terminal/quote", json={"cart": cart}, timeout=10)
    assert q.status_code == 200, q.text
    total = q.json()["quote"]["total_cents"]
    r = term.post(f"{base}/api/terminal/orders", json={
        "client_key": f"rec-{uuid.uuid4()}", "type": "counter", "cart": cart,
        "payments": [{"tender": "card", "amount_cents": total, "last4": "4242", "brand": "VISA"}]}, timeout=15)
    assert r.status_code == 201, r.text
    oid = r.json()["order"]["order"]["id"]
    wait_until(lambda: "REROUTED" in fakes["Front"].text(), timeout=20, what="the Pizza ticket rerouted to Front")

    detail: dict = {}

    def synced() -> bool:
        g = mgr.get(f"{STORE_V1}/me/orders/{oid}", timeout=5)
        if g.status_code != 200 or not g.json().get("prints"):
            return False
        detail.update(g.json())
        return True

    wait_until(synced, timeout=30, what="the order and its print results on main")
    jobs = detail["prints"]
    assert [j["mode"] for j in jobs] == ["placed"], jobs  # a SEND prints the placement plan (stations + a delivery's copies, 2.5.7)
    t = jobs[0]["tickets"]
    assert len(t) == 1, t
    assert t[0]["kind"] == "station" and t[0]["printer"] == "Pizza" and t[0]["result"] == "rerouted", t
    assert t[0]["printed_on"] == "Front" and t[0]["by"] == "auto" and t[0]["attempts"] == 2 and t[0]["at"], t
    pays = detail["payments"]
    assert [(p["method"], p.get("card_brand"), p.get("last4")) for p in pays] == [("card", "visa", "4242")], pays
    meta = detail["order"]["source_metadata"]
    assert meta["device_name"] == "Till 1" and meta.get("staff_name"), meta

    # A reprint is a job of its own (the receipt on Front, printed there).
    r = term.post(f"{base}/api/terminal/orders/{oid}/reprint", json={}, timeout=15)
    assert r.status_code == 200, r.text
    wait_until(lambda: len(mgr.get(f"{STORE_V1}/me/orders/{oid}", timeout=5).json().get("prints", [])) == 2,
               timeout=30, what="the reprint's job on main")
    last = mgr.get(f"{STORE_V1}/me/orders/{oid}", timeout=5).json()["prints"][-1]
    assert last["mode"] == "receipt" and last["tickets"][0]["kind"] == "reprint" and last["tickets"][0]["result"] == "printed", last
    assert last["tickets"][0]["printed_on"] == "Front", last
