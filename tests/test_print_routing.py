"""Routing & emergency reroute (the owner, 2026-11-12; contract/agent.md §2.3c).

A throwaway agent with FOUR fake network printers (Front = receipt, Pizza,
Wings, Spare). The store's routing setting picks where a ticket goes when its
printer can't print: one emergency printer for all, or a per-printer ordered
list; the automatic failover (receipt printer, then the rest) is the last
resort. A rerouted ticket carries two reverse (white on black) double-size
bands, "REROUTED FROM WINGS" then "REDIRIGIDO DE WINGS".
"""

from __future__ import annotations

import uuid

import pytest
import requests

from gatemark import intentional_sleep
from conftest import ADMIN_V1, STORE_V1, FakePrinter, wait_until
from test_kitchen_printing import _accept, _item, _snap, _wait_jobs

NAMES = ("Front", "Pizza", "Wings", "Spare")
def band(text: str, cols: int = 48) -> bytes:
    """GS B 1 (reverse) + the band text centred in the stand-in printer's double-size line (24 / 21 / 16
    for 48 / 42 / 32 columns, contract §2.3c "Paper width") + GS B 0: what the receipt code writes."""
    width = {48: 24, 42: 21, 32: 16}[cols]
    left = (width - len(text)) // 2
    return b"\x1dB\x01" + (" " * left + text + " " * (width - len(text) - left)).encode() + b"\x1dB\x00"


# The four test printers are 80 mm (no print_columns set = 48).
BAND_EN = band("REROUTED FROM WINGS")
BAND_ES = band("REDIRIGIDO DE WINGS")


@pytest.fixture
def routed(admin, main_store, spawn_agent, fakes):
    user, pw = f"mgr{uuid.uuid4().hex[:6]}", "pw-routing"
    r = admin.patch(f"{ADMIN_V1}/tenants/{main_store.tenant_id}/credentials",
                    json={"username": user, "password": pw}, timeout=5)
    assert r.status_code == 204, r.text
    mgr = requests.Session()
    assert mgr.post(f"{STORE_V1}/session", json={"username": user, "password": pw}, timeout=5).status_code == 200
    ids = {}
    for name in NAMES:
        fakes[name] = FakePrinter()
        # main makes the first printer the receipt printer (Front).
        r = mgr.post(f"{STORE_V1}/me/printers", json={"name": name, "ip": fakes[name].addr}, timeout=5)
        assert r.status_code == 201, r.text
        ids[name] = r.json()["id"]

    agent = spawn_agent()
    token = main_store.mint()["token"]
    assert requests.post(f"{agent.base}/api/setup/token", json={"token": token}, timeout=10).status_code == 200
    assert requests.post(f"{agent.base}/api/setup/password", json={"password": "pw", "confirm": "pw"}, timeout=10).status_code == 200
    assert agent.http.post(f"{agent.base}/api/login", json={"password": "pw"}, timeout=10).status_code == 200
    assert agent.http.put(f"{agent.base}/api/settings/deliveries", json={"auto_assign": False}, timeout=10).status_code == 200
    wait_until(lambda: len(agent.http.get(f"{agent.base}/api/settings/routing", timeout=5).json()["printers"]) == 4,
               timeout=20, what="agent to apply the 4-printer roster")
    return {"agent": agent, "ids": ids, "main_store": main_store}


def _put(agent, body: dict) -> requests.Response:
    return agent.http.put(f"{agent.base}/api/settings/routing", json=body, timeout=10)


def _order(routed) -> dict:
    ids = routed["ids"]
    return _snap(routed["main_store"], [_item("Pepperoni Pizza", ids["Pizza"]), _item("Hot Wings", ids["Wings"])])


def test_routing_setting_round_trip_and_validation(routed) -> None:
    a, ids = routed["agent"], routed["ids"]
    got = a.http.get(f"{a.base}/api/settings/routing", timeout=5).json()
    assert got["routing"]["mode"] == "none" and got["modes"] == ["none", "one", "per_printer"]
    by_id = {p["id"]: p for p in got["printers"]}
    assert by_id[ids["Front"]]["is_receipt"] is True
    assert all(p["status"] in ("up", "down", "unknown") for p in got["printers"])
    # Mode none: the automatic order, the receipt printer first.
    first = by_id[ids["Wings"]]["fallbacks"][0]
    assert first["id"] == ids["Front"] and first["by"] == "auto", first

    for body, code in [
        ({"mode": "sometimes"}, "bad_mode"),
        ({"mode": "one"}, "no_emergency_printer"),
        ({"mode": "one", "emergency_printer_id": "nope"}, "unknown_printer"),
        ({"mode": "per_printer", "targets": {ids["Wings"]: ["nope"]}}, "unknown_printer"),
    ]:
        r = _put(a, body)
        assert r.status_code == 400 and r.json()["code"] == code, (body, r.text)
    assert a.http.get(f"{a.base}/api/settings/routing", timeout=5).json()["routing"]["mode"] == "none"

    # Self and duplicates are dropped; the list keeps its order.
    r = _put(a, {"mode": "per_printer", "targets": {ids["Wings"]: [ids["Spare"], ids["Wings"], ids["Spare"], ids["Pizza"]]}})
    assert r.status_code == 200, r.text
    saved = r.json()
    assert saved["routing"]["targets"] == {ids["Wings"]: [ids["Spare"], ids["Pizza"]]}
    wings = next(p for p in saved["printers"] if p["id"] == ids["Wings"])
    # The rule's targets come first (both up, or neither probed yet).
    assert [f["by"] for f in wings["fallbacks"]] == ["rule", "rule", "auto"], wings
    # Every save lands in the settings history.
    changes = a.http.get(f"{a.base}/api/settings/changes", timeout=5).json()["changes"]
    assert any(c["section"] == "routing" for c in changes), changes


def test_one_emergency_printer_takes_the_downed_station(routed, fakes) -> None:
    a, ids = routed["agent"], routed["ids"]
    assert _put(a, {"mode": "one", "emergency_printer_id": ids["Spare"]}).status_code == 200
    fakes["Wings"].stop()
    r = _accept(a, _order(routed))
    assert r.status_code == 200, r.text + a.log_tail()
    _wait_jobs(fakes["Spare"], 1)
    _wait_jobs(fakes["Front"], 1)
    spare = fakes["Spare"].jobs[0]
    assert BAND_EN in spare and BAND_ES in spare, spare
    assert spare.index(BAND_EN) < spare.index(BAND_ES), "English first, then Spanish"
    assert b"1  HOT WINGS" in spare
    intentional_sleep(1.0, "absence: the receipt printer gets only the receipt")
    front = fakes["Front"].text()
    assert len(fakes["Front"].jobs) == 1 and "Total:" in front and "REROUTED" not in front


def test_per_printer_target_down_goes_to_the_next_rule(routed, fakes) -> None:
    a, ids = routed["agent"], routed["ids"]
    assert _put(a, {"mode": "per_printer", "targets": {ids["Wings"]: [ids["Spare"], ids["Pizza"]]}}).status_code == 200
    fakes["Wings"].stop()
    fakes["Spare"].stop()
    r = _accept(a, _order(routed))
    assert r.status_code == 200, r.text + a.log_tail()
    _wait_jobs(fakes["Pizza"], 2)
    rerouted = [j for j in fakes["Pizza"].jobs if BAND_EN in j]
    assert len(rerouted) == 1 and BAND_ES in rerouted[0] and b"1  HOT WINGS" in rerouted[0]
    intentional_sleep(1.0, "absence: the receipt printer gets only the receipt")
    assert len(fakes["Front"].jobs) == 1 and "REROUTED" not in fakes["Front"].text()


def test_every_stand_in_down_falls_back_to_automatic(routed, fakes) -> None:
    a, ids = routed["agent"], routed["ids"]
    assert _put(a, {"mode": "one", "emergency_printer_id": ids["Spare"]}).status_code == 200
    fakes["Wings"].stop()
    fakes["Spare"].stop()
    r = _accept(a, _order(routed))
    assert r.status_code == 200, r.text + a.log_tail()
    # The automatic order: the receipt printer takes the Wings ticket (and its receipt).
    _wait_jobs(fakes["Front"], 2)
    assert any(BAND_EN in j and BAND_ES in j for j in fakes["Front"].jobs)
