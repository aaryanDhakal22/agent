package repositories

import (
	"context"
	"encoding/json"
	"path/filepath"
	"testing"
	"time"

	"quiccpos/agent/internal/domain/terminal"
	"quiccpos/agent/internal/infra/database"
	"quiccpos/agent/internal/migrate"

	"github.com/rs/zerolog"
)

// Shared drafts live in SQLite: they survive a Pi restart, stay per store
// and keep their holder (contract §2.10i).
func TestTerminalDraftsSurviveARestart(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "agent.db")
	if err := migrate.Run(ctx, path); err != nil {
		t.Fatal(err)
	}
	open := func() (*OrderRepository, func()) {
		db, err := database.Open(ctx, path)
		if err != nil {
			t.Fatal(err)
		}
		return NewOrderRepository(db, zerolog.Nop()), func() { db.Close() }
	}
	r, closeDB := open()
	by := terminal.Holder{DeviceID: "dev-1", DeviceName: "Till 1", StaffID: "s-1", StaffName: "Cara S."}
	now := time.Now().UTC().Truncate(time.Millisecond)
	in := terminal.Draft{ID: "d-keep-0001", LocationID: "loc-a", Type: "pickup", Name: "Sam", Draft: json.RawMessage(`{"lines":[]}`)}
	if _, _, err := r.PutTerminalDraft(ctx, in, terminal.DraftPut{By: by}, now); err != nil {
		t.Fatal(err)
	}
	closeDB()

	r, closeDB = open()
	defer closeDB()
	list, err := r.TerminalDrafts(ctx, "loc-a")
	if err != nil || len(list) != 1 {
		t.Fatalf("after restart: %+v %v", list, err)
	}
	d := list[0]
	if d.Holder == nil || d.Holder.DeviceID != "dev-1" || d.StaffName != "Cara S." || d.Type != "pickup" || !d.CreatedAt.Equal(now) {
		t.Fatalf("draft: %+v", d)
	}
	if other, _ := r.TerminalDrafts(ctx, "loc-b"); len(other) != 0 {
		t.Fatal("another store sees none")
	}
	// Still open on dev-1: dev-2 is told who has it.
	if _, _, err := r.PutTerminalDraft(ctx, in, terminal.DraftPut{By: terminal.Holder{DeviceID: "dev-2"}}, now); err == nil {
		t.Fatal("open elsewhere")
	}
	if ok, err := r.DeleteTerminalDraft(ctx, in.ID, "loc-a", "dev-1"); err != nil || !ok {
		t.Fatalf("delete: %v %v", ok, err)
	}
}
