package pricing

import "testing"

func TestDeliverySplit(t *testing.T) {
	i := func(v int64) *int64 { return &v }
	for _, c := range []struct {
		fees   Fees
		charge int64
		store  int64
		driver int64
	}{
		{Fees{DeliveryFeeCents: 400}, 400, 100, 300},                              // the default: $1 + $3
		{Fees{DeliveryFeeCents: 250}, 250, 0, 250},                                // default capped at the fee
		{Fees{DeliveryFeeCents: 500, DeliveryDriverCents: i(200)}, 500, 300, 200}, // the store's own split
		{Fees{DeliveryFeeCents: 500, DeliveryDriverCents: i(0)}, 500, 500, 0},     // all to the store
		{Fees{DeliveryFeeCents: 400}, 0, 0, 0},                                    // free delivery
		{Fees{}, 0, 0, 0},                                                         // no fee
	} {
		st := Effective(StoreSettings{Fees: c.fees}, KindOverride{})
		s, d := DeliverySplit(st, c.charge)
		if s != c.store || d != c.driver || s+d != c.charge {
			t.Errorf("%+v charged %d: store %d driver %d, want %d + %d", c.fees, c.charge, s, d, c.store, c.driver)
		}
	}
}

func TestDriverShareValidate(t *testing.T) {
	i := func(v int64) *int64 { return &v }
	ok := StoreSettings{Fees: Fees{DeliveryFeeCents: 400, DeliveryDriverCents: i(400)}}
	if err := ok.Validate(); err != nil {
		t.Fatal(err)
	}
	for _, d := range []int64{-1, 401} {
		bad := StoreSettings{Fees: Fees{DeliveryFeeCents: 400, DeliveryDriverCents: i(d)}}
		if bad.Validate() == nil {
			t.Errorf("driver share %d of 400 accepted", d)
		}
	}
}
