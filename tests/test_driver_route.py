"""Driver routes (2.3.7): a driver's Pushover device, the test route, the
route Send Out pushes, and system alerts scoped to the admin's devices.

Pushover is a fake HTTP server here (PUSHOVER_API_URL): nothing real is sent.
"""

from __future__ import annotations

import json
import threading
from http.server import BaseHTTPRequestHandler, ThreadingHTTPServer
from urllib.parse import parse_qs

import pytest

from conftest import STORE_V1, FakePrinter, manager_session, provision, wait_until
from test_floor import post
from test_pools import accepted_delivery, drivers


class FakePushover:
    def __init__(self) -> None:
        self.forms: list[dict[str, str]] = []
        self.status, self.body = 200, {"status": 1, "request": "x"}
        fake = self

        class Handler(BaseHTTPRequestHandler):
            def do_POST(self) -> None:  # noqa: N802
                n = int(self.headers.get("Content-Length", 0))
                form = {k: v[0] for k, v in parse_qs(self.rfile.read(n).decode()).items()}
                fake.forms.append(form)
                body = json.dumps(fake.body).encode()
                self.send_response(fake.status)
                self.send_header("Content-Type", "application/json")
                self.send_header("Content-Length", str(len(body)))
                self.end_headers()
                self.wfile.write(body)

            def log_message(self, *a) -> None:
                pass

        self.srv = ThreadingHTTPServer(("127.0.0.1", 0), Handler)
        self.url = f"http://127.0.0.1:{self.srv.server_address[1]}/1/messages.json"
        threading.Thread(target=self.srv.serve_forever, daemon=True).start()

    def routes(self) -> list[dict[str, str]]:
        return [f for f in self.forms if f.get("priority") == "2"]

    def close(self) -> None:
        self.srv.shutdown()


@pytest.fixture
def pushover():
    p = FakePushover()
    yield p
    p.close()


@pytest.fixture
def route_floor(admin, main_store, spawn_agent, fakes, pushover):
    mgr = manager_session(admin, main_store)
    fakes["Front"] = FakePrinter()
    assert mgr.post(f"{STORE_V1}/me/printers", json={"name": "Front", "ip": fakes["Front"].addr}, timeout=5).status_code == 201
    a = spawn_agent(PUSHOVER_APP_TOKEN="app", PUSHOVER_USER_KEY="user", PUSHOVER_API_URL=pushover.url,
                    PUSHOVER_ADMIN_DEVICE="AdminPhone")
    provision(a, main_store)
    wait_until(lambda: len(a.http.get(f"{a.base}/api/printers", timeout=5).json()) == 1, timeout=20, what="printer roster")
    a.store = main_store
    return a


def test_system_alerts_go_to_the_admin_devices(route_floor, pushover) -> None:
    started = [f for f in pushover.forms if f.get("message") == "Agent started"]
    assert started and started[0]["device"] == "AdminPhone" and started[0]["user"] == "user"


def test_device_field_and_test_route(route_floor, pushover) -> None:
    a = route_floor
    r = post(a, "/api/drivers", {"name": "Ann", "pushover_device": "bad name"})
    assert r.status_code == 400 and r.json()["code"] == "bad_device"
    ann = post(a, "/api/drivers", {"name": "Ann", "pushover_device": "ann_phone"}).json()
    assert ann["pushover_device"] == "ann_phone"
    listed = {d["id"]: d for d in a.http.get(f"{a.base}/api/drivers", timeout=5).json()}
    assert listed[ann["id"]]["pushover_device"] == "ann_phone"

    r = post(a, f"/api/drivers/{ann['id']}/route/test")
    assert r.status_code == 200 and r.json() == {"ok": True}, r.text
    route = pushover.routes()[-1]
    assert route["device"] == "ann_phone" and route["url"].startswith("https://www.google.com/maps/dir/?api=1")

    r = a.http.patch(f"{a.base}/api/drivers/{ann['id']}", json={"pushover_device": "a" * 26}, timeout=5)
    assert r.status_code == 400 and r.json()["code"] == "bad_device"
    assert a.http.patch(f"{a.base}/api/drivers/{ann['id']}", json={"pushover_device": ""}, timeout=5).status_code == 204
    r = post(a, f"/api/drivers/{ann['id']}/route/test")
    assert r.status_code == 409 and r.json()["code"] == "no_device"

    assert a.http.patch(f"{a.base}/api/drivers/{ann['id']}", json={"pushover_device": "gone"}, timeout=5).status_code == 204
    pushover.status, pushover.body = 400, {"errors": ["device name is not valid for user"], "status": 0}
    r = post(a, f"/api/drivers/{ann['id']}/route/test")
    assert r.status_code == 502 and r.json()["code"] == "device_not_found", r.text


def test_send_out_pushes_the_route(route_floor, pushover) -> None:
    a = route_floor
    (bob,) = drivers(a, "Bob")
    assert a.http.patch(f"{a.base}/api/drivers/{bob['id']}", json={"pushover_device": "bob"}, timeout=5).status_code == 204
    oid = accepted_delivery(a, ready=True)
    assert post(a, "/api/deliveries/pool", {"order_ids": [oid], "driver_id": bob["id"]}).status_code == 200
    r = post(a, f"/api/drivers/{bob['id']}/send-out", {})
    assert r.status_code == 200, r.text
    wait_until(lambda: any(f.get("device") == "bob" for f in pushover.routes()), timeout=10, what="route pushover")
    route = next(f for f in pushover.routes() if f.get("device") == "bob")
    assert route["title"] == "Route · 1 stop" and route["url_title"] == "Open route"


def test_no_device_no_route(route_floor, pushover) -> None:
    a = route_floor
    (cat,) = drivers(a, "Cat")
    oid = accepted_delivery(a, ready=True)
    assert post(a, "/api/deliveries/pool", {"order_ids": [oid], "driver_id": cat["id"]}).status_code == 200
    assert post(a, f"/api/drivers/{cat['id']}/send-out", {}).status_code == 200
    assert pushover.routes() == []
