package observability

import "testing"

// Guards the runtime-only failure where resource.Merge rejects conflicting
// schema URLs after an OTEL SDK upgrade (go build can't catch it).
func TestNewResourceMergesWithSDKDefault(t *testing.T) {
	res, err := newResource(Config{ServiceName: "test", AppEnv: "dev", Version: "0.0.0"})
	if err != nil {
		t.Fatalf("newResource: %v", err)
	}
	if res.SchemaURL() == "" {
		t.Fatal("expected the SDK default schema URL to be kept")
	}
}
