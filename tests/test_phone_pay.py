"""Send to phone (contract §2.10z): a terminal sends a card charge to a cashier's phone, the phone page charges it
with Square's app (here the Pi's fake Square POS page) and the answer records a square_app payment.

  * a phone code needs a manager; the phone pairs with it into an httpOnly cookie and opens /api/phone/* only
  * charge → open → the fake Square's Approve → /pay/back's answer → paid: the payment on the order with
    Square's card (main's lookup), as the cashier who sent it, order.payment to main; a replayed callback = once
  * phone_busy, over_balance, cancel from the terminal, a decline, went_through + Record it
"""

from __future__ import annotations

import json
import urllib.parse
import uuid

import requests

from conftest import wait_until
from test_terminal import place, rig  # noqa: F401  (rig is a fixture)


def _phone(rig) -> requests.Session:
    agent, term = rig["agent"], rig["term"]
    r = term.post(f"{agent.base}/api/terminal/phones/code", json={}, timeout=5)
    assert r.status_code == 403 and r.json()["code"] == "manager_required" and r.json()["action"] == "pair_phone", r.text
    r = term.post(f"{agent.base}/api/terminal/phones/code", json={"manager_pin": "9876"}, timeout=5)
    assert r.status_code == 200 and len(r.json()["code"]) == 6, r.text
    phone = requests.Session()
    p = phone.post(f"{agent.base}/api/phone/pair", json={"code": r.json()["code"], "name": "Ali's phone"}, timeout=5)
    assert p.status_code == 201 and p.json()["kind"] == "phone" and "quicc_phone" in phone.cookies, p.text
    assert "key" not in p.json()
    return phone


def _lookups(fm, found: bool = True, tip: int = 0) -> None:
    fm.rpc["square.pos_config"] = lambda p: (200, {"app_id": "sandbox-x", "environment": "fake", "linked": True})
    fm.rpc["square.pos_fake_charge"] = lambda p: (200, {"ok": True, "transaction_id": "ord_pos_1", "client_transaction_id": "ctx_1"})
    fm.rpc["square.pos_lookup"] = (lambda p: (200, {"found": True, "payment": {
        "id": "pay_pos_1", "transaction_id": "ord_pos_1", "status": "COMPLETED", "brand": "VISA", "last4": "1111",
        "entry": "CONTACTLESS", "amount_cents": p.get("total_cents", 0) + tip, "tip_cents": tip}})) if found else \
        (lambda p: (200, {"found": False, "code": "not_found"}))


def _charge(rig, order_id: str, amount: int, tip: int = 0, key: str | None = None) -> requests.Response:
    body = {"client_key": key or f"ph-{uuid.uuid4().hex[:12]}", "phone_id": rig["phone_id"], "amount_cents": amount, "tip_cents": tip}
    return rig["term"].post(f"{rig['agent'].base}/api/terminal/orders/{order_id}/phone-charges", json=body, timeout=10)


def _fake_square(phone, base: str, charge: dict, do: str) -> dict:
    """The phone's trip through the fake Square POS: its button → the callback query /pay/back reads."""
    meta = json.dumps({"c": charge["id"], "r": base})
    total = charge["amount_cents"] + charge["tip_cents"]
    r = phone.get(f"{base}/pay/fake-square", params={"do": do, "amount": total, "note": charge["note"], "meta": meta, "cb": "/pay/back"},
                  allow_redirects=False, timeout=10)
    assert r.status_code == 303 and r.headers["Location"].startswith("/pay/back?"), (r.status_code, r.headers)
    q = dict(urllib.parse.parse_qsl(urllib.parse.urlsplit(r.headers["Location"]).query))
    assert json.loads(q["com.squareup.pos.REQUEST_METADATA"])["c"] == charge["id"]
    # What back.html posts (Android's parameters).
    if q.get("com.squareup.pos.SERVER_TRANSACTION_ID"):
        return {"status": "ok", "transaction_id": q["com.squareup.pos.SERVER_TRANSACTION_ID"],
                "client_transaction_id": q.get("com.squareup.pos.CLIENT_TRANSACTION_ID", "")}
    return {"status": "error", "error_code": q.get("com.squareup.pos.ERROR_CODE", "")}


def test_send_to_phone_end_to_end(rig):
    agent, term, fm = rig["agent"], rig["term"], rig["fm"]
    base = agent.base
    _lookups(fm, tip=200)  # the customer added $2.00 on Square's screen
    phone = _phone(rig)
    me = phone.get(f"{base}/api/phone/me", timeout=5).json()
    rig["phone_id"] = me["phone"]["id"]
    assert me["square"]["mode"] == "fake" and me["charges"] == [] and me["timeout_seconds"] == 180, me
    # The phone's key opens nothing else.
    assert phone.get(f"{base}/api/floor", headers={"X-Agent-Device-Key": phone.cookies["quicc_phone"]}, timeout=5).json()["code"] == "phone_only"
    assert phone.get(f"{base}/pay", timeout=5).status_code == 200

    o = place(rig).json()
    oid, total = o["order"]["order"]["id"], o["order"]["order"]["money"]["total_cents"]
    assert _charge(rig, oid, total + 1).json()["code"] == "over_balance"
    r = _charge(rig, oid, total, key="ph-key-e2e-0001")
    assert r.status_code == 201, r.text
    c = r.json()["charge"]
    assert c["status"] == "pending" and c["number"] == o["number"] and c["staff_name"] == "Cara S." and c["note"].startswith(f"Order #{o['number']} QP-"), c
    assert _charge(rig, oid, total, key="ph-key-e2e-0001").json()["duplicate"] is True
    busy = _charge(rig, oid, 100)
    assert busy.status_code == 409 and busy.json()["code"] == "phone_busy" and busy.json()["charge_id"] == c["id"], busy.text
    phones = term.get(f"{base}/api/terminal/phones", timeout=5).json()
    assert phones["phones"][0]["busy_charge_id"] == c["id"] and phones["square"]["mode"] == "fake", phones
    assert [x["id"] for x in phone.get(f"{base}/api/phone/me", timeout=5).json()["charges"]] == [c["id"]]

    # The cashier's terminal locks while the customer taps: the payment is still hers.
    term.post(f"{base}/api/terminal/logout", timeout=5)
    assert phone.post(f"{base}/api/phone/charges/{c['id']}/open", json={}, timeout=5).json()["charge"]["status"] == "in_app"
    answer = _fake_square(phone, base, c, "approve")
    r = phone.post(f"{base}/api/phone/charges/{c['id']}/result", json=answer, timeout=15)
    assert r.status_code == 200 and r.json()["duplicate"] is False, r.text
    paid = r.json()["charge"]
    assert paid["status"] == "paid" and paid["brand"] == "visa" and paid["last4"] == "1111" and paid["entry"] == "contactless", paid
    assert paid["extra_tip_cents"] == 200 and paid["transaction_id"] == "ord_pos_1" and paid["square_payment_id"] == "pay_pos_1", paid
    # A reload of /pay/back sends Square's answer again: nothing twice.
    again = phone.post(f"{base}/api/phone/charges/{c['id']}/result", json=answer, timeout=15).json()
    assert again["duplicate"] is True and again["charge"]["payment_id"] == paid["payment_id"]

    assert term.post(f"{base}/api/terminal/login", json={"pin": "1234"}, timeout=5).status_code == 200
    view = term.get(f"{base}/api/terminal/orders/{oid}", timeout=5).json()
    pays = view["payments"]
    assert len(pays) == 1 and view["order"]["status"]["paid"] is True, view
    p = pays[0]
    assert p["tender"] == "square_app" and p["amount_cents"] == total and p["tip_cents"] == 200 and p["ref"] == "pay_pos_1", p
    assert p["last4"] == "1111" and p["brand"] == "visa" and p["staff_id"] == "00000000-0000-4000-8000-0000000000c1", p
    wait_until(lambda: [e for e in fm.of_type("order.payment") if e.get("order_id") == oid], what="order.payment to main")
    ev = [e for e in fm.of_type("order.payment") if e.get("order_id") == oid][0]
    assert ev["payment"]["method"] == "square_app" and ev["payment"]["tip_cents"] == 200, ev
    lookups = [e for e in fm.rpc_seen if e["type"] == "square.pos_lookup"]
    assert lookups and lookups[0]["payload"]["transaction_id"] == "ord_pos_1", lookups
    charges = term.get(f"{base}/api/terminal/orders/{oid}/phone-charges", timeout=5).json()["charges"]
    assert [x["status"] for x in charges] == ["paid"]


def test_cancel_decline_and_record_it(rig):
    agent, term, fm = rig["agent"], rig["term"], rig["fm"]
    base = agent.base
    _lookups(fm, found=False)
    phone = _phone(rig)
    rig["phone_id"] = phone.get(f"{base}/api/phone/me", timeout=5).json()["phone"]["id"]
    o = place(rig).json()
    oid, total = o["order"]["order"]["id"], o["order"]["order"]["money"]["total_cents"]

    # Cancelled from the terminal: the phone's late cancel is the same, CHARGE refused.
    c = _charge(rig, oid, 500).json()["charge"]
    assert term.post(f"{base}/api/terminal/phone-charges/{c['id']}/cancel", json={}, timeout=5).json()["charge"]["status"] == "cancelled"
    r = phone.post(f"{base}/api/phone/charges/{c['id']}/open", json={}, timeout=5)
    assert r.status_code == 409 and r.json()["code"] == "charge_final", r.text

    # Declined in Square: plain words, nothing recorded.
    c = _charge(rig, oid, 500).json()["charge"]
    answer = _fake_square(phone, base, c, "decline")
    d = phone.post(f"{base}/api/phone/charges/{c['id']}/result", json=answer, timeout=10).json()["charge"]
    assert d["status"] == "declined" and "no internet" in d["message"], d
    c = _charge(rig, oid, 500).json()["charge"]
    answer = _fake_square(phone, base, c, "cancel")
    assert phone.post(f"{base}/api/phone/charges/{c['id']}/result", json=answer, timeout=10).json()["charge"]["status"] == "cancelled"

    # No callback came back; the cashier says it went through; Square doesn't show it yet → unrecorded.
    c = _charge(rig, oid, total).json()["charge"]
    u = phone.post(f"{base}/api/phone/charges/{c['id']}/result", json={"status": "went_through"}, timeout=10).json()["charge"]
    assert u["status"] == "unrecorded" and "doesn't show" in u["message"], u
    r = term.post(f"{base}/api/terminal/phone-charges/{c['id']}/record", json={}, timeout=10)
    assert r.status_code == 409 and r.json()["code"] == "not_found_in_square", r.text
    _lookups(fm, found=True)  # now Square shows it
    r = term.post(f"{base}/api/terminal/phone-charges/{c['id']}/record", json={}, timeout=10)
    assert r.status_code == 200 and r.json()["charge"]["status"] == "paid", r.text
    lk = [e for e in fm.rpc_seen if e["type"] == "square.pos_lookup"][-1]["payload"]
    assert lk["code"] == c["code"] and lk["total_cents"] == total and "transaction_id" not in lk, lk
    view = term.get(f"{base}/api/terminal/orders/{oid}", timeout=5).json()
    assert [p["tender"] for p in view["payments"]] == ["square_app"] and view["order"]["status"]["paid"] is True
    # A paid order takes no more.
    assert _charge(rig, oid, 100).json()["code"] == "over_balance"
