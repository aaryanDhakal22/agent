// Package printernet keeps track of where the roster printers live on the
// LAN. It learns each printer's MAC from the ARP table after a successful
// probe; when a printer has been down for a while it sweeps the local /24 on
// TCP 9100, and if the known MAC now answers at a new IP it tells main
// (RPC printer.moved), which updates the printer and republishes
// store_config. The same sweeps feed the "found on the network" list of
// printers that are not in the roster yet.
package printernet

import (
	"context"
	"encoding/json"
	"fmt"
	"net"
	"sort"
	"strings"
	"sync"
	"time"

	printerApp "quiccpos/agent/internal/application/printer"
	"quiccpos/agent/internal/domain/printer"
	"quiccpos/agent/internal/infra/netscan"

	"github.com/rs/zerolog"
)

// MsgPrinterMoved / MsgPrinterAdd are agent→main RPC sends (main replies).
const (
	MsgPrinterMoved = "printer.moved"
	MsgPrinterAdd   = "printer.add"
)

const (
	// DownBeforeFollow: a printer must have failed probes this long first.
	DownBeforeFollow = 45 * time.Second
	// FollowScanEvery: at most one auto-follow sweep per 2 minutes (global).
	FollowScanEvery = 2 * time.Minute
	// BackgroundScanEvery refreshes the found list while connected.
	BackgroundScanEvery = 10 * time.Minute
	// manualScanMin: ?refresh=1 within this of the last sweep reuses it.
	manualScanMin = 10 * time.Second
	// refreshWait: how long ?refresh=1 waits for the sweep to finish.
	refreshWait = 15 * time.Second
	checkEvery  = 15 * time.Second
	sweepBudget = 45 * time.Second
)

// Store persists learned MACs.
type Store interface {
	ListMACs(ctx context.Context) (map[string]printer.NetAddress, error)
	SaveMAC(ctx context.Context, m printer.NetAddress) error
}

// Sender is rpc.Registry.Send: main's HTTP status + reply payload.
type Sender func(ctx context.Context, msgType string, payload any) (int, json.RawMessage, error)

// Notifier pages the developer (notify.Notifier.Send).
type Notifier func(ctx context.Context, message, sound string) error

// Deps are the watcher's collaborators. Net hooks default to the real
// /proc/net/arp, interfaces and TCP dials; tests replace them.
type Deps struct {
	Store     Store
	Roster    func() []printerApp.RosterPrinter
	Status    func() []printerApp.StatusSnapshot
	Send      Sender
	Notify    Notifier
	Connected func() bool

	ARP   func() (map[string]string, error)
	Nets  func() []netscan.LocalNet
	Sweep func(ctx context.Context, hosts []string) []string
	Now   func() time.Time
}

// Found is one printer seen on the LAN (GET /api/printers/found).
type Found struct {
	IP          string `json:"ip"`
	MAC         string `json:"mac"`
	Known       bool   `json:"known"`
	PrinterName string `json:"printer_name"`
}

// FoundView is GET /api/printers/found.
type FoundView struct {
	ScannedAt *time.Time `json:"scanned_at"`
	Scanning  bool       `json:"scanning"`
	Printers  []Found    `json:"printers"`
}

type seen struct{ ip, mac string }

// Watcher learns MACs, follows moved printers and keeps the found list.
type Watcher struct {
	d      Deps
	logger zerolog.Logger

	mu        sync.Mutex
	macs      map[string]printer.NetAddress // printer id → learned
	seen      []seen                        // last sweep's responders
	scannedAt time.Time
	scanning  bool
	scanDone  chan struct{} // closed when the running sweep ends
	lastAuto  time.Time     // last auto-follow sweep
	wake      chan struct{}
}

func New(d Deps, logger zerolog.Logger) *Watcher {
	if d.ARP == nil {
		d.ARP = netscan.ReadARP
	}
	if d.Nets == nil {
		d.Nets = netscan.LocalNets
	}
	if d.Sweep == nil {
		dial := netscan.TCPDial(netscan.DefaultDialTimeout)
		d.Sweep = func(ctx context.Context, hosts []string) []string {
			return netscan.Sweep(ctx, hosts, netscan.PrinterPort, netscan.DefaultConcurrency, dial)
		}
	}
	if d.Now == nil {
		d.Now = time.Now
	}
	return &Watcher{d: d, macs: map[string]printer.NetAddress{}, wake: make(chan struct{}, 1),
		logger: logger.With().Str("module", "printer-net").Logger()}
}

// SetMain wires the RPC send + "connected to main" check (the registry is
// built after the roster is hydrated). Call before Run.
func (w *Watcher) SetMain(send Sender, connected func() bool) {
	w.d.Send, w.d.Connected = send, connected
}

// Load reads the learned MACs.
func (w *Watcher) Load(ctx context.Context) {
	m, err := w.d.Store.ListMACs(ctx)
	if err != nil {
		w.logger.Warn().Ctx(ctx).Err(err).Msg("printer MACs not loaded")
		return
	}
	w.mu.Lock()
	w.macs = m
	w.mu.Unlock()
}

// MAC returns the learned MAC for a printer id ("" if none).
func (w *Watcher) MAC(printerID string) string {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.macs[printerID].MAC
}

// MACByName is the Registry's lookup (StatusSnapshot.MAC).
func (w *Watcher) MACByName(name string) string {
	for _, p := range w.d.Roster() {
		if p.Name == name {
			return w.MAC(p.ID)
		}
	}
	return ""
}

// OnProbe is the dispatcher's probe hook: after a successful probe, read the
// printer's MAC from the ARP table (on-link addresses only — an off-link
// entry would be the gateway) and store it when the MAC or IP changed.
func (w *Watcher) OnProbe(ctx context.Context, printerID, addr string, up bool) {
	if !up || printerID == "" {
		return
	}
	host := netscan.HostOf(addr)
	ip := net.ParseIP(host)
	if ip == nil || !netscan.OnLink(ip, w.d.Nets()) {
		return
	}
	arp, _ := w.d.ARP()
	mac := arp[ip.To4().String()]
	if mac == "" {
		return
	}
	w.learn(ctx, printerID, mac, ip.To4().String())
}

func (w *Watcher) learn(ctx context.Context, printerID, mac, ip string) {
	w.mu.Lock()
	cur, ok := w.macs[printerID]
	if ok && cur.MAC == mac && cur.IP == ip {
		w.mu.Unlock()
		return // unchanged: no SD write
	}
	na := printer.NetAddress{PrinterID: printerID, MAC: mac, IP: ip, SeenAt: w.d.Now().UTC()}
	w.macs[printerID] = na
	w.mu.Unlock()
	if err := w.d.Store.SaveMAC(ctx, na); err != nil {
		w.logger.Warn().Ctx(ctx).Err(err).Msg("printer MAC not saved")
		return
	}
	w.logger.Info().Ctx(ctx).Str("printer_id", printerID).Str("mac", mac).Str("ip", ip).Msg("printer MAC learned")
}

// Run checks every 15 s for printers to follow, and sweeps for the found
// list every 10 minutes while connected to main (first one a minute in).
func (w *Watcher) Run(ctx context.Context) {
	t := time.NewTicker(checkEvery)
	defer t.Stop()
	nextBG := w.d.Now().Add(time.Minute)
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		case <-w.wake:
		}
		w.Follow(ctx)
		if now := w.d.Now(); !now.Before(nextBG) {
			nextBG = now.Add(BackgroundScanEvery)
			if w.d.Connected == nil || w.d.Connected() {
				w.scan(ctx)
			}
		}
	}
}

// candidate is a roster printer worth looking for.
type candidate struct {
	printerApp.RosterPrinter
	mac string
}

// Follow looks for down printers whose MAC is known; if any, sweeps (≤ 1 per
// 2 min) and reports each one found at a new address to main.
func (w *Watcher) Follow(ctx context.Context) {
	now := w.d.Now()
	status := map[string]printerApp.StatusSnapshot{}
	for _, s := range w.d.Status() {
		status[s.Name] = s
	}
	roster := w.d.Roster()
	var cands []candidate
	w.mu.Lock()
	for _, p := range roster {
		st, ok := status[p.Name]
		if !ok || st.Up || st.LastChecked == nil || st.DownSince == nil || now.Sub(*st.DownSince) < DownBeforeFollow {
			continue
		}
		if net.ParseIP(p.IP) == nil { // empty, or host:port (a fake printer)
			continue
		}
		if mac := w.macs[p.ID].MAC; mac != "" {
			cands = append(cands, candidate{p, mac})
		}
	}
	due := w.lastAuto.IsZero() || now.Sub(w.lastAuto) >= FollowScanEvery
	if len(cands) > 0 && due {
		w.lastAuto = now
	}
	w.mu.Unlock()
	if len(cands) == 0 || !due {
		return
	}
	names := make([]string, len(cands))
	for i, c := range cands {
		names[i] = c.Name
	}
	w.logger.Info().Ctx(ctx).Strs("printers", names).Msg("printer down — looking for it on the network")
	got := w.scan(ctx)
	for _, c := range cands {
		for _, s := range got {
			if s.mac == c.mac && s.ip != c.IP {
				w.moved(ctx, c, s.ip)
				break
			}
		}
	}
}

// moved asks main to update the printer's address.
func (w *Watcher) moved(ctx context.Context, c candidate, newIP string) {
	log := w.logger.With().Str("printer", c.Name).Str("printer_id", c.ID).Str("old_ip", c.IP).Str("new_ip", newIP).Str("mac", c.mac).Logger()
	if w.d.Send == nil {
		return
	}
	sctx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()
	status, body, err := w.d.Send(sctx, MsgPrinterMoved, map[string]string{
		"printer_id": c.ID, "old_ip": c.IP, "new_ip": newIP, "mac": c.mac,
	})
	if err != nil || status != 200 {
		log.Warn().Ctx(ctx).Err(err).Int("status", status).Msg("printer moved — main not told")
		return
	}
	var reply struct {
		Applied bool   `json:"applied"`
		Reason  string `json:"reason"`
	}
	_ = json.Unmarshal(body, &reply)
	if !reply.Applied {
		log.Warn().Ctx(ctx).Str("reason", reply.Reason).Msg("printer moved — main did not apply it")
		return
	}
	log.Info().Ctx(ctx).Msg("printer moved — address updated automatically")
	w.learn(ctx, c.ID, c.mac, newIP)
	if w.d.Notify != nil {
		msg := fmt.Sprintf("Printer %s moved %s → %s — updated automatically", c.Name, c.IP, newIP)
		if err := w.d.Notify(ctx, msg, "classical"); err != nil {
			log.Warn().Ctx(ctx).Err(err).Msg("Pushover failed")
		}
	}
}

// scan sweeps the local /24(s) once (single flight: a caller arriving while
// a sweep runs waits for it) and returns the responders with their MACs.
func (w *Watcher) scan(ctx context.Context) []seen {
	w.mu.Lock()
	if w.scanning {
		done := w.scanDone
		w.mu.Unlock()
		select {
		case <-done:
		case <-ctx.Done():
		}
		w.mu.Lock()
		defer w.mu.Unlock()
		return append([]seen(nil), w.seen...)
	}
	w.scanning = true
	w.scanDone = make(chan struct{})
	w.mu.Unlock()

	sctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), sweepBudget)
	hosts := netscan.SweepHosts(w.d.Nets())
	up := w.d.Sweep(sctx, hosts)
	cancel()
	arp, _ := w.d.ARP()
	res := make([]seen, 0, len(up))
	for _, h := range up {
		res = append(res, seen{ip: h, mac: arp[h]})
	}

	w.mu.Lock()
	w.seen, w.scannedAt, w.scanning = res, w.d.Now().UTC(), false
	close(w.scanDone)
	w.mu.Unlock()
	w.logger.Debug().Ctx(ctx).Int("hosts", len(hosts)).Int("printers", len(res)).Msg("LAN printer sweep done")
	return res
}

// FoundList is GET /api/printers/found. refresh sweeps now (waiting up to
// 15 s for it; a sweep from the last 10 s is reused).
func (w *Watcher) FoundList(ctx context.Context, refresh bool) FoundView {
	if refresh {
		w.mu.Lock()
		fresh := !w.scannedAt.IsZero() && w.d.Now().Sub(w.scannedAt) < manualScanMin
		w.mu.Unlock()
		if !fresh {
			done := make(chan struct{})
			go func() { w.scan(context.WithoutCancel(ctx)); close(done) }()
			select {
			case <-done:
			case <-time.After(refreshWait):
			case <-ctx.Done():
			}
		}
	}
	return w.view()
}

func (w *Watcher) view() FoundView {
	roster := w.d.Roster()
	w.mu.Lock()
	defer w.mu.Unlock()
	v := FoundView{Scanning: w.scanning, Printers: []Found{}}
	if !w.scannedAt.IsZero() {
		at := w.scannedAt
		v.ScannedAt = &at
	}
	for _, s := range w.seen {
		f := Found{IP: s.ip, MAC: s.mac}
		for _, p := range roster {
			if netscan.HostOf(p.IP) == s.ip || (s.mac != "" && strings.EqualFold(w.macs[p.ID].MAC, s.mac)) {
				f.Known, f.PrinterName = true, p.Name
				break
			}
		}
		v.Printers = append(v.Printers, f)
	}
	sort.SliceStable(v.Printers, func(i, j int) bool { return !v.Printers[i].Known && v.Printers[j].Known })
	return v
}

// Unknown lists the found printers that are not in the roster (printer.report).
func (w *Watcher) Unknown() []printerApp.FoundAddr {
	out := []printerApp.FoundAddr{}
	for _, f := range w.view().Printers {
		if !f.Known {
			out = append(out, printerApp.FoundAddr{IP: f.IP, MAC: f.MAC})
		}
	}
	return out
}
