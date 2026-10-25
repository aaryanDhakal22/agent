// Package backup uploads a consistent copy of the agent's SQLite file to main
// every night (04:00 store time; main keeps the newest 2 per location). It is
// the last-resort copy for an SD card that dies with unsynced state: the live
// restore path is agent.state + the order mirror, this is the forensic one.
// POS orders (2.1.0) never reach main with their customer: the copy is
// scrubbed of their names, phones, addresses and points before it leaves.
package backup

import (
	"context"
	"database/sql"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"time"

	"github.com/rs/zerolog"
	_ "modernc.org/sqlite"
)

// Uploader posts the file (mainclient.Client.Upload bound to the current key).
type Uploader func(ctx context.Context, path string, body []byte, contentType string) (int, []byte, error)

type Settings interface {
	GetLastBackup(ctx context.Context) (string, error)
	SetLastBackup(ctx context.Context, at string) error
}

type Service struct {
	db         *sql.DB
	dbPath     string
	upload     Uploader
	settings   Settings
	configured func() bool
	loc        func() *time.Location
	logger     zerolog.Logger

	// Hour is the local hour the nightly backup runs at.
	Hour int
	// TempDir holds the VACUUM INTO copy while it uploads (os.TempDir()).
	TempDir string
}

func New(db *sql.DB, dbPath string, upload Uploader, settings Settings, configured func() bool, loc func() *time.Location, logger zerolog.Logger) *Service {
	return &Service{db: db, dbPath: dbPath, upload: upload, settings: settings, configured: configured, loc: loc,
		Hour: 4, TempDir: os.TempDir(), logger: logger.With().Str("module", "backup").Logger()}
}

// Run checks every 10 minutes whether a backup is due: once a day after Hour
// (store time), or right away when the last one is more than 26 h old.
func (s *Service) Run(ctx context.Context) {
	t := time.NewTicker(10 * time.Minute)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
		if !s.due(ctx, time.Now()) {
			continue
		}
		if _, err := s.Now(ctx); err != nil {
			s.logger.Warn().Ctx(ctx).Err(err).Msg("nightly backup failed; retrying in 10 minutes")
		}
	}
}

func (s *Service) due(ctx context.Context, now time.Time) bool {
	if s.configured != nil && !s.configured() {
		return false
	}
	raw, _ := s.settings.GetLastBackup(ctx)
	last, err := time.Parse(time.RFC3339, raw)
	if err != nil {
		return true // never backed up
	}
	if now.Sub(last) > 26*time.Hour {
		return true
	}
	loc := time.Local
	if s.loc != nil {
		loc = s.loc()
	}
	n := now.In(loc)
	slot := time.Date(n.Year(), n.Month(), n.Day(), s.Hour, 0, 0, 0, loc)
	return !n.Before(slot) && last.Before(slot)
}

// scrubLocalOnly clears the customer and address of every local-only (POS)
// order in the copy at path and drops their located points, then vacuums so
// no freed page still holds them.
func scrubLocalOnly(ctx context.Context, path string) error {
	db, err := sql.Open("sqlite", path)
	if err != nil {
		return err
	}
	defer db.Close()
	for _, q := range []string{
		`DELETE FROM order_geo WHERE order_id IN (SELECT id FROM orders WHERE local_only)`,
		`UPDATE orders SET snapshot = json_remove(json_set(snapshot, '$.customer', json('{}')), '$.fulfillment.address') WHERE local_only`,
		`VACUUM`,
	} {
		if _, err := db.ExecContext(ctx, q); err != nil {
			return err
		}
	}
	return nil
}

// Now takes a backup and uploads it; returns the uploaded size.
func (s *Service) Now(ctx context.Context) (int, error) {
	// The copy goes to the temp dir, not next to the database: on the Pi the
	// agent unit has PrivateTmp and /tmp is RAM, so the nightly copy costs no
	// SD-card writes.
	tmp := filepath.Join(s.TempDir, fmt.Sprintf("quicc-backup-%d.db", time.Now().UnixNano()))
	defer os.Remove(tmp)
	// VACUUM INTO writes a consistent, compacted copy without stopping writers.
	if _, err := s.db.ExecContext(ctx, "VACUUM INTO ?", tmp); err != nil {
		return 0, fmt.Errorf("vacuum into: %w", err)
	}
	if err := scrubLocalOnly(ctx, tmp); err != nil {
		return 0, fmt.Errorf("scrub POS orders: %w", err)
	}
	data, err := os.ReadFile(tmp)
	if err != nil {
		return 0, err
	}
	uctx, cancel := context.WithTimeout(ctx, 5*time.Minute)
	defer cancel()
	status, body, err := s.upload(uctx, "/store/v1/agent/backup", data, "application/vnd.sqlite3")
	if err != nil {
		return 0, err
	}
	if status != http.StatusCreated && status != http.StatusOK {
		return 0, fmt.Errorf("main answered %d: %.200s", status, body)
	}
	if err := s.settings.SetLastBackup(ctx, time.Now().UTC().Format(time.RFC3339)); err != nil {
		return len(data), err
	}
	s.logger.Info().Ctx(ctx).Int("bytes", len(data)).Msg("backup uploaded to main")
	return len(data), nil
}
