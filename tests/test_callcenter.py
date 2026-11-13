"""The call center: the remote terminal over main (contract/agent.md §2.10ab).

The test plays main: the FAKE MAIN of test_terminal.py injects `terminal.relay`
commands into the agent's SSE stream (as main does for the call-center web app)
and reads the `terminal.relay.reply` the agent posts back. The agent runs each
call through its terminal handlers as a call-center seat. Printing goes to fake
TCP printers only (no paper). Each test has its own throwaway agent + store.
"""

from __future__ import annotations

import json
import secrets
import uuid

import pytest
import requests

from conftest import wait_until
from test_terminal import PIN_HASHES, make_rig, pizza_line, staff_config

AGENT = "00000000-0000-4000-8000-0000000000a7"
CASHIER = "00000000-0000-4000-8000-0000000000c1"
TAKEN_BY = "Call center - Elena V."


def cc_config(version: int = 2, pin_hash: str = "") -> dict:
    """main's terminal config with a call-center agent (main sends them with
    no PIN hash: it checks that PIN itself)."""
    cfg = staff_config(version)
    cfg["staff"].append({"id": AGENT, "first_name": "Elena", "last_name": "Vega", "role": "callcenter", "pin_hash": pin_hash})
    return cfg


@pytest.fixture
def rig(admin, main_store, spawn_agent, fakes):
    for r in make_rig(admin, main_store, spawn_agent, fakes):
        r["fm"].terminal_config = cc_config()
        r["fm"].rpc["terminal.relay.reply"] = lambda payload: (200, {})  # recorded in fm.rpc_seen
        yield r


def relay(rig, method: str, path: str, body=None, *, staff: str = AGENT, device: str = "desk-1", query: str = "",
          headers: dict | None = None, rid: str | None = None, timeout: float = 20.0) -> dict:
    """One call of the call-center app, as main relays it: the reply's payload
    {status, headers, body}."""
    fm = rig["fm"]
    rid = rid or "relay-" + secrets.token_hex(6)
    env = {"type": "terminal.relay", "id": rid,
           "payload": {"device": device, "device_name": "Elevate " + device, "staff_id": staff, "method": method, "path": path,
                       "query": query, "headers": headers or {}, "body": body}}
    frame = f"event: command\ndata: {json.dumps(env)}\n\n".encode()
    with fm._lock:
        streams = list(fm._streams)
    assert streams, "no agent SSE stream through the fake main"
    for s in streams:
        with s._wlock:
            s.wfile.write(frame)
            s.wfile.flush()

    def reply():
        with fm._lock:
            return next((e for e in fm.rpc_seen if e.get("id") == rid and e.get("type") == "terminal.relay.reply"), None)

    got = wait_until(reply, timeout=timeout, what=f"the reply to {method} {path}")
    payload = got["payload"]
    assert set(payload) == {"status", "headers", "body"} and isinstance(payload["headers"], dict), payload
    return payload


def order_body(typ: str = "pickup", **extra) -> dict:
    body = {"client_key": f"cc-{uuid.uuid4()}", "type": typ, "cart": {"lines": [pizza_line()]},
            "customer": {"name": "Ada Test", "phone": "4105550101"}, **extra}
    if typ == "delivery":
        body["customer"]["address"] = {"line1": "1 Test St", "city": "Towson", "state": "MD", "zip": "21204"}
    return body


def text_of(printer) -> str:
    return printer.text()


def test_relay_places_like_a_phone_order(rig):
    agent, term, fm, fakes = rig["agent"], rig["term"], rig["fm"], rig["fakes"]
    base = agent.base
    # The agent was made in main a moment ago: the Pi pulls its config for the id it doesn't know.
    sess = relay(rig, "GET", "session")
    assert sess["status"] == 200, sess
    assert sess["body"]["staff"]["id"] == AGENT and sess["body"]["staff"]["name"] == "Elena V." and sess["body"]["role"] == "callcenter"
    assert sess["body"]["limits"] == {"refund_cents": 0, "discount_pct": 0}  # never unlimited

    # The menu with its ETag; If-None-Match -> 304 with no body.
    menu = relay(rig, "GET", "menu")
    assert menu["status"] == 200 and menu["headers"].get("ETag") and "counter_menu" in menu["body"]
    again = relay(rig, "GET", "menu", headers={"If-None-Match": menu["headers"]["ETag"]})
    assert again["status"] == 304 and again["body"] is None

    # The store's one call-center device, made by itself, listed last; it can't be removed.
    devices = agent.http.get(f"{base}/api/devices", timeout=5).json()
    assert [(d["id"], d["kind"], d["name"]) for d in devices][-1] == ("callcenter", "callcenter", "Call center")
    r = agent.http.post(f"{base}/api/devices/callcenter/revoke", timeout=5)
    assert r.status_code == 409 and r.json()["code"] == "callcenter_device"

    # A delivery by the call center: accepted at once (no ring), station tickets at once,
    # and the Pi prints the delivery ticket on the store's receipt printer.
    q = relay(rig, "POST", "quote", {"type": "delivery", "cart": {"lines": [pizza_line()]}})
    assert q["status"] == 200 and q["body"]["quote"]["total_cents"] > 0
    placed = relay(rig, "POST", "orders", order_body("delivery"))
    assert placed["status"] == 201, placed
    so = placed["body"]["order"]
    oid = so["order"]["id"]
    assert so["order"]["taken_by"] == TAKEN_BY and so["order"]["device_kind"] == "callcenter"
    assert so["status"]["state"] == "accepted" and placed["body"]["number"] == 500
    wait_until(lambda: TAKEN_BY in text_of(fakes["Pizza"]), what="the station ticket names who took it")
    wait_until(lambda: "Merchant Copy" in text_of(fakes["Front"]) and "Customer Copy" in text_of(fakes["Front"]),
               what="the delivery ticket on the store's receipt printer")
    assert text_of(fakes["Front"]).count(TAKEN_BY) == 2  # both copies
    # Nothing rings: no order waits for an accept.
    floor = relay(rig, "GET", "floor")  # the Pi's GET /api/floor (the open batch + the floor's settings)
    assert floor["status"] == 200 and floor["body"]["batch"]["id"] > 0 and "kitchen_next" in floor["body"]
    assert agent.http.get(f"{base}/api/orders/{oid}", timeout=5).json()["status"]["state"] == "accepted"

    # main hears order.placed with what its per-agent report reads.
    snap = wait_until(lambda: next((e["snapshot"] for e in fm.of_type("order.placed") if e["order_id"] == oid), None), what="order.placed")
    assert snap["device_kind"] == "callcenter" and snap["staff_id"] == AGENT and snap["device_id"] == "callcenter"
    assert snap["seat_id"] == "callcenter:desk-1" and snap["taken_by"] == TAKEN_BY and snap["fulfillment"]["type"] == "delivery"

    # The store's terminal sees it like any order; the checks list says who took it.
    assert term.get(f"{base}/api/terminal/orders/{oid}", timeout=5).json()["order"]["order"]["taken_by"] == TAKEN_BY
    checks = relay(rig, "GET", "checks", query="batch=current&device_id=callcenter")
    assert checks["status"] == 200 and [(c["order_id"], c["taken_by"], c["staff_id"]) for c in checks["body"]["checks"]] == [(oid, TAKEN_BY, AGENT)]

    assert placed["body"]["delivery_tickets"] == "pi"  # the Pi printed them: a terminal doesn't (2.5.7)

    # A pickup (agent 2.5.7, the owner 2026-11-13): its station tickets only; nothing on the receipt
    # ("Online") printer, which prints only deliveries. Its receipt prints at its Cash Out on a terminal.
    fakes["Front"].clear()
    pizza_jobs = len(fakes["Pizza"].jobs)
    pick = relay(rig, "POST", "orders", order_body("pickup"))
    assert pick["status"] == 201, pick
    assert "delivery_tickets" not in pick["body"]
    pid = pick["body"]["order"]["order"]["id"]
    wait_until(lambda: len(fakes["Pizza"].jobs) > pizza_jobs, what="the pickup's station ticket")
    from gatemark import intentional_sleep
    intentional_sleep(1.0, "absence: no receipt of a pickup on the receipt printer")
    assert not fakes["Front"].jobs, text_of(fakes["Front"])

    # Reprint (the new route, every terminal kind, a person's ask): the customer receipt on the receipt printer.
    jobs = len(fakes["Front"].jobs)
    rp = relay(rig, "POST", f"orders/{pid}/reprint")
    assert rp["status"] == 200 and rp["body"] == {"ok": True, "printed_on": "Front"}, rp
    wait_until(lambda: len(fakes["Front"].jobs) == jobs + 1, what="the reprinted receipt")
    r = term.post(f"{base}/api/terminal/orders/{pid}/reprint", timeout=10)
    assert r.status_code == 200 and r.json() == {"ok": True, "printed_on": "Front"}, r.text
    wait_until(lambda: len(fakes["Front"].jobs) == jobs + 2, what="the terminal's reprint")
    assert term.post(f"{base}/api/terminal/orders/{uuid.uuid4()}/reprint", timeout=10).status_code == 404
    assert [e["detail"]["number"] for e in term.get(f"{base}/api/terminal/journal?limit=50", timeout=5).json() if e["action"] == "reprint"] == [501, 501]

    # A command fanned out twice (the same envelope id) runs once: one order, one reply.
    body, rid = order_body("pickup"), "relay-" + secrets.token_hex(6)
    for _ in range(2):
        relay(rig, "POST", "orders", body, rid=rid)
    with fm._lock:
        assert len([e for e in fm.rpc_seen if e.get("id") == rid]) == 1
    assert len(relay(rig, "GET", "orders")["body"]) == 3


def test_guards_paid_lock_and_wrong_role_pin(rig):
    agent, term, fm, fakes = rig["agent"], rig["term"], rig["fm"], rig["fakes"]
    base = agent.base

    def refused(reply, what):
        return reply["status"] == 403 and reply["body"]["code"] == "not_allowed_for_callcenter" and reply["body"].get("what") == what

    # Routes outside the allowlist.
    for method, path in [("POST", "login"), ("POST", "refunds"), ("GET", "drawer"), ("POST", "drawer/pay-out"), ("POST", "close"),
                         ("GET", "z"), ("GET", "reports/till"), ("POST", "manager-check"), ("GET", "journal"), ("POST", "print"),
                         ("POST", "deliveries/send-out"), ("GET", "../devices")]:
        assert refused(relay(rig, method, path, {"pin": "9876"}), "route"), (method, path)
    # What an allowed call may not carry (nothing is placed, nothing prints).
    line = pizza_line()
    cases = {
        "type": order_body("counter"),
        "discount": order_body(cart={"lines": [line], "discount": {"pct": 5, "reason": "regular"}}),
        "price_change": order_body(cart={"lines": [{**line, "price_cents": 500}]}),
        "comp": order_body(cart={"lines": [{**line, "price_cents": 0}]}),
        "tender": order_body(payments=[{"tender": "cash", "amount_cents": 100}]),
        "refund": order_body(payments=[{"tender": "manual_card", "amount_cents": 100, "refund": True, "reason": "x"}]),
        "manager": order_body(manager_pin="9876"),
    }
    for what, body in cases.items():
        assert refused(relay(rig, "POST", "orders", body), what), what
    assert refused(relay(rig, "POST", "quote", {"type": "pickup", "cart": {"lines": [line], "discount": {"pct": 5, "reason": "x"}}}), "discount")
    assert relay(rig, "GET", "orders")["body"] == [] and not fakes["Pizza"].jobs and not fakes["Front"].jobs
    # An id that isn't a call-center agent: login_required (a cashier's too).
    for staff in (CASHIER, str(uuid.uuid4()), ""):
        got = relay(rig, "GET", "menu", staff=staff)
        assert got["status"] == 401 and got["body"]["code"] == "login_required", staff

    # An order, a typed card on it (cash is refused), then the paid lines are locked.
    soda = {"item_id": "00000000-0000-0000-0000-0000000000a6", "qty": 1}
    placed = relay(rig, "POST", "orders", order_body(cart={"lines": [line, {**soda, "qty": 2}]}))
    assert placed["status"] == 201, placed
    oid, total = placed["body"]["order"]["order"]["id"], placed["body"]["order"]["order"]["money"]["total_cents"]
    assert refused(relay(rig, "POST", f"orders/{oid}/payments", {"client_key": f"p-{uuid.uuid4()}", "tender": "cash", "amount_cents": total}), "tender")
    assert refused(relay(rig, "POST", f"orders/{oid}/payments", {"client_key": f"p-{uuid.uuid4()}", "tender": "manual_card", "amount_cents": 100,
                                                                 "refund": True, "reason": "x"}), "refund")
    paid = relay(rig, "POST", f"orders/{oid}/payments", {"client_key": f"p-{uuid.uuid4()}", "tender": "manual_card", "amount_cents": total,
                                                         "last4": "1881", "square_payment_id": "sq-pay-1", "entry": "keyed"})
    assert paid["status"] == 200 and paid["body"]["balance_cents"] == 0, paid
    wait_until(lambda: fakes["Pizza"].jobs, what="the station ticket")
    jobs = len(fakes["Pizza"].jobs)
    for name, lines in {"a line removed": [line], "a quantity lowered": [line, soda], "a line changed": [{**line, "note": "well done"}, {**soda, "qty": 2}]}.items():
        got = relay(rig, "PUT", f"orders/{oid}/cart", {"client_key": f"c-{uuid.uuid4()}", "cart": {"lines": lines}})
        assert got["status"] == 409 and got["body"]["code"] == "paid_locked" and got["body"]["lines"], (name, got)
    same = relay(rig, "GET", f"orders/{oid}")["body"]["order"]["order"]
    assert same["money"]["total_cents"] == total and len(fakes["Pizza"].jobs) == jobs  # nothing changed, nothing printed
    more = relay(rig, "PUT", f"orders/{oid}/cart", {"client_key": f"c-{uuid.uuid4()}", "cart": {"lines": [line, {**soda, "qty": 3}]}})
    assert more["status"] == 200 and more["body"]["added"] == ["Soda"] and more["body"]["removed"] == [] and more["body"]["balance_cents"] > 0, more
    # Cancel: unpaid only, with a reason.
    assert relay(rig, "POST", f"orders/{oid}/void", {"reason": "customer called back"})["body"]["code"] == "refund_first"
    unpaid = relay(rig, "POST", "orders", order_body())["body"]["order"]["order"]["id"]
    assert relay(rig, "POST", f"orders/{unpaid}/void", {})["status"] == 400
    gone = relay(rig, "POST", f"orders/{unpaid}/void", {"reason": "customer called back"})
    assert gone["status"] == 200 and gone["body"]["order"]["status"]["state"] == "cancelled"

    # A call-center agent's PIN on a store terminal: wrong_pin, and never a manager.
    fm.terminal_config = cc_config(3, pin_hash=PIN_HASHES["9876"])
    fm.terminal_config["staff"] = [s for s in fm.terminal_config["staff"] if s["role"] != "manager"]  # 9876 is the agent's alone now
    fm.command("terminal.changed", {"version": 3})
    wait_until(lambda: term.get(f"{base}/api/terminal/session", timeout=5).json()["config_version"] == 3, what="config v3")
    r = requests.post(f"{base}/api/terminal/login", json={"pin": "9876"}, headers={"X-Agent-Device-Key": term.headers["X-Agent-Device-Key"]}, timeout=5)
    assert r.status_code == 401 and r.json()["code"] == "wrong_pin", r.text
    r = term.post(f"{base}/api/terminal/manager-check", json={"pin": "9876"}, timeout=5)
    assert r.status_code == 401 and r.json()["code"] == "wrong_pin", r.text
    assert relay(rig, "GET", "session")["body"]["staff"]["id"] == AGENT  # the relay still knows them (no PIN travels)
    # The seat is never reachable over plain HTTP: headers don't make a caller the call center.
    spoof = {"X-Callcenter": "1", "X-Callcenter-Staff": AGENT, "X-Agent-Device-Key": "callcenter", "X-Staff-Id": AGENT}
    r = requests.get(f"{base}/api/terminal/session", headers=spoof, timeout=5)
    assert r.status_code == 401 and r.json()["code"] == "device_required"
    assert requests.post(f"{base}/api/terminal/relay", json={}, headers=spoof, timeout=5).status_code in (404, 405)


def test_the_store_wins_and_events_to_main(rig):
    agent, term, fm = rig["agent"], rig["term"], rig["fm"]
    base = agent.base
    seen: list[dict] = []
    fm.rpc["terminal.events"] = lambda payload: (seen.extend(payload["events"]), (200, {}))[1]
    draft = {"type": "pickup", "name": "Sam", "held": False, "draft": {"lines": []}}

    # Nothing is forwarded until main asks.
    r = term.put(f"{base}/api/terminal/drafts/d-store-0001", json=draft, timeout=5)
    assert r.status_code == 200, r.text
    fm.command("terminal.events.want", {"seconds": 120})

    # The store's terminal has it open: the call center gets open_elsewhere, take_over too.
    got = relay(rig, "PUT", "drafts/d-store-0001", draft)
    assert got["status"] == 409 and got["body"]["code"] == "open_elsewhere" and got["body"]["holder"]["device_name"] == "Till 1", got
    got = relay(rig, "PUT", "drafts/d-store-0001", {**draft, "take_over": True})
    assert got["status"] == 409 and got["body"]["code"] == "open_elsewhere", got
    assert relay(rig, "DELETE", "drafts/d-store-0001")["status"] == 409
    held = term.get(f"{base}/api/terminal/drafts", timeout=5).json()["drafts"]
    assert [d["holder"]["device_name"] for d in held] == ["Till 1"]
    assert [e["device_id"] for e in term.get(f"{base}/api/terminal/journal?limit=50", timeout=5).json()
            if e["action"] == "draft.take_over_refused"] == ["callcenter:desk-1"]

    # A seat has it open: the store's terminal takes it over; the seat is told (its seat id in device_id).
    mine = relay(rig, "PUT", "drafts/d-seat-00001", draft)
    assert mine["status"] == 200 and mine["body"]["draft"]["holder"]["device_id"] == "callcenter:desk-1", mine
    r = term.put(f"{base}/api/terminal/drafts/d-seat-00001", json=draft, timeout=5)
    assert r.status_code == 409 and r.json()["code"] == "open_elsewhere"
    r = term.put(f"{base}/api/terminal/drafts/d-seat-00001", json={**draft, "take_over": True}, timeout=5)
    assert r.status_code == 200 and r.json()["draft"]["holder"]["device_name"] == "Till 1", r.text
    taken = wait_until(lambda: next((e for e in seen if e["event"] == "terminal.draft.taken"), None), what="terminal.draft.taken forwarded to main")
    assert taken["data"]["device_id"] == "callcenter:desk-1" and taken["data"]["id"] == "d-seat-00001" and taken["at"]
    # Between two seats: the usual take-over.
    assert relay(rig, "PUT", "drafts/d-seats-0001", draft)["status"] == 200
    assert relay(rig, "PUT", "drafts/d-seats-0001", draft, device="desk-2")["status"] == 409
    two = relay(rig, "PUT", "drafts/d-seats-0001", {**draft, "take_over": True}, device="desk-2")
    assert two["status"] == 200 and two["body"]["draft"]["holder"]["device_id"] == "callcenter:desk-2", two

    # An order the store has open for an edit: no change, no cancel from the call center.
    oid = relay(rig, "POST", "orders", {"client_key": f"cc-{uuid.uuid4()}", "type": "pickup", "cart": {"lines": [pizza_line()]},
                                       "customer": {"name": "Ada Test", "phone": "4105550101"}})["body"]["order"]["order"]["id"]
    r = term.put(f"{base}/api/terminal/drafts/d-edit-00001", json={**draft, "order_id": oid}, timeout=5)
    assert r.status_code == 200, r.text
    two_pizzas = {"client_key": f"c-{uuid.uuid4()}", "cart": {"lines": [pizza_line(), pizza_line()]}}
    assert relay(rig, "PUT", f"orders/{oid}/cart", two_pizzas)["body"]["code"] == "open_elsewhere"
    assert relay(rig, "POST", f"orders/{oid}/void", {"reason": "customer called back"})["body"]["code"] == "open_elsewhere"
    assert term.delete(f"{base}/api/terminal/drafts/d-edit-00001", timeout=5).status_code == 204
    assert relay(rig, "PUT", f"orders/{oid}/cart", two_pizzas)["status"] == 200

    # Every terminal event of the Pi reached main while it listened: the order placed, changed, the drafts.
    wait_until(lambda: {"terminal.order.placed", "terminal.order.changed", "terminal.draft.changed", "terminal.draft.removed", "order.updated"}
               <= {e["event"] for e in seen}, what="the Pi's events forwarded as terminal.events")
    assert all(set(e) == {"event", "data", "at"} for e in seen)
    assert next(e for e in seen if e["event"] == "terminal.order.placed")["data"]["order"]["order"]["id"] == oid


def test_edit_lock_on_a_sent_order(rig):
    """A seat's draft of a sent order is its lock: the store never lists or opens it (its body is the
    call-center app's own); the store wins by opening the order itself."""
    agent, term, fm = rig["agent"], rig["term"], rig["fm"]
    base = agent.base
    seen: list[dict] = []
    fm.rpc["terminal.events"] = lambda payload: (seen.extend(payload["events"]), (200, {}))[1]
    relay(rig, "GET", "session")  # the stream is up and the agent known before main asks for events
    fm.command("terminal.events.want", {"seconds": 120})
    placed = relay(rig, "POST", "orders", {**order_body(), "cart": {"lines": [pizza_line(), {"item_id": "00000000-0000-0000-0000-0000000000a6", "qty": 1}]}})
    assert placed["status"] == 201, placed
    x = placed["body"]["order"]["order"]["id"]
    total = placed["body"]["order"]["order"]["money"]["total_cents"]

    # The seat locks X: a draft with order_id and no lines (the app's own body).
    lock = {"type": "pickup", "name": "Ada Test", "held": False, "order_id": x, "draft": {"cc": {"step": "menu"}}}
    assert relay(rig, "PUT", "drafts/d-cc-lock-01", lock)["status"] == 200
    assert term.get(f"{base}/api/terminal/drafts", timeout=5).json() == {"drafts": []}  # the store's terminal doesn't list it
    assert agent.http.get(f"{base}/api/terminal/drafts", timeout=5).json() == {"drafts": []}  # nor the Pi web page
    assert [d["id"] for d in relay(rig, "GET", "drafts")["body"]["drafts"]] == ["d-cc-lock-01"]
    wait_until(lambda: any(e["event"] == "terminal.draft.changed" and e["data"]["draft"]["id"] == "d-cc-lock-01" for e in seen),
               what="the seat's draft.changed reaches main")

    # T1 opens X for changes with its own draft (no take_over): it wins, the seat's lock is gone and the seat is told.
    mine = {"type": "pickup", "name": "Ada Test", "held": False, "order_id": x, "draft": {"lines": [{"item_id": "x"}]}}
    r = term.put(f"{base}/api/terminal/drafts/d-till-edit1", json=mine, timeout=5)
    assert r.status_code == 200 and r.json()["draft"]["holder"]["device_name"] == "Till 1", r.text
    assert [d["id"] for d in relay(rig, "GET", "drafts")["body"]["drafts"]] == ["d-till-edit1"]
    taken = wait_until(lambda: next((e["data"] for e in seen if e["event"] == "terminal.draft.taken" and e["data"]["id"] == "d-cc-lock-01"), None),
                       what="terminal.draft.taken for the seat")
    assert taken["order_id"] == x and taken["device_id"] == "callcenter:desk-1" and taken["by"]["device_name"] == "Till 1"
    over = [e for e in term.get(f"{base}/api/terminal/journal?limit=50", timeout=5).json() if e["action"] == "draft.take_over"]
    assert [e["detail"]["from_device_id"] for e in over] == ["callcenter:desk-1"] and over[0]["order_id"] == x

    # The seat's late calls on X: open_elsewhere; X's lines are untouched.
    late = {"client_key": f"c-{uuid.uuid4()}", "draft_id": "d-cc-lock-01", "cart": {"lines": [pizza_line()]}}
    for got in (relay(rig, "PUT", f"orders/{x}/cart", late),
                relay(rig, "POST", f"orders/{x}/void", {"reason": "customer called back"}),
                relay(rig, "POST", f"orders/{x}/payments", {"client_key": f"p-{uuid.uuid4()}", "tender": "manual_card", "amount_cents": 100}),
                relay(rig, "PUT", "drafts/d-cc-lock-02", lock),
                relay(rig, "PUT", "drafts/d-cc-lock-02", {**lock, "take_over": True})):
        assert got["status"] == 409 and got["body"]["code"] == "open_elsewhere" and got["body"]["holder"]["device_name"] == "Till 1", got
    after = term.get(f"{base}/api/terminal/orders/{x}", timeout=5).json()
    assert len(after["order"]["order"]["items"]) == 2 and after["order"]["order"]["money"]["total_cents"] == total and after["payments"] == []
    assert after["order"]["status"]["state"] == "accepted"

    # T1 sends its change and lets go; the seat can lock X again.
    # (A store cashier taking a sent line off needs a manager, contract 2.10ac §12: the PIN rides the change.)
    r = term.put(f"{base}/api/terminal/orders/{x}/cart", json={"draft_id": "d-till-edit1", "cart": {"lines": [pizza_line()]},
                                                             "manager_pin": "9876"}, timeout=10)
    assert r.status_code == 200 and r.json()["removed"] == ["Soda"], r.text
    assert relay(rig, "PUT", "drafts/d-cc-lock-02", lock)["status"] == 200
    assert term.get(f"{base}/api/terminal/drafts", timeout=5).json() == {"drafts": []}


def test_quick_add_from_the_call_center_prints_where_the_store_routes_it(rig):
    """Quick add (owner 2026-11-13: the call center and the store's terminals are its only users): the call center's
    custom line is placed like the terminal's Quicc Add and printed where main's
    Quick add item routes (counter menu `quick_add_item_id` + its route): receipt only, then the Pizza station."""
    from test_terminal import counter_menu
    agent, term, fm, fakes = rig["agent"], rig["term"], rig["fm"], rig["fakes"]
    quick = "00000000-0000-4000-8000-000000000a02"

    def use_menu(version: int, route: dict) -> None:
        menu = counter_menu(rig["pizza_id"], version=version)
        menu["quick_add_item_id"] = quick
        menu["routes"][quick] = route
        fm.counter_menu = menu
        fm.command("menu.changed", {"version": version})
        wait_until(lambda: term.get(f"{agent.base}/api/terminal/menu", timeout=5).json()["menu_version"] == version, what=f"menu v{version}")

    ranch = {"custom": True, "name": "Side of ranch", "price_cents": 75, "qty": 3}
    use_menu(21, {"printer_id": "", "no_kitchen": True})
    placed = relay(rig, "POST", "orders", order_body(cart={"lines": [pizza_line(), ranch]}))
    assert placed["status"] == 201, placed
    cu = placed["body"]["order"]["order"]["items"][1]
    assert (cu["sku"], cu["routing"], cu.get("menu_item_id")) == ("custom", "none", quick), cu
    wait_until(lambda: fakes["Pizza"].jobs, what="the pizza station ticket")
    assert "RANCH" not in fakes["Pizza"].text().upper(), fakes["Pizza"].text()

    use_menu(22, {"printer_id": rig["pizza_id"], "name": "Pizza"})
    fakes["Pizza"].clear()
    placed = relay(rig, "POST", "orders", order_body(cart={"lines": [ranch]}))
    assert placed["status"] == 201, placed
    cu = placed["body"]["order"]["order"]["items"][0]
    assert (cu["routing"], cu["printer_id"], cu["menu_item_id"]) == ("matched", rig["pizza_id"], quick), cu
    wait_until(lambda: "RANCH" in fakes["Pizza"].text().upper(), what="the call center's Quick add line on the Pizza station")
    # An open-price item's price is still the store's business (a price on a menu line stays refused).
    refused = relay(rig, "POST", "orders", order_body(cart={"lines": [{**pizza_line(), "price_cents": 500}]}))
    assert refused["status"] == 403 and refused["body"]["what"] == "price_change", refused


def test_takeout_two_customer_copies(rig):
    """main's takeout_two_copies (Store › Settings › Receipts and printing, the owner 2026-11-13; contract §2.10ah).
    Agent 2.5.7: the Pi prints no pickup receipt at placement any more (it prints at Cash Out on a terminal,
    which doubles it); on the Pi the setting never doubles a reprint, and a delivery keeps its Merchant +
    Customer Copy. Fake printers only."""
    agent, term, fm, fakes = rig["agent"], rig["term"], rig["fm"], rig["fakes"]
    base = agent.base

    def config(version: int, on: bool) -> None:
        fm.terminal_config = cc_config(version)
        fm.terminal_config["settings"]["takeout_two_copies"] = on
        fm.command("terminal.changed", {"version": version})
        wait_until(lambda: term.get(f"{base}/api/terminal/session", timeout=5).json().get("config_version") == version,
                   what=f"config v{version}")

    def last_job() -> str:
        return fakes["Front"].jobs[-1].decode("latin-1")

    config(3, True)
    fakes["Front"].clear()
    pizza_jobs = len(fakes["Pizza"].jobs)
    pick = relay(rig, "POST", "orders", order_body("pickup"))
    assert pick["status"] == 201, pick
    pid = pick["body"]["order"]["order"]["id"]
    wait_until(lambda: len(fakes["Pizza"].jobs) > pizza_jobs, what="the pickup's station ticket")

    # A reprint (a person's ask) prints one copy.
    rp = relay(rig, "POST", f"orders/{pid}/reprint")
    assert rp["status"] == 200, rp
    wait_until(lambda: fakes["Front"].jobs, what="the reprinted receipt")
    assert len(fakes["Front"].jobs) == 1, "nothing printed on Front for the pickup but its reprint"
    assert last_job().count("Customer Copy") == 1

    # A delivery: its Merchant + Customer Copy, unchanged.
    dl = relay(rig, "POST", "orders", order_body("delivery"))
    assert dl["status"] == 201, dl
    wait_until(lambda: len(fakes["Front"].jobs) == 2, what="the delivery tickets")
    job = last_job()
    assert job.count("Merchant Copy") == 1 and job.count("Customer Copy") == 1, job
