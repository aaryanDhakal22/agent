package repositories

import (
	"context"
	"testing"
	"time"

	"github.com/google/uuid"

	"quiccpos/agent/internal/domain/terminal"
)

// The hourly retention used to delete a general refund as an orphan payment (it has no order), so the
// Pi's Z left it out: Lombardi's 2026-11-14, a $3.80 "Wrong change" refund at 00:07 was gone by the
// 02:11 close and main's Z check flagged the batch $3.80 off. A general refund now stays while its
// batch is kept, is on that batch's Z, and goes with the batch.
func TestRetentionKeepsAGeneralRefundWithItsBatch(t *testing.T) {
	r := newFloorRepo(t)
	ctx := context.Background()
	arrive(t, r, "pickup")
	p := terminal.Payment{ID: uuid.Must(uuid.NewV7()).String(), ClientKey: "gr-" + uuid.NewString(), At: time.Now().UTC().Truncate(time.Millisecond),
		Tender: terminal.TenderCash, AmountCents: 380, Refund: true, Reason: "Wrong change", RefundKind: terminal.RefundGeneral}
	got, _, err := r.AddGeneralRefund(ctx, p, nil)
	if err != nil {
		t.Fatal(err)
	}
	if got.BatchID == 0 {
		t.Fatal("the general refund has no batch")
	}
	markAllSynced(t, r)
	if _, err := r.TrimBatches(ctx, 3, time.Now().Add(time.Hour)); err != nil { // the hourly retention
		t.Fatal(err)
	}
	if left, err := r.GeneralRefundByKey(ctx, p.ClientKey); err != nil || left == nil {
		t.Fatalf("retention deleted the general refund of the open batch (%v)", err)
	}
	in, err := r.ZInputs(ctx, got.BatchID)
	if err != nil {
		t.Fatal(err)
	}
	z := terminal.BuildZ(in)
	if z.Reconcile.GeneralRefundsCents != 380 || z.GeneralRefunds.Count != 1 {
		t.Fatalf("the Z has general refunds %d cents (%d), want 380 (1)", z.Reconcile.GeneralRefundsCents, z.GeneralRefunds.Count)
	}

	// Its batch closed and trimmed (open + 3 closed kept): the refund goes with it.
	for i := 0; i < 4; i++ {
		if _, _, err := r.CloseBatch(ctx, true); err != nil {
			t.Fatal(err)
		}
		arrive(t, r, "pickup")
	}
	markAllSynced(t, r)
	if _, err := r.TrimBatches(ctx, 3, time.Now().Add(time.Hour)); err != nil {
		t.Fatal(err)
	}
	if left, err := r.GeneralRefundByKey(ctx, p.ClientKey); err != nil || left != nil {
		t.Fatalf("a trimmed batch's general refund is still kept (%v)", err)
	}
}

// Batch J: a general refund to a card stays with its batch too, card block and all (Square's refund id), and the
// Z has it on its card tender, never the cash.
func TestRetentionKeepsAGeneralCardRefundWithItsCard(t *testing.T) {
	r := newFloorRepo(t)
	ctx := context.Background()
	arrive(t, r, "pickup")
	p := terminal.Payment{ID: uuid.Must(uuid.NewV7()).String(), ClientKey: "gc-" + uuid.NewString(), At: time.Now().UTC().Truncate(time.Millisecond),
		Tender: terminal.TenderManualCard, AmountCents: 750, Refund: true, Reason: "Overcharged", RefundKind: terminal.RefundGeneral,
		Brand: terminal.BrandVisa, Last4: "1111", Ref: "sqref_1", CardInfo: terminal.CardInfo{SquareRefundID: "sqref_1", Entry: "keyed"}}
	got, _, err := r.AddGeneralRefund(ctx, p, nil)
	if err != nil {
		t.Fatal(err)
	}
	if got.DrawerID != "" {
		t.Fatalf("a card general refund went in drawer %q", got.DrawerID)
	}
	markAllSynced(t, r)
	if _, err := r.TrimBatches(ctx, 3, time.Now().Add(time.Hour)); err != nil {
		t.Fatal(err)
	}
	left, err := r.GeneralRefundByKey(ctx, p.ClientKey)
	if err != nil || left == nil || left.SquareRefundID != "sqref_1" || left.Tender != terminal.TenderManualCard {
		t.Fatalf("retention lost the card general refund or its card: %+v (%v)", left, err)
	}
	in, err := r.ZInputs(ctx, got.BatchID)
	if err != nil {
		t.Fatal(err)
	}
	z := terminal.BuildZ(in)
	for _, tt := range z.Tenders {
		if tt.Tender == terminal.TenderCash && tt.RefundedCents != 0 {
			t.Fatalf("the Z refunded cash: %+v", tt)
		}
		if tt.Tender == terminal.TenderManualCard && tt.RefundedCents != 750 {
			t.Fatalf("manual_card %+v", tt)
		}
	}
	if z.GeneralRefunds.Cents != 750 || z.CashOutside != 0 {
		t.Fatalf("z general %+v, cash outside %d", z.GeneralRefunds, z.CashOutside)
	}
}
