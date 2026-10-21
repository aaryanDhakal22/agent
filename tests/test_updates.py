"""Pi updates + printers on the network (agent 2.0.6) on throwaway agents.

A dev agent can't update itself (not_supported); AGENT_UPDATE_ENABLED=1 +
AGENT_UPDATE_DIR turn the request/status file handshake on so the test can
play the Pi's root updater: it reads update.request and writes update.status.
UTILITY_URL points the latest-version check at a local fake manifest.
printer.add goes through main — skipped while the dev main predates it.
"""

from __future__ import annotations

import http.server
import json
import os
import stat
import threading
import time
from pathlib import Path

import pytest
import requests

from conftest import MAIN_BASE_URL, FakePrinter, manager_session, provision, wait_until, STORE_V1

NEW_ROUTES = {
    ("GET", "/api/update/status"): "device",
    ("POST", "/api/update"): "session",
    ("POST", "/api/update/rollback"): "session",
    ("GET", "/api/printers/found"): "device",
    ("POST", "/api/printers/add"): "device",
}
STATUS_KEYS = {"current_version", "current_commit", "env", "latest_version", "update_available", "state",
               "pending", "last", "can_update_now", "blocked_reason"}

MANIFEST = """# quicc-manifest v1
# env=staging
# agent_version=9.9.9
{sha}  agent-linux-arm64
"""


@pytest.fixture
def utility():
    """A fake utility host serving /staging/manifest.txt."""
    body = MANIFEST.format(sha="ab" * 32).encode()

    class H(http.server.BaseHTTPRequestHandler):
        def do_GET(self):  # noqa: N802
            if self.path != "/staging/manifest.txt":
                self.send_error(404)
                return
            self.send_response(200)
            self.send_header("Content-Length", str(len(body)))
            self.end_headers()
            self.wfile.write(body)

        def log_message(self, *a):
            pass

    srv = http.server.ThreadingHTTPServer(("127.0.0.1", 0), H)
    threading.Thread(target=srv.serve_forever, daemon=True).start()
    yield f"http://127.0.0.1:{srv.server_address[1]}"
    srv.shutdown()


def test_new_routes_listed(blank_agent):
    routes = requests.get(f"{blank_agent.base}/__routes", timeout=5).json()["routes"]
    got = {(r["method"], r["path"]): r["auth"] for r in routes}
    for k, auth in NEW_ROUTES.items():
        assert got.get(k) == auth, (k, got.get(k))
    # Device / session gated, not LAN-open.
    assert requests.get(f"{blank_agent.base}/api/update/status", timeout=5).status_code == 401
    assert requests.get(f"{blank_agent.base}/api/printers/found", timeout=5).status_code == 401
    assert requests.post(f"{blank_agent.base}/api/update", json={}, timeout=5).status_code == 401


def test_dev_agent_not_supported(main_store, spawn_agent):
    agent = spawn_agent()
    provision(agent, main_store)
    st = agent.http.get(f"{agent.base}/api/update/status", timeout=5).json()
    assert STATUS_KEYS <= set(st), st
    assert st["state"] == "idle" and st["pending"] is None and st["last"] is None
    assert st["can_update_now"] is False and st["blocked_reason"] == "not_supported"
    assert st["env"] == "dev" and st["current_version"]
    for path in ("/api/update", "/api/update/rollback"):
        r = agent.http.post(f"{agent.base}{path}", json={}, timeout=5)
        assert r.status_code == 501 and r.json()["code"] == "not_supported", r.text
    assert not list(agent.dir.glob("update.request"))


def test_update_request_and_status_files(main_store, spawn_agent, tmp_path, utility):
    upd = tmp_path / "upd"
    upd.mkdir()
    agent = spawn_agent(AGENT_UPDATE_ENABLED="1", AGENT_UPDATE_DIR=str(upd), UTILITY_URL=utility)
    provision(agent, main_store)

    # Latest version from the (fake) manifest; the open batch is empty → allowed.
    def status():
        return agent.http.get(f"{agent.base}/api/update/status", timeout=5).json()
    wait_until(lambda: status()["latest_version"] == "9.9.9", timeout=15, what="manifest check")
    st = status()
    assert st["update_available"] is True and st["can_update_now"] is True and st["blocked_reason"] == ""

    r = agent.http.post(f"{agent.base}/api/update", json={}, timeout=5)
    assert r.status_code == 202, r.text
    uid = r.json()["id"]
    assert r.json()["state"] == "requested"
    req_file = upd / "update.request"
    req = json.loads(req_file.read_text())
    assert req["id"] == uid and req["action"] == "update" and req["requested_by"] == "pi-web" and req["at"]
    assert stat.S_IMODE(req_file.stat().st_mode) == 0o644
    assert not [p for p in upd.iterdir() if p.name.startswith(".update.request")], "temp file left behind"

    st = status()
    assert st["state"] == "requested" and st["pending"]["id"] == uid and st["pending"]["force"] is False
    assert st["blocked_reason"] == "update_in_progress"
    r = agent.http.post(f"{agent.base}/api/update/rollback", json={}, timeout=5)
    assert r.status_code == 409 and r.json()["code"] == "update_in_progress", r.text

    # Play the root updater: consume the request, report running, then ok.
    req_file.unlink()
    now = time.strftime("%Y-%m-%dT%H:%M:%SZ", time.gmtime())
    base = {"id": uid, "action": "update", "requested_by": "pi-web", "from_version": st["current_version"],
            "to_version": "9.9.9", "from_sha": "", "to_sha": "ab" * 32, "error": "", "started_at": now}
    (upd / "update.status").write_text(json.dumps({**base, "state": "running", "finished_at": ""}))
    wait_until(lambda: status()["state"] == "running", timeout=20, what="running state")
    (upd / "update.status").write_text(json.dumps({**base, "state": "ok", "finished_at": now}))
    wait_until(lambda: status()["state"] == "ok", timeout=20, what="ok state")
    st = status()
    assert st["last"]["id"] == uid and st["last"]["to_version"] == "9.9.9" and st["pending"] is None
    assert st["can_update_now"] is True

    # A restarted agent still knows the result (and doesn't lose it).
    agent.restart()
    st = status()
    assert st["last"]["id"] == uid and st["state"] == "ok"


def test_printers_mac_and_found(admin, main_store, spawn_agent, fakes):
    mgr = manager_session(admin, main_store)
    fakes["Front"] = FakePrinter()
    r = mgr.post(f"{STORE_V1}/me/printers", json={"name": "Front", "ip": fakes["Front"].addr}, timeout=5)
    assert r.status_code == 201, r.text
    agent = spawn_agent()
    provision(agent, main_store)
    wait_until(lambda: len(agent.http.get(f"{agent.base}/api/printers", timeout=5).json()) == 1,
               timeout=20, what="roster")
    p = agent.http.get(f"{agent.base}/api/printers", timeout=5).json()[0]
    assert "mac" in p and p["mac"] == ""  # a loopback fake printer has no ARP entry

    f = agent.http.get(f"{agent.base}/api/printers/found", timeout=5).json()
    assert set(f) == {"scanned_at", "scanning", "printers"} and isinstance(f["printers"], list)
    f = agent.http.get(f"{agent.base}/api/printers/found?refresh=1", timeout=30).json()
    assert f["scanned_at"] and isinstance(f["printers"], list)
    for x in f["printers"]:
        assert {"ip", "mac", "known", "printer_name"} <= set(x)


def test_printer_add(admin, main_store, spawn_agent):
    agent = spawn_agent()
    provision(agent, main_store)
    bad = agent.http.post(f"{agent.base}/api/printers/add", json={"name": "Grill", "ip": "not-an-ip"}, timeout=5)
    assert bad.status_code == 400 and bad.json()["code"] == "bad_request"
    bad = agent.http.post(f"{agent.base}/api/printers/add", json={"name": "", "ip": "192.0.2.10"}, timeout=5)
    assert bad.status_code == 400

    r = agent.http.post(f"{agent.base}/api/printers/add", json={"name": "Grill", "ip": "192.0.2.10", "mac": "00:11:62:AA:BB:CC"}, timeout=20)
    if r.status_code == 503:
        pytest.skip("dev main doesn't answer printer.add yet")
    assert r.status_code == 201, r.text
    assert r.json()["name"] == "Grill" and r.json()["ip"] == "192.0.2.10" and r.json()["id"]
    mgr = manager_session(admin, main_store)
    printers = mgr.get(f"{STORE_V1}/me/printers", timeout=5).json()
    assert any(p["name"] == "Grill" and p["ip"] == "192.0.2.10" for p in printers), printers
    # main republishes store_config → the agent's roster has it.
    wait_until(lambda: any(p["name"] == "Grill" for p in agent.http.get(f"{agent.base}/api/printers", timeout=5).json()),
               timeout=20, what="roster with Grill")
    dup = agent.http.post(f"{agent.base}/api/printers/add", json={"name": "Grill", "ip": "192.0.2.11"}, timeout=20)
    assert dup.status_code == 409 and dup.json()["code"] == "name_taken", dup.text
