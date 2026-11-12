"""Centurion's first-time setup, the Pi's side (contract/agent.md §2.10y).

  * pairing a terminal with the store's admin pairing code (default 1024, a Pi setting, throttled)
  * GET /api/terminal/setup: store, menu, routing, staff counts, printers, setups
  * a terminal key reads /api/diag (+ terminals[]) and prints test slips (no failover)
  * a setup report is kept on the Pi (per device) and relayed to main with device_kind
  * POST /api/terminal/print: raw bytes to one named printer, journaled
  * POST /api/terminal/square/test-refund: the setup's $1 test charge refunded through main
  * setup.* journal rows with nobody logged in

Uses test_terminal.py's rig (a throwaway agent behind the fake main, fake printers only).
"""

from __future__ import annotations

import base64
import uuid
from datetime import datetime, timezone

import requests

from conftest import wait_until
from test_terminal import rig  # noqa: F401  (rig is a fixture)


def test_pair_with_the_admin_code(rig):  # noqa: F811
    agent = rig["agent"]
    base = agent.base
    r = requests.post(f"{base}/api/pair/admin", json={"code": "4444", "name": "Counter 2"}, timeout=5)
    assert r.status_code == 401 and r.json()["code"] == "wrong_admin_code", r.text
    r = requests.post(f"{base}/api/pair/admin", json={"code": "1024", "name": "Counter 2", "kind": "terminal"}, timeout=5)
    assert r.status_code == 201 and r.json()["kind"] == "terminal", r.text
    key = r.json()["device_key"]
    assert requests.get(f"{base}/api/terminal/session", headers={"X-Agent-Device-Key": key}, timeout=5).status_code == 200
    # Only terminals.
    assert requests.post(f"{base}/api/pair/admin", json={"code": "1024", "name": "Tab", "kind": "tablet"}, timeout=5).status_code == 400
    # The store's own code (the web page's setting); the code is never answered back.
    view = agent.http.get(f"{base}/api/settings/admin-code", timeout=5).json()
    assert view == {"enabled": True, "is_default": True, "length": 4}
    r = agent.http.put(f"{base}/api/settings/admin-code", json={"code": "908172"}, timeout=5)
    assert r.status_code == 200 and "908172" not in r.text and r.json()["length"] == 6, r.text
    assert requests.post(f"{base}/api/pair/admin", json={"code": "1024", "name": "Counter 3"}, timeout=5).status_code == 401
    assert requests.post(f"{base}/api/pair/admin", json={"code": "908172", "name": "Counter 3"}, timeout=5).status_code == 201
    # Off.
    agent.http.put(f"{base}/api/settings/admin-code", json={"enabled": False}, timeout=5)
    r = requests.post(f"{base}/api/pair/admin", json={"code": "908172", "name": "Counter 4"}, timeout=5)
    assert r.status_code == 403 and r.json()["code"] == "admin_code_off", r.text
    # The setting history names the change, never the code.
    hist = agent.http.get(f"{base}/api/settings/history", timeout=5)
    if hist.status_code == 200:
        assert "908172" not in hist.text
    # Wrong guesses pause the address (5 in 15 min), even for the right code.
    agent.http.put(f"{base}/api/settings/admin-code", json={"enabled": True, "code": ""}, timeout=5)
    # (two misses above already count: at most 5 more until the pause)
    for _ in range(5):
        r = requests.post(f"{base}/api/pair/admin", json={"code": "0000", "name": "X"}, timeout=5)
        if r.status_code == 429:
            break
        assert r.status_code == 401, r.text
    r = requests.post(f"{base}/api/pair/admin", json={"code": "1024", "name": "X"}, timeout=5)
    assert r.status_code == 429 and int(r.headers["Retry-After"]) > 0, r.text
    names = {d["name"] for d in agent.http.get(f"{base}/api/devices", timeout=5).json() if d["kind"] == "terminal"}
    assert {"Counter 2", "Counter 3"} <= names and "X" not in names


def test_setup_summary_and_diagnostics(rig):  # noqa: F811
    agent, term, fakes = rig["agent"], rig["term"], rig["fakes"]
    base = agent.base
    s = term.get(f"{base}/api/terminal/setup", timeout=5)
    assert s.status_code == 200, s.text
    s = s.json()
    assert {(p["name"], p["role"]) for p in s["printers"]} == {("Front", "receipt"), ("Pizza", "station")}
    assert s["menu"]["version"] == 7 and s["menu"]["items"] > 0
    pizza = next(p for p in s["routes"]["printers"] if p["name"] == "Pizza")
    assert pizza["items"] == 2 and s["routes"]["unknown_printer"] == []
    assert s["routes"]["unrouted"] == s["menu"]["items"] - 2
    assert s["staff"] == {"managers": 1, "cashiers": 1, "drivers": 0, "others": 0}
    assert s["store"]["counter_start"] == 500 and s["store"]["name"] and "tax_bps" in s["store"]
    assert s["setups"] == []
    # No PIN, hash or staff name anywhere in it.
    assert "pin_hash" not in str(s) and "Khawar" not in str(s)

    # Pi health for the terminal (+ terminals[]).
    d = term.get(f"{base}/api/diag", timeout=5)
    assert d.status_code == 200, d.text
    assert [t["name"] for t in d.json()["terminals"]] == ["Till 1"]

    # A test slip on the Pizza station only: no failover, names the terminal.
    pizza_id = rig["pizza_id"]
    fakes["Pizza"].clear()
    fakes["Front"].clear()
    r = term.post(f"{base}/api/printers/test", json={"printer_id": pizza_id}, timeout=20)
    assert r.status_code == 200 and r.json()["results"][0]["ok"], r.text
    wait_until(lambda: "Till 1" in fakes["Pizza"].text(), what="the test slip on Pizza")
    assert fakes["Front"].jobs == []
    # A dead printer's test says so and nothing comes out elsewhere.
    fakes["Pizza"].stop()
    r = term.post(f"{base}/api/printers/test", json={"printer_id": pizza_id}, timeout=30).json()["results"][0]
    assert not r["ok"] and r["error_code"] in ("unreachable", "write_failed")
    assert fakes["Front"].jobs == []


def test_setup_report_kept_and_relayed(rig):  # noqa: F811
    agent, term, fm = rig["agent"], rig["term"], rig["fm"]
    base = agent.base
    seen: list[dict] = []

    def diag(payload):
        seen.append(payload)
        return 200, {"id": payload["report"]["id"], "stored": True, "duplicate": False}

    fm.rpc["diag.report"] = diag
    rid = str(uuid.uuid4())
    at = datetime.now(timezone.utc).replace(microsecond=0).isoformat().replace("+00:00", "Z")
    report = {"v": 1, "id": rid, "kind": "setup", "outcome": "not_fixed", "created_at": at,
              "headline": "Till 1 set up - drawer not confirmed", "failed": ["drawer"], "steps": []}
    r = term.post(f"{base}/api/diag/report", json=report, timeout=15)
    assert r.status_code == 202 and r.json()["stored"], r.text  # the relay's answer (as for a tablet)
    assert seen and seen[0]["device_kind"] == "terminal" and seen[0]["tablet_name"] == "Till 1"
    devices = agent.http.get(f"{base}/api/devices", timeout=5).json()
    till = next(d for d in devices if d["name"] == "Till 1")
    assert till["setup"]["outcome"] == "not_fixed" and till["setup"]["failed"] == ["drawer"] and till["setup"]["report_id"] == rid
    s = term.get(f"{base}/api/terminal/setup", timeout=5).json()
    assert [(x["name"], x["kind"], x["kind_of_report"], x["outcome"]) for x in s["setups"]] == [("Till 1", "terminal", "setup", "not_fixed")]
    # Kept even when main can't take it.
    fm.rpc["diag.report"] = lambda p: (503, {"error": "down"})
    ok = dict(report, id=str(uuid.uuid4()), kind="full_check", outcome="ok", failed=[])
    assert term.post(f"{base}/api/diag/report", json=ok, timeout=15).status_code == 502
    s = term.get(f"{base}/api/terminal/setup", timeout=5).json()
    assert s["setups"][0]["kind_of_report"] == "full_check" and s["setups"][0]["outcome"] == "ok"


def test_terminal_print_to_a_named_printer(rig):  # noqa: F811
    agent, term, fakes = rig["agent"], rig["term"], rig["fakes"]
    base = agent.base
    job = b"\x1b@BACKUP RECEIPT TEST\n\n\n\x1dV\x00"
    fakes["Front"].clear()
    fakes["Pizza"].clear()
    r = term.post(f"{base}/api/terminal/print", json={"printer": "Front", "kind": "test",
                                                       "bytes_b64": base64.b64encode(job).decode()}, timeout=20)
    assert r.status_code == 200 and r.json() == {"ok": True, "printer": "Front"}, r.text
    wait_until(lambda: job in fakes["Front"].jobs, what="the exact bytes on Front")
    assert fakes["Pizza"].jobs == []
    r = term.post(f"{base}/api/terminal/print", json={"printer": "Nowhere", "kind": "test", "bytes_b64": "G0A="}, timeout=5)
    assert r.status_code == 404 and r.json()["code"] == "unknown_printer"
    # A dead printer: 502, never another printer.
    fakes["Front"].stop()
    r = term.post(f"{base}/api/terminal/print", json={"printer": "Front", "kind": "receipt", "bytes_b64": "G0A="}, timeout=30)
    assert r.status_code == 502 and r.json()["code"] == "print_failed", r.text
    assert fakes["Pizza"].jobs == []
    rows = [e for e in term.get(f"{base}/api/terminal/journal?limit=50", timeout=5).json() if e["action"] == "terminal_print"]
    assert len(rows) == 2 and {r["detail"]["ok"] for r in rows} == {True, False}


def test_test_refund_through_main(rig):  # noqa: F811
    agent, term, fm = rig["agent"], rig["term"], rig["fm"]
    base = agent.base
    fm.rpc["square.test_refund"] = lambda p: (200, {"refund_id": "rf-1", "status": "PENDING"}) if p["amount_cents"] == 100 else (400, {"error": "x"})
    body = {"square_payment_id": "pay-123", "amount_cents": 100, "idempotency_key": "setup-" + uuid.uuid4().hex[:12]}
    r = term.post(f"{base}/api/terminal/square/test-refund", json=body, timeout=10)
    assert r.status_code == 200 and r.json()["refund_id"] == "rf-1" and r.json()["ok"], r.text
    assert fm.rpc_seen[-1]["payload"] == body
    fm.rpc["square.test_refund"] = lambda p: (409, {"error": "Square isn't connected", "code": "square_not_linked"})
    r = term.post(f"{base}/api/terminal/square/test-refund", json=body, timeout=10)
    assert r.status_code == 409 and r.json()["code"] == "square_not_linked", r.text
    assert term.post(f"{base}/api/terminal/square/test-refund", json=dict(body, amount_cents=500), timeout=5).status_code == 400
    rows = [e for e in term.get(f"{base}/api/terminal/journal?limit=50", timeout=5).json() if e["action"] == "setup.card_refund"]
    assert len(rows) == 2


def test_setup_journal_logged_out(rig):  # noqa: F811
    agent, term = rig["agent"], rig["term"]
    base = agent.base
    assert term.post(f"{base}/api/terminal/logout", timeout=5).status_code == 204
    for action in ("setup.started", "setup.finished", "admin_drawer_test"):
        r = term.post(f"{base}/api/terminal/journal", json={"action": action, "detail": {"step": "receipt_printer"}}, timeout=5)
        assert r.status_code == 201 and r.json()["staff_id"] == "", r.text
    r = term.post(f"{base}/api/terminal/journal", json={"action": "no_sale"}, timeout=5)
    assert r.status_code == 401 and r.json()["code"] == "login_required"
