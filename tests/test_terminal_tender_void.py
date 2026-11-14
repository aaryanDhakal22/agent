"""Void a tender from Cash Out (contract §2.10al, batch C, the owner 2026-11-14 #13).

A throwaway agent behind test_terminal's fake main, a cashier logged in (refund limit 0 on this rig):
  * a cash tender voided by the cashier, no manager: a refund of kind tender_void pointing at it, kick_drawer, the
    rest due again, journal tender.void + drawer.kick, main's order.payment negative with kind tender_void;
    the same key again = the stored void, another key = already
  * a reader card with Square's id: main's square.refund asked for what Square charged, the refund id kept; a
    Square-app tender: recorded, refund_in_square
  * refusals: a refund is not a tender (409 not_voidable_tender), an unknown payment (404), no key (400)
"""

from __future__ import annotations

import uuid

from conftest import wait_until
from test_terminal import place, rig  # noqa: F401  (rig is a fixture)


def _key() -> str:
    return f"tv-{uuid.uuid4().hex[:16]}"


def _void(rig, oid: str, pid: str, key: str | None = None, **extra):
    return rig["term"].post(f"{rig['agent'].base}/api/terminal/checks/{oid}/tenders/{pid}/void",
                            json={"client_key": key or _key(), **extra}, timeout=10)


def _pay(rig, oid: str, **body) -> dict:
    r = rig["term"].post(f"{rig['agent'].base}/api/terminal/orders/{oid}/payments", json=body, timeout=10)
    assert r.status_code == 200, r.text
    return r.json()


def test_cash_tender_void_by_a_cashier(rig):  # noqa: F811
    agent, term, fm = rig["agent"], rig["term"], rig["fm"]
    placed = place(rig).json()["order"]
    oid, total = placed["order"]["id"], placed["order"]["money"]["total_cents"]
    cash = _pay(rig, oid, tender="cash", amount_cents=total - 500, tendered_cents=total)
    card = _pay(rig, oid, tender="square_app", amount_cents=500)
    assert card["balance_cents"] == 0

    key = _key()
    r = _void(rig, oid, cash["payment"]["id"], key=key, reason="Wrong tender")
    assert r.status_code == 200, r.text
    v = r.json()
    ref = v["refund"]
    assert ref["refund"] and ref["refund_kind"] == "tender_void" and ref["refund_of"] == cash["payment"]["id"], ref
    assert ref["tender"] == "cash" and ref["amount_cents"] == total - 500 and ref["reason"] == "Wrong tender"
    assert v["kick_drawer"] is True and v["refund_in_square"] is False and v["balance_cents"] == total - 500, v
    assert not v["order"]["status"].get("paid"), v["order"]["status"]
    # The same key again: the stored void; another key: already (nothing new).
    again = _void(rig, oid, cash["payment"]["id"], key=key, reason="Wrong tender").json()
    assert again["duplicate"] and again["refund"]["id"] == ref["id"] and not again["kick_drawer"]
    other = _void(rig, oid, cash["payment"]["id"]).json()
    assert other["already"] and other["refund"]["id"] == ref["id"]
    view = term.get(f"{agent.base}/api/terminal/orders/{oid}", timeout=5).json()
    assert len(view["payments"]) == 3, view["payments"]
    # Journaled under the cashier, the drawer kick too.
    j = term.get(f"{agent.base}/api/terminal/journal?limit=50", timeout=5).json()
    tv = [e for e in j if e["action"] == "tender.void"]
    assert len(tv) == 1 and tv[0]["detail"]["payment_id"] == cash["payment"]["id"], tv
    assert [e for e in j if e["action"] == "drawer.kick" and e["detail"]["reason"] == "tender_void"]
    # main hears a negative order.payment marked tender_void, pointing at the tender.
    wait_until(lambda: any(e["payment"].get("kind") == "tender_void" for e in fm.of_type("order.payment")), what="the void synced")
    ev = next(e for e in fm.of_type("order.payment") if e["payment"].get("kind") == "tender_void")
    assert ev["payment"]["amount_cents"] == -(total - 500) and ev["payment"]["refund_of"] == cash["payment"]["id"], ev

    # The Square-app tender: recorded, the cashier told to give it back in Square's app.
    sq = _void(rig, oid, card["payment"]["id"]).json()
    assert sq["refund_in_square"] is True and sq["kick_drawer"] is False and sq["balance_cents"] == total, sq


def test_card_tender_void_refunds_on_square(rig):  # noqa: F811
    fm = rig["fm"]
    seen: list[dict] = []

    def refund(p: dict):
        seen.append(p)
        return 200, {"ok": True, "refund_id": "rf_tv_1", "status": "COMPLETED"}

    fm.rpc["square.refund"] = refund
    placed = place(rig).json()["order"]
    oid, total = placed["order"]["id"], placed["order"]["money"]["total_cents"]
    card = _pay(rig, oid, tender="card", amount_cents=total, tip_cents=200, brand="visa", last4="4242",
                square_payment_id="pay_reader_1", entry="chip")
    r = _void(rig, oid, card["payment"]["id"])
    assert r.status_code == 200, r.text
    v = r.json()
    assert seen and seen[0]["payment_id"] == "pay_reader_1" and seen[0]["amount_cents"] == total + 200, seen
    assert v["refund"]["square_refund_id"] == "rf_tv_1" and v["refund"]["last4"] == "4242", v["refund"]
    assert v["refund_in_square"] is False and v["balance_cents"] == total


def test_tender_void_refusals(rig):  # noqa: F811
    placed = place(rig).json()["order"]
    oid = placed["order"]["id"]
    cash = _pay(rig, oid, tender="cash", amount_cents=1000)
    rf = _void(rig, oid, cash["payment"]["id"]).json()["refund"]
    r = _void(rig, oid, rf["id"])
    assert r.status_code == 409 and r.json()["code"] == "not_voidable_tender", r.text
    r = _void(rig, oid, str(uuid.uuid4()))
    assert r.status_code == 404 and r.json()["code"] == "not_found", r.text
    r = rig["term"].post(f"{rig['agent'].base}/api/terminal/checks/{oid}/tenders/{cash['payment']['id']}/void", json={}, timeout=5)
    assert r.status_code == 400 and r.json()["code"] == "client_key_required", r.text
