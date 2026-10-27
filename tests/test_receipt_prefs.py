"""Receipt settings (agent 2.0.2) on a throwaway agent: GET/PUT
/api/settings/receipt with a paired tablet's key, PRINT SAMPLE on FAKE
printers (only the chosen printer, never a fallback), and the settings riding
agent.state to main so a replacement Pi gets them back on link."""

from __future__ import annotations

import secrets
import time

import pytest
import requests

from conftest import ADMIN_V1, STORE_V1, FakePrinter, manager_session, wait_until

CUT = b"\x1d\x56\x42\x03"

DEFAULTS = {
    "kitchen_item_size": "large",
    "kitchen_number_size": "xlarge",
    "footer_text": None,
    "footer_size": "xlarge",
    "copies": 1,
    "show_store_address": True,
    "show_phone": True,
    "extra_paper": True,
    "driver_report_size": "normal",
    "spacing": {"item_mod": 0, "mod_mod": 0, "item_item": 0},
}


def _pair(agent, name: str = "Counter") -> dict:
    code = agent.http.post(f"{agent.base}/api/pairing/code", timeout=5).json()["code"]
    r = requests.post(f"{agent.base}/api/pair", json={"code": code, "name": name}, timeout=5)
    assert r.status_code == 201, r.text
    return {"X-Agent-Device-Key": r.json()["key"]}


@pytest.fixture
def store(admin, main_store, spawn_agent, fakes):
    """A linked throwaway agent with Front (receipt) + Pizza fake printers and
    one paired tablet."""
    mgr = manager_session(admin, main_store)
    ids = {}
    for name in ("Front", "Pizza"):
        fakes[name] = FakePrinter()
        r = mgr.post(f"{STORE_V1}/me/printers", json={"name": name, "ip": fakes[name].addr}, timeout=5)
        assert r.status_code == 201, r.text
        ids[name] = r.json()["id"]
    agent = spawn_agent()
    token = main_store.mint()["token"]
    assert requests.post(f"{agent.base}/api/setup/token", json={"token": token}, timeout=10).status_code == 200
    assert agent.http.post(f"{agent.base}/api/setup/password", json={"password": "pw", "confirm": "pw"}, timeout=10).status_code == 200
    wait_until(lambda: len(agent.http.get(f"{agent.base}/api/printers", timeout=5).json()) == 2,
               timeout=20, what="agent to apply the 2-printer roster")
    return {"agent": agent, "tab": _pair(agent), "ids": ids, "token": token}


def test_receipt_routes_need_a_device(store) -> None:
    a = store["agent"]
    assert requests.get(f"{a.base}/api/settings/receipt", timeout=5).status_code == 401
    assert requests.put(f"{a.base}/api/settings/receipt", json=DEFAULTS, timeout=5).status_code == 401
    assert requests.post(f"{a.base}/api/printers/sample", json={}, timeout=5).status_code == 401
    # The web-UI session works too.
    assert a.http.get(f"{a.base}/api/settings/receipt", timeout=5).status_code == 200


def test_get_defaults_put_and_reject(store) -> None:
    a, tab = store["agent"], store["tab"]
    r = requests.get(f"{a.base}/api/settings/receipt", headers=tab, timeout=5)
    assert r.status_code == 200, r.text
    body = r.json()
    assert body["prefs"] == DEFAULTS and body["defaults"] == DEFAULTS
    assert body["footer_effective"] == "ONLINE ORDER" and body["footer_store_default"] == "ONLINE ORDER"
    assert body["location_footer"] == ""

    want = {**DEFAULTS, "kitchen_item_size": "xlarge", "kitchen_number_size": "huge", "footer_text": "THANK YOU",
            "footer_size": "large", "copies": 2, "show_phone": False}
    r = requests.put(f"{a.base}/api/settings/receipt", headers=tab, json=want, timeout=5)
    assert r.status_code == 200, r.text
    assert r.json()["prefs"] == want and r.json()["footer_effective"] == "THANK YOU"
    assert r.json()["footer_store_default"] == "ONLINE ORDER"
    got = requests.get(f"{a.base}/api/settings/receipt", headers=tab, timeout=5).json()
    assert got["prefs"] == want and got["defaults"] == DEFAULTS

    for bad, reason in (
        ({**want, "copies": 3}, "copies must be 1 or 2"),
        ({**want, "kitchen_item_size": "huge"}, "kitchen_item_size"),
        ({**want, "footer_size": "tiny"}, "footer_size"),
        ({**want, "footer_text": "X" * 33}, "at most 32"),
        ({**want, "footer_text": "MERCI CAFÉ"}, "footer_text"),
    ):
        r = requests.put(f"{a.base}/api/settings/receipt", headers=tab, json=bad, timeout=5)
        assert r.status_code == 400 and r.json()["code"] == "bad_request", (bad, r.text)
        assert reason in r.json()["error"], r.json()
    # A rejected PUT changes nothing.
    assert requests.get(f"{a.base}/api/settings/receipt", headers=tab, timeout=5).json()["prefs"] == want

    # Reset to defaults (the tablet sends the defaults object back).
    r = requests.put(f"{a.base}/api/settings/receipt", headers=tab, json=DEFAULTS, timeout=5)
    assert r.status_code == 200 and r.json()["prefs"] == DEFAULTS


def test_print_sample(store, fakes) -> None:
    a, tab, ids = store["agent"], store["tab"], store["ids"]
    front, pizza = fakes["Front"], fakes["Pizza"]
    prefs = {**DEFAULTS, "footer_text": "THANK YOU", "copies": 2}
    assert requests.put(f"{a.base}/api/settings/receipt", headers=tab, json=prefs, timeout=5).status_code == 200

    # Default: the receipt printer.
    r = requests.post(f"{a.base}/api/printers/sample", headers=tab, json={}, timeout=30)
    assert r.status_code == 200, r.text
    assert r.json()["results"] == [{"id": ids["Front"], "name": "Front", "ok": True}]
    wait_until(lambda: len(front.jobs) >= 1, timeout=10, what="sample on Front")
    time.sleep(0.5)
    assert len(front.jobs) == 1 and not pizza.jobs, "the sample prints only on the chosen printer"
    job = front.jobs[0]
    assert job.count(b"CUSTOMER, SAMPLE") == 3, "two receipt copies + the kitchen ticket"
    assert job.count(b"THANK YOU") == 2
    assert job.count(CUT) == 5, "2 copies x (cut, feed, cut) + the kitchen ticket's cut"
    for want in (b"#1234", b"Note: extra ranch please", b"SAMPLE - not a real order", b"PAID - CARD"):
        assert want in job, want

    # A chosen printer; the settings in force are the saved ones.
    prefs = {**DEFAULTS, "footer_text": "", "extra_paper": False}
    assert requests.put(f"{a.base}/api/settings/receipt", headers=tab, json=prefs, timeout=5).status_code == 200
    r = requests.post(f"{a.base}/api/printers/sample", headers=tab, json={"printer_id": ids["Pizza"]}, timeout=30)
    assert r.status_code == 200 and r.json()["results"][0]["ok"] is True, r.text
    wait_until(lambda: len(pizza.jobs) >= 1, timeout=10, what="sample on Pizza")
    job = pizza.jobs[0]
    assert job.count(CUT) == 2 and b"ONLINE ORDER" not in job and b"THANK YOU" not in job
    assert len(front.jobs) == 1

    r = requests.post(f"{a.base}/api/printers/sample", headers=tab, json={"printer_id": "nope"}, timeout=10)
    assert r.status_code == 404 and r.json()["code"] == "not_configured"


def test_receipt_prefs_back_up_and_restore_on_link(admin, main_store, store, spawn_agent, fakes) -> None:
    """A change rides agent.state to main; a blank Pi linked with the manager
    login gets it back (and prints with it)."""
    a, tab = store["agent"], store["tab"]
    want = {**DEFAULTS, "kitchen_item_size": "normal", "footer_text": "SEE YOU SOON", "show_store_address": False}
    assert requests.put(f"{a.base}/api/settings/receipt", headers=tab, json=want, timeout=5).status_code == 200

    def backed_up():
        r = requests.get(f"{STORE_V1}/agent/restore", headers={"X-STORE-API-KEY": store["token"]}, timeout=5)
        state = (r.json() or {}).get("state") or {} if r.status_code == 200 else {}
        return state.get("receipt_prefs") == want

    wait_until(backed_up, timeout=20, what="receipt_prefs in the agent.state main keeps")

    user, pw = f"mgr{secrets.token_hex(3)}", "pw-link"
    r = admin.patch(f"{ADMIN_V1}/tenants/{main_store.tenant_id}/credentials", json={"username": user, "password": pw}, timeout=5)
    assert r.status_code == 204, r.text
    b = spawn_agent()
    r = requests.post(f"{b.base}/api/setup/link", json={"username": user, "password": pw, "tablet_name": "Swap"}, timeout=60)
    assert r.status_code == 200, r.text
    key = {"X-Agent-Device-Key": r.json()["device_key"]}
    got = requests.get(f"{b.base}/api/settings/receipt", headers=key, timeout=5).json()
    assert got["prefs"] == want, got

    # Printing on the new Pi uses them right away.
    wait_until(lambda: len(requests.get(f"{b.base}/api/printers", headers=key, timeout=5).json()) == 2,
               timeout=20, what="new agent to apply the roster")
    front = fakes["Front"]
    before = len(front.jobs)
    r = requests.post(f"{b.base}/api/printers/sample", headers=key, json={}, timeout=30)
    assert r.status_code == 200 and r.json()["results"][0]["ok"] is True, r.text
    wait_until(lambda: len(front.jobs) > before, timeout=10, what="sample from the new Pi")
    assert b"SEE YOU SOON" in front.jobs[-1]

