package main

import (
	"bytes"
	"encoding/json"
	"strings"
	"testing"

	"quiccpos/agent/internal/application/dispatch"
)

func TestRun(t *testing.T) {
	in := `{"now":"2026-10-22T18:00:00Z","target_minutes":45,"soon_ready_minutes":5,
	 "store":{"lat":40.6687,"lng":-74.1143},
	 "orders":[
	  {"id":"a","placed_at":"2026-10-22T17:30:00Z","ready":true,"lat":40.6687,"lng":-74.0900,"located":true},
	  {"id":"b","placed_at":"2026-10-22T17:40:00Z","ready_at":"2026-10-22T18:03:00Z","lat":40.6687,"lng":-74.0800,"located":true},
	  {"id":"c","placed_at":"2026-10-22T17:50:00Z","ready":true,"located":false}],
	 "drivers":[{"id":"d1","name":"Ann","pool":[]}],
	 "times":{"store":{"to":{"a":260,"b":360},"from":{"a":270,"b":370}},"pairs":{"a":{"b":110},"b":{"a":115}}},
	 "budget_ms":200}`
	var out bytes.Buffer
	if err := run(strings.NewReader(in), &out, true); err != nil {
		t.Fatal(err)
	}
	var plan dispatch.Output
	if err := json.Unmarshal(out.Bytes(), &plan); err != nil {
		t.Fatalf("%v\n%s", err, out.String())
	}
	if len(plan.Runs) != 1 || strings.Join(plan.Runs[0].OrderIDs, ",") != "a,b" || plan.Fallback {
		t.Fatalf("plan %s", out.String())
	}
	if len(plan.Unassigned) != 1 || plan.Unassigned[0].Reason != dispatch.ReasonNoLocation {
		t.Fatalf("unassigned %+v", plan.Unassigned)
	}
	if !strings.Contains(out.String(), "\n  \"runs\"") {
		t.Fatal("-pretty should indent")
	}
	if err := run(strings.NewReader("{"), &out, false); err == nil {
		t.Fatal("bad JSON accepted")
	}
}
