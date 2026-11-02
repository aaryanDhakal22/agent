"""2.4.2: a tablet joins a linked Pi with the store manager login
(POST /api/pair/manager) — for when the only tablet was reinstalled and there
is no working tablet to get a code from."""

from __future__ import annotations

import requests

from test_floor import floor  # noqa: F401  (fixture re-export)
from test_link import _manager


def _pair(a, user: str, pw: str, name: str = "Counter tablet") -> requests.Response:
    return requests.post(f"{a.base}/api/pair/manager", json={"username": user, "password": pw, "tablet_name": name}, timeout=30)


def test_pair_with_manager_login(admin, main_store, blank_agent) -> None:
    a = blank_agent
    user, pw = _manager(admin, main_store)
    r = _pair(a, user, pw)
    assert r.status_code == 409 and r.json()["code"] == "not_linked", r.text

    assert requests.post(f"{a.base}/api/setup/link", json={"username": user, "password": pw}, timeout=60).status_code == 200

    r = _pair(a, user, pw, name="")
    assert r.status_code == 400 and r.json()["code"] == "name_required"
    r = _pair(a, user, "nope")
    assert r.status_code == 401 and r.json()["code"] == "bad_login"

    r = _pair(a, user.upper(), pw)  # the username is case-insensitive, like the web login
    assert r.status_code == 201, r.text
    body = r.json()
    assert body["key"].startswith("qagt_") and body["kind"] == "tablet" and body["name"] == "Counter tablet"
    assert requests.get(f"{a.base}/api/floor", headers={"X-Agent-Device-Key": body["key"]}, timeout=5).status_code == 200

    # Wrong passwords hit the same per-IP throttle as the web login.
    codes = [_pair(a, user, "nope").status_code for _ in range(6)]
    assert codes[:5] == [401] * 5 and codes[5] == 429, codes


def test_pair_manager_needs_a_manager_login(floor) -> None:  # noqa: F811
    # A token-provisioned Pi (local password, no manager login): use a code.
    r = _pair(floor, "anyone", "aaryan")
    assert r.status_code == 409 and r.json()["code"] == "manager_login_unavailable", r.text
