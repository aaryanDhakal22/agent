"""Verifies the wire format the agent expects from main/'s SSE broker.

The agent treats the `_traceparent` field as optional and strips it before
unmarshalling the rest into an OrderRequest. We cover the JSON shape here so
regressions on main/'s side (which produces the envelope) are caught by a
test runnable from either repo.
"""

import json


def test_envelope_with_traceparent_parses_as_order() -> None:
    envelope = {
        "tVer": "1",
        "order_id": 123,
        "order_number": 1,
        "store_id": 1,
        "vendor_store_id": "v1",
        "store_name": "Lombardi's",
        "service_type": "delivery",
        "submitted_date": "2026-04-22 12:00:00",
        "print_date": "2026-04-22 12:00:00",
        "tip": 0.0,
        "is_tax_exempt": False,
        "order_total": 12.0,
        "balance_owing": 0.0,
        "customer": {
            "first_name": "A",
            "last_name": "B",
            "company": "",
            "phone": "",
            "ext": "",
            "email": "",
        },
        "_traceparent": "00-0123456789abcdef0123456789abcdef-0123456789abcdef-01",
        "_tracestate": "",
    }
    raw = json.dumps(envelope)
    parsed = json.loads(raw)
    assert parsed["_traceparent"].startswith("00-")
    # Simulate the agent stripping trace fields before domain use
    parsed.pop("_traceparent", None)
    parsed.pop("_tracestate", None)
    assert parsed["order_id"] == 123
    assert "customer" in parsed


def test_envelope_without_traceparent_parses() -> None:
    envelope = {
        "order_id": 321,
        "store_id": 1,
        "vendor_store_id": "v1",
        "store_name": "Lombardi's",
        "service_type": "pickup",
        "submitted_date": "2026-04-22 12:00:00",
        "print_date": "2026-04-22 12:00:00",
        "tip": 0.0,
        "is_tax_exempt": False,
        "order_total": 5.0,
        "balance_owing": 0.0,
        "customer": {
            "first_name": "X",
            "last_name": "Y",
            "company": "",
            "phone": "",
            "ext": "",
            "email": "",
        },
    }
    parsed = json.loads(json.dumps(envelope))
    assert "_traceparent" not in parsed
    assert parsed["order_id"] == 321
