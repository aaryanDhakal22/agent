"""The terminal's Manager tab + the owner's permissions (2026-11-12), end to end on a throwaway Pi:

  * refunds: a cashier refunds cash up to the cashier limit per order (several small refunds add up),
    a card refund and cash back for a card sale need a manager, a manager logged in needs no PIN
  * the till report (contract §2.10m): one drawer session and the whole batch, card brands, the money
    identity (total collected + unpaid = gross receipts)
  * driver reports listed and printed from a terminal (on the Pi's receipt printer)
  * the day's close from a terminal needs a manager

Uses test_terminal.py's rig (a throwaway agent behind the fake main, fake printers only).
"""

from __future__ import annotations

import uuid

from conftest import wait_until
from test_terminal import pizza_line, rig  # noqa: F401  (rig is a fixture)
from test_terminal_driver import pair_terminal


def _place(rig, typ="counter", payments=None, term=None) -> tuple[str, int]:
    term = term or rig["term"]
    body = {"client_key": f"k-{uuid.uuid4()}", "type": typ, "cart": {"lines": [pizza_line()]}}
    if payments is not None:
        body["payments"] = payments
    r = term.post(f"{rig['agent'].base}/api/terminal/orders", json=body, timeout=10)
    assert r.status_code == 201, r.text
    so = r.json()["order"]["order"]
    return so["id"], so["money"]["total_cents"]


def _manager_terminal(rig):
    t = pair_terminal(rig["agent"], "Till 2")
    wait_until(lambda: t.post(f"{rig['agent'].base}/api/terminal/login", json={"pin": "9876"}, timeout=5).status_code == 200,
               what="manager login")
    return t


def test_refund_rules(rig):  # noqa: F811
    base, term = rig["agent"].base, rig["term"]
    oid, total = _place(rig, payments=[{"tender": "cash", "amount_cents": 2357, "tendered_cents": 3000}])

    def refund(t, tender, cents, **kw):
        return t.post(f"{base}/api/terminal/orders/{oid}/payments",
                      json={"tender": tender, "amount_cents": cents, "refund": True, "reason": "cold", **kw}, timeout=5)

    # The cashier's $10 (cashier_refund_cents) is per order: $6 + $4, then one cent more needs a manager.
    assert refund(term, "cash", 600).status_code == 200
    assert refund(term, "cash", 400).status_code == 200
    r = refund(term, "cash", 1)
    assert r.status_code == 403 and r.json()["code"] == "manager_required" and r.json()["action"] == "refund", r.text
    assert r.json()["limit"] == 1000 and r.json()["requested"] == 1001
    assert refund(term, "cash", 1, manager_pin="9876").json()["payment"]["approved_by"] == "s-manager"
    # A manager logged in acts without a PIN.
    mgr = _manager_terminal(rig)
    assert refund(mgr, "cash", 100).status_code == 200

    # A card sale: card refund (any amount) and cash back both need a manager.
    oid, total = _place(rig, payments=[{"tender": "card", "amount_cents": 2357, "brand": "MASTERCARD"}])
    r = refund(term, "card", 100)
    assert r.status_code == 403 and r.json()["action"] == "card_refund"
    r = refund(term, "cash", 100)
    assert r.status_code == 403 and r.json()["action"] == "cash_for_card"
    assert refund(term, "card", 100, manager_pin="9876").status_code == 200
    pays = term.get(f"{base}/api/terminal/orders/{oid}", timeout=5).json()["payments"]
    assert pays[0]["brand"] == "mastercard"
    actions = [e["action"] for e in term.get(f"{base}/api/terminal/journal", timeout=5).json()]
    assert actions.count("override") == 2 and actions.count("refund") == 5


def test_till_report_drawer_and_batch(rig):  # noqa: F811
    base, term = rig["agent"].base, rig["term"]
    r = term.post(f"{base}/api/terminal/drawer/open", json={"start_cents": 10000}, timeout=5)
    assert r.status_code == 201, r.text
    drawer = r.json()["drawer"]["id"]
    _place(rig, payments=[{"tender": "cash", "amount_cents": 2357, "tendered_cents": 3000}])
    _place(rig, typ="pickup", payments=[{"tender": "card", "amount_cents": 2357, "tip_cents": 300, "brand": "VISA"}])
    _place(rig, typ="pickup", payments=[{"tender": "square_app", "amount_cents": 2000}])  # 357 still due
    r = term.get(f"{base}/api/terminal/reports/till?drawer={drawer}", timeout=10)
    assert r.status_code == 200, r.text
    t = r.json()["report"]
    s = t["summary"]
    assert t["scope"] == "drawer" and t["state"] == "open" and t["drawer"]["device_name"] == "Till 1" and t["batch"]["state"] == "open"
    assert s["gross_sales_cents"] == 3 * 2224 and s["tax_cents"] == 3 * 133 and s["tips_collected_cents"] == 300
    assert s["total_cash_cents"] == 2357 and s["total_credit_card_cents"] == 2357 + 300 + 2000 and s["unpaid_cents"] == 357
    assert s["total_collected_cents"] + s["unpaid_cents"] == s["gross_receipts_cents"]
    cards = {c["brand"]: c for c in t["detail"]["cards"]}
    assert list(cards) == ["amex", "discover", "mastercard", "visa", "other"]
    assert cards["visa"]["count"] == 1 and cards["visa"]["amount_cents"] == 2657 and cards["other"]["label"] == "Credit Other"
    assert t["detail"]["categories_total"]["amount_cents"] == 3 * 2224
    assert [(e["name"], e["count"]) for e in t["detail"]["employees"]] == [("Cara S.", 3)]
    b = term.get(f"{base}/api/terminal/reports/till?batch=current", timeout=10).json()["report"]
    assert b["scope"] == "batch" and b["drawer"] is None and b["summary"]["gross_receipts_cents"] == s["gross_receipts_cents"]
    assert term.get(f"{base}/api/terminal/reports/till?drawer=nope", timeout=5).status_code == 404
    assert term.get(f"{base}/api/terminal/reports/till?drawer={drawer}&batch=1", timeout=5).status_code == 400
    batches = term.get(f"{base}/api/terminal/reports/batches", timeout=5).json()["batches"]
    assert batches and batches[0]["id"] == b["batch"]["id"]
    # No login: 401.
    assert term.post(f"{base}/api/terminal/logout", timeout=5).status_code == 204
    r = term.get(f"{base}/api/terminal/reports/till", timeout=5)
    assert r.status_code == 401 and r.json()["code"] == "login_required"


def test_driver_reports_and_the_close_from_a_terminal(rig):  # noqa: F811
    agent, term, fakes = rig["agent"], rig["term"], rig["fakes"]
    base = agent.base
    d = agent.http.post(f"{base}/api/drivers", json={"name": "Ali"}, timeout=5).json()
    assert agent.http.post(f"{base}/api/drivers/{d['id']}/clock-in", timeout=5).status_code == 200
    reps = term.get(f"{base}/api/terminal/reports/drivers", timeout=5).json()
    assert [r["driver_name"] for r in reps["reports"]] == ["Ali"]
    before = len(fakes["Front"].jobs)
    r = term.post(f"{base}/api/terminal/reports/drivers/print", json={"driver_id": d["id"]}, timeout=30)
    assert r.status_code == 200, r.text
    assert [(p["driver_id"], p["printed_on"]) for p in r.json()["printed"]] == [(d["id"], "Front")]
    wait_until(lambda: len(fakes["Front"].jobs) > before, what="driver report on the receipt printer")
    assert "DRIVER REPORT" in fakes["Front"].text()
    assert term.post(f"{base}/api/terminal/reports/drivers/print", json={"driver_id": "nobody"}, timeout=10).status_code == 404

    # The day's close: a cashier needs a manager's PIN.
    r = term.post(f"{base}/api/terminal/close", json={"force": True}, timeout=10)
    assert r.status_code == 403 and r.json()["code"] == "manager_required" and r.json()["action"] == "close_batch", r.text
    r = term.post(f"{base}/api/terminal/close", json={"force": True, "manager_pin": "1234"}, timeout=10)
    assert r.json()["code"] == "manager_pin_invalid"
    r = term.post(f"{base}/api/terminal/close", json={"force": True, "manager_pin": "9876"}, timeout=30)
    assert r.status_code == 200 and r.json()["closed"]["closed_at"] and r.json()["opened"]["id"] != r.json()["closed"]["id"], r.text
    entries = term.get(f"{base}/api/terminal/journal", timeout=5).json()
    close = [e for e in entries if e["action"] == "batch.close"]
    assert close and close[0]["detail"]["approved_by"] == "s-manager"
    # The closed batch's reports are still there.
    closed_id = r.json()["closed"]["id"]
    assert term.get(f"{base}/api/terminal/reports/drivers?batch={closed_id}", timeout=5).json()["batch_id"] == closed_id
    till = term.get(f"{base}/api/terminal/reports/till?batch={closed_id}", timeout=10).json()["report"]
    assert till["state"] == "closed" and till["batch"]["closed_at"]
