package printernet

import (
	"context"
	"encoding/json"
	"net"
	"sync"
	"testing"
	"time"

	printerApp "quiccpos/agent/internal/application/printer"
	"quiccpos/agent/internal/domain/printer"
	"quiccpos/agent/internal/infra/netscan"

	"github.com/rs/zerolog"
)

type memStore struct {
	mu     sync.Mutex
	m      map[string]printer.NetAddress
	writes int
}

func (s *memStore) ListMACs(context.Context) (map[string]printer.NetAddress, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := map[string]printer.NetAddress{}
	for k, v := range s.m {
		out[k] = v
	}
	return out, nil
}
func (s *memStore) SaveMAC(_ context.Context, m printer.NetAddress) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.writes++
	s.m[m.PrinterID] = m
	return nil
}

type rig struct {
	store   *memStore
	roster  []printerApp.RosterPrinter
	status  []printerApp.StatusSnapshot
	arp     map[string]string
	answers []string // hosts answering on 9100
	sweeps  int
	sends   []map[string]string
	applied bool
	pages   []string
	now     time.Time
}

func newRig() *rig {
	return &rig{
		store:  &memStore{m: map[string]printer.NetAddress{}},
		roster: []printerApp.RosterPrinter{{ID: "p-pizza", Name: "Pizza", IP: "192.168.1.40"}, {ID: "p-front", Name: "Front", IP: "192.168.1.41", IsReceipt: true}},
		arp:    map[string]string{"192.168.1.1": "a4:2b:b0:11:22:33", "192.168.1.40": "00:11:62:aa:bb:cc"},
		now:    time.Date(2026, 10, 21, 18, 0, 0, 0, time.UTC),
	}
}

func (g *rig) watcher() *Watcher {
	_, n, _ := net.ParseCIDR("192.168.1.0/24")
	w := New(Deps{
		Store:  g.store,
		Roster: func() []printerApp.RosterPrinter { return g.roster },
		Status: func() []printerApp.StatusSnapshot { return g.status },
		Send: func(_ context.Context, msgType string, payload any) (int, json.RawMessage, error) {
			if msgType != MsgPrinterMoved {
				panic(msgType)
			}
			g.sends = append(g.sends, payload.(map[string]string))
			if g.applied {
				return 200, json.RawMessage(`{"applied":true,"reason":""}`), nil
			}
			return 200, json.RawMessage(`{"applied":false,"reason":"stale_old_ip"}`), nil
		},
		Notify: func(_ context.Context, msg, _ string) error { g.pages = append(g.pages, msg); return nil },
		ARP:    func() (map[string]string, error) { return g.arp, nil },
		Nets: func() []netscan.LocalNet {
			return []netscan.LocalNet{{Iface: "eth0", IP: net.ParseIP("192.168.1.219").To4(), Net: n}}
		},
		Sweep: func(_ context.Context, hosts []string) []string {
			g.sweeps++
			if len(hosts) != 253 {
				panic("hosts")
			}
			return g.answers
		},
		Now: func() time.Time { return g.now },
	}, zerolog.Nop())
	w.Load(context.Background())
	return w
}

func TestLearnMAC(t *testing.T) {
	g := newRig()
	w := g.watcher()
	ctx := context.Background()
	w.OnProbe(ctx, "p-pizza", "192.168.1.40", true)
	if w.MAC("p-pizza") != "00:11:62:aa:bb:cc" || g.store.writes != 1 {
		t.Fatalf("mac %q writes %d", w.MAC("p-pizza"), g.store.writes)
	}
	// Unchanged → no SD write.
	w.OnProbe(ctx, "p-pizza", "192.168.1.40", true)
	if g.store.writes != 1 {
		t.Fatalf("rewrote an unchanged MAC: %d", g.store.writes)
	}
	// Down probe, no ARP entry, off-link (the ARP table would hold the
	// gateway), fake host:port — none learn anything.
	w.OnProbe(ctx, "p-front", "192.168.1.41", false)
	w.OnProbe(ctx, "p-front", "192.168.1.41", true)
	g.arp["192.168.2.41"] = "a4:2b:b0:11:22:33"
	w.OnProbe(ctx, "p-front", "192.168.2.41", true)
	w.OnProbe(ctx, "p-front", "127.0.0.1:9100", true)
	if w.MAC("p-front") != "" || g.store.writes != 1 {
		t.Fatalf("learned %q", w.MAC("p-front"))
	}
	if w.MACByName("Pizza") != "00:11:62:aa:bb:cc" || w.MACByName("Nope") != "" {
		t.Fatal("MACByName")
	}
	// The same MAC at a new IP is a change worth writing.
	g.arp["192.168.1.47"] = "00:11:62:aa:bb:cc"
	w.OnProbe(ctx, "p-pizza", "192.168.1.47", true)
	if g.store.writes != 2 || g.store.m["p-pizza"].IP != "192.168.1.47" {
		t.Fatalf("move not stored: %+v", g.store.m)
	}
	// A restart reads them back.
	if g.watcher().MAC("p-pizza") != "00:11:62:aa:bb:cc" {
		t.Fatal("not loaded")
	}
}

func down(name string, since time.Time) printerApp.StatusSnapshot {
	return printerApp.StatusSnapshot{Name: name, Up: false, LastChecked: &since, DownSince: &since}
}

func TestFollowMovedPrinter(t *testing.T) {
	g := newRig()
	g.store.m["p-pizza"] = printer.NetAddress{PrinterID: "p-pizza", MAC: "00:11:62:aa:bb:cc", IP: "192.168.1.40"}
	w := g.watcher()
	ctx := context.Background()

	// Down for only 30 s: too early.
	g.status = []printerApp.StatusSnapshot{down("Pizza", g.now.Add(-30*time.Second))}
	w.Follow(ctx)
	if g.sweeps != 0 {
		t.Fatal("swept too early")
	}
	// Down 50 s: sweep; the MAC answers at .47 → printer.moved.
	g.status = []printerApp.StatusSnapshot{down("Pizza", g.now.Add(-50*time.Second))}
	g.answers = []string{"192.168.1.47", "192.168.1.60"}
	g.arp = map[string]string{"192.168.1.47": "00:11:62:aa:bb:cc", "192.168.1.60": "00:11:62:99:99:99"}
	w.Follow(ctx)
	if g.sweeps != 1 || len(g.sends) != 1 {
		t.Fatalf("sweeps %d sends %v", g.sweeps, g.sends)
	}
	want := map[string]string{"printer_id": "p-pizza", "old_ip": "192.168.1.40", "new_ip": "192.168.1.47", "mac": "00:11:62:aa:bb:cc"}
	for k, v := range want {
		if g.sends[0][k] != v {
			t.Fatalf("payload %v", g.sends[0])
		}
	}
	if len(g.pages) != 0 {
		t.Fatal("paged although main didn't apply it")
	}
	// Rate limit: another check a minute later doesn't sweep.
	g.now = g.now.Add(time.Minute)
	w.Follow(ctx)
	if g.sweeps != 1 {
		t.Fatal("swept twice within 2 minutes")
	}
	// After 2 min: sweep again; this time main applies it.
	g.now = g.now.Add(90 * time.Second)
	g.applied = true
	w.Follow(ctx)
	if g.sweeps != 2 || len(g.sends) != 2 || len(g.pages) != 1 {
		t.Fatalf("sweeps %d sends %d pages %v", g.sweeps, len(g.sends), g.pages)
	}
	if g.pages[0] != "Printer Pizza moved 192.168.1.40 → 192.168.1.47 — updated automatically" {
		t.Fatalf("page %q", g.pages[0])
	}
	if g.store.m["p-pizza"].IP != "192.168.1.47" {
		t.Fatal("new IP not stored")
	}
}

func TestFollowSkips(t *testing.T) {
	g := newRig()
	w := g.watcher()
	ctx := context.Background()
	// Down long enough but no MAC known → no sweep.
	g.status = []printerApp.StatusSnapshot{down("Pizza", g.now.Add(-5*time.Minute))}
	w.Follow(ctx)
	if g.sweeps != 0 {
		t.Fatal("swept for a printer with no known MAC")
	}
	// MAC known, still at the same IP (just off) → sweep, but nothing sent.
	g.store.m["p-pizza"] = printer.NetAddress{PrinterID: "p-pizza", MAC: "00:11:62:aa:bb:cc", IP: "192.168.1.40"}
	w = g.watcher()
	g.answers = []string{"192.168.1.40"}
	g.arp = map[string]string{"192.168.1.40": "00:11:62:aa:bb:cc"}
	w.Follow(ctx)
	if g.sweeps != 1 || len(g.sends) != 0 {
		t.Fatalf("sweeps %d sends %v", g.sweeps, g.sends)
	}
}

func TestFoundList(t *testing.T) {
	g := newRig()
	g.store.m["p-front"] = printer.NetAddress{PrinterID: "p-front", MAC: "00:11:62:00:00:01", IP: "192.168.1.41"}
	w := g.watcher()
	if v := w.FoundList(context.Background(), false); v.ScannedAt != nil || len(v.Printers) != 0 || v.Printers == nil {
		t.Fatalf("before a sweep: %+v", v)
	}
	g.answers = []string{"192.168.1.40", "192.168.1.52", "192.168.1.60"}
	// .40 is Pizza by IP; .52 is Front by MAC (moved, not followed yet); .60 is new.
	g.arp = map[string]string{"192.168.1.52": "00:11:62:00:00:01", "192.168.1.60": "00:11:62:99:99:99"}
	v := w.FoundList(context.Background(), true)
	if v.ScannedAt == nil || v.Scanning || len(v.Printers) != 3 {
		t.Fatalf("view %+v", v)
	}
	if p := v.Printers[0]; p.IP != "192.168.1.60" || p.Known || p.MAC != "00:11:62:99:99:99" {
		t.Fatalf("unknown first: %+v", v.Printers)
	}
	names := map[string]string{}
	for _, p := range v.Printers[1:] {
		if !p.Known {
			t.Fatalf("%+v", p)
		}
		names[p.IP] = p.PrinterName
	}
	if names["192.168.1.40"] != "Pizza" || names["192.168.1.52"] != "Front" {
		t.Fatalf("names %v", names)
	}
	if u := w.Unknown(); len(u) != 1 || u[0].IP != "192.168.1.60" {
		t.Fatalf("unknown %v", u)
	}
	// A refresh right after reuses the sweep.
	w.FoundList(context.Background(), true)
	if g.sweeps != 1 {
		t.Fatalf("sweeps %d", g.sweeps)
	}
}
