package repositories

import (
	"context"
	"testing"

	"quiccpos/agent/internal/domain/order"
)

func TestReceiptPrefs_DefaultsAndRoundTrip(t *testing.T) {
	ctx := context.Background()
	r := newFloorRepo(t)
	// A fresh (or upgraded) agent stores '{}' = every default.
	got, err := r.ReceiptPrefs(ctx)
	if err != nil || got != order.DefaultReceiptPrefs() {
		t.Fatalf("fresh prefs %+v %v, want defaults", got, err)
	}
	st, err := r.AgentState(ctx)
	if err != nil || st.ReceiptPrefs == nil || *st.ReceiptPrefs != order.DefaultReceiptPrefs() {
		t.Fatalf("agent.state receipt_prefs %+v %v", st.ReceiptPrefs, err)
	}

	none := ""
	p := order.DefaultReceiptPrefs()
	p.KitchenNumberSize, p.FooterSize, p.ExtraPaper, p.ShowStoreAddress, p.FooterText = order.SizeHuge, order.SizeNormal, false, false, &none
	if err := r.SetReceiptPrefs(ctx, p); err != nil {
		t.Fatal(err)
	}
	got, err = r.ReceiptPrefs(ctx)
	if err != nil || got.KitchenNumberSize != order.SizeHuge || got.FooterSize != order.SizeNormal ||
		got.ExtraPaper || got.ShowStoreAddress || got.FooterText == nil || *got.FooterText != "" {
		t.Fatalf("round trip %+v %v", got, err)
	}
}

// A restore from an older agent's state (no receipt_prefs) keeps the defaults;
// an invalid one is dropped instead of failing the restore.
func TestRestore_ReceiptPrefsAbsentOrInvalid(t *testing.T) {
	ctx := context.Background()
	for name, rp := range map[string]*order.ReceiptPrefs{
		"absent":  nil,
		"invalid": {KitchenItemSize: "gigantic", Copies: 9},
	} {
		b := newFloorRepo(t)
		if _, err := b.Restore(ctx, order.RestoreData{State: &order.AgentState{Version: 1, ReceiptPrefs: rp}}); err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		if got, _ := b.ReceiptPrefs(ctx); got != order.DefaultReceiptPrefs() {
			t.Errorf("%s: got %+v, want defaults", name, got)
		}
	}
}
