"""Print width per printer (contract/agent.md §2.3c "Paper width", the owner 2026-11-12).

The Pi's Printers page runs the width test on one printer: it prints a letter ruler (the terminal's), staff
pick the last letter on its first line, the Pi saves the printer's width in main (RPC printer.width), main
sends the store_config back, and every slip on that printer is laid out for it: a ticket rerouted there
from an 80 mm station too.
"""

from __future__ import annotations

import uuid

import requests

from conftest import ADMIN_V1, STORE_V1, FakePrinter, wait_until
from test_kitchen_printing import _accept, _item, _snap, _wait_jobs
from test_print_routing import band

RULER = b"-" * 30 + b"ABCDEFGHIJKLMNOPQRSTUVWXYZ23456789"


def _rig(admin, main_store, spawn_agent, fakes) -> dict:
    user, pw = f"mgr{uuid.uuid4().hex[:6]}", "pw-width"
    r = admin.patch(f"{ADMIN_V1}/tenants/{main_store.tenant_id}/credentials",
                    json={"username": user, "password": pw}, timeout=5)
    assert r.status_code == 204, r.text
    mgr = requests.Session()
    assert mgr.post(f"{STORE_V1}/session", json={"username": user, "password": pw}, timeout=5).status_code == 200
    ids = {}
    for name in ("Front", "Wings", "Online"):
        fakes[name] = FakePrinter()
        r = mgr.post(f"{STORE_V1}/me/printers", json={"name": name, "ip": fakes[name].addr}, timeout=5)
        assert r.status_code == 201, r.text
        assert r.json()["print_columns"] == 48, r.text  # every printer starts at 80 mm
        ids[name] = r.json()["id"]
    agent = spawn_agent()
    token = main_store.mint()["token"]
    assert requests.post(f"{agent.base}/api/setup/token", json={"token": token}, timeout=10).status_code == 200
    assert requests.post(f"{agent.base}/api/setup/password", json={"password": "pw", "confirm": "pw"}, timeout=10).status_code == 200
    assert agent.http.post(f"{agent.base}/api/login", json={"password": "pw"}, timeout=10).status_code == 200
    assert agent.http.put(f"{agent.base}/api/settings/deliveries", json={"auto_assign": False}, timeout=10).status_code == 200
    wait_until(lambda: len(_diag_printers(agent)) == 3, timeout=20, what="agent to apply the 3-printer roster")
    return {"agent": agent, "ids": ids, "mgr": mgr, "main_store": main_store}


def _diag_printers(agent) -> dict:
    return {p["id"]: p for p in agent.http.get(f"{agent.base}/api/diag", timeout=10).json().get("printers") or []}


def _width(agent, printer_id: str, **body) -> requests.Response:
    return agent.http.post(f"{agent.base}/api/printers/width", json={"printer_id": printer_id, **body}, timeout=20)


def test_width_test_saves_the_width_and_slips_follow_it(admin, main_store, spawn_agent, fakes) -> None:
    rig = _rig(admin, main_store, spawn_agent, fakes)
    a, ids, mgr = rig["agent"], rig["ids"], rig["mgr"]
    online = ids["Online"]
    assert _diag_printers(a)[online]["print_columns"] == 48

    # The ruler prints on that printer only (never a stand-in).
    r = a.http.post(f"{a.base}/api/printers/width-test", json={"printer_id": online}, timeout=20)
    assert r.status_code == 200 and r.json()["results"][0]["ok"], r.text
    _wait_jobs(fakes["Online"], 1)
    assert RULER in fakes["Online"].jobs[0] and b"Which letter is LAST" in fakes["Online"].jobs[0]
    assert not fakes["Front"].jobs and not fakes["Wings"].jobs

    # Staff read L (42 characters): saved in main, back on the Pi through store_config.
    for body, code in [({"letter": "0"}, "bad_letter"), ({"letter": "A"}, "too_narrow"), ({"print_columns": 40}, "bad_request")]:
        r = _width(a, online, **body)
        assert r.status_code == 400 and r.json()["code"] == code, (body, r.text)
    r = _width(a, online, letter="L")
    assert r.status_code == 200, r.text + a.log_tail()
    assert r.json() == {"printer_id": online, "name": "Online", "columns_read": 42, "print_columns": 42}
    mine = {p["id"]: p for p in mgr.get(f"{STORE_V1}/me/printers", timeout=5).json()}
    assert mine[online]["print_columns"] == 42
    wait_until(lambda: _diag_printers(a)[online]["print_columns"] == 42, timeout=20, what="the Pi to get width 42")

    # A test slip there is laid out for 42.
    r = a.http.post(f"{a.base}/api/printers/test", json={"printer_id": online}, timeout=20)
    assert r.status_code == 200, r.text
    _wait_jobs(fakes["Online"], 2)
    slip = fakes["Online"].jobs[1]
    assert b"-" * 42 + b"\n" in slip and b"-" * 43 not in slip and b"Width:   42 characters" in slip, slip

    # A Wings ticket (an 80 mm station) rerouted to Online: laid out for 42, the bands 21 wide.
    assert a.http.put(f"{a.base}/api/settings/routing", json={"mode": "one", "emergency_printer_id": online},
                      timeout=10).status_code == 200
    fakes["Wings"].stop()
    r = _accept(a, _snap(rig["main_store"], [_item("Hot Wings", ids["Wings"]), _item("Garlic Knots", ids["Front"])]))
    assert r.status_code == 200, r.text + a.log_tail()
    wait_until(lambda: any(band("REROUTED FROM WINGS", 42) in j for j in fakes["Online"].jobs), timeout=15,
               what="the rerouted Wings ticket on Online")
    ticket = next(j for j in fakes["Online"].jobs if band("REROUTED FROM WINGS", 42) in j)
    assert band("REDIRIGIDO DE WINGS", 42) in ticket and b"1  HOT WINGS" in ticket
    assert b"-" * 42 in ticket and b"-" * 43 not in ticket, ticket

    # The line didn't wrap (9 = 64 characters): back to 80 mm.
    r = _width(a, online, letter="9")
    assert r.status_code == 200 and r.json()["print_columns"] == 48 and r.json()["columns_read"] == 64, r.text
    wait_until(lambda: _diag_printers(a)[online]["print_columns"] == 48, timeout=20, what="the Pi to get width 48")
