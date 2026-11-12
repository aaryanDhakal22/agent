// Package watchdog speaks systemd's sd_notify protocol (hardening F-6): READY=1
// once the agent is up, then WATCHDOG=1 at a third of WatchdogSec — but only
// while the agent is really alive (the one SQLite connection answers within
// 5 s, the SSE goroutine has ticked). A wedged agent (a stuck transaction
// holding the only connection, a dead SSE loop) stops pinging and systemd
// restarts it, where it used to stay "active" doing nothing. Without
// NOTIFY_SOCKET (dev, tests, a unit without Type=notify) it does nothing.
package watchdog

import (
	"context"
	"database/sql"
	"net"
	"os"
	"strconv"
	"time"

	"github.com/rs/zerolog"
)

// Notify sends one sd_notify state ("READY=1", "WATCHDOG=1", "STOPPING=1")
// to $NOTIFY_SOCKET; false when there is no socket.
func Notify(state string) (bool, error) {
	sock := os.Getenv("NOTIFY_SOCKET")
	if sock == "" {
		return false, nil
	}
	addr := &net.UnixAddr{Name: sock, Net: "unixgram"}
	if sock[0] == '@' { // abstract namespace
		addr.Name = "\x00" + sock[1:]
	}
	conn, err := net.DialUnix("unixgram", nil, addr)
	if err != nil {
		return false, err
	}
	defer conn.Close()
	if _, err := conn.Write([]byte(state)); err != nil {
		return false, err
	}
	return true, nil
}

// Interval is how often to ping: half of $WATCHDOG_USEC (systemd's own
// advice), else def. 0 when there is no watchdog.
func Interval(def time.Duration) time.Duration {
	if os.Getenv("NOTIFY_SOCKET") == "" {
		return 0
	}
	if us, err := strconv.ParseInt(os.Getenv("WATCHDOG_USEC"), 10, 64); err == nil && us > 0 {
		return time.Duration(us) * time.Microsecond / 2
	}
	return def
}

// Check is one liveness probe.
type Check func(ctx context.Context) error

// DBCheck: SELECT 1 on the agent's one connection within 5 s.
func DBCheck(db *sql.DB) Check {
	return func(ctx context.Context) error {
		cctx, cancel := context.WithTimeout(ctx, 5*time.Second)
		defer cancel()
		var one int
		return db.QueryRowContext(cctx, "SELECT 1").Scan(&one)
	}
}

// Run sends READY=1, then WATCHDOG=1 every interval while every check passes,
// and STOPPING=1 when ctx ends. No-op without NOTIFY_SOCKET.
func Run(ctx context.Context, interval time.Duration, checks []Check, logger zerolog.Logger) {
	if os.Getenv("NOTIFY_SOCKET") == "" || interval <= 0 {
		return
	}
	lg := logger.With().Str("module", "watchdog").Logger()
	if _, err := Notify("READY=1"); err != nil {
		lg.Warn().Err(err).Msg("sd_notify READY failed")
	}
	t := time.NewTicker(interval)
	defer t.Stop()
	failing := false
	for {
		select {
		case <-ctx.Done():
			_, _ = Notify("STOPPING=1")
			return
		case <-t.C:
		}
		var bad error
		for _, c := range checks {
			if err := c(ctx); err != nil {
				bad = err
				break
			}
		}
		if bad != nil {
			if !failing {
				lg.Error().Err(bad).Msg("Agent not healthy — withholding the systemd watchdog ping")
			}
			failing = true
			continue
		}
		failing = false
		if _, err := Notify("WATCHDOG=1"); err != nil {
			lg.Warn().Err(err).Msg("sd_notify WATCHDOG failed")
		}
	}
}
