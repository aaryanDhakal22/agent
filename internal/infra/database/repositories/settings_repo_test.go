package repositories

import (
	"context"
	"path/filepath"
	"testing"

	"quiccpos/agent/internal/infra/database"
	"quiccpos/agent/internal/migrate"

	"github.com/rs/zerolog"
)

// TestSettingsRepositoryProvisioningRoundTrip runs the settings singleton
// through the provisioning lifecycle on a freshly migrated SQLite file:
// blank → token + session secret + password → ClearProvisioning, which must
// wipe the token and password but keep the session secret.
func TestSettingsRepositoryProvisioningRoundTrip(t *testing.T) {
	ctx := context.Background()
	dbPath := filepath.Join(t.TempDir(), "agent.db")
	if err := migrate.Run(ctx, dbPath); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	db, err := database.Open(ctx, dbPath)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	defer db.Close()

	repo := NewSettingsRepository(db, zerolog.Nop())

	// Fresh DB: everything blank.
	if key, err := repo.GetStoreAPIKey(ctx); err != nil || key != "" {
		t.Fatalf("fresh key = %q, %v; want empty", key, err)
	}
	if hash, hint, err := repo.GetUIPassword(ctx); err != nil || hash != "" || hint != "" {
		t.Fatalf("fresh password = %q/%q, %v; want empty", hash, hint, err)
	}
	if s, err := repo.GetSessionSecret(ctx); err != nil || s != "" {
		t.Fatalf("fresh session secret = %q, %v; want empty", s, err)
	}

	// Provision.
	must := func(err error) {
		t.Helper()
		if err != nil {
			t.Fatal(err)
		}
	}
	must(repo.SetStoreAPIKey(ctx, "qsk_test"))
	must(repo.SetSessionSecret(ctx, "deadbeef"))
	must(repo.SetUIPassword(ctx, "$2a$hash", "the usual"))

	if key, _ := repo.GetStoreAPIKey(ctx); key != "qsk_test" {
		t.Fatalf("key = %q, want qsk_test", key)
	}
	if hash, hint, _ := repo.GetUIPassword(ctx); hash != "$2a$hash" || hint != "the usual" {
		t.Fatalf("password = %q/%q", hash, hint)
	}

	// Unlink / revoke.
	must(repo.ClearProvisioning(ctx))
	if key, _ := repo.GetStoreAPIKey(ctx); key != "" {
		t.Fatalf("key after clear = %q, want empty", key)
	}
	if hash, hint, _ := repo.GetUIPassword(ctx); hash != "" || hint != "" {
		t.Fatalf("password after clear = %q/%q, want empty", hash, hint)
	}
	if s, _ := repo.GetSessionSecret(ctx); s != "deadbeef" {
		t.Fatalf("session secret after clear = %q; ClearProvisioning must not touch it", s)
	}
}
