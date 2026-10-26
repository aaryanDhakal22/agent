package observability

import (
	"os"
	"strings"
	"testing"
)

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

func TestNewResourceStoreAttributes(t *testing.T) {
	get := func(cfg Config) map[string]string {
		res, err := newResource(cfg)
		if err != nil {
			t.Fatalf("newResource: %v", err)
		}
		m := map[string]string{}
		for _, kv := range res.Attributes() {
			m[string(kv.Key)] = kv.Value.Emit()
		}
		return m
	}
	hn, _ := os.Hostname()

	linked := get(Config{ServiceName: "quicc-agent", AppEnv: "staging", Tenant: "lombardis", Location: "main"})
	if linked["tenant"] != "lombardis" || linked["location"] != "main" {
		t.Fatalf("tenant/location = %q/%q", linked["tenant"], linked["location"])
	}
	if hn != "" && linked["service.instance.id"] != "lombardis-"+hn {
		t.Fatalf("service.instance.id = %q, want lombardis-%s", linked["service.instance.id"], hn)
	}

	blank := get(Config{ServiceName: "quicc-agent", AppEnv: "dev"})
	if blank["tenant"] != Unlinked || blank["location"] != Unlinked {
		t.Fatalf("unlinked tenant/location = %q/%q", blank["tenant"], blank["location"])
	}
	if !strings.HasPrefix(blank["service.instance.id"], Unlinked+"-") {
		t.Fatalf("unlinked service.instance.id = %q", blank["service.instance.id"])
	}
}
