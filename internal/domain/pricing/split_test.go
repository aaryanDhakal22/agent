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

// TestDefaultDeliveryFee: a new store charges $4 for delivery, $3 of it the
// driver's (the owner, 2026-11-11).
func TestDefaultDeliveryFee(t *testing.T) {
	d := DefaultStore()
	if err := d.Validate(); err != nil {
		t.Fatal(err)
	}
	eff := Effective(d, KindOverride{})
	if eff.DeliveryFeeCents != 400 || eff.DeliveryDriverCents != 300 {
		t.Fatalf("delivery %d, driver %d", eff.DeliveryFeeCents, eff.DeliveryDriverCents)
	}
	if st, dr := DeliverySplit(eff, 400); st != 100 || dr != 300 {
		t.Fatalf("split %d + %d", st, dr)
	}
}

// Delivery fee by distance (the owner, 2026-11-13): ascending rows, the flat
// fee kept equal to the first row's (older readers), the driver's share at
// most the lowest row's fee, never part of the bands.
func TestDeliveryBandsValidate(t *testing.T) {
	driver := int64(300)
	s := DefaultStore()
	s.Fees.DeliveryBands = []DeliveryBand{{UpToM: 3219, FeeCents: 400}, {UpToM: 8047, FeeCents: 600}}
	if err := s.Validate(); err != nil {
		t.Fatal(err)
	}
	if s.Fees.DeliveryFeeCents != 400 || *s.Fees.DeliveryDriverCents != driver {
		t.Fatalf("flat fee %d driver %d", s.Fees.DeliveryFeeCents, *s.Fees.DeliveryDriverCents)
	}
	eff := Effective(s, KindOverride{})
	if len(eff.DeliveryBands) != 2 || eff.DeliveryDriverCents != driver {
		t.Fatalf("effective %+v", eff)
	}
	// The driver's pay doesn't follow the band: a far delivery splits $3 + $3.
	if store, drv := DeliverySplit(eff, 600); store != 300 || drv != 300 {
		t.Fatalf("split %d + %d", store, drv)
	}
	bad := [][]DeliveryBand{
		{{UpToM: 8047, FeeCents: 400}, {UpToM: 3219, FeeCents: 600}}, // not ascending
		{{UpToM: 3219, FeeCents: 400}, {UpToM: 3219, FeeCents: 600}}, // the same reach twice
		{{UpToM: 0, FeeCents: 400}},
		{{UpToM: MaxBandM + 1, FeeCents: 400}},
		{{UpToM: 3219, FeeCents: 10001}},
		{{UpToM: 3219, FeeCents: 200}}, // under the driver's $3
		make([]DeliveryBand, MaxDeliveryBands+1),
	}
	for i, b := range bad {
		s := DefaultStore()
		s.Fees.DeliveryBands = b
		if err := s.Validate(); err == nil {
			t.Errorf("bad bands %d accepted: %+v", i, b)
		}
	}
	m := func(v int64) *int64 { return &v }
	for _, c := range []struct {
		d    *int64
		want int64
	}{{nil, 400}, {m(0), 400}, {m(3219), 400}, {m(3220), 600}, {m(99999), 600}} {
		if got := DeliveryFeeFor(999, s.Fees.DeliveryBands, c.d); got != c.want {
			t.Errorf("DeliveryFeeFor(%v) = %d, want %d", c.d, got, c.want)
		}
	}
	if got := DeliveryFeeFor(999, nil, m(5)); got != 999 {
		t.Errorf("no bands: %d", got)
	}
}
