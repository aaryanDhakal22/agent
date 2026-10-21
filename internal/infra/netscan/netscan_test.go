package netscan

import (
	"context"
	"errors"
	"net"
	"strings"
	"sync/atomic"
	"testing"
)

const arpSample = `IP address       HW type     Flags       HW address            Mask     Device
192.168.1.1      0x1         0x2         a4:2b:b0:11:22:33     *        eth0
192.168.1.40     0x1         0x2         00:11:62:AA:BB:CC     *        eth0
192.168.1.41     0x1         0x0         00:00:00:00:00:00     *        eth0
192.168.1.42     0x1         0x0         00:11:62:dd:ee:ff     *        eth0
192.168.1.43     0x1         0x6         00:11:62:00:00:01     *        eth0
garbage
`

func TestParseARP(t *testing.T) {
	m := ParseARP(strings.NewReader(arpSample))
	if m["192.168.1.40"] != "00:11:62:aa:bb:cc" {
		t.Fatalf("complete entry: %v", m)
	}
	if _, ok := m["192.168.1.41"]; ok {
		t.Fatal("incomplete all-zero entry kept")
	}
	if _, ok := m["192.168.1.42"]; ok {
		t.Fatal("entry without ATF_COM kept")
	}
	if m["192.168.1.43"] != "00:11:62:00:00:01" {
		t.Fatal("permanent+complete entry dropped")
	}
	if len(m) != 3 {
		t.Fatalf("got %v", m)
	}
}

func lnet(cidr, own string) LocalNet {
	_, n, _ := net.ParseCIDR(cidr)
	return LocalNet{Iface: "eth0", IP: net.ParseIP(own).To4(), Net: n}
}

func TestOnLinkGatewayCase(t *testing.T) {
	nets := []LocalNet{lnet("192.168.1.0/24", "192.168.1.219")}
	if !OnLink(net.ParseIP("192.168.1.40"), nets) {
		t.Fatal("on-link printer")
	}
	// A printer on another subnet is reached through the router: its "MAC"
	// would be the gateway's — never trusted.
	if OnLink(net.ParseIP("192.168.2.40"), nets) || OnLink(net.ParseIP("127.0.0.1"), nets) {
		t.Fatal("off-link address trusted")
	}
	if HostOf("127.0.0.1:9100") != "127.0.0.1" || HostOf("192.168.1.4") != "192.168.1.4" {
		t.Fatal("HostOf")
	}
	if NormalizeMAC("00-11-62-AA-BB-CC") != "00:11:62:aa:bb:cc" || NormalizeMAC("00:00:00:00:00:00") != "" || NormalizeMAC("x") != "" {
		t.Fatal("NormalizeMAC")
	}
}

func TestSweepHosts(t *testing.T) {
	h := SweepHosts([]LocalNet{lnet("192.168.1.0/24", "192.168.1.219"), lnet("10.0.0.0/8", "10.1.2.3")})
	if len(h) != 253+253 {
		t.Fatalf("got %d hosts", len(h))
	}
	for _, x := range h {
		if x == "192.168.1.219" || x == "10.1.2.3" || strings.HasSuffix(x, ".0") || strings.HasSuffix(x, ".255") {
			t.Fatalf("swept %s", x)
		}
	}
	if !strings.HasPrefix(h[300], "10.1.2.") {
		t.Fatalf("a /8 sweeps only the /24 around us: %s", h[300])
	}
	// A /25 sweeps only its half.
	if n := len(SweepHosts([]LocalNet{lnet("192.168.5.0/25", "192.168.5.10")})); n != 126 {
		t.Fatalf("/25: %d", n)
	}
}

func TestSweepBounded(t *testing.T) {
	var inflight, peak atomic.Int32
	dial := func(_ context.Context, addr string) error {
		n := inflight.Add(1)
		for {
			p := peak.Load()
			if n <= p || peak.CompareAndSwap(p, n) {
				break
			}
		}
		defer inflight.Add(-1)
		if addr == "192.168.1.47:9100" || addr == "192.168.1.60:9100" {
			return nil
		}
		return errors.New("refused")
	}
	hosts := SweepHosts([]LocalNet{lnet("192.168.1.0/24", "192.168.1.219")})
	got := Sweep(context.Background(), hosts, PrinterPort, 8, dial)
	if len(got) != 2 || got[0] != "192.168.1.47" || got[1] != "192.168.1.60" {
		t.Fatalf("got %v", got)
	}
	if peak.Load() > 8 {
		t.Fatalf("concurrency %d", peak.Load())
	}
}
