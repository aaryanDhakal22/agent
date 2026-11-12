package terminal

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"sort"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"quiccpos/agent/internal/domain/order"
	"quiccpos/agent/internal/domain/terminal"

	"github.com/google/uuid"
	"github.com/rs/zerolog"
)

// The store's customers on the Pi (TERMINAL-PLAN 1d "Customers", "Phone
// delivery flow"; contract §2.10j): a full copy of main's customers kept by
// its change feed, looked up by phone with no internet, and customers added
// or changed at a terminal (saved at once, sent to main as customer.upsert).

// Main's feed and its command.
const (
	PathCustomers       = "/store/v1/agent/customers"
	MsgCustomersChanged = "customers.changed"
	// EventCustomerChanged (SSE): a terminal added or changed a customer {customer, device_id}.
	EventCustomerChanged = "terminal.customer.changed"
	// EventCustomersSynced (SSE): the copy took main's changes {changed, deleted, count}.
	EventCustomersSynced = "terminal.customers.synced"
)

// Lookup limits.
const (
	MinLookupDigits  = 3
	MaxLookupResults = 20
	feedPageSize     = 1000
	geoIndexMaxAge   = time.Minute
)

// CustomerRepo is the customers' storage (repositories.OrderRepository).
type CustomerRepo interface {
	CustomerSyncState(ctx context.Context) (terminal.CustomerSync, error)
	ResetCustomers(ctx context.Context, locationID string, keepLocal bool) error
	ApplyCustomerPage(ctx context.Context, locationID string, page terminal.FeedPage, now time.Time) (int, error)
	Customer(ctx context.Context, id string) (terminal.Customer, error)
	CustomerByPhone(ctx context.Context, phone string) (terminal.Customer, error)
	FindCustomers(ctx context.Context, digits string, limit int) ([]terminal.Customer, error)
	CountCustomers(ctx context.Context) (int64, error)
	SaveTerminalCustomer(ctx context.Context, c terminal.Customer, wire map[string]any, journal []terminal.Entry, now time.Time) error
	TouchCustomerOrder(ctx context.Context, id, phone string, at time.Time) error
	SetCustomerAddressPoint(ctx context.Context, id string, lat, lng float64) error
	LocatedDeliveries(ctx context.Context) ([]terminal.LocatedAddress, error)
	OrdersMentioning(ctx context.Context, text string, limit int) ([]order.StoredOrder, error)
}

// QueryFetcher GETs a main route with a query string, with the store key.
type QueryFetcher func(ctx context.Context, path, rawQuery string) (int, []byte, error)

// StoreArea is the store's pin and delivery radius (nil pin / radius 0 = no
// area check).
type StoreArea func() (lat, lng *float64, radiusM int64)

// CustomerBook keeps the copy in step with main and answers the lookups.
type CustomerBook struct {
	repo     CustomerRepo
	fetch    QueryFetcher
	pub      Publisher
	logger   zerolog.Logger
	location func() string
	area     StoreArea
	kick     chan struct{}
	pull     sync.Mutex
	geoMu    sync.Mutex
	geoAt    time.Time
	geo      []terminal.LocatedAddress
	now      func() time.Time
}

// NewCustomerBook builds the book. location is the linked store's location
// id ("" = not linked: no pulls).
func NewCustomerBook(repo CustomerRepo, fetch QueryFetcher, pub Publisher, location func() string, area StoreArea,
	logger zerolog.Logger) *CustomerBook {
	return &CustomerBook{repo: repo, fetch: fetch, pub: pub, location: location, area: area,
		logger: logger.With().Str("module", "customers").Logger(), kick: make(chan struct{}, 1), now: time.Now}
}

// SetCustomers wires the customer book into the terminal API.
func (s *Service) SetCustomers(b *CustomerBook) { s.cust = b }

// Kick asks for a pull of main's changes, without waiting.
func (b *CustomerBook) Kick() {
	select {
	case b.kick <- struct{}{}:
	default:
	}
}

// HandleChanged is main's `customers.changed` command: pull (no reply).
func (b *CustomerBook) HandleChanged(_ context.Context, _ json.RawMessage) (any, error) {
	b.Kick()
	return nil, nil
}

// Run pulls on every kick and every `every` (a quiet store too: main's
// cursor keeps moving and a missed command is caught up).
func (b *CustomerBook) Run(ctx context.Context, every time.Duration, linked func() bool) {
	t := time.NewTicker(every)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-b.kick:
		case <-t.C:
		}
		if linked != nil && !linked() {
			continue
		}
		if err := b.Pull(ctx); err != nil {
			if errors.Is(err, errNoStore) {
				// Linked, but the store's handshake isn't in yet: soon again.
				time.AfterFunc(5*time.Second, b.Kick)
				continue
			}
			lvl := b.logger.Warn()
			if errors.Is(err, ErrMainUnsupported) {
				lvl = b.logger.Debug()
			}
			lvl.Err(err).Msg("customer feed pull failed")
		}
	}
}

// errNoStore: no store handshake yet (the location isn't known).
var errNoStore = errors.New("no store yet")

// Pull takes every change main has for the store since the saved cursor,
// page by page (each page saved with its cursor in one transaction). A new
// store starts over; main's reset drops the copy (keeping what terminals
// changed) and starts again.
func (b *CustomerBook) Pull(ctx context.Context) error {
	b.pull.Lock()
	defer b.pull.Unlock()
	loc := b.location()
	if loc == "" {
		return errNoStore
	}
	st, err := b.repo.CustomerSyncState(ctx)
	if err != nil {
		return err
	}
	if st.LocationID != loc {
		// Another store's copy goes whole; a first pull keeps what the
		// terminals already added (this store's).
		if st.LocationID != "" {
			b.logger.Info().Ctx(ctx).Msg("Linked to another store: the customer copy starts over")
		}
		if err := b.repo.ResetCustomers(ctx, loc, st.LocationID == ""); err != nil {
			return err
		}
		st = terminal.CustomerSync{LocationID: loc}
	}
	cursor, changed, deleted, resets := st.Cursor, 0, 0, 0
	for page := 0; page < 10000; page++ {
		q := url.Values{"limit": {fmt.Sprint(feedPageSize)}}
		if cursor != "" {
			q.Set("since", cursor)
		}
		status, body, err := b.fetch(ctx, PathCustomers, q.Encode())
		switch {
		case err != nil:
			return err
		case status == 404 || status == 405 || status == 501:
			return ErrMainUnsupported
		case status == 400 && cursor != "":
			// A cursor main doesn't take: start over.
			b.logger.Warn().Ctx(ctx).Msg("main refused the customer cursor: starting the copy over")
			cursor = ""
			continue
		case status != 200:
			return fmt.Errorf("GET %s: HTTP %d", PathCustomers, status)
		}
		var p terminal.FeedPage
		if err := json.Unmarshal(body, &p); err != nil {
			return fmt.Errorf("customer feed: %w", err)
		}
		if p.Reset {
			if resets++; resets > 1 {
				return errors.New("customer feed: reset twice in one pull")
			}
			b.logger.Info().Ctx(ctx).Msg("main reset the customer feed: starting the copy over")
			if err := b.repo.ResetCustomers(ctx, loc, true); err != nil {
				return err
			}
			cursor = ""
			continue
		}
		n, err := b.repo.ApplyCustomerPage(ctx, loc, p, b.now())
		if err != nil {
			return err
		}
		changed += n
		deleted += len(p.Deleted)
		cursor = p.Cursor
		if !p.HasMore {
			break
		}
	}
	if changed > 0 || deleted > 0 {
		count, _ := b.repo.CountCustomers(ctx)
		b.logger.Info().Ctx(ctx).Int("changed", changed).Int("deleted", deleted).Int64("count", count).Msg("Customers synced from main")
		if b.pub != nil {
			b.pub.Publish(EventCustomersSynced, map[string]any{"changed": changed, "deleted": deleted, "count": count})
		}
	}
	return nil
}

// NoteOrder remembers an order for the customer with this id or phone (the
// lookup's last_order_at). Best effort.
func (b *CustomerBook) NoteOrder(ctx context.Context, id, phone string, at time.Time) {
	phone = terminal.NormPhone(phone)
	if id == "" && phone == "" {
		return
	}
	if err := b.repo.TouchCustomerOrder(ctx, id, phone, at); err != nil {
		b.logger.Debug().Ctx(ctx).Err(err).Msg("customer last order not noted")
	}
}

// --- lookup ------------------------------------------------------------------------------

// AreaInfo is the store's delivery area as the lookup used it (nil: no area check).
type AreaInfo struct {
	RadiusMi float64 `json:"radius_mi"`
	MarginMi float64 `json:"margin_mi"`
}

// CustomerLookup is GET /api/terminal/customers' answer.
type CustomerLookup struct {
	Customers []terminal.Customer `json:"customers"`
	Area      *AreaInfo           `json:"area"`
	Count     int64               `json:"count"`     // customers the Pi holds
	SyncedAt  *time.Time          `json:"synced_at"` // the last good pull (null = never)
}

// marginMi is main's out_of_area_margin_mi (default 1).
func (s *Service) marginMi() float64 {
	cfg, _ := s.store.Config()
	if cfg == nil {
		return terminal.DefaultOutOfAreaMarginMi
	}
	var v struct {
		Margin *float64 `json:"out_of_area_margin_mi"`
	}
	if json.Unmarshal(cfg.Settings, &v) != nil || v.Margin == nil || *v.Margin < 0 {
		return terminal.DefaultOutOfAreaMarginMi
	}
	return *v.Margin
}

// ErrCustomersOff: the customer book isn't wired (503 customers_unavailable).
var ErrCustomersOff = errors.New("the customers aren't running on this Pi")

func (s *Service) customers() (*CustomerBook, error) {
	if s.cust == nil {
		return nil, ErrCustomersOff
	}
	return s.cust, nil
}

// LookupCustomers finds the customers whose phone is (10 digits), starts
// with, or ends with (the last 4) the typed digits — newest order first,
// with their addresses placed against the delivery area. Fewer than 3
// digits: an empty list. Works with no internet.
func (s *Service) LookupCustomers(ctx context.Context, phone string) (*CustomerLookup, error) {
	b, err := s.customers()
	if err != nil {
		return nil, err
	}
	d := terminal.Digits(phone)
	if len(d) == 11 && d[0] == '1' {
		d = d[1:]
	}
	if len(d) > 10 || utf8.RuneCountInString(phone) > 32 {
		return nil, badRequest("phone is at most 10 digits")
	}
	out := &CustomerLookup{Customers: []terminal.Customer{}}
	if n, err := b.repo.CountCustomers(ctx); err == nil {
		out.Count = n
	}
	if st, err := b.repo.CustomerSyncState(ctx); err == nil && !st.SyncedAt.IsZero() {
		t := st.SyncedAt
		out.SyncedAt = &t
	}
	if len(d) < MinLookupDigits {
		out.Area = s.areaInfo()
		return out, nil
	}
	list, err := b.repo.FindCustomers(ctx, d, MaxLookupResults)
	if err != nil {
		return nil, err
	}
	out.Customers = list
	out.Area = s.placeAll(ctx, out.Customers)
	return out, nil
}

func (s *Service) areaInfo() *AreaInfo {
	if s.cust == nil || s.cust.area == nil {
		return nil
	}
	lat, lng, r := s.cust.area()
	if lat == nil || lng == nil || r <= 0 {
		return nil
	}
	return &AreaInfo{RadiusMi: float64(int64(float64(r)/1609.344*10+0.5)) / 10, MarginMi: s.marginMi()}
}

// placeAll gives every address its area (when the store has a radius): its
// point, else one learned from a located delivery to the same place.
func (s *Service) placeAll(ctx context.Context, cs []terminal.Customer) *AreaInfo {
	info := s.areaInfo()
	if info == nil {
		return nil
	}
	lat, lng, radius := s.cust.area()
	for i := range cs {
		for j := range cs[i].Addresses {
			a := &cs[i].Addresses[j]
			if a.Lat == nil || a.Lng == nil {
				if p, ok := s.cust.located(ctx, a.Line1, a.Zip); ok {
					a.Lat, a.Lng = &p.Lat, &p.Lng
					if err := s.cust.repo.SetCustomerAddressPoint(ctx, a.ID, p.Lat, p.Lng); err != nil {
						s.logger.Debug().Ctx(ctx).Err(err).Msg("address point not kept")
					}
				}
			}
			if a.Lat == nil || a.Lng == nil {
				a.Area = &terminal.Area{State: terminal.AreaUnknown}
				continue
			}
			ar := terminal.AreaOf(terminal.DistanceM(*lat, *lng, *a.Lat, *a.Lng), radius, info.MarginMi)
			a.Area = &ar
		}
	}
	return info
}

// located is the point of a located delivery to the same place (the Pi's
// orders, refreshed at most once a minute).
func (b *CustomerBook) located(ctx context.Context, line1, zip string) (terminal.LocatedAddress, bool) {
	b.geoMu.Lock()
	if b.now().Sub(b.geoAt) > geoIndexMaxAge {
		if pts, err := b.repo.LocatedDeliveries(ctx); err == nil {
			b.geo, b.geoAt = pts, b.now()
		}
	}
	pts := b.geo
	b.geoMu.Unlock()
	for _, p := range pts {
		if terminal.SameAddress(p.Line1, p.Zip, line1, zip) {
			return p, true
		}
	}
	return terminal.LocatedAddress{}, false
}

// --- add / edit at the terminal ---------------------------------------------------------

// AddressIn is an address typed at the terminal (id = the one to change).
type AddressIn struct {
	ID    string `json:"id"`
	Label string `json:"label"`
	Line1 string `json:"line1"`
	Line2 string `json:"line2"`
	City  string `json:"city"`
	State string `json:"state"`
	Zip   string `json:"zip"`
	Notes string `json:"notes"`
}

func (in AddressIn) clean() (terminal.CustomerAddress, error) {
	a := terminal.CustomerAddress{ID: strings.TrimSpace(in.ID), Label: terminal.Clean(in.Label, 40), Line1: terminal.Clean(in.Line1, 120),
		Line2: terminal.Clean(in.Line2, 120), City: terminal.Clean(in.City, 80), State: strings.ToUpper(terminal.Clean(in.State, 2)),
		Zip: terminal.Clean(in.Zip, 10), Notes: strings.TrimSpace(in.Notes)}
	if utf8.RuneCountInString(a.Notes) > 300 {
		return a, badRequest("address notes are at most 300 characters")
	}
	if a.Line1 == "" || a.City == "" {
		return a, badRequest("an address needs line1 (the street) and city")
	}
	return a, nil
}

// CustomerIn is POST /api/terminal/customers.
type CustomerIn struct {
	Phone     string     `json:"phone"`
	FirstName string     `json:"first_name"`
	LastName  string     `json:"last_name"`
	Notes     string     `json:"notes"`
	Address   *AddressIn `json:"address"`
}

// CustomerPatch is PATCH /api/terminal/customers/{id} (absent = unchanged).
type CustomerPatch struct {
	Phone     *string    `json:"phone"`
	FirstName *string    `json:"first_name"`
	LastName  *string    `json:"last_name"`
	Notes     *string    `json:"notes"`
	Address   *AddressIn `json:"address"`
}

// CustomerResult is a write's answer: the customer (with areas), whether it
// was new, and the address the write added or changed.
type CustomerResult struct {
	Customer  terminal.Customer `json:"customer"`
	Created   bool              `json:"created"`
	AddressID string            `json:"address_id,omitempty"`
	Area      *AreaInfo         `json:"area"`
}

func cleanNotes(s string) (string, error) {
	s = strings.TrimSpace(s)
	if utf8.RuneCountInString(s) > 1000 {
		return "", badRequest("notes are at most 1000 characters")
	}
	return s, nil
}

// putAddress adds a (or, with its id, changes it): the same place as one the
// customer has gets that one's blanks filled. → the address id.
func putAddress(c *terminal.Customer, a terminal.CustomerAddress) (string, error) {
	if a.ID != "" {
		for i := range c.Addresses {
			if c.Addresses[i].ID == a.ID {
				a.Default, a.CreatedAt = c.Addresses[i].Default, c.Addresses[i].CreatedAt
				if !terminal.SameAddress(c.Addresses[i].Line1, c.Addresses[i].Zip, a.Line1, a.Zip) {
					a.Lat, a.Lng = nil, nil // moved: its point is learned again
				} else {
					a.Lat, a.Lng = c.Addresses[i].Lat, c.Addresses[i].Lng
				}
				c.Addresses[i] = a
				return a.ID, nil
			}
		}
		return "", badRequest("the customer has no address " + a.ID)
	}
	for i := range c.Addresses {
		h := &c.Addresses[i]
		if terminal.SameAddress(h.Line1, h.Zip, a.Line1, a.Zip) {
			for _, f := range []struct {
				dst *string
				src string
			}{{&h.Label, a.Label}, {&h.Line2, a.Line2}, {&h.City, a.City}, {&h.State, a.State}, {&h.Zip, a.Zip}, {&h.Notes, a.Notes}} {
				if *f.dst == "" {
					*f.dst = f.src
				}
			}
			return h.ID, nil
		}
	}
	if len(c.Addresses) >= terminal.MaxCustomerAddresses {
		return "", terminal.ErrTooManyAddresses
	}
	a.ID = uuid.NewString()
	a.Default = len(c.Addresses) == 0
	a.CreatedAt = time.Now().UTC().Format(time.RFC3339)
	c.Addresses = append(c.Addresses, a)
	return a.ID, nil
}

// AddCustomer adds a customer at the terminal (first name + phone required;
// an address needs line1 + city). A phone the store already has (one
// customer per phone) gets the address added to that customer and only its
// empty name / notes filled: 200 with created false.
func (s *Service) AddCustomer(ctx context.Context, a Actor, in CustomerIn) (*CustomerResult, error) {
	b, err := s.customers()
	if err != nil {
		return nil, err
	}
	st, _, err := s.staffOf(a)
	if err != nil {
		return nil, err
	}
	phone := terminal.NormPhone(in.Phone)
	if phone == "" {
		return nil, badRequest("phone must be a 10-digit US number")
	}
	first, last := terminal.Clean(in.FirstName, 60), terminal.Clean(in.LastName, 60)
	if first == "" {
		return nil, badRequest("first_name is required")
	}
	notes, err := cleanNotes(in.Notes)
	if err != nil {
		return nil, err
	}
	var addr *terminal.CustomerAddress
	if in.Address != nil {
		ca, err := in.Address.clean()
		if err != nil {
			return nil, err
		}
		ca.ID = ""
		addr = &ca
	}
	c, err := b.repo.CustomerByPhone(ctx, phone)
	created := errors.Is(err, terminal.ErrCustomerNotFound)
	if err != nil && !created {
		return nil, err
	}
	var set []string
	if created {
		c = terminal.Customer{ID: uuid.NewString(), Phone: phone, FirstName: first, LastName: last, Name: terminal.JoinName(first, last),
			Notes: notes, Addresses: []terminal.CustomerAddress{}}
		set = []string{"name", "notes"}
	} else {
		if c.FirstName == "" && c.LastName == "" && c.Name == "" {
			c.FirstName, c.LastName, c.Name = first, last, terminal.JoinName(first, last)
			set = append(set, "name")
		}
		if c.Notes == "" && notes != "" {
			c.Notes = notes
			set = append(set, "notes")
		}
	}
	return s.saveCustomer(ctx, a, st, c, addr, set, created)
}

// EditCustomer changes a customer's names / notes and adds or changes one
// address. The phone never changes (a new number is a new customer).
func (s *Service) EditCustomer(ctx context.Context, a Actor, id string, in CustomerPatch) (*CustomerResult, error) {
	b, err := s.customers()
	if err != nil {
		return nil, err
	}
	st, _, err := s.staffOf(a)
	if err != nil {
		return nil, err
	}
	c, err := b.repo.Customer(ctx, id)
	if err != nil {
		return nil, err
	}
	if in.Phone != nil && terminal.NormPhone(*in.Phone) != c.Phone {
		return nil, badRequest("a customer's phone can't change: add a new customer for the new number")
	}
	var set []string
	if in.FirstName != nil || in.LastName != nil {
		first, last := c.FirstName, c.LastName
		if in.FirstName != nil {
			first = terminal.Clean(*in.FirstName, 60)
		}
		if in.LastName != nil {
			last = terminal.Clean(*in.LastName, 60)
		}
		if first == "" {
			return nil, badRequest("first_name is required")
		}
		c.FirstName, c.LastName, c.Name = first, last, terminal.JoinName(first, last)
		set = append(set, "name")
	}
	if in.Notes != nil {
		notes, err := cleanNotes(*in.Notes)
		if err != nil {
			return nil, err
		}
		c.Notes = notes
		set = append(set, "notes")
	}
	var addr *terminal.CustomerAddress
	if in.Address != nil {
		ca, err := in.Address.clean()
		if err != nil {
			return nil, err
		}
		addr = &ca
	}
	if len(set) == 0 && addr == nil {
		return nil, badRequest("nothing to change")
	}
	return s.saveCustomer(ctx, a, st, c, addr, set, false)
}

// AddCustomerAddress adds an address to a customer (the same place: its
// blanks filled).
func (s *Service) AddCustomerAddress(ctx context.Context, a Actor, id string, in AddressIn) (*CustomerResult, error) {
	in.ID = ""
	return s.EditCustomer(ctx, a, id, CustomerPatch{Address: &in})
}

// saveCustomer saves the customer with the address put in, its
// customer.upsert (the fields in set + the touched address) and a journal
// row (no names or numbers in it), then tells the terminals.
func (s *Service) saveCustomer(ctx context.Context, a Actor, st terminal.Staff, c terminal.Customer, addr *terminal.CustomerAddress,
	set []string, created bool) (*CustomerResult, error) {
	touched := []map[string]any{}
	addressID := ""
	if addr != nil {
		id, err := putAddress(&c, *addr)
		if err != nil {
			return nil, err
		}
		addressID = id
		for _, x := range c.Addresses {
			if x.ID == id {
				touched = append(touched, map[string]any{"id": x.ID, "label": x.Label, "line1": x.Line1, "line2": x.Line2,
					"city": x.City, "state": x.State, "zip": x.Zip, "notes": x.Notes, "default": x.Default})
			}
		}
	}
	if set == nil {
		set = []string{}
	}
	wire := map[string]any{"customer": map[string]any{"id": c.ID, "phone": c.Phone, "first_name": c.FirstName, "last_name": c.LastName,
		"notes": c.Notes, "addresses": touched}, "set": set}
	action := terminal.ActionCustomerEdit
	if created {
		action = terminal.ActionCustomerAdd
	}
	d := map[string]any{"customer_id": c.ID, "fields": set}
	if addressID != "" {
		d["address_id"] = addressID
	}
	journal := []terminal.Entry{{StaffID: st.ID, DeviceID: a.DeviceID, Action: action, Detail: detail(d)}}
	now := s.now()
	if err := s.cust.repo.SaveTerminalCustomer(ctx, c, wire, journal, now); err != nil {
		if errors.Is(err, terminal.ErrPhoneTaken) {
			return nil, badRequest("that phone belongs to another customer")
		}
		return nil, err
	}
	s.orders.JournalWritten() // wakes the floor sync (the outbox has the event)
	saved, err := s.cust.repo.Customer(ctx, c.ID)
	if err != nil {
		return nil, err
	}
	res := &CustomerResult{Customer: saved, Created: created, AddressID: addressID}
	list := []terminal.Customer{saved}
	res.Area = s.placeAll(ctx, list)
	res.Customer = list[0]
	s.publish(EventCustomerChanged, map[string]any{"customer": res.Customer, "device_id": a.DeviceID})
	s.logger.Info().Ctx(ctx).Str("customer_id", c.ID).Bool("created", created).Str("staff_id", st.ID).Msg("Customer saved at a terminal")
	return res, nil
}

// --- the customer's orders -----------------------------------------------------------------

// CustomerOrders is GET /api/terminal/customers/{id}/orders' answer: the
// orders the Pi still holds for the customer (the open batch + the kept
// closed ones), newest first. main's history isn't asked (the Pi only).
type CustomerOrders struct {
	Orders []order.StoredOrder `json:"orders"`
	Source string              `json:"source"` // "pi"
}

// CustomerOrdersOf lists a customer's recent orders on the Pi: by its id on
// a terminal order, or its phone on any order (online, website, counter).
func (s *Service) CustomerOrdersOf(ctx context.Context, id string, limit int) (*CustomerOrders, error) {
	b, err := s.customers()
	if err != nil {
		return nil, err
	}
	if limit <= 0 || limit > 50 {
		limit = 10
	}
	c, err := b.repo.Customer(ctx, id)
	if err != nil {
		return nil, err
	}
	seen := map[string]bool{}
	var out []order.StoredOrder
	for _, text := range []string{c.ID, c.Phone[6:]} {
		cands, err := b.repo.OrdersMentioning(ctx, text, 200)
		if err != nil {
			return nil, err
		}
		for _, so := range cands {
			if seen[so.Order.ID] || !(terminal.NormPhone(so.Order.Customer.Phone) == c.Phone || customerIDOf(so.Order) == c.ID) {
				continue
			}
			seen[so.Order.ID] = true
			out = append(out, so)
		}
	}
	sort.SliceStable(out, func(i, j int) bool { return out[i].Order.PlacedAt.After(out[j].Order.PlacedAt) })
	if len(out) > limit {
		out = out[:limit]
	}
	if out == nil {
		out = []order.StoredOrder{}
	}
	return &CustomerOrders{Orders: out, Source: "pi"}, nil
}

// customerIDOf is a terminal order's customer id (source_metadata.terminal.customer_id).
func customerIDOf(o order.Snapshot) string {
	t, _ := o.SourceMeta["terminal"].(map[string]any)
	id, _ := t["customer_id"].(string)
	return id
}
