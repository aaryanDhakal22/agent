"""Quibase setup + diagnostics surface (agent 2.0.1) on a throwaway agent.

GET /api/diag, the printer test print / probe against FAKE printers (one
dead: the slip must never be rerouted), tablet self-rename, a pairing code
minted with a device key, UDP LAN discovery, and the two calls that go
through main (report token mint, diag.report relay) — those skip when the
dev main predates the tablet-tokens route.
"""

from __future__ import annotations

import json
import secrets
import socket
import time
import uuid
from contextlib import closing

import pytest
import requests

from conftest import ADMIN_V1, MAIN_BASE_URL, STORE_V1, FakePrinter, manager_session, wait_until


def _dead_addr() -> str:
    """A loopback address nothing listens on (connection refused)."""
    with closing(socket.socket(socket.AF_INET, socket.SOCK_STREAM)) as s:
        s.bind(("127.0.0.1", 0))
        return f"127.0.0.1:{s.getsockname()[1]}"


def _pair(agent, name: str = "Counter") -> dict:
    code = agent.http.post(f"{agent.base}/api/pairing/code", timeout=5).json()["code"]
    r = requests.post(f"{agent.base}/api/pair", json={"code": code, "name": name}, timeout=5)
    assert r.status_code == 201, r.text
    return {"X-Agent-Device-Key": r.json()["key"], "_id": r.json()["device_id"]}


def _hdr(tab: dict) -> dict:
    return {"X-Agent-Device-Key": tab["X-Agent-Device-Key"]}


@pytest.fixture
def diag(admin, main_store, spawn_agent, fakes):
    """A linked, logged-in throwaway agent with Front (receipt, alive) + Dead
    (nothing listening) printers, opening hours + quiet_alert_minutes set, and
    one paired tablet."""
    mgr = manager_session(admin, main_store)
    loc = mgr.get(f"{STORE_V1}/me/location", timeout=5).json()
    body = {k: loc[k] for k in ("name", "timezone", "daily_reset_hour", "phone", "address_line1", "address_line2", "config")}
    hours = {d: [{"open": "00:00", "close": "23:59"}] for d in ("mon", "tue", "wed", "thu", "fri", "sat", "sun")}
    body["config"] = {**(body["config"] or {}), "opening_hours": hours, "quiet_alert_minutes": 45}
    r = mgr.patch(f"{STORE_V1}/me/location", json=body, timeout=5)
    assert r.status_code == 200, r.text

    fakes["Front"] = FakePrinter()
    ids = {}
    for name, addr in (("Front", fakes["Front"].addr), ("Dead", _dead_addr())):
        r = mgr.post(f"{STORE_V1}/me/printers", json={"name": name, "ip": addr}, timeout=5)
        assert r.status_code == 201, r.text
        ids[name] = r.json()["id"]

    agent = spawn_agent()
    token = main_store.mint()["token"]
    assert requests.post(f"{agent.base}/api/setup/token", json={"token": token}, timeout=10).status_code == 200
    assert agent.http.post(f"{agent.base}/api/setup/password", json={"password": "pw", "confirm": "pw"}, timeout=10).status_code == 200
    wait_until(lambda: len(agent.http.get(f"{agent.base}/api/printers", timeout=5).json()) == 2,
               timeout=20, what="agent to apply the 2-printer roster")
    tab = _pair(agent, "Counter")
    return {"agent": agent, "ids": ids, "tab": tab, "store": main_store, "location": loc}


def test_diag_shape(diag) -> None:
    a, tab = diag["agent"], diag["tab"]
    assert requests.get(f"{a.base}/api/diag", timeout=5).status_code == 401

    def connected():
        d = requests.get(f"{a.base}/api/diag", headers=_hdr(tab), timeout=10).json()
        return d if d["main"]["state"] == "connected" else None

    d = wait_until(connected, timeout=20, what="diag to show main connected")
    assert set(d) >= {"version", "hostname", "store_name", "uptime_s", "main", "health", "problems", "printers",
                      "orders", "open_now", "quiet_alert_minutes", "floor_backlog", "acks_pending"}
    assert d["store_name"] == diag["location"]["name"]
    assert d["main"]["reachable"] is True and d["main"]["connected_since"]
    assert set(d["main"]) >= {"state", "connected_since", "last_event_at", "last_error", "reconnects", "reachable"}
    assert "file_stale" in d["health"] and "disk_free_bytes" in d["health"]
    assert isinstance(d["open_now"], bool)
    assert d["quiet_alert_minutes"] == 45
    assert d["orders"] == {"last_arrived_at": None, "open_batch_orders": 0, "waiting": 0}
    names = [p["name"] for p in d["printers"]]
    assert sorted(names) == ["Dead", "Front"]
    front = next(p for p in d["printers"] if p["name"] == "Front")
    assert front["id"] == diag["ids"]["Front"] and front["is_receipt"] is True
    assert set(front) >= {"id", "name", "ip", "up", "is_receipt", "last_error", "down_since"}
    dead = wait_until(lambda: next(p for p in requests.get(f"{a.base}/api/diag", headers=_hdr(tab), timeout=10).json()["printers"]
                                   if p["name"] == "Dead" and p["last_checked"]), timeout=15, what="dead printer probed")
    assert dead["up"] is False and dead["down_since"]
    assert "printer_down" in requests.get(f"{a.base}/api/diag", headers=_hdr(tab), timeout=10).json()["problems"]

    # The web UI session works too; /api/session carries the new fields.
    assert a.http.get(f"{a.base}/api/diag", timeout=10).status_code == 200
    s = a.session()
    assert s["store_name"] == diag["location"]["name"] and s["version"] and s["hostname"]


def test_printer_test_never_fails_over(diag, fakes) -> None:
    a, tab, ids = diag["agent"], diag["tab"], diag["ids"]
    front = fakes["Front"]

    r = requests.post(f"{a.base}/api/printers/test", headers=_hdr(tab), json={}, timeout=30)
    assert r.status_code == 200, r.text
    res = {x["name"]: x for x in r.json()["results"]}
    assert res["Front"]["ok"] is True and res["Front"]["id"] == ids["Front"]
    assert res["Dead"]["ok"] is False and res["Dead"]["error_code"] == "unreachable" and res["Dead"]["error"]
    wait_until(lambda: len(front.jobs) >= 1, timeout=10, what="test slip on Front")
    time.sleep(0.5)
    assert len(front.jobs) == 1, "the dead printer's slip must not be rerouted to Front"
    slip = front.text()
    for want in ("TEST - Quibase setup", "FRONT", "Tablet:  Counter", "If you can read this, this printer works."):
        assert want in slip, want
    assert all(b < 0x80 for b in front.jobs[0]), "the test slip is ASCII only"

    # One printer by id.
    r = requests.post(f"{a.base}/api/printers/test", headers=_hdr(tab), json={"printer_id": ids["Dead"]}, timeout=30)
    assert [(x["name"], x["error_code"]) for x in r.json()["results"]] == [("Dead", "unreachable")]
    assert len(front.jobs) == 1

    # Probe only: no paper.
    r = requests.post(f"{a.base}/api/printers/test", headers=_hdr(tab), json={"probe_only": True}, timeout=30)
    res = {x["name"]: x for x in r.json()["results"]}
    assert res["Front"]["ok"] is True and res["Dead"]["error_code"] == "unreachable"
    time.sleep(0.5)
    assert len(front.jobs) == 1, "probe_only must not print"

    r = requests.post(f"{a.base}/api/printers/test", headers=_hdr(tab), json={"printer_id": str(uuid.uuid4())}, timeout=10)
    assert r.json()["results"][0]["error_code"] == "not_configured"


def test_rename_self_shows_in_devices(diag) -> None:
    a, tab = diag["agent"], diag["tab"]
    r = requests.patch(f"{a.base}/api/device/self", headers=_hdr(tab), json={"name": "  Front counter "}, timeout=5)
    assert r.status_code == 200, r.text
    assert r.json() == {"id": tab["_id"], "name": "Front counter"}
    devs = a.http.get(f"{a.base}/api/devices", timeout=5).json()
    assert next(d for d in devs if d["id"] == tab["_id"])["name"] == "Front counter"
    for bad in ("", " ", "x" * 41):
        r = requests.patch(f"{a.base}/api/device/self", headers=_hdr(tab), json={"name": bad}, timeout=5)
        assert r.status_code == 400 and r.json()["code"] == "name_required", bad
    r = a.http.patch(f"{a.base}/api/device/self", json={"name": "Web"}, timeout=5)
    assert r.status_code == 400 and r.json()["code"] == "not_a_device"


def test_tablet_minted_code_pairs_another_tablet(diag) -> None:
    a, tab = diag["agent"], diag["tab"]
    r = requests.post(f"{a.base}/api/pairing/code", headers=_hdr(tab), timeout=5)
    assert r.status_code == 200, r.text
    r = requests.post(f"{a.base}/api/pair", json={"code": r.json()["code"], "name": "Second"}, timeout=5)
    assert r.status_code == 201, r.text
    key2 = r.json()["key"]
    assert requests.get(f"{a.base}/api/floor", headers={"X-Agent-Device-Key": key2}, timeout=5).status_code == 200
    assert requests.post(f"{a.base}/api/pairing/code", timeout=5).status_code == 401


def _free_udp_port() -> int:
    with closing(socket.socket(socket.AF_INET, socket.SOCK_DGRAM)) as s:
        s.bind(("0.0.0.0", 0))
        return s.getsockname()[1]


def _probe(port: int, nonce: str, size: int = 256, timeout: float = 1.0) -> dict | None:
    body = json.dumps({"q": "quibase-discover", "v": 1, "nonce": nonce}).encode()
    body += b" " * max(0, size - len(body))
    with closing(socket.socket(socket.AF_INET, socket.SOCK_DGRAM)) as s:
        s.settimeout(timeout)
        s.sendto(body, ("127.0.0.1", port))
        try:
            data, _ = s.recvfrom(4096)
        except socket.timeout:
            return None
    assert len(data) < len(body), "the reply must be smaller than the probe"
    return json.loads(data)


def test_udp_discovery(main_store, spawn_agent) -> None:
    port = _free_udp_port()
    a = spawn_agent(AGENT_DISCOVERY_PORT=str(port))
    nonce = secrets.token_hex(8)
    rep = wait_until(lambda: _probe(port, nonce), timeout=10, what="discovery reply")
    assert rep["svc"] == "quicc-agent" and rep["v"] == 1 and rep["nonce"] == nonce
    assert rep["port"] == a.port and rep["linked"] is False and rep["store"] == ""
    assert "link_available" in rep and rep["version"] and rep["host"]
    assert _probe(port, nonce, size=50, timeout=0.5) is None, "a short probe must be dropped"
    assert _probe(port, "not-hex-nonce!!!", timeout=0.5) is None

    token = main_store.mint()["token"]
    assert requests.post(f"{a.base}/api/setup/token", json={"token": token}, timeout=10).status_code == 200
    rep = wait_until(lambda: (r := _probe(port, secrets.token_hex(8))) and r["store"] and r,
                     timeout=20, what="discovery to report the linked store")
    assert rep["linked"] is True


# --- through main: report token + diag.report relay --------------------------


@pytest.fixture
def main_has_tablet_tokens() -> None:
    r = requests.post(f"{STORE_V1}/agent/tablet-tokens", json={}, timeout=5)
    if r.status_code == 404:
        pytest.skip("dev main has no /store/v1/agent/tablet-tokens yet (main 2.0.1)")


def _report(**over) -> dict:
    rep = {"v": 1, "id": str(uuid.uuid4()), "created_at": time.strftime("%Y-%m-%dT%H:%M:%SZ", time.gmtime()),
           "kind": "guide", "scenario": "printers", "outcome": "fixed", "fixed_by": "printer_power",
           "headline": "pytest: printer fixed", "tablet": {"name": "spoofed", "app_version": "1.3.0"},
           "steps": [], "log": ["pytest"]}
    rep.update(over)
    return rep


def test_report_token(diag, main_has_tablet_tokens) -> None:
    a, tab = diag["agent"], diag["tab"]
    r = requests.post(f"{a.base}/api/device/report-token", headers=_hdr(tab), timeout=15)
    assert r.status_code == 200, r.text
    body = r.json()
    assert body["token"] and body["main_url"].rstrip("/") == MAIN_BASE_URL
    assert set(body) == {"token", "main_url"}
    assert a.http.post(f"{a.base}/api/device/report-token", timeout=5).json()["code"] == "not_a_device"

    # The token reaches main directly (the "Pi is down" path).
    direct = requests.post(f"{STORE_V1}/tablet/diag-reports", headers={"X-Quibase-Report-Key": body["token"]},
                           json=_report(), timeout=10)
    if direct.status_code != 404:
        assert direct.status_code in (200, 202), direct.text

    # Unpairing the tablet revokes its token on main (in the background).
    assert requests.post(f"{a.base}/api/device/unpair", headers=_hdr(tab), timeout=5).status_code == 204
    if direct.status_code != 404:
        wait_until(lambda: requests.post(f"{STORE_V1}/tablet/diag-reports", headers={"X-Quibase-Report-Key": body["token"]},
                                         json=_report(), timeout=10).status_code == 401,
                   timeout=15, what="tablet token revoked after unpair")


def test_diag_report_relay(diag, main_has_tablet_tokens) -> None:
    a, tab = diag["agent"], diag["tab"]
    rep = _report()
    r = requests.post(f"{a.base}/api/diag/report", headers=_hdr(tab), json=rep, timeout=20)
    if r.status_code == 502 and r.json().get("code") == "main_unsupported":
        pytest.skip("dev main doesn't handle RPC diag.report yet")
    assert r.status_code == 202, r.text
    body = r.json()
    assert body["ok"] is True and body.get("id") == rep["id"] and body.get("stored") is True and body.get("duplicate") is False

    again = requests.post(f"{a.base}/api/diag/report", headers=_hdr(tab), json=rep, timeout=20)
    assert again.status_code == 202 and again.json().get("duplicate") is True, again.text

    # Stored with the tablet identity from the device key (not the body) + the Pi's part.
    got = diag_admin_get(rep["id"])
    assert got["via"] == "pi", got
    assert got["tablet_id"] == tab["_id"] and got["tablet_name"] == "Counter", got
    assert got["report"]["pi"]["version"] and "health" in got["report"]["pi"], got["report"].get("pi")

    big = {**_report(), "log": ["x" * 1000] * 300}
    r = requests.post(f"{a.base}/api/diag/report", headers=_hdr(tab), json=big, timeout=20)
    assert r.status_code == 413 and r.json()["code"] == "too_large"
    r = requests.post(f"{a.base}/api/diag/report", headers=_hdr(tab), data="nope", timeout=10)
    assert r.status_code == 400
    r = requests.post(f"{a.base}/api/diag/report", headers=_hdr(tab), json={"v": 1}, timeout=20)
    assert r.status_code == 400 and r.json()["code"] == "bad_report", r.text


def diag_admin_get(report_id: str) -> dict:
    """The stored report from main's admin API."""
    from conftest import ADMIN_API_KEY

    r = requests.get(f"{ADMIN_V1}/diag-reports/{report_id}", headers={"Authorization": f"Bearer {ADMIN_API_KEY}"}, timeout=10)
    assert r.status_code == 200, r.text
    return r.json()
