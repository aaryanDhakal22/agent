// Package backup uploads a consistent copy of the agent's SQLite file to main
// every night (04:00 store time; main keeps the newest 2 per location). It is
// the last-resort copy for an SD card that dies with unsynced state: the live
// restore path is agent.state + the order mirror, this is the forensic one.
package backup

import (
	"context"
	"database/sql"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"sync"
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

	// Hardening F-7: the copy reads through its own read-only connection
	// (the agent's one connection stays free for orders), and only when the
	// floor is quiet and not around the auto-close minute.
	quiet  func(ctx context.Context) bool
	avoid  func() (hour, minute int, ok bool)
	roOnce sync.Once
	ro     *sql.DB
	roErr  error
	OpenRO func(path string) (*sql.DB, error) // tests swap it
}

// SetQuiet: the backup waits while quiet answers false (orders in play on
// the floor). nil = always quiet.
func (s *Service) SetQuiet(f func(ctx context.Context) bool) { s.quiet = f }

// SetAvoid: the backup never starts from 5 minutes before to 15 minutes
// after this store-time minute (the batch auto-close; ok=false = none).
func (s *Service) SetAvoid(f func() (hour, minute int, ok bool)) { s.avoid = f }

// openReadOnly opens the agent's file a second time, read-only, with its own
// connection (VACUUM INTO works on a read-only database).
func openReadOnly(path string) (*sql.DB, error) {
	db, err := sql.Open("sqlite", "file:"+path+"?mode=ro&_pragma=busy_timeout(5000)")
	if err != nil {
		return nil, err
	}
	db.SetMaxOpenConns(1)
	return db, nil
}

// Close closes the read-only connection (shutdown).
func (s *Service) Close() {
	if s.ro != nil {
		s.ro.Close()
	}
}

// reader is the connection VACUUM INTO runs on: the read-only one when the
// file path is known, else the shared handle (tests).
func (s *Service) reader() *sql.DB {
	if s.dbPath == "" {
		return s.db
	}
	s.roOnce.Do(func() {
		open := s.OpenRO
		if open == nil {
			open = openReadOnly
		}
		s.ro, s.roErr = open(s.dbPath)
	})
	if s.roErr != nil || s.ro == nil {
		s.logger.Warn().Err(s.roErr).Msg("read-only connection for the backup unavailable; using the shared one")
		return s.db
	}
	return s.ro
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
	if s.held(ctx, now) {
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

// held: not now — orders are in play (the backup reads the whole file), or
// it is the auto-close's minute (Close batch writes a lot at once). The
// ">26 h old" rule used to start a backup at dinner time after an outage.
func (s *Service) held(ctx context.Context, now time.Time) bool {
	if s.quiet != nil && !s.quiet(ctx) {
		return true
	}
	if s.avoid == nil {
		return false
	}
	h, m, ok := s.avoid()
	if !ok {
		return false
	}
	loc := time.Local
	if s.loc != nil {
		loc = s.loc()
	}
	n := now.In(loc)
	at := time.Date(n.Year(), n.Month(), n.Day(), h, m, 0, 0, loc)
	for _, a := range []time.Time{at.AddDate(0, 0, -1), at, at.AddDate(0, 0, 1)} {
		if !n.Before(a.Add(-5*time.Minute)) && n.Before(a.Add(15*time.Minute)) {
			return true
		}
	}
	return false
}

// Now takes a backup and uploads it; returns the uploaded size.
func (s *Service) Now(ctx context.Context) (int, error) {
	// The copy goes to the temp dir, not next to the database: on the Pi the
	// agent unit has PrivateTmp and /tmp is RAM, so the nightly copy costs no
	// SD-card writes.
	tmp := filepath.Join(s.TempDir, fmt.Sprintf("quicc-backup-%d.db", time.Now().UnixNano()))
	defer os.Remove(tmp)
	// VACUUM INTO writes a consistent, compacted copy without stopping writers.
	if _, err := s.reader().ExecContext(ctx, "VACUUM INTO ?", tmp); err != nil {
		return 0, fmt.Errorf("vacuum into: %w", err)
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
