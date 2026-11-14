"""Card tips the driver types before Close batch (contract/agent.md §2.10am, batch F, the owner 2026-11-14 #25 / #26).

A throwaway agent behind test_terminal's FAKE MAIN (the driver view's rig, test_terminal_driver.drig), fake
printers only. A cashier places a card-paid counter delivery; the manager sends it out with Ali; Ali closes it and
types the tip on his Tips tab. It is on his report at once (off his handout) but nothing goes to main before Close
batch; a manager corrects it from Quibase / the web UI; the press of Close batch puts it on the card payment
(order.payment to main: amount 0, the tip); after the close it is read-only. The 60-minute old-delivery rule (#27)
and the Square tip adjustment are covered with an injected clock / fake Square by the Go tests
(internal/application/terminal/driver_tips_test.go) and main's squarepay tests.
"""

from __future__ import annotations

from conftest import wait_until
from test_terminal_driver import delivery, drig  # noqa: F401  (the fixture)


def tip_payments(fm, oid: str) -> list[dict]:
    """main's order.payment events for oid that carry only a tip (amount 0)."""
    out = []
    for e in fm.of_type("order.payment"):
        if e.get("order_id") != oid:
            continue
        p = e.get("payment") or e
        if p.get("amount_cents", 0) == 0 and p.get("tip_cents", 0):
            out.append(p)
    return out


def test_typed_tip_on_report_at_once_editable_and_applied_at_close(drig):  # noqa: F811
    agent, ali, fm = drig["agent"], drig["ali"], drig["fm"]
    base = agent.base
    me = drig["logins"]["5511"]["driver"]["id"]
    card = delivery(drig, paid=True)   # square_app: a card paid in store
    cash = delivery(drig)
    # The manager sends both out with Ali; he closes them on his terminal.
    assert agent.http.post(f"{base}/api/drivers/{me}/clock-in", timeout=5).status_code == 200
    r = agent.http.post(f"{base}/api/drivers/{me}/send-out", json={"include_order_ids": [card, cash]}, timeout=10)
    assert r.status_code == 200, r.text
    for oid in (card, cash):
        assert ali.post(f"{base}/api/terminal/driver/orders/{oid}/close", timeout=10).status_code == 200

    # His Tips tab: the card delivery waits on the pile, the cash one never does.
    tips = ali.get(f"{base}/api/terminal/driver/tips", timeout=5).json()
    assert [t["order_id"] for t in tips["pile"]] == [card] and tips["typed"] == []
    assert tips["pile"][0]["tip_kind"] == "card" and tips["pile"][0]["tip_state"] == "none"
    before = tips["report"]
    assert before["tips_to_type"] == 1 and before["typed_tip_cents"] == 0

    # Add tip $4.00: on the report at once, off the handout (after the 6 % tip tax), nothing to main yet.
    r = ali.post(f"{base}/api/terminal/driver/orders/{card}/tip", json={"tip_cents": 400}, timeout=5)
    assert r.status_code == 200 and r.json()["tip_state"] == "tipped" and r.json()["ticket"]["typed_tip_cents"] == 400, r.text
    rep = ali.get(f"{base}/api/terminal/driver/report", timeout=5).json()
    assert rep["tip_cents"] == before["tip_cents"] + 400 and rep["owed_cents"] == before["owed_cents"] - 376
    assert rep["typed_tip_cents"] == 400 and rep["tips_to_type"] == 0
    line = next(x for x in rep["lines"] if x["order_id"] == card)
    assert line["tippable"] and line["tip_state"] == "tipped" and line["typed_tip_cents"] == 400 and line["tip_cents"] == 400
    tips = ali.get(f"{base}/api/terminal/driver/tips", timeout=5).json()
    assert tips["pile"] == [] and [t["order_id"] for t in tips["typed"]] == [card]
    assert tip_payments(fm, card) == []
    # Cash: not tippable; someone else's order: not his.
    r = ali.post(f"{base}/api/terminal/driver/orders/{cash}/no-tip", timeout=5)
    assert r.status_code == 409 and r.json()["code"] == "not_tippable", r.text
    r = drig["sam"].post(f"{base}/api/terminal/driver/orders/{card}/no-tip", timeout=5)
    assert r.status_code == 403 and r.json()["code"] == "not_your_order", r.text

    # The manager corrects it (Quibase / the web UI) until Close batch.
    r = agent.http.put(f"{base}/api/orders/{card}/driver-tip", json={"tip_state": "tipped", "tip_cents": 500}, timeout=5)
    assert r.status_code == 200 and r.json()["typed_tip_cents"] == 500 and r.json()["driver_id"] == me, r.text
    assert r.json()["report"]["tip_cents"] == before["tip_cents"] + 500
    r = agent.http.put(f"{base}/api/orders/{card}/driver-tip", json={"tip_state": "tipped"}, timeout=5)
    assert r.status_code == 400, r.text
    r = agent.http.put(f"{base}/api/orders/{cash}/driver-tip", json={"tip_state": "no_tip"}, timeout=5)
    assert r.status_code == 409 and r.json()["code"] == "not_tippable", r.text
    assert tip_payments(fm, card) == []

    # The press of Close batch: the tip goes on the card payment (order.payment, amount 0, $5.00).
    r = agent.http.post(f"{base}/api/batch/close", json={"force": True}, timeout=30)
    assert r.status_code == 200, r.text
    wait_until(lambda: sum(p.get("tip_cents", 0) for p in tip_payments(fm, card)) == 500, what="the typed tip to main at the close")
    # Closed batches are read-only.
    r = agent.http.put(f"{base}/api/orders/{card}/driver-tip", json={"tip_state": "no_tip"}, timeout=5)
    assert r.status_code == 409 and r.json()["code"] == "batch_closed", r.text
    actions = [e["action"] for e in drig["till"].get(f"{base}/api/terminal/journal", timeout=5).json()]
    assert actions.count("driver.tip") >= 3 and "payment.tip" in actions
