"""Agent 2.3.11 settings history on a throwaway agent (fake printers only):

  * a settings PUT that changed something is kept with who made it — the
    paired tablet by name, or the Pi web page (manager session);
  * GET /api/settings/changes (device auth), newest first, survives a restart;
  * the developer-mode feed has the change as kind "settings".
"""

from __future__ import annotations

import requests

from test_floor import floor  # noqa: F401  (fixture)
from test_v220 import pair


def test_settings_changes_name_the_tablet(floor):  # noqa: F811
    a = floor
    tablet = pair(a, "Counter")

    assert requests.get(f"{a.base}/api/settings/changes", timeout=5).status_code == 401

    r = requests.put(f"{a.base}/api/settings/tickets", json={"online_card": False, "online_cash_delivery": False},
                     headers=tablet, timeout=5)
    assert r.status_code == 200, r.text
    # The same values again: nothing new.
    assert requests.put(f"{a.base}/api/settings/tickets", json={"online_card": False}, headers=tablet,
                        timeout=5).status_code == 200
    # The web page (a.http holds the manager session) puts one back.
    assert a.http.put(f"{a.base}/api/settings/tickets", json={"online_card": True}, timeout=5).status_code == 200

    def changes() -> list[dict]:
        r = requests.get(f"{a.base}/api/settings/changes", headers=tablet, timeout=5)
        assert r.status_code == 200, r.text
        assert r.json()["keep_days"] == 90
        return [c for c in r.json()["changes"] if c["section"] == "tickets"]  # provision also set deliveries

    got = changes()
    assert [c["by"]["kind"] for c in got] == ["web", "tablet"], got
    web, tab = got
    assert web["by"]["name"] == "Pi web page" and web["section"] == "tickets"
    assert web["changes"] == [{"key": "online_card", "from": False, "to": True}]
    assert tab["by"]["name"] == "Counter" and tab["by"]["device_id"]
    assert tab["changes"] == [
        {"key": "online_card", "from": True, "to": False},
        {"key": "online_cash_delivery", "from": True, "to": False},
    ]

    texts = [e["text"] for e in requests.get(f"{a.base}/api/dev/events?since=0", headers=tablet, timeout=5).json()["events"]
             if e["kind"] == "settings"]
    assert "Kitchen tickets: online_card on → off, online_cash_delivery on → off (tablet Counter)" in texts, texts

    # Kept in the Pi's database: still there after a restart.
    a.restart()
    assert changes() == got
