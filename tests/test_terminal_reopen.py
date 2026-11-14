"""Reopen a check from All checks (contract/agent.md 2.10ak, batch B, the owner 2026-11-14): any cashier, the
open batch only; a pickup / counter check goes back on the Pickup list as a paid takeout, a delivery is marked open
again; picked up again = off the list; closed batches stay read-only. Plus the overview's summary over every check."""

from __future__ import annotations

import uuid

from test_terminal import pizza_line, rig  # noqa: F401  (rig is a fixture)


def _place(rig, typ="counter", payments=None, **extra) -> str:
    body = {"client_key": f"k-{uuid.uuid4()}", "type": typ, "cart": {"lines": [pizza_line()]}, **extra}
    if payments is not None:
        body["payments"] = payments
    r = rig["term"].post(f"{rig['agent'].base}/api/terminal/orders", json=body, timeout=10)
    assert r.status_code == 201, r.text
    return r.json()["order"]["order"]["id"]


def _pickups(rig) -> dict[str, dict]:
    r = rig["term"].get(f"{rig['agent'].base}/api/terminal/pickups", timeout=5)
    assert r.status_code == 200, r.text
    return {o["order"]["id"]: o for o in r.json()["orders"]}


def test_reopen_lands_on_pickup_as_paid_then_closed_batch_is_read_only(rig):  # noqa: F811
    base, term = rig["agent"].base, rig["term"]
    q = term.post(f"{base}/api/terminal/quote", json={"type": "counter", "cart": {"lines": [pizza_line()]}}, timeout=5).json()
    total = q["quote"]["total_cents"]
    paid = _place(rig, payments=[{"tender": "cash", "amount_cents": total, "tendered_cents": total}])
    assert paid not in _pickups(rig), "a paid counter check is off the Pickup list"

    # The cashier (rig logs in 1234) reopens it: no manager.
    r = term.post(f"{base}/api/terminal/checks/{paid}/reopen", json={}, timeout=5)
    assert r.status_code == 200, r.text
    body = r.json()
    assert body["reopened"] is True and body["check"]["reopened"] is True and body["check"]["paid"] is True
    assert [p["tender"] for p in body["payments"]] == ["cash"]
    assert body["order"]["status"]["reopened_at"]
    listed = _pickups(rig)
    assert paid in listed and listed[paid]["status"].get("cash_due_cents", 0) == 0
    one = term.get(f"{base}/api/terminal/checks/{paid}", timeout=5).json()
    assert any(e["action"] == "check.reopen" for e in one["journal"])

    # Picked up = closed again: off the list.
    r = term.post(f"{base}/api/terminal/orders/{paid}/picked-up", json={}, timeout=5)
    assert r.status_code == 200 and not r.json()["already"], r.text
    assert paid not in _pickups(rig)

    # A delivery: open again (Dispatch shows it under New).
    did = _place(rig, typ="delivery", customer={"name": "Erik Davis", "phone": "4105550101",
                                                  "address": {"line1": "12 Main St", "city": "Towson"}})
    r = term.post(f"{base}/api/terminal/checks/{did}/reopen", json={}, timeout=5)
    assert r.status_code == 200 and r.json()["check"]["type"] == "delivery" and r.json()["check"]["reopened"], r.text

    # The overview's summary counts every matching check, whatever the page.
    page = term.get(f"{base}/api/terminal/checks", params={"limit": 1}, timeout=5).json()
    assert len(page["checks"]) == 1 and page["total"] == 2 and page["summary"]["count"] == 2
    assert page["summary"]["total_cents"] >= total and page["summary"]["voided"] == 0

    # Unknown, then a closed batch: read-only.
    assert term.post(f"{base}/api/terminal/checks/{uuid.uuid4()}/reopen", json={}, timeout=5).status_code == 404
    assert rig["agent"].http.post(f"{base}/api/batch/close", json={"force": True}, timeout=30).status_code == 200
    r = term.post(f"{base}/api/terminal/checks/{paid}/reopen", json={}, timeout=5)
    assert r.status_code == 409 and r.json()["code"] == "batch_closed", r.text
