package recovery

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"quiccpos/agent/internal/domain/order"

	"github.com/rs/zerolog"
)

type fakeMain struct {
	linkStatus int
	linkBody   string
	restore    string
	cred       string
	secrets    string // "" = an older main (404)
	posts      int
	err        error
}

func (f *fakeMain) Get(_ context.Context, key, path, _ string) (int, []byte, error) {
	if f.err != nil {
		return 0, nil, f.err
	}
	switch path {
	case "/store/v1/agent/restore":
		return 200, []byte(f.restore), nil
	case "/store/v1/agent/credential":
		return 200, []byte(f.cred), nil
	case "/store/v1/agent/secrets":
		if f.secrets != "" {
			return 200, []byte(f.secrets), nil
		}
	}
	return 404, nil, nil
}

func (f *fakeMain) Post(_ context.Context, key, path string, body []byte) (int, []byte, error) {
	f.posts++
	if f.err != nil {
		return 0, nil, f.err
	}
	return f.linkStatus, []byte(f.linkBody), nil
}

type fakeSettings struct {
	order.SettingsRepository
	key  string
	li   order.LinkInfo
	hash string
	hint string
}

func (s *fakeSettings) GetStoreAPIKey(context.Context) (string, error) { return s.key, nil }
func (s *fakeSettings) SetStoreAPIKey(_ context.Context, k string) error {
	s.key = k
	return nil
}
func (s *fakeSettings) GetLinkInfo(context.Context) (order.LinkInfo, error) { return s.li, nil }
func (s *fakeSettings) SetLinkInfo(_ context.Context, li order.LinkInfo, hash, hint string) error {
	s.li, s.hash, s.hint = li, hash, hint
	return nil
}
func (s *fakeSettings) SetPushoverSecrets(_ context.Context, sec order.Secrets) error {
	s.li.Secrets.PushoverAppToken, s.li.Secrets.PushoverUserKey = sec.PushoverAppToken, sec.PushoverUserKey
	s.li.Secrets.PushoverBossKey, s.li.Secrets.PushoverAdminDevices = sec.PushoverBossKey, sec.PushoverAdminDevices
	return nil
}
func (s *fakeSettings) GetUIPassword(context.Context) (string, string, error) {
	return s.hash, s.hint, nil
}
func (s *fakeSettings) SetUIPasswordHash(_ context.Context, h string) error {
	s.hash = h
	return nil
}

type fakeRepo struct {
	got  *order.RestoreData
	fail error
}

func (r *fakeRepo) AgentState(context.Context) (order.AgentState, error) {
	return order.AgentState{}, nil
}
func (r *fakeRepo) Restore(_ context.Context, d order.RestoreData) (order.RestoreResult, error) {
	if r.fail != nil {
		return order.RestoreResult{}, r.fail
	}
	r.got = &d
	return order.RestoreResult{Orders: len(d.Orders)}, nil
}

const linkOK = `{"api_key":"qsk_new","device_id":"dev-1","tenant":{"name":"Lombardi's","slug":"lombardis"},
 "location":{"name":"Main"},"manager":{"username":"lombardispizza","password_hash":"$2a$10$x"},
 "secrets":{"pushover_app_token":"app","pushover_user_key":"usr","otlp_endpoint":"https://otlp","tailscale_authkey":"tskey-1"},
 "revoked_agents":["a","b"]}`

func TestLinkStoresKeyAfterRestore(t *testing.T) {
	m := &fakeMain{linkStatus: 200, linkBody: linkOK, restore: `{"orders":[{"order":{"id":"o1"}}]}`}
	st := &fakeSettings{}
	repo := &fakeRepo{}
	svc := New(st, repo, m, zerolog.Nop())
	dir := t.TempDir()
	svc.TailscaleKeyFile = filepath.Join(dir, "tailscale.key")
	var applied order.Secrets
	svc.OnSecrets = func(s order.Secrets) { applied = s }

	out, err := svc.Link(context.Background(), " lombardispizza ", "pw", "pi")
	if err != nil {
		t.Fatal(err)
	}
	if st.key != "qsk_new" || out.Revoked != 2 || out.Restored == nil || out.Restored.Orders != 1 {
		t.Fatalf("out %+v key %q", out, st.key)
	}
	if st.hash != "$2a$10$x" || st.li.ManagerUsername != "lombardispizza" || st.li.DeviceID != "dev-1" {
		t.Fatalf("link info %+v hash %q", st.li, st.hash)
	}
	if st.li.Secrets.TailscaleAuthKey != "" {
		t.Fatal("the tailscale key must never be stored in the DB")
	}
	b, err := os.ReadFile(svc.TailscaleKeyFile)
	if err != nil || string(b) != "tskey-1\n" {
		t.Fatalf("tailscale key file %q %v", b, err)
	}
	if applied.PushoverAppToken != "app" {
		t.Fatal("secrets not applied at runtime")
	}
	raw, _ := json.Marshal(out)
	for _, secret := range []string{"qsk_new", "tskey", `"app"`} {
		if strings.Contains(string(raw), secret) {
			t.Fatalf("the outcome JSON must not carry the key or secrets: %s", raw)
		}
	}
	if _, err := svc.Link(context.Background(), "x", "y", "pi"); !errors.Is(err, ErrAlreadyLinked) {
		t.Fatalf("second link: %v", err)
	}
}

func TestLinkErrors(t *testing.T) {
	cases := []struct {
		m    *fakeMain
		want error
	}{
		{&fakeMain{linkStatus: 401}, ErrBadLogin},
		{&fakeMain{linkStatus: 429}, ErrThrottled},
		{&fakeMain{err: errors.New("dial tcp: no route")}, ErrNoInternet},
	}
	for _, c := range cases {
		st := &fakeSettings{}
		_, err := New(st, &fakeRepo{}, c.m, zerolog.Nop()).Link(context.Background(), "u", "p", "pi")
		if !errors.Is(err, c.want) {
			t.Fatalf("got %v want %v", err, c.want)
		}
		if st.key != "" {
			t.Fatal("no key may be stored on a failed link")
		}
	}
}

func TestLinkRestoreFailureStoresNothing(t *testing.T) {
	m := &fakeMain{linkStatus: 200, linkBody: linkOK, restore: `{}`}
	st := &fakeSettings{}
	_, err := New(st, &fakeRepo{fail: errors.New("disk full")}, m, zerolog.Nop()).Link(context.Background(), "u", "p", "pi")
	if err == nil || st.key != "" {
		t.Fatalf("err %v key %q — a failed restore must leave the Pi unlinked (retry is safe)", err, st.key)
	}
	// Local data present: keep it, link anyway.
	st2 := &fakeSettings{}
	if _, err := New(st2, &fakeRepo{fail: order.ErrNotEmpty}, m, zerolog.Nop()).Link(context.Background(), "u", "p", "pi"); err != nil || st2.key == "" {
		t.Fatalf("not-empty: %v key %q", err, st2.key)
	}
}

func TestRefreshManagerHash(t *testing.T) {
	m := &fakeMain{cred: `{"username":"lombardispizza","password_hash":"$2a$10$new"}`}
	st := &fakeSettings{key: "k", hash: "$2a$10$old", li: order.LinkInfo{ManagerUsername: "lombardispizza"}}
	changed, err := New(st, &fakeRepo{}, m, zerolog.Nop()).RefreshManagerHash(context.Background())
	if err != nil || !changed || st.hash != "$2a$10$new" {
		t.Fatalf("changed %v err %v hash %q", changed, err, st.hash)
	}
	// Provisioned the old way (no manager): the local password stays.
	st2 := &fakeSettings{key: "k", hash: "$2a$10$local"}
	if changed, _ := New(st2, &fakeRepo{}, m, zerolog.Nop()).RefreshManagerHash(context.Background()); changed || st2.hash != "$2a$10$local" {
		t.Fatal("a token-provisioned agent's password must not be replaced")
	}
}

func TestRefreshSecrets(t *testing.T) {
	ctx := context.Background()
	m := &fakeMain{secrets: `{"pushover_app_token":"app","pushover_user_key":"usr","pushover_admin_devices":"AaryanPhone"}`}
	st := &fakeSettings{key: "k", li: order.LinkInfo{Secrets: order.Secrets{PushoverAppToken: "app", PushoverUserKey: "usr",
		OTLPEndpoint: "https://otlp"}}}
	svc := New(st, &fakeRepo{}, m, zerolog.Nop())
	var applied *order.Secrets
	svc.OnSecrets = func(sec order.Secrets) { applied = &sec }
	changed, err := svc.RefreshSecrets(ctx)
	if err != nil || !changed || st.li.Secrets.PushoverAdminDevices != "AaryanPhone" {
		t.Fatalf("changed %v err %v li %+v", changed, err, st.li.Secrets)
	}
	if applied == nil || applied.PushoverAdminDevices != "AaryanPhone" || applied.OTLPEndpoint != "https://otlp" {
		t.Fatalf("applied %+v", applied)
	}
	// Same again: nothing to apply.
	applied = nil
	if changed, _ := svc.RefreshSecrets(ctx); changed || applied != nil {
		t.Fatal("an unchanged refresh must not apply")
	}
	// An older main (404) keeps what was stored.
	m.secrets = ""
	if changed, err := svc.RefreshSecrets(ctx); changed || err != nil || st.li.Secrets.PushoverAdminDevices != "AaryanPhone" {
		t.Fatalf("404: changed %v err %v", changed, err)
	}
}
