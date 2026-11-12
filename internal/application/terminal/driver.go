package terminal

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	orderApp "quiccpos/agent/internal/application/order"
	"quiccpos/agent/internal/domain/order"
	"quiccpos/agent/internal/domain/terminal"

	"github.com/google/uuid"
)

// The driver view (contract/agent.md §2.10b): a staff member with the role
// driver, logged in on a terminal, assigns himself deliveries nobody took
// (Dispatch: unassigned past the store's driver_claim_after_minutes; they
// become his and delivered, he never goes out), sees his tickets, adds a
// tip, "closes" an order (delivered) and prints his report. He is a Pi driver on the roster,
// linked at his first login by staff id. Every action is journaled and
// announced as terminal.driver.* on /api/events; the floor changes go
// through the same transitions as Quibase's (order.updated, drivers.*).

// SSE events of the driver view.
const (
	EventDriverLinked  = "terminal.driver.linked"
	EventDriverClaimed = "terminal.driver.claimed"
	EventDriverTip     = "terminal.driver.tip"
	EventDriverClosed  = "terminal.driver.closed"
	EventDriverReport  = "terminal.driver.report"
)

// MaxClaim is the most orders one claim may take.
const MaxClaim = 20

// Floor is what the driver view needs from the floor (orderApp.Service).
type Floor interface {
	ListDrivers(ctx context.Context) ([]order.Driver, error)
	CreateDriver(ctx context.Context, name, posNumber string) (*order.Driver, error)
	OpenDeliveries(ctx context.Context) ([]order.StoredOrder, error)
	UnassignedDelivered(ctx context.Context) ([]order.StoredOrder, error)
	ListByBatch(ctx context.Context, batchID int64, offset, limit int) ([]order.StoredOrder, error)
	GetOrder(ctx context.Context, id string) (*order.StoredOrder, error)
	ClaimDelivered(ctx context.Context, driverID string, ids []string, cutoff time.Time, by order.MarkedBy) ([]order.StoredOrder, error)
	DeliverOne(ctx context.Context, driverID, orderID string) (*order.StoredOrder, bool, error)
	SetTip(ctx context.Context, id string, cents int64) (*order.StoredOrder, error)
	SetDoorTip(ctx context.Context, id string, cents int64) (*order.StoredOrder, error)
	DriverReport(ctx context.Context, driverID string) (*order.DriverReport, error)
	PrintDriverReport(ctx context.Context, driverID, trigger string) (*orderApp.DriverReportPrint, error)
}

// DriverLinks stores which Pi driver each driver staff member is.
type DriverLinks interface {
	TerminalDriverLink(ctx context.Context, staffID string) (string, error)
	TerminalDriverLinks(ctx context.Context) (map[string]string, error)
	LinkTerminalDriver(ctx context.Context, staffID, driverID string) error
}

// driverView is the driver view's state on the Service.
type driverView struct {
	floor  Floor
	links  DriverLinks
	linkMu sync.Mutex
}

// SetFloor wires the driver view (cmd/main.go): the floor and the link store.
func (s *Service) SetFloor(f Floor, links DriverLinks) {
	s.drv.floor, s.drv.links = f, links
}

var errNoFloor = errors.New("the driver view isn't running")

// DriverPublic is a driver as the driver view shows him.
type DriverPublic struct {
	ID          string     `json:"id"`
	Name        string     `json:"name"`
	Status      string     `json:"status"`
	ClockedInAt *time.Time `json:"clocked_in_at,omitempty"`
}

func driverPublic(d order.Driver) *DriverPublic {
	return &DriverPublic{ID: d.ID, Name: d.Name, Status: d.Status, ClockedInAt: d.ClockedInAt}
}

func sameName(a, b string) bool {
	return strings.EqualFold(strings.Join(strings.Fields(a), " "), strings.Join(strings.Fields(b), " "))
}

// rosterName is the name a driver staff member gets on the roster (1-40
// characters): "First L.".
func rosterName(st terminal.Staff) string {
	n := st.Name()
	if n == "" {
		n = "Driver"
	}
	if utf8.RuneCountInString(n) > 40 {
		n = string([]rune(n)[:40])
	}
	return n
}

// ensureDriver is the staff member's Pi driver: the linked one, else an
// unlinked active driver with the same name ("First Last", "First L.", or
// the first name when only one driver has it), else a new one. Linked by
// staff id; journaled + announced when a link is made.
func (s *Service) ensureDriver(ctx context.Context, a Actor, st terminal.Staff) (*order.Driver, error) {
	f, links := s.drv.floor, s.drv.links
	if f == nil || links == nil {
		return nil, errNoFloor
	}
	s.drv.linkMu.Lock()
	defer s.drv.linkMu.Unlock()
	drivers, err := f.ListDrivers(ctx)
	if err != nil {
		return nil, err
	}
	linked, err := links.TerminalDriverLink(ctx, st.ID)
	if err != nil {
		return nil, err
	}
	for i := range drivers {
		if drivers[i].ID == linked {
			return &drivers[i], nil
		}
	}
	all, err := links.TerminalDriverLinks(ctx)
	if err != nil {
		return nil, err
	}
	taken := map[string]bool{}
	for staff, drv := range all {
		if staff != st.ID {
			taken[drv] = true
		}
	}
	var pick *order.Driver
	full := strings.TrimSpace(st.FirstName + " " + st.LastName)
	for _, want := range []string{full, st.Name()} {
		for i := range drivers {
			if pick == nil && !taken[drivers[i].ID] && want != "" && sameName(drivers[i].Name, want) {
				pick = &drivers[i]
			}
		}
	}
	if pick == nil && strings.TrimSpace(st.FirstName) != "" {
		var hits []int
		for i := range drivers {
			if !taken[drivers[i].ID] && sameName(drivers[i].Name, st.FirstName) {
				hits = append(hits, i)
			}
		}
		if len(hits) == 1 {
			pick = &drivers[hits[0]]
		}
	}
	created := false
	if pick == nil {
		d, err := f.CreateDriver(ctx, rosterName(st), "")
		if err != nil {
			return nil, err
		}
		pick, created = d, true
	}
	if err := links.LinkTerminalDriver(ctx, st.ID, pick.ID); err != nil {
		return nil, err
	}
	s.journal(ctx, terminal.Entry{StaffID: st.ID, DeviceID: a.DeviceID, Action: terminal.ActionDriverLinked,
		Detail: detail(map[string]any{"driver_id": pick.ID, "driver_name": pick.Name, "created": created})})
	s.publish(EventDriverLinked, map[string]any{"staff_id": st.ID, "driver_id": pick.ID, "driver_name": pick.Name, "created": created})
	s.logger.Info().Ctx(ctx).Str("staff_id", st.ID).Str("driver_id", pick.ID).Bool("created", created).Msg("Driver staff linked to the roster")
	return pick, nil
}

// driverOf is the driver logged in on the terminal and his Pi driver
// (terminal.ErrDriverOnly for another role).
func (s *Service) driverOf(ctx context.Context, a Actor) (terminal.Staff, *order.Driver, error) {
	st, _, err := s.staffOf(a)
	if err != nil {
		return st, nil, err
	}
	if st.Role != terminal.RoleDriver {
		return st, nil, terminal.ErrDriverOnly
	}
	d, err := s.ensureDriver(ctx, a, st)
	return st, d, err
}

// --- views -----------------------------------------------------------------------------

// DriverAddress is a delivery address.
type DriverAddress struct {
	Line1 string `json:"line1"`
	Line2 string `json:"line2,omitempty"`
	City  string `json:"city,omitempty"`
	State string `json:"state,omitempty"`
	Zip   string `json:"zip,omitempty"`
	Notes string `json:"notes,omitempty"`
}

// Payment states a driver sees.
const (
	PayPaid     = "paid"      // nothing to collect
	PayCashDue  = "cash_due"  // collect cash_due_cents
	PayCardHold = "card_hold" // a website card order main hasn't charged yet (a tip raises the hold)
)

// cardChannel: a website / channel order paid by card (main holds the card;
// ADD TIP raises it). Counter orders are the terminal's own: never.
func cardChannel(so order.StoredOrder) bool {
	return so.IsChannel() && so.Order.Source != order.SourceCounter && so.Order.CashDue() == 0
}

func paymentOf(so order.StoredOrder) string {
	switch {
	case so.Status.CashDueCents > 0:
		return PayCashDue
	case cardChannel(so) && !so.Status.TipLocked:
		return PayCardHold
	}
	return PayPaid
}

func addressOf(so order.StoredOrder) (*DriverAddress, string) {
	a := so.Order.Fulfillment.Address
	if a == nil {
		return nil, ""
	}
	return &DriverAddress{Line1: strings.TrimSpace(a.Line1), Line2: strings.TrimSpace(a.Line2), City: a.City, State: a.State,
		Zip: a.PostalCode, Notes: a.Notes}, order.StreetOnly(a.Line1)
}

// AvailableOrder is one entry of the claim list.
type AvailableOrder struct {
	OrderID string `json:"order_id"`
	Number  *int64 `json:"number"`
	// State: accepted (cooking), ready, or delivered (marked delivered
	// with no driver).
	State   string         `json:"state"`
	Street  string         `json:"street"`
	Address *DriverAddress `json:"address"`
	// Since: the clock the rule counts from (order.ClaimSince: accepted,
	// else arrived); WaitingMinutes = whole minutes since then.
	Since          time.Time  `json:"since"`
	WaitingMinutes int64      `json:"waiting_minutes"`
	ReadyAt        *time.Time `json:"ready_at"`
	Payment        string     `json:"payment"`
	CashDueCents   int64      `json:"cash_due_cents"`
	TotalCents     int64      `json:"total_cents"`
}

// claimCutoff: an order waiting since at or before this may be claimed
// (now - the store's driver_claim_after_minutes, default 45).
func (s *Service) claimCutoff(now time.Time) time.Time {
	cfg, _ := s.store.Config()
	return now.Add(-time.Duration(cfg.DriverClaimAfterMinutes()) * time.Minute)
}

// Available is GET /api/terminal/driver/available: the Dispatch list -
// deliveries of the open batch nobody took (no driver, in no pool; cooking,
// ready, or delivered with no driver) waiting longer than the store's
// driver_claim_after_minutes, longest waiting first (first come, first
// served).
func (s *Service) Available(ctx context.Context, a Actor) ([]AvailableOrder, error) {
	if _, _, err := s.driverOf(ctx, a); err != nil {
		return nil, err
	}
	open, err := s.drv.floor.OpenDeliveries(ctx)
	if err != nil {
		return nil, err
	}
	done, err := s.drv.floor.UnassignedDelivered(ctx)
	if err != nil {
		return nil, err
	}
	now := s.now()
	cutoff := s.claimCutoff(now)
	var list []order.StoredOrder
	for _, so := range append(open, done...) {
		if order.ClaimableLate(so, so.Status.BatchID, cutoff) {
			list = append(list, so)
		}
	}
	sort.SliceStable(list, func(i, j int) bool { return order.ClaimSince(list[i]).Before(order.ClaimSince(list[j])) })
	out := make([]AvailableOrder, 0, len(list))
	for _, so := range list {
		addr, street := addressOf(so)
		since := order.ClaimSince(so)
		out = append(out, AvailableOrder{OrderID: so.Order.ID, Number: so.Number(), State: string(so.Status.State), Street: street,
			Address: addr, Since: since, WaitingMinutes: int64(now.Sub(since) / time.Minute), ReadyAt: so.Status.ReadyAt,
			Payment: paymentOf(so), CashDueCents: so.Status.CashDueCents, TotalCents: so.Order.Money.Total})
	}
	return out, nil
}

// DriverTicket is one of his orders.
type DriverTicket struct {
	OrderID       string         `json:"order_id"`
	Number        *int64         `json:"number"`
	State         string         `json:"state"`
	Name          string         `json:"name"`
	Phone         string         `json:"phone"`
	Address       *DriverAddress `json:"address"`
	Street        string         `json:"street"`
	Notes         string         `json:"notes"`
	Geo           *DriverGeo     `json:"geo"`
	Payment       string         `json:"payment"`
	CashDueCents  int64          `json:"cash_due_cents"`
	TotalCents    int64          `json:"total_cents"`
	TipCents      int64          `json:"tip_cents"`
	AddedTipCents int64          `json:"added_tip_cents"`
	DoorTipCents  int64          `json:"door_tip_cents"`
	TipKind       string         `json:"tip_kind"` // card = raises the card hold, door = recorded on his report
	TipLocked     bool           `json:"tip_locked"`
	RunSeq        *int64         `json:"run_seq"`
	OutAt         *time.Time     `json:"out_at"`
	DeliveredAt   *time.Time     `json:"delivered_at"`
}

// DriverGeo is the order's map point (nil until main located it).
type DriverGeo struct {
	Lat     float64 `json:"lat"`
	Lng     float64 `json:"lng"`
	Quality string  `json:"quality,omitempty"`
}

func ticketOf(so order.StoredOrder) DriverTicket {
	addr, street := addressOf(so)
	t := DriverTicket{OrderID: so.Order.ID, Number: so.Number(), State: string(so.Status.State), Name: so.Order.Customer.DisplayName,
		Phone: so.Order.Customer.Phone, Address: addr, Street: street, Notes: so.Order.Notes, Payment: paymentOf(so),
		CashDueCents: so.Status.CashDueCents, TotalCents: so.Order.Money.Total, AddedTipCents: so.Status.AddedTipCents,
		DoorTipCents: so.Status.DoorTipCents, TipLocked: so.Status.TipLocked, RunSeq: so.Status.RunSeq, OutAt: so.Status.OutAt,
		DeliveredAt: so.Status.DeliveredAt, TipKind: "door"}
	t.TipCents = so.Order.Money.Tip + so.Status.AddedTipCents + so.Status.DoorTipCents
	if cardChannel(so) {
		t.TipKind = "card"
	}
	if g := so.Geo; g != nil && g.Lat != nil && g.Lng != nil {
		t.Geo = &DriverGeo{Lat: *g.Lat, Lng: *g.Lng, Quality: g.Quality}
	}
	return t
}

// MineView is GET /api/terminal/driver/mine.
type MineView struct {
	Driver    *DriverPublic  `json:"driver"`
	Out       []DriverTicket `json:"out"`
	Delivered []DriverTicket `json:"delivered"`
}

// Mine is his orders: out (run order) and the ones he closed in the open
// batch (a tip can still be added to those).
func (s *Service) Mine(ctx context.Context, a Actor) (*MineView, error) {
	_, d, err := s.driverOf(ctx, a)
	if err != nil {
		return nil, err
	}
	return s.mine(ctx, d)
}

func (s *Service) mine(ctx context.Context, d *order.Driver) (*MineView, error) {
	v := &MineView{Driver: driverPublic(*d), Out: []DriverTicket{}, Delivered: []DriverTicket{}}
	open, err := s.drv.floor.OpenDeliveries(ctx)
	if err != nil {
		return nil, err
	}
	var out []order.StoredOrder
	for _, so := range open {
		if so.Status.State == order.StateOnDelivery && so.Status.DriverID == d.ID {
			out = append(out, so)
		}
	}
	sort.SliceStable(out, func(i, j int) bool { return seqOf(out[i]) < seqOf(out[j]) })
	for _, so := range out {
		v.Out = append(v.Out, ticketOf(so))
	}
	batch, err := s.drv.floor.ListByBatch(ctx, 0, 0, 2000)
	if err != nil {
		return nil, err
	}
	for _, so := range batch { // newest first
		if so.Status.State == order.StateDelivered && so.Status.DriverID == d.ID {
			v.Delivered = append(v.Delivered, ticketOf(so))
		}
	}
	return v, nil
}

func seqOf(so order.StoredOrder) int64 {
	if so.Status.RunSeq != nil {
		return *so.Status.RunSeq
	}
	return 1 << 40
}

// --- actions ---------------------------------------------------------------------------

// ClaimIn is POST /api/terminal/driver/claim.
type ClaimIn struct {
	OrderIDs []string `json:"order_ids"`
}

// ClaimResult is the claim's answer: the orders are his, delivered.
type ClaimResult struct {
	Driver *DriverPublic       `json:"driver"`
	Orders []order.StoredOrder `json:"orders"`
}

// Claim assigns the orders to him and marks them delivered (when they
// weren't): all or nothing, first come first served (*order.ErrTaken, 409
// taken, for an order someone got first or that isn't claimable). He does
// not go out and his status doesn't change; the orders count on his driver
// report. A driver can never unassign.
func (s *Service) Claim(ctx context.Context, a Actor, in ClaimIn) (*ClaimResult, error) {
	st, d, err := s.driverOf(ctx, a)
	if err != nil {
		return nil, err
	}
	ids := make([]string, 0, len(in.OrderIDs))
	seen := map[string]bool{}
	for _, id := range in.OrderIDs {
		if _, err := uuid.Parse(id); err != nil {
			return nil, badRequest("order_ids must be order uuids")
		}
		if !seen[id] {
			seen[id] = true
			ids = append(ids, id)
		}
	}
	if len(ids) == 0 || len(ids) > MaxClaim {
		return nil, badRequest(fmt.Sprintf("order_ids: 1-%d orders", MaxClaim))
	}
	by := order.MarkedBy{Kind: order.ByTerminal, Name: d.Name, DeviceID: a.DeviceID}
	got, err := s.drv.floor.ClaimDelivered(ctx, d.ID, ids, s.claimCutoff(s.now()), by)
	if err != nil {
		var tk *order.ErrTaken
		if errors.As(err, &tk) {
			s.logger.Info().Ctx(ctx).Str("driver_id", d.ID).Strs("taken", tk.IDs).Msg("Driver claim lost: taken")
		}
		return nil, err
	}
	claimed := make([]string, 0, len(got))
	numbers := make([]int64, 0, len(got))
	for _, so := range got {
		claimed = append(claimed, so.Order.ID)
		if n := so.Number(); n != nil {
			numbers = append(numbers, *n)
		}
	}
	s.journal(ctx, terminal.Entry{StaffID: st.ID, DeviceID: a.DeviceID, Action: terminal.ActionDriverClaim,
		Detail: detail(map[string]any{"driver_id": d.ID, "order_ids": ids, "delivered": claimed, "numbers": numbers})})
	s.publish(EventDriverClaimed, map[string]any{"staff_id": st.ID, "driver_id": d.ID, "driver_name": d.Name, "order_ids": ids,
		"delivered": claimed})
	s.orders.DevEvent(fmt.Sprintf("%s claimed %d unassigned deliveries on a terminal", d.Name, len(ids)))
	orders := make([]order.StoredOrder, 0, len(ids))
	for _, id := range ids { // the request's order; already-his ones too
		so, err := s.drv.floor.GetOrder(ctx, id)
		if err != nil {
			return nil, err
		}
		orders = append(orders, *so)
	}
	return &ClaimResult{Driver: driverPublic(*d), Orders: orders}, nil
}

// his loads one of his orders (out or delivered by him in the open batch).
func (s *Service) his(ctx context.Context, d *order.Driver, id string) (*order.StoredOrder, error) {
	if _, err := uuid.Parse(id); err != nil {
		return nil, order.ErrNotFound
	}
	so, err := s.drv.floor.GetOrder(ctx, id)
	if err != nil {
		return nil, err
	}
	if so.Status.DriverID != d.ID || (so.Status.State != order.StateOnDelivery && so.Status.State != order.StateDelivered) {
		return nil, terminal.ErrNotYourOrder
	}
	return so, nil
}

// TipIn is POST /api/terminal/driver/orders/{id}/tip.
type TipIn struct {
	TipCents *int64 `json:"tip_cents"`
}

// TipResult is the order with its tip and how it was taken.
type TipResult struct {
	Ticket DriverTicket `json:"ticket"`
	Kind   string       `json:"kind"`
}

// Tip sets the whole tip added after the order (absolute) on one of his
// orders: a website card order raises the card hold (ADD TIP's rules: 409
// tip_locked once charged); a cash / counter order records it on his report
// (a tip he got at the door).
func (s *Service) Tip(ctx context.Context, a Actor, id string, in TipIn) (*TipResult, error) {
	st, d, err := s.driverOf(ctx, a)
	if err != nil {
		return nil, err
	}
	if in.TipCents == nil || *in.TipCents < 0 || *in.TipCents > order.MaxAddedTipCents {
		return nil, badRequest(fmt.Sprintf("tip_cents is 0-%d", order.MaxAddedTipCents))
	}
	cur, err := s.his(ctx, d, id)
	if err != nil {
		return nil, err
	}
	kind, prev := "door", cur.Status.DoorTipCents
	var so *order.StoredOrder
	if cardChannel(*cur) {
		kind, prev = "card", cur.Status.AddedTipCents
		so, err = s.drv.floor.SetTip(ctx, id, *in.TipCents)
	} else {
		so, err = s.drv.floor.SetDoorTip(ctx, id, *in.TipCents)
	}
	if err != nil {
		return nil, err
	}
	s.journal(ctx, terminal.Entry{StaffID: st.ID, DeviceID: a.DeviceID, Action: terminal.ActionDriverTip, OrderID: id,
		Detail: detail(map[string]any{"driver_id": d.ID, "tip_cents": *in.TipCents, "prev_cents": prev, "kind": kind})})
	s.publish(EventDriverTip, map[string]any{"staff_id": st.ID, "driver_id": d.ID, "order_id": id, "tip_cents": *in.TipCents, "kind": kind})
	return &TipResult{Ticket: ticketOf(*so), Kind: kind}, nil
}

// CloseResult is POST /api/terminal/driver/orders/{id}/close's answer.
type CloseResult struct {
	Ticket   DriverTicket  `json:"ticket"`
	Returned bool          `json:"returned"`
	Driver   *DriverPublic `json:"driver"`
}

// CloseOrder is his "close": the order is delivered (it is closed for good
// at Close batch or by a manager on the Pi). His last open order puts him
// back in the queue (returned).
func (s *Service) CloseOrder(ctx context.Context, a Actor, id string) (*CloseResult, error) {
	st, d, err := s.driverOf(ctx, a)
	if err != nil {
		return nil, err
	}
	cur, err := s.his(ctx, d, id)
	if err != nil {
		return nil, err
	}
	if cur.Status.State != order.StateOnDelivery {
		return nil, fmt.Errorf("already closed: %w", order.ErrInvalidState)
	}
	so, returned, err := s.drv.floor.DeliverOne(ctx, d.ID, id)
	if err != nil {
		return nil, err
	}
	if returned {
		d.Status = order.DriverAvailable
	}
	s.journal(ctx, terminal.Entry{StaffID: st.ID, DeviceID: a.DeviceID, Action: terminal.ActionDriverClose, OrderID: id,
		Detail: detail(map[string]any{"driver_id": d.ID, "number": so.Number(), "returned": returned})})
	s.publish(EventDriverClosed, map[string]any{"staff_id": st.ID, "driver_id": d.ID, "order_id": id, "returned": returned})
	return &CloseResult{Ticket: ticketOf(*so), Returned: returned, Driver: driverPublic(*d)}, nil
}

// Report is GET /api/terminal/driver/report: his driver report for the open
// batch so far (cash, tips, orders), as the slip prints it.
func (s *Service) Report(ctx context.Context, a Actor) (*order.DriverReport, error) {
	_, d, err := s.driverOf(ctx, a)
	if err != nil {
		return nil, err
	}
	return s.drv.floor.DriverReport(ctx, d.ID)
}

// PrintReport prints his report on the receipt printer (the existing
// driver-report slip). The result says where it printed, or why not.
func (s *Service) PrintReport(ctx context.Context, a Actor) (*orderApp.DriverReportPrint, error) {
	st, d, err := s.driverOf(ctx, a)
	if err != nil {
		return nil, err
	}
	res, err := s.drv.floor.PrintDriverReport(ctx, d.ID, "terminal")
	if err != nil {
		return nil, err
	}
	printed := res.Error == ""
	s.journal(ctx, terminal.Entry{StaffID: st.ID, DeviceID: a.DeviceID, Action: terminal.ActionDriverReport,
		Detail: detail(map[string]any{"driver_id": d.ID, "printed": printed, "printed_on": res.PrintedOn, "error": res.Error})})
	s.publish(EventDriverReport, map[string]any{"staff_id": st.ID, "driver_id": d.ID, "printed": printed, "printed_on": res.PrintedOn})
	return res, nil
}
