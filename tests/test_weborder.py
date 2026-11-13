"""Website / channel orders on the floor (2.5.0, weborder stage 3) on a
throwaway agent with fake printers, against dev main:

  * a channel order (source web / operator) prints the station tickets + the
    receipt on accept even with every online ticket switch off;
  * ADD TIP reaches main's card hold (order.tip); Close batch's batch.closed
    makes main capture it; tips lock when the driver clocks out / at Close
    batch; cash orders can't take one;
  * CANCEL ORDER after accept prints a VOID slip on the station and main
    cancels the order and releases the card hold;
  * the online-ordering proxies (pause / resume, sold out) round-trip.

Website orders are placed on main's /channel/v1/orders (a web channel key,
main's fake Square — SQUARE_ENV=fake on dev); the clock-out lock uses a
delivery fed through Resend (main doesn't hold it, which is fine there).
"""

from __future__ import annotations

import secrets
import time
import uuid
from datetime import datetime, timezone

import pytest
import requests

from gatemark import intentional_sleep
from conftest import ADMIN_V1, MAIN_BASE_URL, STORE_V1, FakePrinter, manager_session, provision, wait_until

CH_V1 = f"{MAIN_BASE_URL}/channel/v1"
FAKE_SQUARE = f"{MAIN_BASE_URL}/__fake/square"
OFF = {"online_card": False, "online_cash_pickup": False, "online_cash_delivery": False}


def _ok(r: requests.Response, status: int = 200):
    assert r.status_code == status, f"{r.status_code}: {r.text}"
    return r.json() if r.content else None


@pytest.fixture
def web(admin, main_store, spawn_agent, fakes):
    """main store with Front (receipt) + Pizza fake printers, a one-item menu
    on Pizza, Square (fake) connected, web + operator channel keys, and a
    provisioned agent with every online ticket switch off."""
    if requests.get(f"{FAKE_SQUARE}/_fake/payments", timeout=5).status_code != 200:
        pytest.skip("main isn't running the fake Square (SQUARE_ENV=fake)")
    mgr = manager_session(admin, main_store)
    me = f"{STORE_V1}/me"
    ids = {}
    for name in ("Front", "Pizza"):
        fakes[name] = FakePrinter()
        ids[name] = _ok(mgr.post(f"{me}/printers", json={"name": name, "ip": fakes[name].addr}, timeout=5), 201)["id"]
    menu = _ok(mgr.post(f"{me}/menu/menus", json={"name": "Main"}, timeout=5), 201)
    cat = _ok(mgr.post(f"{me}/menu/menus/{menu['id']}/categories", json={"name": "Pizza", "default_printer_id": ids["Pizza"]}, timeout=5), 201)
    item = _ok(mgr.post(f"{me}/menu/categories/{cat['id']}/items", json={"name": "Cheese Pizza", "price_cents": 1299}, timeout=5), 201)
    # Square (main's fake): the connect page → main's callback.
    url = _ok(mgr.post(f"{me}/square/connect", timeout=5))["url"]
    assert "square=connected" in requests.get(url, timeout=10).url
    keys = {}
    for kind in ("web", "operator"):
        keys[kind] = _ok(admin.post(f"{ADMIN_V1}/channels", json={"location_id": main_store.location_id, "kind": kind,
                                                                 "name": kind.title()}, timeout=5), 201)["key"]
    a = spawn_agent()
    provision(a, main_store)
    wait_until(lambda: len(a.http.get(f"{a.base}/api/printers", timeout=5).json()) == 2, timeout=20, what="printer roster")
    _ok(a.http.put(f"{a.base}/api/settings/tickets", json=OFF, timeout=5))
    a.store, a.item, a.keys = main_store, item, keys
    return a


def place(a, kind: str = "web", method: str = "card", token: str = "cnon:card-nonce-ok") -> dict:
    """Places a pickup on main through a channel; waits for the Pi to hold it."""
    body = {"client_order_id": "t-" + secrets.token_hex(6), "type": "pickup", "lines": [{"item_id": a.item["id"], "qty": 1}],
            "payment": {"method": method, "token": token},
            "customer": {"name": "Ada Lovelace", "phone": "(410) 555-0101", "email": "ada@example.com"}}
    o = _ok(requests.post(f"{CH_V1}/orders", json=body, headers={"Authorization": f"Bearer {a.keys[kind]}"}, timeout=10), 201)
    wait_until(lambda: a.http.get(f"{a.base}/api/orders/{o['id']}", timeout=5).status_code == 200, timeout=20,
               what="the order on the Pi")
    return o


def hold(admin, oid: str) -> dict:
    return _ok(admin.get(f"{ADMIN_V1}/card-holds/{oid}", timeout=5))


def channel_status(a, oid: str) -> str:
    return _ok(requests.get(f"{CH_V1}/orders/{oid}", headers={"Authorization": f"Bearer {a.keys['web']}"}, timeout=5))["status"]


def post(a, path: str, body: dict | None = None) -> requests.Response:
    return a.http.post(f"{a.base}{path}", json=body, timeout=30)


def wait_jobs(fake: FakePrinter, n: int) -> None:
    wait_until(lambda: len(fake.jobs) >= n, timeout=10, what=f"{n} job(s) on the fake printer")


def resend(a, source: str, fulfillment: str, card: bool) -> str:
    """A channel order straight into the Pi (main doesn't hold it)."""
    oid = str(uuid.uuid4())
    snap = {"id": oid, "tenant_id": a.store.tenant_id, "location_id": a.store.location_id, "source": source,
            "source_event_id": str(int(time.time() * 1000) % 10**7), "placed_at": datetime.now(timezone.utc).isoformat(),
            "customer": {"display_name": "Pytest Web"}, "fulfillment": {"type": fulfillment, "address": {"line1": "1 Main St"}},
            "money": {"subtotal_cents": 2000, "tip_cents": 300, "total_cents": 2300},
            "items": [{"id": str(uuid.uuid4()), "name": "Thing", "quantity": 1, "unit_price_cents": 2000, "line_total_cents": 2000}]}
    if card:
        snap["payments"] = [{"method": "card", "amount_cents": 2300}]
    _ok(a.http.post(f"{a.base}/api/resend", json=snap, timeout=10))
    return oid


def test_channel_order_prints_stations_and_receipt_with_switches_off(web, fakes) -> None:
    a = web
    for kind, method, token in (("web", "card", "cnon:card-nonce-ok"), ("operator", "cash", "")):
        for f in fakes.values():
            f.clear()
        o = place(a, kind, method, token)
        got = _ok(a.http.get(f"{a.base}/api/orders/{o['id']}", timeout=5))
        assert got["order"]["source"] == kind
        _ok(post(a, f"/api/orders/{o['id']}/accept", {"prep_minutes": 15}))  # the accept answers once it printed
        wait_jobs(fakes["Pizza"], 1)
        assert "1  Cheese Pizza" in fakes["Pizza"].text(), fakes["Pizza"].text()
        # 2.5.7 (the owner, 2026-11-13): a pickup's customer receipt never prints on the receipt printer.
        assert fakes["Front"].jobs == [], fakes["Front"].text()
    # A website delivery prints its receipt too, whatever the switches say.
    for f in fakes.values():
        f.clear()
    oid = resend(a, "web", "delivery", card=True)
    _ok(post(a, f"/api/orders/{oid}/accept"))
    wait_jobs(fakes["Front"], 1)
    assert "Total:" in fakes["Front"].text()
    # The same switches leave a vendor card delivery receipt-only; a vendor pickup prints its stations only (2.5.7).
    for f in fakes.values():
        f.clear()
    oid = resend(a, "online", "delivery", card=True)
    _ok(post(a, f"/api/orders/{oid}/accept"))
    wait_jobs(fakes["Front"], 1)
    intentional_sleep(1.0, "absence: nothing else may print")
    assert fakes["Pizza"].jobs == []


def test_add_tip_reaches_main_and_close_batch_captures(admin, web) -> None:
    a = web
    o = place(a)
    _ok(post(a, f"/api/orders/{o['id']}/accept", {"prep_minutes": 15}))
    so = _ok(post(a, f"/api/orders/{o['id']}/tip", {"tip_cents": 500}))
    assert so["status"]["added_tip_cents"] == 500 and not so["status"].get("tip_locked")
    wait_until(lambda: hold(admin, o["id"])["added_tip_cents"] == 500, timeout=20, what="order.tip on main's hold")
    # Bad input / not tippable.
    assert post(a, f"/api/orders/{o['id']}/tip", {"tip_cents": 50001}).json()["code"] == "bad_request"
    assert post(a, f"/api/orders/{o['id']}/tip", {}).status_code == 400
    assert post(a, f"/api/orders/{uuid.uuid4()}/tip", {"tip_cents": 1}).status_code == 404
    cash = place(a, "operator", "cash", "")
    r = post(a, f"/api/orders/{cash['id']}/tip", {"tip_cents": 100})
    assert r.status_code == 409 and r.json()["code"] == "not_tippable", r.text

    _ok(post(a, f"/api/orders/{cash['id']}/accept"))
    _ok(post(a, f"/api/orders/{o['id']}/ready"))
    _ok(post(a, f"/api/orders/{cash['id']}/ready"))
    _ok(post(a, "/api/batch/close", {"force": True}))
    # batch.closed: main captures the hold with the tip; the tip is final here.
    h = None

    def captured():
        nonlocal h
        admin.post(f"{ADMIN_V1}/card-holds/tick", timeout=30)
        h = hold(admin, o["id"])
        return h["status"] == "captured"

    wait_until(captured, timeout=30, interval=1, what="main to capture at batch.closed")
    assert h["capture_reason"] == "batch_close" and h["captured_cents"] == o["money"]["total_cents"] + 500, h
    got = _ok(a.http.get(f"{a.base}/api/orders/{o['id']}", timeout=5))
    assert got["status"]["tip_locked"] is True
    r = post(a, f"/api/orders/{o['id']}/tip", {"tip_cents": 600})
    assert r.status_code == 409 and r.json()["code"] in ("tip_locked", "not_tippable"), r.text


def test_tips_lock_when_the_driver_clocks_out(web) -> None:
    a = web
    d = _ok(post(a, "/api/drivers", {"name": "Sam"}), 201)
    _ok(post(a, f"/api/drivers/{d['id']}/clock-in"))
    card = resend(a, "web", "delivery", card=True)
    cash = resend(a, "web", "delivery", card=False)
    for oid in (card, cash):
        _ok(post(a, f"/api/orders/{oid}/accept"))
        _ok(post(a, f"/api/orders/{oid}/ready"))
    _ok(post(a, "/api/deliveries/assign", {"driver_id": d["id"], "order_ids": [card, cash]}))
    so = _ok(post(a, f"/api/orders/{card}/tip", {"tip_cents": 400}))
    assert so["status"]["added_tip_cents"] == 400
    assert post(a, f"/api/orders/{cash}/tip", {"tip_cents": 100}).json()["code"] == "not_tippable"
    _ok(post(a, f"/api/drivers/{d['id']}/returned"))

    rep = next(x for x in _ok(a.http.get(f"{a.base}/api/batches/0/driver-reports", timeout=5)) if x["driver_id"] == d["id"])
    line = next(x for x in rep["lines"] if x["order_id"] == card)
    assert line["tip_cents"] == 700 and line["added_tip_cents"] == 400 and line["tip_pending"] is True, line
    assert rep["pending_tip_cents"] == 700, rep

    _ok(post(a, f"/api/drivers/{d['id']}/clock-out"))
    got = _ok(a.http.get(f"{a.base}/api/orders/{card}", timeout=5))
    assert got["status"]["tip_locked"] is True, got["status"]
    r = post(a, f"/api/orders/{card}/tip", {"tip_cents": 500})
    assert r.status_code == 409 and r.json()["code"] == "tip_locked", r.text
    rep = next(x for x in _ok(a.http.get(f"{a.base}/api/batches/0/driver-reports", timeout=5)) if x["driver_id"] == d["id"])
    assert rep["pending_tip_cents"] == 0 and not next(x for x in rep["lines"] if x["order_id"] == card).get("tip_pending")


def test_cancel_order_after_accept_voids_and_main_cancels(admin, web, fakes) -> None:
    a = web
    o = place(a)
    _ok(post(a, f"/api/orders/{o['id']}/accept", {"prep_minutes": 15}))
    wait_jobs(fakes["Pizza"], 1)  # the accept's tickets (a pickup: no receipt since 2.5.7), before the clear below
    for f in fakes.values():
        f.clear()
    assert post(a, f"/api/orders/{o['id']}/cancel-order", {"reason": "because"}).json()["code"] == "bad_request"
    assert post(a, f"/api/orders/{o['id']}/cancel-order", {"reason": "other", "note": "x" * 141}).status_code == 400
    so = _ok(post(a, f"/api/orders/{o['id']}/cancel-order", {"reason": "customer_asked", "note": "Called to cancel"}))
    assert so["status"]["state"] == "cancelled" and so["status"]["cancel_reason"] == "customer_asked"
    wait_jobs(fakes["Pizza"], 1)
    # The VOID slip: REMOVE / QUITAR, each line "(VOID)" + the item struck through (a picture).
    assert "REMOVE / QUITAR" in fakes["Pizza"].text() and "(VOID) \x1b*!" in fakes["Pizza"].text(), fakes["Pizza"].text()
    intentional_sleep(1.0, "absence: no receipt for a cancel")
    assert fakes["Front"].jobs == []
    # main: cancelled (after accept, by the reason), the card hold released.
    wait_until(lambda: channel_status(a, o["id"]) == "cancelled", timeout=20, what="main to cancel the order")
    wait_until(lambda: hold(admin, o["id"])["status"] == "cancelled", timeout=20, what="the card hold released")
    # Not again, and never a vendor order.
    assert post(a, f"/api/orders/{o['id']}/cancel-order", {"reason": "other"}).json()["code"] == "not_cancellable"
    online = resend(a, "online", "pickup", card=True)
    assert post(a, f"/api/orders/{online}/cancel-order", {"reason": "other"}).json()["code"] == "not_cancellable"
    # A waiting (never printed) website order: cancelled, no VOID.
    w = place(a)
    for f in fakes.values():
        f.clear()
    _ok(post(a, f"/api/orders/{w['id']}/cancel-order", {"reason": "other"}))
    intentional_sleep(1.0, "absence: nothing may print")
    assert fakes["Pizza"].jobs == [] and fakes["Front"].jobs == []


def test_online_proxies_round_trip(web) -> None:
    a = web
    st = _ok(a.http.get(f"{a.base}/api/online", timeout=15))
    assert st["state"] in ("open", "closed") and st["pause"] is None, st
    st = _ok(post(a, "/api/online/pause", {"preset": "30m", "note": "Oven down"}))
    assert st["state"] == "paused" and st["pause"]["note"] == "Oven down", st
    assert _ok(a.http.get(f"{a.base}/api/online", timeout=15))["state"] == "paused"
    assert post(a, "/api/online/pause", {"preset": "forever"}).status_code == 400
    r = post(a, "/api/online/pause", {"preset": "15m", "note": "x" * 141})
    assert r.status_code == 400 and r.json()["code"] == "bad_request", r.text  # main's 400
    st = _ok(a.http.delete(f"{a.base}/api/online/pause", timeout=15))
    assert st["state"] != "paused" and st["pause"] is None, st

    stock = _ok(a.http.get(f"{a.base}/api/online/menu", timeout=15))
    item = next(i for i in stock["items"] if i["id"] == a.item["id"])
    assert item["name"] == "Cheese Pizza" and item["category"] == "Pizza" and item["in_stock"] is True, item
    assert isinstance(stock["groups"], list)
    row = _ok(post(a, "/api/online/stock", {"kind": "item", "id": a.item["id"], "in_stock": False, "hold": True}))
    assert row["in_stock"] is False and row["hold"] is True, row
    item = next(i for i in _ok(a.http.get(f"{a.base}/api/online/menu", timeout=15))["items"] if i["id"] == a.item["id"])
    assert item["in_stock"] is False and item["hold"] is True
    row = _ok(post(a, "/api/online/stock", {"kind": "item", "id": a.item["id"], "in_stock": True}))
    assert row["in_stock"] is True
    assert post(a, "/api/online/stock", {"kind": "size", "id": a.item["id"], "in_stock": False}).status_code == 400
    assert post(a, "/api/online/stock", {"kind": "item", "id": str(uuid.uuid4()), "in_stock": False}).status_code == 400
    # Floor auth: a stranger gets nothing.
    assert requests.get(f"{a.base}/api/online", timeout=5).status_code == 401


def test_tablet_key_drives_tip_cancel_and_online(web, fakes) -> None:
    """Quibase's path: a paired tablet's key (not the web-UI session) adds a
    tip, cancels and pauses; an earlier batch's order can't be cancelled."""
    a = web
    code = _ok(post(a, "/api/pairing/code"))["code"]
    key = _ok(requests.post(f"{a.base}/api/pair", json={"code": code, "name": "Counter"}, timeout=5), 201)["key"]
    tab = {"X-Agent-Device-Key": key}

    def tpost(path: str, body: dict | None = None) -> requests.Response:
        return requests.post(f"{a.base}{path}", json=body, headers=tab, timeout=30)

    o = place(a)
    _ok(tpost(f"/api/orders/{o['id']}/accept", {"prep_minutes": 15}))
    wait_jobs(fakes["Pizza"], 1)
    assert _ok(tpost(f"/api/orders/{o['id']}/tip", {"tip_cents": 300}))["status"]["added_tip_cents"] == 300
    # Same tip again: answered, nothing new for main (the repo tests pin the event count).
    assert _ok(tpost(f"/api/orders/{o['id']}/tip", {"tip_cents": 300}))["status"]["added_tip_cents"] == 300
    st = _ok(requests.get(f"{a.base}/api/online", headers=tab, timeout=15))
    assert st["state"] in ("open", "closed"), st
    assert _ok(tpost("/api/online/pause", {"preset": "15m"}))["state"] == "paused"
    assert _ok(requests.delete(f"{a.base}/api/online/pause", headers=tab, timeout=15))["pause"] is None

    # A ready pickup left at Close batch: main captured its card, so it is
    # never cancelled from the floor afterwards (refunds are Square's).
    _ok(tpost(f"/api/orders/{o['id']}/ready"))
    _ok(post(a, "/api/batch/close", {"force": True}))
    r = tpost(f"/api/orders/{o['id']}/cancel-order", {"reason": "customer_asked"})
    assert r.status_code == 409 and r.json()["code"] == "not_cancellable", r.text
    # A fresh one in the new batch: cancelled with the tablet key.
    w = place(a)
    so = _ok(tpost(f"/api/orders/{w['id']}/cancel-order", {"reason": "other", "note": "Duplicate"}))
    assert so["status"]["state"] == "cancelled" and so["status"]["cancel_reason"] == "other"
