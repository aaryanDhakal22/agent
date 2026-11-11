"""Terminal stage 0d: the Pi as the POS terminals' server (/api/terminal/*).

Each test spawns a throwaway agent linked to a throwaway store on dev main,
through a FAKE MAIN: a small streaming proxy in front of dev main that
answers the two routes main may not have yet (GET /store/v1/agent/
counter-menu and /terminal-config) with test data, injects main's commands
(menu.changed / terminal.changed) into the SSE stream, and records the
floor.events the agent posts (order.placed / order.payment / order.void /
journal.entries) before passing them on. Everything else is dev main's.
Printing goes to fake TCP printers only (no paper).

A last test checks the live dev main routes once they exist (skips until).
"""

from __future__ import annotations

import json
import secrets
import threading
import time
import uuid
from http.server import BaseHTTPRequestHandler, ThreadingHTTPServer
from pathlib import Path

import pytest
import requests

from conftest import (MAIN_BASE_URL, STORE_V1, FakePrinter, manager_session, provision, wait_until)

CASE = Path(__file__).resolve().parent.parent / "internal/domain/pricing/testdata/cases/33_one_specialty_on_a_half.json"
CHEESE = "00000000-0000-0000-0000-0000000000a1"
HAWAIIAN = "00000000-0000-0000-0000-0000000000a2"
SODA = "00000000-0000-0000-0000-0000000000a6"
TOPPINGS = "00000000-0000-0000-0000-0000000000c1"
PEPPERONI = "00000000-0000-0000-0000-0000000000d1"
XL = "00000000-0000-0000-0000-0000000000b2"

# bcrypt (cost 4) of the test PINs, made with the agent's golang.org/x/crypto/bcrypt.
PIN_HASHES = {"1234": "$2a$04$p7jbKst29O/j4QbrTI2NuOefRCEyZ2FGm0E4FDkH27jLwY4Pp1rke",
              "9876": "$2a$04$OpuJEb338VLK8ztyrOJJjOUOkj98dIiSJeyt6MHQOMPg5vDbH0mvG"}


def _bcrypt(pin: str) -> str:
    return PIN_HASHES[pin]


class FakeMain:
    """A streaming proxy to dev main with the terminal routes faked."""

    def __init__(self) -> None:
        self.counter_menu: dict | None = None
        self.terminal_config: dict | None = None
        self.events: list[dict] = []
        self.fetches: dict[str, int] = {}
        self._streams: list = []
        self._lock = threading.Lock()
        outer = self

        class H(BaseHTTPRequestHandler):
            protocol_version = "HTTP/1.0"  # one response per connection; SSE ends at close

            def log_message(self, *a):  # quiet
                pass

            def _fake(self, body: dict | None) -> None:
                outer.fetches[self.path] = outer.fetches.get(self.path, 0) + 1
                if body is None:
                    self.send_response(404)
                    self.end_headers()
                    return
                raw = json.dumps(body).encode()
                self.send_response(200)
                self.send_header("Content-Type", "application/json")
                self.send_header("Content-Length", str(len(raw)))
                self.end_headers()
                self.wfile.write(raw)

            def _proxy(self, method: str) -> None:
                if self.path == "/store/v1/agent/counter-menu":
                    return self._fake(outer.counter_menu)
                if self.path == "/store/v1/agent/terminal-config":
                    return self._fake(outer.terminal_config)
                n = int(self.headers.get("Content-Length") or 0)
                body = self.rfile.read(n) if n else None
                if method == "POST" and self.path == "/store/v1/agent/rpc" and body:
                    env = json.loads(body)
                    if env.get("type") == "floor.events":
                        with outer._lock:
                            outer.events.extend(env["payload"]["events"])
                    if env.get("id", "").startswith("fake-"):  # a reply to a command we injected
                        self.send_response(200)
                        self.end_headers()
                        return
                headers = {k: v for k, v in self.headers.items() if k.lower() not in ("host", "content-length", "connection")}
                up = requests.request(method, MAIN_BASE_URL + self.path, data=body, headers=headers, stream=True, timeout=(5, None))
                self.send_response(up.status_code)
                for k, v in up.headers.items():
                    if k.lower() not in ("transfer-encoding", "connection", "content-length", "content-encoding"):
                        self.send_header(k, v)
                self.end_headers()
                if up.headers.get("Content-Type", "").startswith("text/event-stream"):
                    with outer._lock:
                        outer._streams.append(self)
                    self._wlock = threading.Lock()
                    try:
                        for line in up.iter_lines(decode_unicode=False):
                            with self._wlock:
                                self.wfile.write(line + b"\n")
                                self.wfile.flush()
                    except OSError:
                        pass
                    finally:
                        with outer._lock:
                            if self in outer._streams:
                                outer._streams.remove(self)
                        up.close()
                    return
                self.wfile.write(up.content)

            def do_GET(self):
                self._proxy("GET")

            def do_POST(self):
                self._proxy("POST")

            def do_PUT(self):
                self._proxy("PUT")

            def do_PATCH(self):
                self._proxy("PATCH")

            def do_DELETE(self):
                self._proxy("DELETE")

        self.srv = ThreadingHTTPServer(("127.0.0.1", 0), H)
        self.srv.daemon_threads = True
        self.url = f"http://127.0.0.1:{self.srv.server_address[1]}"
        threading.Thread(target=self.srv.serve_forever, daemon=True).start()

    def command(self, typ: str, payload: dict) -> None:
        """Inject a main→agent command frame into every open agent stream."""
        frame = f"event: command\ndata: {json.dumps({'type': typ, 'id': 'fake-' + secrets.token_hex(4), 'payload': payload})}\n\n".encode()
        with self._lock:
            streams = list(self._streams)
        assert streams, "no agent SSE stream through the fake main"
        for s in streams:
            with s._wlock:
                s.wfile.write(frame)
                s.wfile.flush()

    def of_type(self, typ: str) -> list[dict]:
        with self._lock:
            return [e for e in self.events if e.get("type") == typ]

    def stop(self) -> None:
        self.srv.shutdown()
        self.srv.server_close()


def staff_config(version: int = 1, counter_start: int = 500) -> dict:
    return {"version": version,
            "staff": [{"id": "s-cashier", "first_name": "Cara", "last_name": "Smith", "role": "cashier", "pin_hash": _bcrypt("1234")},
                      {"id": "s-manager", "first_name": "Khawar", "last_name": "Ali", "role": "manager", "pin_hash": _bcrypt("9876")}],
            # main's terminal settings (contract/main.md §2.5zb)
            "settings": {"counter_start": counter_start, "auto_lock_minutes": 3, "cashier_refund_cents": 1000,
                         "cashier_discount_pct": 10, "receipt_auto_done_seconds": 15}}


def counter_menu(printer_id: str, version: int = 7) -> dict:
    ctx = json.loads(CASE.read_text())["context"]
    return {"menu_version": version, "build": {"view": {"kind": "counter", "name": "Counter"}, "layout": {"tabs": []}},
            "pricing": ctx, "routes": {CHEESE: {"printer_id": printer_id, "name": "CHEESE PIZZA"}, HAWAIIAN: {"printer_id": printer_id}}}


def pizza_line() -> dict:
    return {"item_id": CHEESE, "size_id": XL, "qty": 1, "halves": [{"item_id": HAWAIIAN, "half": 2}],
            "options": [{"group_id": TOPPINGS, "option_id": PEPPERONI, "portion": "first"}]}


@pytest.fixture
def rig(admin, main_store, spawn_agent, fakes):
    """A linked throwaway agent behind the fake main, a Pizza station + a
    receipt printer (fake), a paired terminal logged in as the cashier."""
    fm = FakeMain()
    mgr = manager_session(admin, main_store)
    fakes["Pizza"], fakes["Front"] = FakePrinter(), FakePrinter()
    r = mgr.post(f"{STORE_V1}/me/printers", json={"name": "Pizza", "ip": fakes["Pizza"].addr}, timeout=5)
    assert r.status_code == 201, r.text
    pizza_id = r.json()["id"]
    r = mgr.post(f"{STORE_V1}/me/printers", json={"name": "Front", "ip": fakes["Front"].addr, "is_receipt": True}, timeout=5)
    assert r.status_code == 201, r.text
    fm.counter_menu, fm.terminal_config = counter_menu(pizza_id), staff_config()
    agent = spawn_agent(MAIN_SERVER_URL=fm.url)
    provision(agent, main_store)
    code = agent.http.post(f"{agent.base}/api/pairing/code", timeout=5).json()["code"]
    r = requests.post(f"{agent.base}/api/pair", json={"code": code, "name": "Till 1", "kind": "terminal"}, timeout=5)
    assert r.status_code == 201 and r.json()["kind"] == "terminal", r.text
    term = requests.Session()
    term.headers["X-Agent-Device-Key"] = r.json()["key"]
    wait_until(lambda: term.get(f"{agent.base}/api/terminal/menu", timeout=5).status_code == 200, timeout=30, what="counter menu fetched")
    wait_until(lambda: term.post(f"{agent.base}/api/terminal/login", json={"pin": "1234"}, timeout=5).status_code == 200,
               timeout=30, what="terminal config fetched + login")
    yield {"agent": agent, "term": term, "fm": fm, "fakes": fakes, "pizza_id": pizza_id, "mgr": mgr}
    fm.stop()


def place(rig, key: str | None = None, **extra) -> requests.Response:
    body = {"client_key": key or f"k-{uuid.uuid4()}", "type": "counter", "cart": {"lines": [pizza_line()]}, **extra}
    return rig["term"].post(f"{rig['agent'].base}/api/terminal/orders", json=body, timeout=10)


def test_device_kinds_and_gates(rig, admin):
    agent, term = rig["agent"], rig["term"]
    devices = agent.http.get(f"{agent.base}/api/devices", timeout=5).json()
    assert [d["kind"] for d in devices] == ["terminal"]
    # A terminal key: no floor routes besides events / floor.
    assert term.get(f"{agent.base}/api/drivers", timeout=5).json()["code"] == "terminal_only"
    assert term.get(f"{agent.base}/api/floor", timeout=5).status_code == 200
    # A tablet key: no terminal routes.
    code = agent.http.post(f"{agent.base}/api/pairing/code", timeout=5).json()["code"]
    tab = requests.post(f"{agent.base}/api/pair", json={"code": code, "name": "Kitchen"}, timeout=5).json()["key"]
    r = requests.get(f"{agent.base}/api/terminal/menu", headers={"X-Agent-Device-Key": tab}, timeout=5)
    assert r.status_code == 403 and r.json()["code"] == "terminal_required"
    assert requests.get(f"{agent.base}/api/terminal/menu", timeout=5).status_code == 401


def test_login_menu_quote(rig):
    agent, term = rig["agent"], rig["term"]
    base = agent.base
    r = term.post(f"{base}/api/terminal/login", json={"pin": "0000"}, timeout=5)
    assert r.status_code == 401 and r.json()["code"] == "wrong_pin"
    assert term.post(f"{base}/api/terminal/login", json={"pin": "12x"}, timeout=5).json()["code"] == "bad_pin"
    r = term.post(f"{base}/api/terminal/login", json={"pin": "1234"}, timeout=5).json()
    assert r["staff"]["id"] == "s-cashier" and r["limits"] == {"refund_cents": 1000, "discount_pct": 10}
    assert r["settings"]["auto_lock_minutes"] == 3
    m = term.get(f"{base}/api/terminal/menu", timeout=5)
    assert m.json()["menu_version"] == 7 and m.json()["counter_menu"]["build"]["view"]["kind"] == "counter"
    assert term.get(f"{base}/api/terminal/menu", headers={"If-None-Match": m.headers["ETag"]}, timeout=5).status_code == 304
    q = term.post(f"{base}/api/terminal/quote", json={"cart": {"lines": [pizza_line(), {"item_id": SODA, "qty": 2}]}}, timeout=5).json()
    assert q["quote"]["lines"][0]["unit_cents"] == 2224 and q["quote"]["subtotal_cents"] == 2224 + 398 and not q["quote"]["errors"]


def test_wrong_pins_are_throttled(rig):
    agent, term = rig["agent"], rig["term"]
    for _ in range(5):
        assert term.post(f"{agent.base}/api/terminal/login", json={"pin": "0000"}, timeout=5).status_code == 401
    r = term.post(f"{agent.base}/api/terminal/login", json={"pin": "1234"}, timeout=5)
    assert r.status_code == 429 and r.json()["code"] == "too_many_attempts" and int(r.headers["Retry-After"]) > 0


def test_order_number_tickets_sync_idempotent(rig):
    agent, fm, fakes = rig["agent"], rig["fm"], rig["fakes"]
    r = place(rig, key="k-first-order-1", payments=[{"tender": "cash", "amount_cents": 2357, "tendered_cents": 3000}], note="napkins")
    assert r.status_code == 201, r.text
    res = r.json()
    so = res["order"]
    assert res["number"] == 500 and not res["duplicate"] and res["change_cents"] == 3000 - 2357
    assert so["status"]["state"] == "accepted" and so["order"]["source"] == "counter" and so["status"]["paid"]
    # Station ticket on the Pizza printer in the half-and-half format; no receipt on the Pi.
    wait_until(lambda: fakes["Pizza"].jobs, what="pizza station ticket")
    ticket = fakes["Pizza"].text()
    assert "500" in ticket and "1ST HALF" in ticket and "2ND HALF" in ticket
    assert not fakes["Front"].jobs
    # Same key: the same order, no second ticket, no new number.
    again = place(rig, key="k-first-order-1")
    assert again.status_code == 200 and again.json()["duplicate"] and again.json()["order"]["order"]["id"] == so["order"]["id"]
    assert place(rig).json()["number"] == 501
    # Shows on the floor like any order (Quibase's Kitchen).
    floor = agent.http.get(f"{agent.base}/api/orders?batch=current", timeout=5).json()
    assert so["order"]["id"] in [o["order"]["id"] for o in floor]
    # main hears order.placed + journal.entries.
    wait_until(lambda: fm.of_type("order.placed"), what="order.placed synced")
    placed = fm.of_type("order.placed")[0]
    assert placed["snapshot"]["id"] == so["order"]["id"] and placed["snapshot"]["kitchen_order_number"] == 500
    wait_until(lambda: any(e["action"] == "order.placed" for ev in fm.of_type("journal.entries") for e in ev["entries"]),
               what="journal synced")


def test_pick_up_list_payment_void_limits(rig):
    agent, term, fm, fakes = rig["agent"], rig["term"], rig["fm"], rig["fakes"]
    base = agent.base
    oid = place(rig).json()["order"]["order"]["id"]
    total = 2357
    open_ids = [o["order"]["id"] for o in term.get(f"{base}/api/terminal/orders?open=1", timeout=5).json()]
    assert oid in open_ids
    r = term.post(f"{base}/api/terminal/orders/{oid}/payments", json={"tender": "cash", "amount_cents": total + 1}, timeout=5)
    assert r.status_code == 409 and r.json()["code"] == "over_balance"
    r = term.post(f"{base}/api/terminal/orders/{oid}/payments",
                  json={"tender": "square_app", "amount_cents": total, "client_key": "pay-key-0001"}, timeout=5).json()
    assert r["balance_cents"] == 0 and r["order"]["status"]["paid"]
    dup = term.post(f"{base}/api/terminal/orders/{oid}/payments",
                    json={"tender": "square_app", "amount_cents": total, "client_key": "pay-key-0001"}, timeout=5).json()
    assert dup["duplicate"] and dup["payment"]["id"] == r["payment"]["id"]
    wait_until(lambda: fm.of_type("order.payment"), what="order.payment synced")
    # Void of a paid order over the cashier's $10: a manager's PIN.
    r = term.post(f"{base}/api/terminal/orders/{oid}/void", json={"reason": "customer left"}, timeout=5)
    assert r.status_code == 403 and r.json()["code"] == "manager_required" and r.json()["requested"] == total
    r = term.post(f"{base}/api/terminal/orders/{oid}/void", json={"reason": "customer left", "manager_pin": "1234"}, timeout=5)
    assert r.json()["code"] == "manager_pin_invalid"
    r = term.post(f"{base}/api/terminal/orders/{oid}/void", json={"reason": "customer left", "manager_pin": "9876"}, timeout=5).json()
    assert r["refunded_cents"] == total and r["order"]["status"]["state"] == "cancelled"
    wait_until(lambda: any("VOID" in j.decode("latin-1") for j in fakes["Pizza"].jobs), what="VOID slip")
    wait_until(lambda: fm.of_type("order.void"), what="order.void synced")
    assert fm.of_type("order.void")[0]["approved_by"] == "s-manager"
    # A discount over 10 % needs the manager too.
    r = place(rig, cart={"lines": [pizza_line()], "discount": {"pct": 20, "reason": "regular"}})
    assert r.status_code == 403 and r.json()["code"] == "manager_required"
    assert place(rig, cart={"lines": [pizza_line()], "discount": {"pct": 20, "reason": "regular"}}, manager_pin="9876").status_code == 201
    # Terminal-only journal rows; Pi ones refused.
    assert term.post(f"{base}/api/terminal/journal", json={"action": "no_sale", "detail": {"drawer": 1}}, timeout=5).status_code == 201
    assert term.post(f"{base}/api/terminal/journal", json={"action": "void"}, timeout=5).status_code == 400
    actions = [e["action"] for e in term.get(f"{base}/api/terminal/journal", timeout=5).json()]
    for a in ("login", "order.placed", "payment", "void", "refund", "override", "discount", "no_sale"):
        assert a in actions, a


def test_counter_number_resets_at_close_batch(rig):
    agent = rig["agent"]
    first = place(rig).json()
    assert first["number"] == 500
    oid = first["order"]["order"]["id"]
    assert rig["term"].post(f"{agent.base}/api/terminal/orders/{oid}/void", json={"reason": "test"}, timeout=5).status_code == 200
    r = agent.http.post(f"{agent.base}/api/batch/close", json={"force": True}, timeout=30)
    assert r.status_code == 200, r.text
    assert place(rig).json()["number"] == 500


def test_commands_refetch_and_announce(rig):
    agent, term, fm = rig["agent"], rig["term"], rig["fm"]
    events: list[str] = []
    stop = threading.Event()

    def listen():
        with term.get(f"{agent.base}/api/events", stream=True, timeout=(5, 30)) as s:
            for line in s.iter_lines(decode_unicode=True):
                if stop.is_set():
                    return
                if line and line.startswith("event:"):
                    events.append(line.split(":", 1)[1].strip())

    threading.Thread(target=listen, daemon=True).start()
    time.sleep(0.5)  # the stream is open (a missed frame would only fail this test)
    fm.counter_menu = counter_menu(rig["pizza_id"], version=8)
    fm.command("menu.changed", {"version": 8})
    wait_until(lambda: term.get(f"{agent.base}/api/terminal/menu", timeout=5).json()["menu_version"] == 8, what="menu v8")
    fm.terminal_config = staff_config(version=2)
    fm.terminal_config["staff"] = fm.terminal_config["staff"][1:]  # the cashier left
    fm.command("terminal.changed", {"version": 2})
    wait_until(lambda: term.get(f"{agent.base}/api/terminal/session", timeout=5).json()["config_version"] == 2, what="config v2")
    assert term.get(f"{agent.base}/api/terminal/session", timeout=5).json()["staff"] is None
    assert place(rig).json()["code"] == "login_required"
    wait_until(lambda: "terminal.menu.changed" in events and "terminal.config.changed" in events, what="SSE terminal events")
    stop.set()


def test_live_main_terminal_routes(admin, main_store):
    """Dev main's own routes (the main side of 0d). Skips until they exist."""
    token = main_store.mint()["token"]
    h = {"X-STORE-API-KEY": token}
    r = requests.get(f"{MAIN_BASE_URL}/store/v1/agent/terminal-config", headers=h, timeout=5)
    if r.status_code == 404:
        pytest.skip("dev main has no GET /store/v1/agent/terminal-config yet")
    assert r.status_code == 200 and "staff" in r.json() and "version" in r.json()
    r = requests.get(f"{MAIN_BASE_URL}/store/v1/agent/counter-menu", headers=h, timeout=10)
    assert r.status_code in (200, 404), r.text
