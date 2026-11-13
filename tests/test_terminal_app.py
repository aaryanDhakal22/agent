"""Terminal self-update, the Pi's side (contract §2.10w).

A release uploaded to main for the terminal's app reaches the Pi's cache
(sha256-checked) and is served to terminal keys only on
/api/terminal/app/latest + /apk; install_window says whether this is a
quiet moment (the open batch has no orders); Close batch announces
terminal.app.install. The agent is spawned with its own app name
(AGENT_TERMINAL_APP) so the test never touches dev main's real terminal
releases, and the release is deleted again.
"""

from __future__ import annotations

import hashlib
import json
import secrets
import threading
import time
import uuid

import pytest
import requests

from conftest import ADMIN_V1, wait_until
from test_terminal import make_rig, pizza_line


@pytest.fixture
def app_rig(admin, main_store, spawn_agent, fakes):
    app = f"t{secrets.token_hex(5)}"
    gen = make_rig(admin, main_store, spawn_agent, fakes, AGENT_TERMINAL_APP=app)
    rig = next(gen)
    rig["app"] = app
    yield rig
    try:
        next(gen)
    except StopIteration:
        pass


def test_terminal_app_cache_and_install_moment(app_rig, admin):
    agent, term, app = app_rig["agent"], app_rig["term"], app_rig["app"]
    base = agent.base
    assert term.get(f"{base}/api/terminal/app/latest", timeout=5).status_code == 204
    assert term.get(f"{base}/api/terminal/app/apk", timeout=5).json()["code"] == "not_found"
    apk = b"PK\x03\x04" + secrets.token_bytes(2048)
    code = int(time.time())  # version codes are epoch seconds (the console's too)
    r = admin.post(f"{ADMIN_V1}/app-releases", params={"app": app, "version_code": code, "version_name": "0.0.test"},
                   data=apk, timeout=30)
    assert r.status_code == 201, r.text
    try:
        agent.restart()  # checked at start (and every reconnect / 5 min)
        wait_until(lambda: term.get(f"{base}/api/terminal/app/latest", timeout=5).status_code == 200, timeout=30, what="terminal APK cached")
        latest = term.get(f"{base}/api/terminal/app/latest", timeout=5).json()
        assert latest["version_code"] == code and latest["sha256"] == hashlib.sha256(apk).hexdigest() and latest["size"] == len(apk)
        assert latest["install_window"] is True  # nothing in the open batch yet
        got = term.get(f"{base}/api/terminal/app/apk", timeout=10)
        assert got.status_code == 200 and got.content == apk and got.headers["X-Sha256"] == latest["sha256"]
        part = term.get(f"{base}/api/terminal/app/apk", headers={"Range": "bytes=0-3"}, timeout=10)
        assert part.status_code == 206 and part.content == b"PK\x03\x04"
        # Not for a tablet, nor without a key; Quibase's own cache stays empty.
        code6 = agent.http.post(f"{base}/api/pairing/code", json={"kind": "tablet"}, timeout=5).json()["code"]
        tab = requests.post(f"{base}/api/pair", json={"code": code6, "name": "Tab", "kind": "tablet"}, timeout=5).json()
        tablet = {"X-Agent-Device-Key": tab["key"]}
        assert requests.get(f"{base}/api/terminal/app/latest", headers=tablet, timeout=5).json()["code"] == "terminal_required"
        assert requests.get(f"{base}/api/terminal/app/apk", timeout=5).status_code == 401
        assert requests.get(f"{base}/api/app/latest", headers=tablet, timeout=5).status_code == 204

        # An order: no longer a quiet moment.
        assert term.post(f"{base}/api/terminal/login", json={"pin": "1234"}, timeout=5).status_code == 200
        q = term.post(f"{base}/api/terminal/quote", json={"cart": {"lines": [pizza_line()]}}, timeout=5).json()
        r = term.post(f"{base}/api/terminal/orders", json={"client_key": f"k-{uuid.uuid4()}", "type": "counter", "cart": {"lines": [pizza_line()]},
                                                         "payments": [{"tender": "cash", "amount_cents": q["quote"]["total_cents"]}]}, timeout=10)
        assert r.status_code == 201, r.text
        assert term.get(f"{base}/api/terminal/app/latest", timeout=5).json()["install_window"] is False

        # Close batch: terminal.app.install, and a quiet moment again.
        seen: list[dict] = []
        stop = threading.Event()

        def listen():
            with term.get(f"{base}/api/events", stream=True, timeout=(5, 60)) as s:
                typ = ""
                for line in s.iter_lines(decode_unicode=True):
                    if stop.is_set():
                        return
                    if line.startswith("event:"):
                        typ = line.split(":", 1)[1].strip()
                    elif line.startswith("data:") and typ == "terminal.app.install":
                        seen.append(json.loads(line.split(":", 1)[1]))

        threading.Thread(target=listen, daemon=True).start()
        time.sleep(0.5)  # the stream is open (a missed frame would only fail this test)
        assert agent.http.post(f"{base}/api/batch/close", json={"force": True}, timeout=30).status_code == 200
        wait_until(lambda: seen, timeout=30, what="terminal.app.install after Close batch")
        assert seen[0]["version_code"] == code and seen[0]["sha256"] == latest["sha256"] and seen[0]["reason"] == "batch_closed"
        assert term.get(f"{base}/api/terminal/app/latest", timeout=5).json()["install_window"] is True
        stop.set()
    finally:
        admin.delete(f"{ADMIN_V1}/app-releases/{code}", params={"app": app}, timeout=5)


def test_update_now_from_main(app_rig, admin):
    """Update from home (contract 2.10w "From home", 2.5.3): a terminal reports its update state (it reaches
    /api/diag's terminals[] = agent.health's); main's terminals.update_now gets the newest release cached
    first (none cached before here), announces terminal.app.install {reason: update_now} and answers per
    terminal."""
    agent, term, fm, app = app_rig["agent"], app_rig["term"], app_rig["fm"], app_rig["app"]
    base = agent.base
    assert term.get(f"{base}/api/terminal/app/latest", timeout=5).status_code == 204
    r = term.post(f"{base}/api/terminal/app/state", json={"version_name": "0.0.1", "version_code": 1, "state": "up_to_date",
                                                         "android": "11 (API 30)", "permissions": {"install_packages": True}}, timeout=5)
    assert r.status_code == 204, r.text
    apk = b"PK\x03\x04" + secrets.token_bytes(2048)
    code = int(time.time())
    r = admin.post(f"{ADMIN_V1}/app-releases", params={"app": app, "version_code": code, "version_name": "0.0.now"},
                   data=apk, timeout=30)
    assert r.status_code == 201, r.text
    try:
        seen: list[dict] = []

        def listen():
            with term.get(f"{base}/api/events", stream=True, timeout=(5, 60)) as s:
                typ = ""
                for line in s.iter_lines(decode_unicode=True):
                    if line.startswith("event:"):
                        typ = line.split(":", 1)[1].strip()
                    elif line.startswith("data:") and typ == "terminal.app.install":
                        seen.append(json.loads(line.split(":", 1)[1]))
                        return

        threading.Thread(target=listen, daemon=True).start()

        def connected():
            terms = agent.http.get(f"{base}/api/diag", timeout=5).json()["terminals"]
            return terms if terms and terms[0]["connected"] else None
        terms = wait_until(connected, timeout=10, what="the terminal on /api/events")
        assert terms[0]["update"]["state"] == "up_to_date" and terms[0]["update"]["android"] == "11 (API 30)", terms

        fm.rpc["terminals.update_now.reply"] = lambda payload: (200, {})
        fm.command("terminals.update_now", {"requested_by": "pytest"})
        reply = wait_until(lambda: next((e for e in fm.rpc_seen if e["type"] == "terminals.update_now.reply"), None),
                           timeout=30, what="the update_now reply")["payload"]
        assert reply["ok"] is True and reply["release"]["version_code"] == code, reply
        assert len(reply["terminals"]) == 1 and reply["terminals"][0]["told"] is True, reply
        wait_until(lambda: seen, timeout=10, what="terminal.app.install {reason: update_now}")
        assert seen[0]["reason"] == "update_now" and seen[0]["version_code"] == code
        assert term.get(f"{base}/api/terminal/app/latest", timeout=5).json()["version_code"] == code
    finally:
        admin.delete(f"{ADMIN_V1}/app-releases/{code}", params={"app": app}, timeout=5)
