package escpos

import "testing"

func TestDialAddr(t *testing.T) {
	for in, want := range map[string]string{
		"192.168.1.50":    "192.168.1.50:9100",
		"127.0.0.1:9101":  "127.0.0.1:9101",
		"printer.lan":     "printer.lan:9100",
		"fe80::1":         "[fe80::1]:9100",
		"[fe80::1]:19100": "[fe80::1]:19100",
	} {
		if got := dialAddr(in); got != want {
			t.Errorf("dialAddr(%q) = %q, want %q", in, got, want)
		}
	}
}
