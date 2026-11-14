"""A general refund to a card (batch J, the owner 2026-11-14; contract §2.10x "A general refund to a card").

A throwaway agent behind test_terminal's fake main, a cashier logged in:
  * unlinked: the card field's token → main's square.general_refund, a manager every time (403 before Square is
    asked), recorded only after Square took it: tender manual_card, order "", ref = Square's refund id, no drawer,
    no kick; the same key again = the stored refund (Square asked once); main hears refund.general on manual_card;
    the till has it on the card's brand row and general_card_refunds, never the cash
  * a refused / unknown answer (402 refund_failed, 402 unlinked_unsupported, 503 not known) records nothing
  * the fallback: GET /api/terminal/refunds/card-payments lists main's square.card_payments, then a linked
    square.refund on the picked payment
"""

from __future__ import annotations

import uuid

from conftest import wait_until
from test_terminal import rig  # noqa: F401  (rig is a fixture)


def _key() -> str:
    return f"g-{uuid.uuid4().hex[:16]}"


def _refund(rig, **body):
    return rig["term"].post(f"{rig['agent'].base}/api/terminal/refunds", json=body, timeout=15)


def test_unlinked_card_refund_is_a_managers_and_never_cash(rig):  # noqa: F811
    agent, term, fm = rig["agent"], rig["term"], rig["fm"]
    seen: list[dict] = []

    def general(p: dict):
        seen.append(p)
        return 200, {"ok": True, "refund_id": "sqref_unlinked_1", "status": "PENDING", "brand": "VISA", "last4": "5858",
                     "unlinked": True}

    fm.rpc["square.general_refund"] = general
    key = _key()
    body = {"client_key": key, "tender": "manual_card", "amount_cents": 750, "reason": "Overcharged yesterday",
            "card_nonce": "cnon:card-nonce-ok", "brand": "visa", "last4": "1111"}
    r = _refund(rig, **body)
    assert r.status_code == 403 and r.json()["code"] == "manager_required", r.text
    assert not seen, "Square was asked before the manager allowed it"
    r = _refund(rig, **body, manager_pin="9876")
    assert r.status_code == 200, r.text
    res = r.json()
    p = res["payment"]
    assert p["tender"] == "manual_card" and p["order_id"] == "" and p["refund_kind"] == "general" and p["refund"], p
    assert p["ref"] == "sqref_unlinked_1" and p["square_refund_id"] == "sqref_unlinked_1" and p["last4"] == "5858", p
    assert p["brand"] == "visa" and not p.get("drawer_id") and res["kick_drawer"] is False, res
    assert len(seen) == 1 and seen[0]["nonce"] == "cnon:card-nonce-ok" and seen[0]["amount_cents"] == 750, seen
    # The same tap again: the stored refund, Square not asked twice.
    again = _refund(rig, **body, manager_pin="9876").json()
    assert again["duplicate"] and again["payment"]["id"] == p["id"] and len(seen) == 1, again
    # main hears refund.general on the card's method with Square's refund id.
    wait_until(lambda: any(e["payment"]["id"] == p["id"] for e in fm.of_type("refund.general")), what="the card refund synced")
    ev = next(e for e in fm.of_type("refund.general") if e["payment"]["id"] == p["id"])
    assert ev["payment"]["method"] == "manual_card" and ev["payment"]["amount_cents"] == -750 and ev["payment"]["ref"] == "sqref_unlinked_1", ev
    # The journal: no drawer kick, no card token anywhere.
    j = term.get(f"{agent.base}/api/terminal/journal?limit=50", timeout=5).json()
    assert not [e for e in j if e["action"] == "drawer.kick"], j
    assert [e for e in j if e["action"] == "square.general_refund" and e["detail"]["ok"]], j
    assert "cnon:" not in str(j)
    # The till: on the Visa row and general_card_refunds, never the cash.
    till = term.get(f"{agent.base}/api/terminal/reports/till?batch=current", timeout=10).json()["report"]
    d = till["detail"]
    assert d["general_card_refunds"] == {"count": 1, "amount_cents": -750}, d["general_card_refunds"]
    assert d["general_refunds"]["amount_cents"] == -750 and till["summary"]["total_cash_cents"] == 0, till["summary"]
    assert till["summary"]["total_credit_card_cents"] == -750, till["summary"]


def test_refused_or_unknown_card_refund_records_nothing(rig):  # noqa: F811
    agent, term, fm = rig["agent"], rig["term"], rig["fm"]
    answers = [
        (200, {"ok": False, "code": "refund_failed", "square_code": "NOT_FOUND", "error": "Card nonce not found"}, 402, "refund_failed"),
        (200, {"ok": False, "code": "unlinked_unsupported", "error": "no unlinked refunds"}, 402, "unlinked_unsupported"),
        (500, {}, 503, "square_unavailable"),
    ]
    key = _key()
    body = {"client_key": key, "tender": "manual_card", "amount_cents": 500, "reason": "Wrong charge",
            "card_nonce": "cnon:card-nonce-ok", "manager_pin": "9876"}
    for status, answer, want, code in answers:
        fm.rpc["square.general_refund"] = lambda p, s=status, a=answer: (s, a)
        r = _refund(rig, **body)
        assert r.status_code == want and r.json()["code"] == code, (code, r.status_code, r.text)
    checks = term.get(f"{agent.base}/api/terminal/journal?limit=100", timeout=5).json()
    assert not [e for e in checks if e["action"] == "refund"], "a refused / unknown card refund was recorded"
    assert not fm.of_type("refund.general")
    # The manager tries again with the same key after no answer: recorded once.
    fm.rpc["square.general_refund"] = lambda p: (200, {"ok": True, "refund_id": "sqref_retry", "status": "PENDING"})
    r = _refund(rig, **body)
    assert r.status_code == 200 and not r.json()["duplicate"], r.text


def test_fallback_lists_card_payments_then_a_linked_refund(rig):  # noqa: F811
    fm = rig["fm"]
    listed: list[dict] = []
    refunded: list[dict] = []

    def card_payments(p: dict):
        listed.append(p)
        return 200, {"ok": True, "payments": [{"square_payment_id": "pay_big", "at": "2026-11-13T19:00:00Z", "amount_cents": 2000,
                                               "refundable_cents": 2000, "brand": "MASTERCARD", "last4": "4242", "entry": "chip"}]}

    def refund(p: dict):
        refunded.append(p)
        return 200, {"ok": True, "refund_id": "sqref_linked", "status": "PENDING"}

    fm.rpc["square.card_payments"] = card_payments
    fm.rpc["square.refund"] = refund
    base = rig["agent"].base
    r = rig["term"].get(f"{base}/api/terminal/refunds/card-payments?last4=4242&amount_cents=650", timeout=10)
    assert r.status_code == 200, r.text
    pays = r.json()["payments"]
    assert [x["square_payment_id"] for x in pays] == ["pay_big"] and listed[0] == {"last4": "4242", "amount_cents": 650}, (pays, listed)
    assert rig["term"].get(f"{base}/api/terminal/refunds/card-payments?last4=42&amount_cents=650", timeout=5).status_code == 400
    r = _refund(rig, client_key=_key(), tender="card", amount_cents=650, reason="Overcharged", square_payment_id="pay_big",
                brand="mastercard", last4="4242", manager_pin="9876")
    assert r.status_code == 200, r.text
    p = r.json()["payment"]
    assert refunded and refunded[0]["payment_id"] == "pay_big" and refunded[0]["amount_cents"] == 650, refunded
    assert p["tender"] == "card" and p["square_payment_id"] == "pay_big" and p["square_refund_id"] == "sqref_linked", p
    assert p["brand"] == "mastercard" and p["last4"] == "4242" and not p.get("drawer_id"), p
