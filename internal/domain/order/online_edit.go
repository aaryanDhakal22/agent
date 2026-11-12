package order

import "encoding/json"

// Online orders = called-in orders (agent contract §2.10aa): what the Pi
// keeps about an online / website order in its own corner of the snapshot's
// source_metadata (MetaPi), never main's fields.

// MetaPi is the source_metadata key of the Pi's notes on an order from main:
// {placed_direct, online: OnlineBase (from the first terminal edit),
// refund_owed_cents}.
const MetaPi = "pi"

// OnlineBase is an online order as it arrived, kept at its first terminal
// edit: its own lines and money, and what it arrived paid with (the vendor's /
// website's card: total - the one cash rule). Every edit is priced from it.
type OnlineBase struct {
	Items     []LineItem `json:"items"`
	Money     Money      `json:"money"`
	PaidCents int64      `json:"paid_cents"`
}

func (s Snapshot) piMeta() map[string]any {
	m, _ := s.SourceMeta[MetaPi].(map[string]any)
	return m
}

// PlacedDirect: the order was placed on arrival (§2.10aa, status.placed_direct).
func (s Snapshot) PlacedDirect() bool {
	v, _ := s.piMeta()["placed_direct"].(bool)
	return v
}

// OnlineBase is the order as it arrived (nil = never edited at a terminal).
func (s Snapshot) OnlineBase() *OnlineBase {
	raw, ok := s.piMeta()["online"]
	if !ok || raw == nil {
		return nil
	}
	b, err := json.Marshal(raw)
	if err != nil {
		return nil
	}
	var ob OnlineBase
	if json.Unmarshal(b, &ob) != nil {
		return nil
	}
	return &ob
}

// RefundOwedCents is what an edited online order owes its customer (the
// manager refunds it in the vendor's site / Square; no money moved here).
func (s Snapshot) RefundOwedCents() int64 {
	switch v := s.piMeta()["refund_owed_cents"].(type) {
	case float64:
		return int64(v)
	case int64:
		return v
	case int:
		return int64(v)
	case json.Number:
		n, _ := v.Int64()
		return n
	}
	return 0
}

// WithPiMeta returns a copy of the snapshot's source_metadata with the Pi's
// key set to the given fields merged over what it had.
func (s Snapshot) WithPiMeta(fields map[string]any) map[string]any {
	meta := make(map[string]any, len(s.SourceMeta)+1)
	for k, v := range s.SourceMeta {
		meta[k] = v
	}
	pi := map[string]any{}
	for k, v := range s.piMeta() {
		pi[k] = v
	}
	for k, v := range fields {
		pi[k] = v
	}
	meta[MetaPi] = pi
	return meta
}
