package terminal

import (
	"context"
	"encoding/json"
	"fmt"
	"sort"
	"strconv"
	"strings"
	"time"

	"quiccpos/agent/internal/domain/device"
	"quiccpos/agent/internal/domain/order"
	"quiccpos/agent/internal/domain/terminal"
)

// The All Checks Overview (contract/agent.md §2.10o, the owner 2026-11-12):
// every order the Pi holds (the open batch + the kept closed ones), any
// source, filtered and paginated; one order in full. Read only.

// Check is one order in the overview.
type Check struct {
	OrderID       string        `json:"order_id"`
	Number        *int64        `json:"number"`
	BatchID       int64         `json:"batch_id"`
	Source        string        `json:"source"`
	Type          string        `json:"type"` // counter | pickup | delivery
	State         string        `json:"state"`
	Voided        bool          `json:"voided"`
	PlacedAt      time.Time     `json:"placed_at"`
	Customer      CheckCustomer `json:"customer"`
	Street        string        `json:"street"`
	Items         int64         `json:"items"`
	TotalCents    int64         `json:"total_cents"`
	Paid          bool          `json:"paid"`
	CashDueCents  int64         `json:"cash_due_cents"`
	TipCents      int64         `json:"tip_cents"`
	RefundedCents int64         `json:"refunded_cents"`
	Tenders       []string      `json:"tenders"`
	StaffID       string        `json:"staff_id"`
	StaffName     string        `json:"staff_name"`
	DeviceID      string        `json:"device_id"`
	DeviceName    string        `json:"device_name"`
	// TakenBy: "Call center - <Name>" on an order the call center placed
	// (contract §2.10ab); "" on every other one. OutOfArea: its address is
	// outside the store's area (the agent acknowledged the warning).
	TakenBy   string `json:"taken_by"`
	OutOfArea bool   `json:"out_of_area"`
	// The Dispatch / Pickup bars (2026-11-12): the delivery's full address
	// (null for a pickup / counter check), its driver (the run's, else the
	// pool's; "" = none), how many payments (refunds and added tips
	// included) and the promised time (order.PromisedAt: the later of the
	// ready time and the customer's promised time; null when neither).
	Address       *CheckAddress `json:"address"`
	DriverID      string        `json:"driver_id"`
	DriverName    string        `json:"driver_name"`
	PaymentsCount int           `json:"payments_count"`
	PromisedAt    *time.Time    `json:"promised_at"`
	// RefundOwedCents (an online order edited at a terminal, §2.10aa): what
	// its customer is owed (the manager refunds it outside); 0 = none.
	RefundOwedCents int64 `json:"refund_owed_cents"`
	// Batch B (contract 2.10ak): reopened = open again after a reopen from
	// All checks (order.Status.OpenAgain), when it was last reopened, and
	// when the kitchen marked it READY (the rows' marker; null = not ready).
	Reopened   bool       `json:"reopened"`
	ReopenedAt *time.Time `json:"reopened_at"`
	ReadyAt    *time.Time `json:"ready_at"`

	// What the filters look at besides the fields above.
	staffs, devices map[string]bool
	search          string
}

// CheckAddress is a delivery's address on the overview.
type CheckAddress struct {
	Line1 string `json:"line1"`
	Line2 string `json:"line2"`
	City  string `json:"city"`
	State string `json:"state"`
	Zip   string `json:"zip"`
	Notes string `json:"notes"`
}

// CheckCustomer is the check's customer.
type CheckCustomer struct {
	Name  string `json:"name"`
	Phone string `json:"phone"`
}

// ChecksQuery is GET /api/terminal/checks' filters.
type ChecksQuery struct {
	Batch     string // <id> | current | all ("" = current)
	From, To  *time.Time
	Types     map[string]bool
	Sources   map[string]bool
	States    map[string]bool
	Paid      *bool
	StaffID   string
	DeviceID  string
	Tenders   map[string]bool
	MinCents  *int64
	MaxCents  *int64
	Text      string
	Limit     int
	Offset    int
	RawFromTo [2]string // as given (dates are store-local)
}

// ChecksPage is the overview's answer.
type ChecksPage struct {
	Checks   []Check `json:"checks"`
	Total    int     `json:"total"`
	Limit    int     `json:"limit"`
	Offset   int     `json:"offset"`
	BatchIDs []int64 `json:"batch_ids"`
	// Summary is over every check the filters match, not just this page
	// (contract 2.10ak: All checks' footer).
	Summary ChecksSummary `json:"summary"`
}

// ChecksSummary is the overview's totals over every matching check: how many,
// their totals and what is left to collect (voided / cancelled ones left out
// of the money, counted in Voided).
type ChecksSummary struct {
	Count      int   `json:"count"`
	TotalCents int64 `json:"total_cents"`
	DueCents   int64 `json:"due_cents"`
	Voided     int   `json:"voided"`
}

// summarize is the summary of checks.
func summarize(all []Check) ChecksSummary {
	sum := ChecksSummary{Count: len(all)}
	for _, c := range all {
		if c.Voided || c.State == string(order.StateCancelled) {
			sum.Voided++
			continue
		}
		sum.TotalCents += c.TotalCents
		sum.DueCents += c.CashDueCents
	}
	return sum
}

// CheckDetail is one order in full.
type CheckDetail struct {
	Check       Check              `json:"check"`
	Order       order.StoredOrder  `json:"order"`
	Payments    []terminal.Payment `json:"payments"`
	Adjustments []order.Discount   `json:"adjustments"`
	Journal     []terminal.Entry   `json:"journal"`
}

// ParseChecksQuery reads the overview's query (a bad value = ErrBadRequest).
func ParseChecksQuery(get func(string) string, loc *time.Location) (ChecksQuery, error) {
	q := ChecksQuery{Batch: strings.TrimSpace(get("batch")), Limit: 50}
	set := func(name string, allowed ...string) (map[string]bool, error) {
		v := strings.TrimSpace(get(name))
		if v == "" {
			return nil, nil
		}
		ok := map[string]bool{}
		for _, a := range allowed {
			ok[a] = true
		}
		out := map[string]bool{}
		for _, part := range strings.Split(v, ",") {
			p := strings.ToLower(strings.TrimSpace(part))
			if p == "" {
				continue
			}
			if len(allowed) > 0 && !ok[p] {
				return nil, badRequest(fmt.Sprintf("%s: %q isn't one of %s", name, p, strings.Join(allowed, ", ")))
			}
			out[p] = true
		}
		return out, nil
	}
	var err error
	if q.Types, err = set("type", "counter", "pickup", "delivery"); err != nil {
		return q, err
	}
	if q.Sources, err = set("source"); err != nil {
		return q, err
	}
	if q.States, err = set("state", "arrived", "scheduled", "accepted", "ready", "on_delivery", "delivered", "cancelled", "voided"); err != nil {
		return q, err
	}
	if q.Tenders, err = set("tender", terminal.TenderCash, terminal.TenderCard, terminal.TenderSquareApp, terminal.TenderManualCard,
		terminal.TenderComp, terminal.TenderOnline); err != nil {
		return q, err
	}
	switch v := strings.TrimSpace(get("paid")); v {
	case "":
	case "true", "1", "yes":
		t := true
		q.Paid = &t
	case "false", "0", "no":
		f := false
		q.Paid = &f
	default:
		return q, badRequest("paid is true or false")
	}
	cents := func(name string) (*int64, error) {
		v := strings.TrimSpace(get(name))
		if v == "" {
			return nil, nil
		}
		n, err := strconv.ParseInt(v, 10, 64)
		if err != nil || n < 0 {
			return nil, badRequest(name + " is a whole number of cents")
		}
		return &n, nil
	}
	if q.MinCents, err = cents("min_cents"); err != nil {
		return q, err
	}
	if q.MaxCents, err = cents("max_cents"); err != nil {
		return q, err
	}
	when := func(name string, end bool) (*time.Time, error) {
		v := strings.TrimSpace(get(name))
		if v == "" {
			return nil, nil
		}
		if t, err := time.Parse(time.RFC3339, v); err == nil {
			return &t, nil
		}
		d, err := time.ParseInLocation("2006-01-02", v, loc)
		if err != nil {
			return nil, badRequest(name + " is YYYY-MM-DD or RFC 3339")
		}
		if end {
			d = d.AddDate(0, 0, 1).Add(-time.Nanosecond)
		}
		return &d, nil
	}
	if q.From, err = when("from", false); err != nil {
		return q, err
	}
	if q.To, err = when("to", true); err != nil {
		return q, err
	}
	q.StaffID, q.DeviceID = strings.TrimSpace(get("staff_id")), strings.TrimSpace(get("device_id"))
	q.Text = strings.ToLower(strings.TrimSpace(get("q")))
	if v := strings.TrimSpace(get("limit")); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil || n < 1 || n > 200 {
			return q, badRequest("limit is 1-200")
		}
		q.Limit = n
	}
	if v := strings.TrimSpace(get("offset")); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil || n < 0 {
			return q, badRequest("offset is 0 or more")
		}
		q.Offset = n
	}
	return q, nil
}

// checkBatches are the batches a query searches (newest first).
func (s *Service) checkBatches(ctx context.Context, which string) ([]int64, error) {
	if s.reports == nil {
		return nil, ErrReportsOff
	}
	bs, err := s.reports.ListBatches(ctx)
	if err != nil {
		return nil, err
	}
	var out []int64
	switch which {
	case "", "current", "open":
		for _, b := range bs {
			if b.ClosedAt == nil {
				out = append(out, b.ID)
			}
		}
	case "all":
		for _, b := range bs {
			out = append(out, b.ID)
		}
	default:
		id, err := strconv.ParseInt(which, 10, 64)
		if err != nil || id <= 0 {
			return nil, badRequest("batch is a number, current or all")
		}
		for _, b := range bs {
			if b.ID == id {
				out = append(out, id)
			}
		}
		if out == nil {
			return nil, ErrReportNotFound
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i] > out[j] })
	return out, nil
}

// checksOf builds the checks of one batch (newest first) with the batch's
// payments and journal by order.
func (s *Service) checksOf(ctx context.Context, batchID int64, staffNames map[string]string) ([]Check, terminal.ZInput, error) {
	repo := s.zrepo()
	if repo == nil {
		return nil, terminal.ZInput{}, ErrReportsOff
	}
	in, err := repo.ZInputs(ctx, batchID)
	if err != nil {
		return nil, in, err
	}
	driverNames := map[string]string{} // a pooled delivery's driver
	if s.drv.floor != nil {
		if ds, err := s.drv.floor.ListDrivers(ctx); err == nil {
			for _, d := range ds {
				driverNames[d.ID] = d.Name
			}
		}
	}
	pays := map[string][]terminal.Payment{}
	for _, p := range in.Payments {
		pays[p.OrderID] = append(pays[p.OrderID], p)
	}
	devName := map[string]string{"web": "Pi web page"}
	for _, d := range in.Drawers {
		devName[d.DeviceID] = d.DeviceName
	}
	placed := map[string]terminal.Entry{}
	for _, e := range in.Journal {
		switch e.Action {
		case terminal.ActionLogin:
			var d struct {
				Name string `json:"device_name"`
			}
			if json.Unmarshal(e.Detail, &d) == nil && d.Name != "" && devName[e.DeviceID] == "" {
				devName[e.DeviceID] = d.Name
			}
		case terminal.ActionOrderPlaced:
			placed[e.OrderID] = e
		}
	}
	out := make([]Check, 0, len(in.Orders))
	for _, so := range in.Orders {
		snap := so.Order
		c := Check{OrderID: snap.ID, Number: so.Number(), BatchID: batchID, Source: snap.Source, State: string(so.Status.State),
			Voided:   so.Status.State == order.StateCancelled && snap.Source == order.SourceCounter && so.Status.CancelReason == terminal.VoidReason,
			PlacedAt: snap.PlacedAt.UTC(), Customer: CheckCustomer{Name: snap.Customer.DisplayName, Phone: snap.Customer.Phone},
			TotalCents: snap.Money.Total, Paid: so.Status.CashDueCents == 0, CashDueCents: so.Status.CashDueCents,
			RefundOwedCents: so.Status.RefundOwedCents,
			Tenders:         []string{}, staffs: map[string]bool{}, devices: map[string]bool{}}
		if so.Status.State == order.StateCancelled {
			c.Paid, c.CashDueCents = false, 0 // nothing to collect; the paid filter skips it
		}
		if c.PlacedAt.IsZero() {
			c.PlacedAt = so.Status.ArrivalDate.UTC()
		}
		switch {
		case so.IsDelivery():
			c.Type = terminal.TypeDelivery
		case snap.Fulfillment.Type == "pickup":
			c.Type = terminal.TypePickup
		default:
			c.Type = terminal.TypeCounter
		}
		if a := snap.Fulfillment.Address; a != nil {
			c.Street = order.StreetOnly(a.Line1)
			if c.Type == terminal.TypeDelivery {
				c.Address = &CheckAddress{Line1: a.Line1, Line2: a.Line2, City: a.City, State: a.State, Zip: a.PostalCode, Notes: a.Notes}
			}
		}
		c.DriverID, c.DriverName = so.Status.DriverID, so.Status.DriverName
		if c.DriverID == "" && so.Status.PoolDriverID != "" {
			c.DriverID, c.DriverName = so.Status.PoolDriverID, driverNames[so.Status.PoolDriverID]
		}
		c.PaymentsCount = len(pays[snap.ID])
		c.Reopened = so.Status.OpenAgain() // batch B (2.10ak)
		if t := so.Status.ReopenedAt; t != nil {
			u := t.UTC()
			c.ReopenedAt = &u
		}
		if t := so.Status.ReadyAt; t != nil && so.Status.State == order.StateReady {
			u := t.UTC()
			c.ReadyAt = &u
		}
		if t := order.PromisedAt(so, 0); !t.IsZero() && (so.Status.EstimatedReadyAt != nil || snap.Fulfillment.PromisedAt != nil) {
			u := t.UTC()
			c.PromisedAt = &u
		}
		words := []string{c.Customer.Name, c.Customer.Phone, c.Street, snap.Notes}
		for _, it := range snap.Items {
			c.Items += int64(max(it.Quantity, 1))
			words = append(words, it.Name)
		}
		if c.Number != nil {
			words = append(words, strconv.FormatInt(*c.Number, 10))
		}
		c.search = strings.ToLower(strings.Join(words, "\x00"))
		if e, ok := placed[snap.ID]; ok {
			c.StaffID, c.DeviceID = e.StaffID, e.DeviceID
			c.staffs[e.StaffID], c.devices[e.DeviceID] = true, true
		}
		tenders := map[string]bool{}
		c.TipCents = snap.Money.Tip
		for _, p := range pays[snap.ID] {
			if p.Refund {
				c.RefundedCents += p.AmountCents + p.TipCents
				c.TipCents -= p.TipCents
			} else {
				tenders[p.Tender] = true
				c.TipCents += p.TipCents
			}
			c.staffs[p.StaffID], c.devices[p.DeviceID] = true, true
		}
		if snap.Source != order.SourceCounter && snap.Money.Total-snap.CashDue() > 0 {
			tenders[terminal.TenderOnline] = true
		}
		for _, t := range append(append([]string(nil), terminal.ZTenderOrder...), terminal.TenderOnline) {
			if tenders[t] {
				c.Tenders = append(c.Tenders, t)
				delete(tenders, t)
			}
		}
		c.StaffName, c.DeviceName = staffNames[c.StaffID], devName[c.DeviceID]
		c.OutOfArea = snap.OutOfArea && c.Type == terminal.TypeDelivery
		if snap.DeviceKind == terminal.DeviceKindCallcenter { // ?device_id=callcenter finds every seat's
			c.TakenBy = snap.TakenBy
			c.devices[device.CallcenterID] = true
			if c.DeviceName == "" {
				c.DeviceName = device.CallcenterName
			}
		}
		out = append(out, c)
	}
	sort.SliceStable(out, func(i, j int) bool { return out[i].PlacedAt.After(out[j].PlacedAt) })
	return out, in, nil
}

func (q ChecksQuery) matches(c Check) bool {
	state := c.State
	if c.Voided {
		state = "voided"
	}
	switch {
	case q.Types != nil && !q.Types[c.Type],
		q.Sources != nil && !q.Sources[strings.ToLower(c.Source)],
		q.States != nil && !q.States[state],
		q.Paid != nil && (*q.Paid != c.Paid || c.State == string(order.StateCancelled)),
		q.StaffID != "" && !c.staffs[q.StaffID],
		q.DeviceID != "" && !c.devices[q.DeviceID],
		q.MinCents != nil && c.TotalCents < *q.MinCents,
		q.MaxCents != nil && c.TotalCents > *q.MaxCents,
		q.From != nil && c.PlacedAt.Before(*q.From),
		q.To != nil && c.PlacedAt.After(*q.To):
		return false
	}
	if q.Tenders != nil {
		any := false
		for _, t := range c.Tenders {
			any = any || q.Tenders[t]
		}
		if !any {
			return false
		}
	}
	if q.Text != "" {
		digits := strings.Map(func(r rune) rune {
			if r >= '0' && r <= '9' {
				return r
			}
			return -1
		}, q.Text)
		if !strings.Contains(c.search, q.Text) && (digits == "" || digits != q.Text || !strings.Contains(c.search, digits)) {
			return false
		}
	}
	return true
}

func (s *Service) staffNames() map[string]string {
	names := map[string]string{}
	if cfg, _ := s.store.Config(); cfg != nil {
		for _, st := range cfg.Staff {
			names[st.ID] = st.Name()
		}
	}
	return names
}

// Checks is GET /api/terminal/checks.
func (s *Service) Checks(ctx context.Context, a Actor, q ChecksQuery) (*ChecksPage, error) {
	if _, _, err := s.reportStaff(a); err != nil {
		return nil, err
	}
	ids, err := s.checkBatches(ctx, q.Batch)
	if err != nil {
		return nil, err
	}
	names := s.staffNames()
	page := &ChecksPage{Checks: []Check{}, Limit: q.Limit, Offset: q.Offset, BatchIDs: ids}
	if page.BatchIDs == nil {
		page.BatchIDs = []int64{}
	}
	var all []Check
	for _, id := range ids {
		cs, _, err := s.checksOf(ctx, id, names)
		if err != nil {
			return nil, err
		}
		for _, c := range cs {
			if q.matches(c) {
				all = append(all, c)
			}
		}
	}
	sort.SliceStable(all, func(i, j int) bool { return all[i].PlacedAt.After(all[j].PlacedAt) })
	page.Total = len(all)
	page.Summary = summarize(all)
	if q.Offset < len(all) {
		page.Checks = all[q.Offset:min(len(all), q.Offset+q.Limit)]
	}
	return page, nil
}

// CheckByID is GET /api/terminal/checks/{id}.
func (s *Service) CheckByID(ctx context.Context, a Actor, id string) (*CheckDetail, error) {
	if _, _, err := s.reportStaff(a); err != nil {
		return nil, err
	}
	ids, err := s.checkBatches(ctx, "all")
	if err != nil {
		return nil, err
	}
	names := s.staffNames()
	for _, b := range ids {
		cs, in, err := s.checksOf(ctx, b, names)
		if err != nil {
			return nil, err
		}
		for i, c := range cs {
			if c.OrderID != id {
				continue
			}
			d := &CheckDetail{Check: cs[i], Payments: []terminal.Payment{}, Adjustments: []order.Discount{}, Journal: []terminal.Entry{}}
			for _, so := range in.Orders {
				if so.Order.ID == id {
					d.Order = withOnlineCart(so) // an online order's cart (§2.10aa)
				}
			}
			if pays, err := s.repo.OrderPayments(ctx, id); err == nil && pays != nil {
				d.Payments = pays
			}
			d.Adjustments = append(d.Adjustments, d.Order.Order.Money.Discounts...)
			for _, e := range in.Journal {
				if e.OrderID == id {
					d.Journal = append(d.Journal, e)
				}
			}
			sort.SliceStable(d.Journal, func(i, j int) bool { return d.Journal[i].At.Before(d.Journal[j].At) })
			return d, nil
		}
	}
	return nil, ErrReportNotFound
}
