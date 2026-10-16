// Package recovery brings a blank agent (a freshly installed or swapped SD
// card) back into service with nothing but the store manager login:
//
//  1. Link: main checks the login, mints this agent's key, revokes every
//     other agent of the store (an old Pi that comes back wipes itself) and
//     hands over the runtime secrets (Pushover, OTEL, Tailscale) + the manager
//     password hash (offline web-UI unlock).
//  2. Restore: the open batch comes back from main — orders with their floor
//     state, the kitchen counter, the drivers with queue + open shifts, the
//     settings — before the order stream starts, so nothing interleaves.
//
// Nothing secret ever lives on the SD card; everything arrives here over HTTPS
// after a person proves they run the store.
package recovery

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"

	"quiccpos/agent/internal/domain/order"

	"github.com/rs/zerolog"
)

// MainClient is the slice of mainclient.Client used here.
type MainClient interface {
	Get(ctx context.Context, key, path, rawQuery string) (int, []byte, error)
	Post(ctx context.Context, key, path string, body []byte) (int, []byte, error)
}

var (
	ErrAlreadyLinked = errors.New("this Pi is already linked")
	ErrBadLogin      = errors.New("wrong username or password")
	ErrThrottled     = errors.New("too many attempts — wait a few minutes and try again")
	ErrNoInternet    = errors.New("can't reach the QuiccPOS server — check the internet connection")
)

// Outcome is what a successful link did.
type Outcome struct {
	APIKey   string               `json:"-"`
	Tenant   string               `json:"tenant"`
	Location string               `json:"location"`
	Username string               `json:"username"`
	Restored *order.RestoreResult `json:"restored,omitempty"`
	Secrets  order.Secrets        `json:"-"`
	Revoked  int                  `json:"revoked_agents"`
}

type linkResponse struct {
	APIKey   string `json:"api_key"`
	DeviceID string `json:"device_id"`
	Tenant   struct {
		Name string `json:"name"`
		Slug string `json:"slug"`
	} `json:"tenant"`
	Location struct {
		Name string `json:"name"`
	} `json:"location"`
	Manager struct {
		Username     string `json:"username"`
		PasswordHash string `json:"password_hash"`
	} `json:"manager"`
	Secrets order.Secrets `json:"secrets"`
	Revoked []string      `json:"revoked_agents"`
}

type Service struct {
	settings order.SettingsRepository
	repo     order.RecoveryRepository
	main     MainClient
	logger   zerolog.Logger

	// TailscaleKeyFile is where a linked agent drops the Tailscale auth key
	// for the root `quicc-tailscale-join` unit (skipped when the directory
	// doesn't exist — dev, tests).
	TailscaleKeyFile string
	// OnSecrets applies new secrets at runtime (Pushover keys).
	OnSecrets func(order.Secrets)
}

func New(settings order.SettingsRepository, repo order.RecoveryRepository, main MainClient, logger zerolog.Logger) *Service {
	return &Service{settings: settings, repo: repo, main: main,
		TailscaleKeyFile: "/var/lib/quicc-agent/tailscale.key",
		logger:           logger.With().Str("module", "recovery").Logger()}
}

// Link links this blank agent with the store manager login and restores the
// open batch. agentName labels the device in main (e.g. "lombardis-pi").
func (s *Service) Link(ctx context.Context, username, password, agentName string) (*Outcome, error) {
	key, err := s.settings.GetStoreAPIKey(ctx)
	if err != nil {
		return nil, err
	}
	if key != "" {
		return nil, ErrAlreadyLinked
	}
	body, _ := json.Marshal(map[string]string{"username": strings.TrimSpace(username), "password": password, "agent_name": agentName})
	status, raw, err := s.main.Post(ctx, "", "/store/v1/agent/link", body)
	if err != nil {
		return nil, ErrNoInternet
	}
	switch status {
	case http.StatusOK:
	case http.StatusUnauthorized:
		return nil, ErrBadLogin
	case http.StatusTooManyRequests:
		return nil, ErrThrottled
	default:
		return nil, fmt.Errorf("the server answered %d: %s", status, truncate(string(raw), 200))
	}
	var lr linkResponse
	if err := json.Unmarshal(raw, &lr); err != nil || lr.APIKey == "" {
		return nil, fmt.Errorf("unexpected link answer from the server")
	}

	// Restore BEFORE persisting the key: the order stream only starts once
	// the key is set, so restored orders and new arrivals can't interleave.
	out := &Outcome{APIKey: lr.APIKey, Tenant: lr.Tenant.Name, Location: lr.Location.Name,
		Username: lr.Manager.Username, Secrets: lr.Secrets, Revoked: len(lr.Revoked)}
	data, err := s.fetchRestore(ctx, lr.APIKey)
	if err != nil {
		return nil, err
	}
	res, err := s.repo.Restore(ctx, *data)
	switch {
	case err == nil:
		out.Restored = &res
	case errors.Is(err, order.ErrNotEmpty):
		s.logger.Warn().Ctx(ctx).Msg("link: local data present — keeping it, restore skipped")
	default:
		return nil, fmt.Errorf("restore: %w", err)
	}

	if err := s.settings.SetStoreAPIKey(ctx, lr.APIKey); err != nil {
		return nil, err
	}
	li := order.LinkInfo{Secrets: lr.Secrets, ManagerUsername: lr.Manager.Username, DeviceID: lr.DeviceID}
	li.Secrets.TailscaleAuthKey = "" // never stored; handed to the join unit below
	if err := s.settings.SetLinkInfo(ctx, li, lr.Manager.PasswordHash, "Your store manager login ("+lr.Manager.Username+")"); err != nil {
		return nil, err
	}
	if lr.Secrets.TailscaleAuthKey != "" {
		s.writeTailscaleKey(lr.Secrets.TailscaleAuthKey)
	}
	if s.OnSecrets != nil {
		s.OnSecrets(lr.Secrets)
	}
	s.logger.Info().Ctx(ctx).Str("tenant", lr.Tenant.Slug).Int("revoked_agents", len(lr.Revoked)).
		Interface("restored", out.Restored).Msg("agent linked with the manager login")
	return out, nil
}

func (s *Service) fetchRestore(ctx context.Context, key string) (*order.RestoreData, error) {
	var lastErr error
	for attempt := 0; attempt < 3; attempt++ {
		status, raw, err := s.main.Get(ctx, key, "/store/v1/agent/restore", "")
		switch {
		case err != nil:
			lastErr = ErrNoInternet
		case status != http.StatusOK:
			lastErr = fmt.Errorf("restore: the server answered %d", status)
		default:
			var d order.RestoreData
			if err := json.Unmarshal(raw, &d); err != nil {
				return nil, fmt.Errorf("restore: bad answer: %w", err)
			}
			return &d, nil
		}
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-time.After(time.Duration(attempt+1) * time.Second):
		}
	}
	return nil, lastErr
}

func (s *Service) writeTailscaleKey(key string) {
	dir := filepath.Dir(s.TailscaleKeyFile)
	if st, err := os.Stat(dir); err != nil || !st.IsDir() {
		return
	}
	tmp := s.TailscaleKeyFile + ".tmp"
	if err := os.WriteFile(tmp, []byte(key+"\n"), 0o600); err != nil {
		s.logger.Warn().Err(err).Msg("write tailscale key failed")
		return
	}
	if err := os.Rename(tmp, s.TailscaleKeyFile); err != nil {
		s.logger.Warn().Err(err).Msg("place tailscale key failed")
	}
}

// RefreshManagerHash pulls the current manager login hash from main (the
// password may have changed in /app/store). Returns true if it was updated.
func (s *Service) RefreshManagerHash(ctx context.Context) (bool, error) {
	key, err := s.settings.GetStoreAPIKey(ctx)
	if err != nil || key == "" {
		return false, err
	}
	li, err := s.settings.GetLinkInfo(ctx)
	if err != nil || li.ManagerUsername == "" {
		return false, err // provisioned the old way: local password stays
	}
	status, raw, err := s.main.Get(ctx, key, "/store/v1/agent/credential", "")
	if err != nil || status != http.StatusOK {
		return false, err
	}
	var m struct {
		Username     string `json:"username"`
		PasswordHash string `json:"password_hash"`
	}
	if err := json.Unmarshal(raw, &m); err != nil || m.PasswordHash == "" {
		return false, err
	}
	hash, _, err := s.settings.GetUIPassword(ctx)
	if err != nil {
		return false, err
	}
	if hash == m.PasswordHash {
		return false, nil
	}
	return true, s.settings.SetUIPasswordHash(ctx, m.PasswordHash)
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n]
}
