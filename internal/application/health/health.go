// Package health is the agent's heartbeat (RPC report `agent.health`, every
// 5 minutes + on connect): disk, temperature, memory, uptime, SD-card errors,
// the order/floor backlogs and the last backup — stored by main for the fleet
// view, and exported as OTEL gauges for alerts.
//
// SD-card errors and under-voltage come from a small root timer on the Pi
// (`quicc-health.timer`) that writes HealthFile; without it (dev, tests) those
// fields are simply absent.
package health

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/rs/zerolog"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/metric"
)

// MsgAgentHealth is the report type main stores.
const MsgAgentHealth = "agent.health"

// DefaultHealthFile is written by the Pi's root health timer.
const DefaultHealthFile = "/var/lib/quicc-agent/health.json"

type Reporter func(ctx context.Context, msgType string, payload any) error

// Report is one heartbeat.
type Report struct {
	At             time.Time      `json:"at"`
	DeviceID       string         `json:"device_id,omitempty"`
	Version        string         `json:"version"`
	Hostname       string         `json:"hostname"`
	ProcessUptimeS int64          `json:"process_uptime_s"`
	SystemUptimeS  int64          `json:"system_uptime_s,omitempty"`
	DiskFreeBytes  uint64         `json:"disk_free_bytes"`
	DiskTotalBytes uint64         `json:"disk_total_bytes"`
	DBBytes        int64          `json:"db_bytes"`
	TempC          float64        `json:"temp_c,omitempty"`
	MemAvailableKB int64          `json:"mem_available_kb,omitempty"`
	Load1          float64        `json:"load1,omitempty"`
	SSEConnected   bool           `json:"sse_connected"`
	SSEReconnects  int64          `json:"sse_reconnects"`
	AcksPending    int            `json:"acks_pending"`
	FloorBacklog   int64          `json:"floor_backlog"`
	PrintersUp     int            `json:"printers_up"`
	PrintersDown   int            `json:"printers_down"`
	LastBackupAt   string         `json:"last_backup_at,omitempty"`
	System         map[string]any `json:"system,omitempty"`  // from HealthFile (sd_errors, throttled, …)
	Pi             SystemHealth   `json:"pi"`                // System, parsed (throttled bits, staleness)
	Update         any            `json:"update,omitempty"`  // update.Health: versions, update state, last result
	Tablets        any            `json:"tablets,omitempty"` // 2.2.0: paired tablets + their Quibase version
	Problems       []string       `json:"problems,omitempty"`
}

// Inputs are the live readings the heartbeat can't take itself.
type Inputs struct {
	DeviceID     func(ctx context.Context) string
	SSE          func() (connected bool, reconnects int64)
	AcksPending  func() int
	FloorBacklog func(ctx context.Context) int64
	Printers     func() (up, down int)
	LastBackup   func(ctx context.Context) string
	// Update is the Pi update block (update.Requester.Health).
	Update func(ctx context.Context) any
	// Tablets are the paired tablets with their Quibase version (2.2.0).
	Tablets func(ctx context.Context) any
}

type Heartbeat struct {
	in         Inputs
	report     Reporter
	configured func() bool
	dbPath     string
	version    string
	started    time.Time
	logger     zerolog.Logger

	// HealthFile is the root timer's output (overridable for tests).
	HealthFile string

	mu   sync.Mutex
	last Report
	wake chan struct{}
}

func New(in Inputs, report Reporter, configured func() bool, dbPath, version string, logger zerolog.Logger) *Heartbeat {
	h := &Heartbeat{in: in, report: report, configured: configured, dbPath: dbPath, version: version,
		started: time.Now(), HealthFile: DefaultHealthFile, wake: make(chan struct{}, 1),
		logger: logger.With().Str("module", "health").Logger()}
	if v := os.Getenv("AGENT_HEALTH_FILE"); v != "" {
		h.HealthFile = v
	}
	h.registerGauges()
	return h
}

// Wake sends a heartbeat soon (on connect).
func (h *Heartbeat) Wake() {
	select {
	case h.wake <- struct{}{}:
	default:
	}
}

// Last is the newest collected report (for /api/health).
func (h *Heartbeat) Last() Report {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.last
}

func (h *Heartbeat) Run(ctx context.Context, every time.Duration) {
	t := time.NewTicker(every)
	defer t.Stop()
	h.collect(ctx)
	for {
		select {
		case <-ctx.Done():
			return
		case <-h.wake:
		case <-t.C:
		}
		r := h.collect(ctx)
		if h.configured != nil && !h.configured() {
			continue
		}
		rctx, cancel := context.WithTimeout(ctx, 10*time.Second)
		if err := h.report(rctx, MsgAgentHealth, r); err != nil {
			h.logger.Debug().Err(err).Msg("agent.health not sent")
		}
		cancel()
		if len(r.Problems) > 0 {
			h.logger.Warn().Ctx(ctx).Strs("problems", r.Problems).Msg("health problems")
		}
	}
}

// Thresholds that make a reading a "problem" (logged + listed for alerts).
const (
	minFreeBytes = 500 << 20 // 500 MB
	maxTempC     = 80
)

// Snapshot takes a fresh reading now (GET /api/diag, the diag.report RPC) —
// the same report the heartbeat sends, without sending it.
func (h *Heartbeat) Snapshot(ctx context.Context) Report { return h.collect(ctx) }

func (h *Heartbeat) collect(ctx context.Context) Report {
	r := Report{At: time.Now().UTC(), Version: h.version, ProcessUptimeS: int64(time.Since(h.started).Seconds())}
	r.Hostname, _ = os.Hostname()
	if h.in.DeviceID != nil {
		r.DeviceID = h.in.DeviceID(ctx)
	}
	var fs syscall.Statfs_t
	if err := syscall.Statfs(filepath.Dir(h.dbPath), &fs); err == nil {
		r.DiskFreeBytes = fs.Bavail * uint64(fs.Bsize)
		r.DiskTotalBytes = fs.Blocks * uint64(fs.Bsize)
	}
	if st, err := os.Stat(h.dbPath); err == nil {
		r.DBBytes = st.Size()
	}
	if b, err := os.ReadFile("/sys/class/thermal/thermal_zone0/temp"); err == nil {
		if v, err := strconv.ParseFloat(strings.TrimSpace(string(b)), 64); err == nil {
			r.TempC = v / 1000
		}
	}
	if v, ok := HostUptime(); ok {
		r.SystemUptimeS = int64(v)
	}
	if b, err := os.ReadFile("/proc/loadavg"); err == nil {
		if f := strings.Fields(string(b)); len(f) > 0 {
			r.Load1, _ = strconv.ParseFloat(f[0], 64)
		}
	}
	if b, err := os.ReadFile("/proc/meminfo"); err == nil {
		for _, line := range strings.Split(string(b), "\n") {
			if strings.HasPrefix(line, "MemAvailable:") {
				if f := strings.Fields(line); len(f) >= 2 {
					r.MemAvailableKB, _ = strconv.ParseInt(f[1], 10, 64)
				}
			}
		}
	}
	if h.in.SSE != nil {
		r.SSEConnected, r.SSEReconnects = h.in.SSE()
	}
	if h.in.AcksPending != nil {
		r.AcksPending = h.in.AcksPending()
	}
	if h.in.FloorBacklog != nil {
		r.FloorBacklog = h.in.FloorBacklog(ctx)
	}
	if h.in.Printers != nil {
		r.PrintersUp, r.PrintersDown = h.in.Printers()
	}
	if h.in.LastBackup != nil {
		r.LastBackupAt = h.in.LastBackup(ctx)
	}
	if h.in.Update != nil {
		r.Update = h.in.Update(ctx)
	}
	if h.in.Tablets != nil {
		r.Tablets = h.in.Tablets(ctx)
	}
	if b, err := os.ReadFile(h.HealthFile); err == nil {
		var sys map[string]any
		if json.Unmarshal(b, &sys) == nil {
			r.System = sys
		}
	}
	r.Pi = ParseSystem(r.System, r.At)

	if r.DiskTotalBytes > 0 && r.DiskFreeBytes < minFreeBytes {
		r.Problems = append(r.Problems, "disk_low")
	}
	if r.TempC >= maxTempC {
		r.Problems = append(r.Problems, "hot")
	}
	if n, ok := r.System["sd_errors"].(float64); ok && n > 0 {
		r.Problems = append(r.Problems, "sd_errors")
	}
	if th, ok := r.System["throttled"].(string); ok && th != "" && th != "0x0" {
		r.Problems = append(r.Problems, "undervoltage_or_throttled")
	}
	if r.PrintersDown > 0 {
		r.Problems = append(r.Problems, "printer_down")
	}
	h.mu.Lock()
	h.last = r
	h.mu.Unlock()
	return r
}

// HostUptime is seconds since the machine booted (/proc/uptime); ok=false
// where that can't be read (not Linux). The Pi's health file doesn't carry
// it: the kernel's own counter is read directly.
func HostUptime() (float64, bool) {
	b, err := os.ReadFile("/proc/uptime")
	if err != nil {
		return 0, false
	}
	f := strings.Fields(string(b))
	if len(f) == 0 {
		return 0, false
	}
	v, err := strconv.ParseFloat(f[0], 64)
	return v, err == nil
}

// registerGauges exports the newest readings (agent.disk.free, agent.temp,
// agent.sd.errors) so Grafana can alert on them.
func (h *Heartbeat) registerGauges() {
	m := otel.Meter("quiccpos/agent")
	disk, err1 := m.Int64ObservableGauge("agent.disk.free", metric.WithUnit("By"), metric.WithDescription("Free bytes on the agent's data disk"))
	temp, err2 := m.Float64ObservableGauge("agent.temp", metric.WithUnit("Cel"), metric.WithDescription("SoC temperature"))
	sd, err3 := m.Int64ObservableGauge("agent.sd.errors", metric.WithDescription("SD-card I/O errors seen by the kernel since boot"))
	if err1 != nil || err2 != nil || err3 != nil {
		return
	}
	_, _ = m.RegisterCallback(func(_ context.Context, o metric.Observer) error {
		r := h.Last()
		if r.At.IsZero() {
			return nil
		}
		o.ObserveInt64(disk, int64(r.DiskFreeBytes))
		if r.TempC > 0 {
			o.ObserveFloat64(temp, r.TempC)
		}
		if n, ok := r.System["sd_errors"].(float64); ok {
			o.ObserveInt64(sd, int64(n))
		}
		return nil
	}, disk, temp, sd)
}
