package order

import (
	"encoding/json"
	"fmt"
	"strings"
	"time"
)

// The store hub integration (2.1.0; the callout box since H3, 2026-11-12:
// SoftTouch is gone): the feature toggles and kitchen-ticket switches staff
// control, the default pickup / delivery times, the hub's reported status,
// and the cursor feed the hub reads (integration_events).

const (
	// SourceOnline is the vendor's online orders (relayed by online/);
	// SourceWeb the store's own website - every other source is one of the
	// store's channels too (Snapshot.IsChannel).
	SourceOnline = "online"
	SourceWeb    = "web"
)

// Features are the feature toggles (settings.features).
type Features struct {
	FutureOrders bool `json:"future_orders"`
	PaidWave     bool `json:"paid_wave"`
}

// DefaultFeatures: everything on.
func DefaultFeatures() Features {
	return Features{FutureOrders: true, PaidWave: true}
}

// TicketToggles are the kitchen station ticket switches (settings.ticket_toggles).
type TicketToggles struct {
	OnlineCashPickup   bool `json:"online_cash_pickup"`
	OnlineCashDelivery bool `json:"online_cash_delivery"`
	OnlineCard         bool `json:"online_card"`
	// 2.4.0: a future order's first ACCEPT prints, as an early warning, its
	// kitchen tickets (DO NOT MAKE + when it is due) / its customer receipt
	// (a FUTURE bar on top). Off by default.
	FutureKitchen bool `json:"future_kitchen"`
	FutureReceipt bool `json:"future_receipt"`
}

// DefaultTicketToggles: online orders print their station tickets; the
// early future tickets are off.
func DefaultTicketToggles() TicketToggles {
	return TicketToggles{OnlineCashPickup: true, OnlineCashDelivery: true, OnlineCard: true}
}

// Times are the store's default pickup (X) and delivery (Y) times in minutes.
type Times struct {
	DefaultPickupMinutes   int64 `json:"default_pickup_minutes"`
	DefaultDeliveryMinutes int64 `json:"default_delivery_minutes"`
}

// DefaultTimes are the migration defaults.
var DefaultTimes = Times{DefaultPickupMinutes: 15, DefaultDeliveryMinutes: 35}

// Validate checks each time is 1-240 minutes.
func (t Times) Validate() error {
	if t.DefaultPickupMinutes < 1 || t.DefaultPickupMinutes > 240 {
		return fmt.Errorf("default_pickup_minutes must be 1-240")
	}
	if t.DefaultDeliveryMinutes < 1 || t.DefaultDeliveryMinutes > 240 {
		return fmt.Errorf("default_delivery_minutes must be 1-240")
	}
	return nil
}

// For is the default time for a fulfillment type: Y for a delivery, X else.
func (t Times) For(fulfillmentType string) int64 {
	if strings.EqualFold(strings.TrimSpace(fulfillmentType), "delivery") {
		return t.DefaultDeliveryMinutes
	}
	return t.DefaultPickupMinutes
}

// IntegrationSettings is the {features, tickets, times, finishing, callout}
// set served in /api/floor.settings, SSE settings.updated and the feed's
// settings.changed (finishing + callout since 00019).
type IntegrationSettings struct {
	Features  Features          `json:"features"`
	Tickets   TicketToggles     `json:"tickets"`
	Times     Times             `json:"times"`
	Finishing FinishingSettings `json:"finishing"`
	Callout   CalloutSettings   `json:"callout"`
	Look      LookSettings      `json:"look"`
}

// DefaultIntegrationSettings are the migration defaults.
func DefaultIntegrationSettings() IntegrationSettings {
	return IntegrationSettings{Features: DefaultFeatures(), Tickets: DefaultTicketToggles(), Times: DefaultTimes,
		Finishing: DefaultFinishing(), Callout: DefaultCallout(), Look: DefaultLook()}
}

// DecodeFeatures reads stored features over the defaults (missing keys keep
// their default; ” and '{}' are all defaults).
func DecodeFeatures(raw []byte) (Features, error) {
	f := DefaultFeatures()
	if len(raw) == 0 {
		return f, nil
	}
	err := json.Unmarshal(raw, &f)
	return f, err
}

// DecodeTicketToggles reads stored ticket switches over the defaults.
func DecodeTicketToggles(raw []byte) (TicketToggles, error) {
	t := DefaultTicketToggles()
	if len(raw) == 0 {
		return t, nil
	}
	err := json.Unmarshal(raw, &t)
	return t, err
}

// IsFuture reports whether an online order is a future order: future orders
// on, and promised_at later than now + its default time. A scheduled or
// re-rung order stays future until accepted.
func IsFuture(so StoredOrder, set IntegrationSettings, now time.Time) bool {
	if !set.Features.FutureOrders {
		return false
	}
	switch so.Status.State {
	case StateScheduled:
		return true
	case StateArrived:
		if so.Status.Rerung {
			return true
		}
		p := so.Order.Fulfillment.PromisedAt
		return p != nil && p.After(now.Add(time.Duration(set.Times.For(so.Order.Fulfillment.Type))*time.Minute))
	}
	return false
}

// --- hub status ----------------------------------------------------------------

// Insert modes an older hub reported (its per-write mode for the SoftTouch
// insert; the callout box sends "off").
const (
	InsertOff    = "off"
	InsertShadow = "shadow"
	InsertLive   = "live"
)

// Problem severities.
const (
	SeverityInfo    = "info"
	SeverityWarn    = "warn"
	SeveritySerious = "serious"
)

// IntegrationStaleAfter: no status PUT for this long = the hub link is down.
const IntegrationStaleAfter = 60 * time.Second

// Problem is one thing the hub (or the agent about the hub) reports.
type Problem struct {
	Code     string `json:"code"`
	Text     string `json:"text"`
	Severity string `json:"severity"`
}

// IntegrationStatus is the hub's last PUT /api/integration/status plus when
// it arrived (kept in memory, the last one also in settings).
type IntegrationStatus struct {
	OK           bool      `json:"ok"`
	InsertMode   string    `json:"insert_mode"`
	OperationDay *string   `json:"operation_day"`
	Problems     []Problem `json:"problems"`
	Version      string    `json:"version"`
	// Callout (hub 0.2.0): the order callout's voices + state as the hub
	// reports them (passed through to the tablet as is).
	Callout json.RawMessage `json:"callout,omitempty"`
	SeenAt  time.Time       `json:"seen_at"`
}

// IntegrationView is /api/floor.integration and SSE integration.status.
type IntegrationView struct {
	Connected    bool       `json:"connected"`
	OK           bool       `json:"ok"`
	InsertMode   string     `json:"insert_mode"`
	Problems     []Problem  `json:"problems"`
	LastSeenAt   *time.Time `json:"last_seen_at"`
	OperationDay *string    `json:"operation_day,omitempty"`
	Version      string     `json:"version,omitempty"`
	// Callout: the hub's callout report (voices, rendering, last spoken);
	// null before a hub that has the callout reported.
	Callout json.RawMessage `json:"callout,omitempty"`
}

// ProblemLinkDown is added when the hub went quiet (the code stays
// "pos_link_down": the tablets know it).
var ProblemLinkDown = Problem{Code: "pos_link_down", Text: "Hub link down", Severity: SeveritySerious}

// View is the status as staff see it at now: connected only while the hub
// reported within IntegrationStaleAfter (else a serious "Hub link down",
// once a hub was ever seen).
func (st IntegrationStatus) View(now time.Time) IntegrationView {
	v := IntegrationView{InsertMode: st.InsertMode, Problems: []Problem{}, Version: st.Version, OperationDay: st.OperationDay, Callout: st.Callout}
	if v.InsertMode == "" {
		v.InsertMode = InsertOff
	}
	if st.SeenAt.IsZero() {
		return v // no hub has ever reported: nothing to show
	}
	seen := st.SeenAt.UTC()
	v.LastSeenAt = &seen
	v.Connected = now.Sub(st.SeenAt) <= IntegrationStaleAfter
	if !v.Connected {
		v.Problems = append(v.Problems, ProblemLinkDown)
		return v
	}
	v.OK = st.OK
	v.Problems = append(v.Problems, st.Problems...)
	return v
}

// --- the integration feed ------------------------------------------------------

// Feed event types (integration_events.type).
const (
	FeedOrderReady      = "order.ready"
	FeedSettingsChanged = "settings.changed"
	// FeedCalloutTest (00019): speak a callout now on the store speaker - a
	// TEST CALLOUT or a voice PREVIEW from the tablet.
	FeedCalloutTest = "callout.test"
	// FeedCalloutRender (2.3.2): render a callout for the tablet's PREVIEW
	// "on this tablet" and upload the WAV to the Pi (nothing plays).
	FeedCalloutRender = "callout.render"
)

// FeedRetention is how long feed events are kept.
const FeedRetention = 72 * time.Hour

// FeedEvent is one entry of GET /api/integration/events.
type FeedEvent struct {
	Seq  int64           `json:"seq"`
	Type string          `json:"type"`
	At   time.Time       `json:"at"`
	Data json.RawMessage `json:"data"`
}

// FeedOrderRef identifies an order on the feed.
type FeedOrderRef struct {
	OrderID string `json:"order_id"`
	Source  string `json:"source"`
}

// RefOf is the feed reference of an order.
func RefOf(so StoredOrder) FeedOrderRef {
	return FeedOrderRef{OrderID: so.Order.ID, Source: so.Order.Source}
}

// OrderReadyData is an order.ready event (the hub's order callout input).
// Paid / CashDueCents (00019): what the Pi knows of the order's money at
// ready (paid and Snapshot.CashDue; a terminal order: its own balance).
type OrderReadyData struct {
	FeedOrderRef
	Number          *int64 `json:"number"`
	FulfillmentType string `json:"fulfillment_type"`
	Paid            bool   `json:"paid"`
	CashDueCents    int64  `json:"cash_due_cents"`
	// Callout false (2.4.0): READY was held on the tablet - not called out.
	// Absent (an older Pi) = true.
	Callout *bool `json:"callout,omitempty"`
}
