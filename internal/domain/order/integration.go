package order

import (
	"crypto/sha1"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/google/uuid"
)

// The store hub integration (2.1.0): POS orders pulled from the store's old
// POS, the feature toggles and kitchen-ticket switches staff control, the
// default pickup / delivery times, the hub's reported status, and the cursor
// feed the hub reads (integration_events).

// SourcePOS is Snapshot.Source of an order pulled from the POS;
// ExtSourceSoftTouch the only POS there is (Status.ExtSource).
const (
	SourcePOS          = "pos"
	ExtSourceSoftTouch = "softtouch"
)

// Features are the 2.1.0 feature toggles (settings.features).
type Features struct {
	PosPull         bool `json:"pos_pull"`
	OrderInsert     bool `json:"order_insert"`
	DriverWriteback bool `json:"driver_writeback"`
	AddressHeal     bool `json:"address_heal"`
	FutureOrders    bool `json:"future_orders"`
	PaidWave        bool `json:"paid_wave"`
}

// DefaultFeatures: everything on except the online order insert (off until
// proven at the store).
func DefaultFeatures() Features {
	return Features{PosPull: true, OrderInsert: false, DriverWriteback: true, AddressHeal: true, FutureOrders: true, PaidWave: true}
}

// TicketToggles are the kitchen station ticket switches (settings.ticket_toggles).
type TicketToggles struct {
	PosDelivery        bool `json:"pos_delivery"`
	PosTakeout         bool `json:"pos_takeout"`
	PosCounter         bool `json:"pos_counter"`
	OnlineCashPickup   bool `json:"online_cash_pickup"`
	OnlineCashDelivery bool `json:"online_cash_delivery"`
	OnlineCard         bool `json:"online_card"`
}

// DefaultTicketToggles: the POS prints its own orders' tickets; online
// orders print as before.
func DefaultTicketToggles() TicketToggles {
	return TicketToggles{OnlineCashPickup: true, OnlineCashDelivery: true, OnlineCard: true}
}

// POSFor is the switch for a POS order's tickets: delivery, takeout (pickup)
// or counter (dine_in).
func (t TicketToggles) POSFor(fulfillmentType string) bool {
	switch strings.ToLower(strings.TrimSpace(fulfillmentType)) {
	case "delivery":
		return t.PosDelivery
	case "pickup":
		return t.PosTakeout
	default:
		return t.PosCounter
	}
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

// IntegrationSettings is the {features, tickets, times} triple served in
// /api/floor.settings, SSE settings.updated and the feed's settings.changed.
type IntegrationSettings struct {
	Features Features      `json:"features"`
	Tickets  TicketToggles `json:"tickets"`
	Times    Times         `json:"times"`
}

// DefaultIntegrationSettings are the migration defaults.
func DefaultIntegrationSettings() IntegrationSettings {
	return IntegrationSettings{Features: DefaultFeatures(), Tickets: DefaultTicketToggles(), Times: DefaultTimes}
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
	if !set.Features.FutureOrders || so.IsPOS() {
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

// Insert modes the hub reports (its per-write mode for the online insert).
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

// IntegrationStaleAfter: no status PUT for this long = the POS link is down.
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
	SeenAt       time.Time `json:"seen_at"`
}

// IntegrationView is /api/floor.integration and SSE integration.status.
type IntegrationView struct {
	Connected    bool       `json:"connected"`
	OK           bool       `json:"ok"`
	InsertMode   string     `json:"insert_mode"`
	InsertLive   bool       `json:"insert_live"`
	Problems     []Problem  `json:"problems"`
	LastSeenAt   *time.Time `json:"last_seen_at"`
	OperationDay *string    `json:"operation_day,omitempty"`
	Version      string     `json:"version,omitempty"`
}

// ProblemLinkDown is added when the hub went quiet.
var ProblemLinkDown = Problem{Code: "pos_link_down", Text: "POS link down", Severity: SeveritySerious}

// View is the status as staff see it at now: connected only while the hub
// reported within IntegrationStaleAfter (else a serious "POS link down", once
// a hub was ever seen); insert_live = features.order_insert && mode live &&
// connected.
func (st IntegrationStatus) View(now time.Time, f Features) IntegrationView {
	v := IntegrationView{InsertMode: st.InsertMode, Problems: []Problem{}, Version: st.Version, OperationDay: st.OperationDay}
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
	v.InsertLive = f.OrderInsert && st.InsertMode == InsertLive
	return v
}

// --- POS orders --------------------------------------------------------------

// ExternalOrder is PUT /api/integration/orders/{ext_ref}: one POS check as
// the hub sees it.
type ExternalOrder struct {
	Source          string           `json:"source"`
	Hash            string           `json:"hash"`
	DisplayNumber   int64            `json:"display_number"`
	CheckType       string           `json:"check_type"` // delivery | pickup | dine_in
	SentAt          *time.Time       `json:"sent_at"`
	PlacedAt        *time.Time       `json:"placed_at"`
	Customer        ExternalCustomer `json:"customer"`
	Address         *Address         `json:"address"`
	Items           []ExternalItem   `json:"items"`
	Notes           string           `json:"notes"`
	Money           ExternalMoney    `json:"money"`
	Paid            bool             `json:"paid"`
	CashDueCents    int64            `json:"cash_due_cents"`
	TipCents        int64            `json:"tip_cents"`
	BoardHidden     bool             `json:"board_hidden"`
	DriverPosNumber *string          `json:"driver_pos_number"`
}

type ExternalCustomer struct {
	DisplayName string `json:"display_name"`
	Phone       string `json:"phone"`
}

type ExternalItem struct {
	Name           string         `json:"name"`
	Size           string         `json:"size"`
	Quantity       int            `json:"quantity"`
	UnitPriceCents int64          `json:"unit_price_cents"`
	LineTotalCents int64          `json:"line_total_cents"`
	Notes          string         `json:"notes"`
	PrinterID      string         `json:"printer_id"`
	Children       []ExternalItem `json:"children"`
}

type ExternalMoney struct {
	SubtotalCents    int64 `json:"subtotal_cents"`
	TaxCents         int64 `json:"tax_cents"`
	TipCents         int64 `json:"tip_cents"`
	DeliveryFeeCents int64 `json:"delivery_fee_cents"`
	TotalCents       int64 `json:"total_cents"`
}

// Validate checks the upsert body.
func (e *ExternalOrder) Validate() error {
	if e.Source == "" {
		e.Source = ExtSourceSoftTouch
	}
	if e.Source != ExtSourceSoftTouch {
		return fmt.Errorf("source must be %q", ExtSourceSoftTouch)
	}
	switch e.CheckType {
	case "delivery", "pickup", "dine_in":
	default:
		return fmt.Errorf("check_type must be delivery, pickup or dine_in")
	}
	if strings.TrimSpace(e.Hash) == "" {
		return fmt.Errorf("hash is required")
	}
	if e.CashDueCents < 0 || e.TipCents < 0 {
		return fmt.Errorf("cash_due_cents and tip_cents must not be negative")
	}
	return nil
}

// posItemNS seeds the stable line-item ids of POS orders.
var posItemNS = uuid.MustParse("5b0f3a52-1c41-4f7e-9e0b-7f4f0c9a2d10")

// Snapshot builds the canonical order for a POS check. Items keep stable ids
// (check + position); a line with a Pi printer is routed there, one without
// is receipt-only (the POS decides what the kitchen gets). Payments carry
// the hub's figures so Snapshot.CashDue is the cash still due.
func (e ExternalOrder) Snapshot(id, tenantID, locationID, extRef string, now time.Time) Snapshot {
	s := Snapshot{
		ID: id, TenantID: tenantID, LocationID: locationID, Source: SourcePOS,
		SourceEventID: e.Source + ":" + extRef, Status: "accepted", Currency: "USD",
		Customer:   Customer{DisplayName: strings.TrimSpace(e.Customer.DisplayName), Phone: strings.TrimSpace(e.Customer.Phone)},
		Notes:      e.Notes,
		ReceivedAt: now.UTC(),
	}
	s.Fulfillment.Type = e.CheckType
	if e.CheckType == "delivery" && e.Address != nil {
		a := *e.Address
		s.Fulfillment.Address = &a
	}
	if e.PlacedAt != nil {
		s.PlacedAt = e.PlacedAt.UTC()
	} else if e.SentAt != nil {
		s.PlacedAt = e.SentAt.UTC()
	} else {
		s.PlacedAt = now.UTC()
	}
	tip := e.Money.TipCents
	if tip == 0 {
		tip = e.TipCents
	}
	s.Money = Money{Subtotal: e.Money.SubtotalCents, Tax: e.Money.TaxCents, Tip: tip, Total: e.Money.TotalCents}
	if e.Money.DeliveryFeeCents > 0 {
		s.Money.Fees = []Fee{{Type: "delivery", Label: "Delivery fee", Amount: e.Money.DeliveryFeeCents}}
	}
	if e.CashDueCents > 0 {
		s.Payments = append(s.Payments, PaymentSummary{Method: "cash", AmountCents: e.CashDueCents})
	}
	if rest := e.Money.TotalCents - e.CashDueCents; rest > 0 {
		s.Payments = append(s.Payments, PaymentSummary{Method: "other", AmountCents: rest})
	}
	s.Items = make([]LineItem, 0, len(e.Items))
	for i, it := range e.Items {
		li := externalLine(it, extRef, fmt.Sprint(i), i)
		li.PrinterID = strings.TrimSpace(it.PrinterID)
		li.Routing = RoutingUnassigned
		if li.PrinterID != "" {
			li.Routing = RoutingMatched
		}
		s.Items = append(s.Items, li)
	}
	return s
}

func externalLine(it ExternalItem, extRef, path string, pos int) LineItem {
	qty := it.Quantity
	if qty <= 0 {
		qty = 1
	}
	li := LineItem{
		ID: uuid.NewSHA1(posItemNS, []byte(extRef+"/"+path)).String(), Position: pos,
		Name: it.Name, Size: it.Size, Quantity: qty, UnitPrice: it.UnitPriceCents, LineTotal: it.LineTotalCents, Notes: it.Notes,
	}
	for j, c := range it.Children {
		li.Children = append(li.Children, externalLine(c, extRef, fmt.Sprintf("%s.%d", path, j), j))
	}
	return li
}

// WipePII clears a POS order's customer name, phone and address (Close
// batch); items, money, number and times stay.
func (s *Snapshot) WipePII() {
	s.Customer = Customer{}
	s.Fulfillment.Address = nil
}

// --- the integration feed ------------------------------------------------------

// Feed event types (integration_events.type).
const (
	FeedOrderAccepted    = "order.accepted"
	FeedRunSent          = "run.sent"
	FeedOrderReady       = "order.ready"
	FeedAddressConfirmed = "address.confirmed"
	FeedOrderCancelled   = "order.cancelled"
	FeedSettingsChanged  = "settings.changed"
	FeedBatchClosed      = "batch.closed"
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

// FeedDriver is a driver on a run.sent event (pos_number null when unset).
type FeedDriver struct {
	ID        string  `json:"id"`
	Name      string  `json:"name"`
	PosNumber *string `json:"pos_number"`
}

// FeedOrderRef identifies an order on the feed.
type FeedOrderRef struct {
	OrderID string  `json:"order_id"`
	Source  string  `json:"source"`
	ExtRef  *string `json:"ext_ref"`
}

// RefOf is the feed reference of an order.
func RefOf(so StoredOrder) FeedOrderRef {
	r := FeedOrderRef{OrderID: so.Order.ID, Source: so.Order.Source}
	if so.Status.ExtRef != "" {
		v := so.Status.ExtRef
		r.ExtRef = &v
	}
	return r
}

// RunSentData is a run.sent event.
type RunSentData struct {
	Driver FeedDriver     `json:"driver"`
	RunID  string         `json:"run_id"`
	Orders []FeedOrderRef `json:"orders"`
}

// OrderReadyData is an order.ready event (the future TV callout's input).
type OrderReadyData struct {
	FeedOrderRef
	Number          *int64 `json:"number"`
	FulfillmentType string `json:"fulfillment_type"`
}

// AddressConfirmedData is an address.confirmed event (address null for a pin).
type AddressConfirmedData struct {
	OrderID string            `json:"order_id"`
	ExtRef  *string           `json:"ext_ref"`
	How     string            `json:"how"` // candidate | pin
	Address *ConfirmedAddress `json:"address"`
	Lat     float64           `json:"lat"`
	Lng     float64           `json:"lng"`
}

// BatchClosedData is a batch.closed event.
type BatchClosedData struct {
	BatchID  int64     `json:"batch_id"`
	ClosedAt time.Time `json:"closed_at"`
}

// OrderAcceptedData is an order.accepted event (online orders only).
type OrderAcceptedData struct {
	Order StoredOrder `json:"order"`
}

// PosOpaqueID is a POS stop's anonymous id in delivery.run (no order id,
// name or address ever reaches main): "pos:" + sha1(batch id + check)[:12].
func PosOpaqueID(batchID int64, extRef string) string {
	sum := sha1.Sum([]byte(fmt.Sprintf("%d%s", batchID, extRef)))
	return "pos:" + hex.EncodeToString(sum[:])[:12]
}
