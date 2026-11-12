package repositories

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"time"

	"quiccpos/agent/internal/domain/order"
	"quiccpos/agent/internal/domain/terminal"
	"quiccpos/agent/internal/infra/database/models"

	"github.com/google/uuid"
)

// The store's customers on the Pi (TERMINAL-PLAN 1d, contract §2.10j): main's
// change feed applied page by page, phone lookups, and the terminals' own
// changes (saved with their customer.upsert floor event + journal row in one
// transaction).

// CustomerSyncState reads the feed's state (zero when never pulled).
func (r *OrderRepository) CustomerSyncState(ctx context.Context) (st terminal.CustomerSync, err error) {
	ctx, _, end := dbSpan(ctx, "customers.sync_state")
	defer end(&err)
	row, err := models.New(r.db).GetCustomerSync(ctx)
	if errors.Is(err, sql.ErrNoRows) {
		return terminal.CustomerSync{}, nil
	}
	if err != nil {
		return terminal.CustomerSync{}, err
	}
	st = terminal.CustomerSync{LocationID: row.LocationID, Cursor: row.Cursor}
	if row.SyncedAt != "" {
		st.SyncedAt, _ = parseTS(row.SyncedAt)
	}
	return st, nil
}

// ResetCustomers starts the copy over for a store: everything goes (another
// store's Pi), or, keepLocal, all but what a terminal changed and main
// hasn't sent back yet (the feed's reset).
func (r *OrderRepository) ResetCustomers(ctx context.Context, locationID string, keepLocal bool) (err error) {
	ctx, _, end := dbSpan(ctx, "customers.reset")
	defer end(&err)
	return r.withTx(ctx, func(q *models.Queries) error {
		if keepLocal {
			if err := q.DeleteSyncedCustomers(ctx); err != nil {
				return err
			}
			if err := q.DeleteOrphanCustomerAddresses(ctx); err != nil {
				return err
			}
		} else {
			if err := q.DeleteAllCustomers(ctx); err != nil {
				return err
			}
			if err := q.DeleteAllCustomerAddresses(ctx); err != nil {
				return err
			}
		}
		return q.PutCustomerSync(ctx, models.PutCustomerSyncParams{LocationID: locationID, Cursor: "", SyncedAt: ""})
	})
}

// ApplyCustomerPage applies one page of main's feed and saves its cursor, in
// one transaction: each customer replaces the Pi's copy (by id; another row
// with its phone is dropped: the Pi's own copy main merged), unless a
// terminal changed it after main's copy was written; deleted ids go.
// Address points the Pi learned are kept.
func (r *OrderRepository) ApplyCustomerPage(ctx context.Context, locationID string, page terminal.FeedPage, now time.Time) (applied int, err error) {
	ctx, _, end := dbSpan(ctx, "customers.apply_page")
	defer end(&err)
	err = r.withTx(ctx, func(q *models.Queries) error {
		for _, fc := range page.Customers {
			phone := terminal.NormPhone(fc.Phone)
			if fc.ID == "" || phone == "" {
				continue
			}
			cur, err := q.GetCustomer(ctx, fc.ID)
			switch {
			case err == nil:
				if cur.LocalAt != "" {
					if la, perr := parseTS(cur.LocalAt); perr == nil && la.After(fc.UpdatedAt) {
						continue // a terminal's newer change, on its way to main
					}
				}
			case !errors.Is(err, sql.ErrNoRows):
				return err
			}
			if other, err := q.GetCustomerByPhone(ctx, phone); err == nil && other.ID != fc.ID {
				if err := dropCustomer(ctx, q, other.ID); err != nil {
					return err
				}
			} else if err != nil && !errors.Is(err, sql.ErrNoRows) {
				return err
			}
			c := terminal.Customer{ID: fc.ID, Phone: phone, FirstName: fc.FirstName, LastName: fc.LastName, Name: fc.Name,
				Notes: fc.Notes, Source: fc.Source, Addresses: fc.Addresses}
			if err := writeCustomer(ctx, q, c, fmtTS(fc.UpdatedAt), ""); err != nil {
				return err
			}
			applied++
		}
		for _, id := range page.Deleted {
			if err := dropCustomer(ctx, q, id); err != nil {
				return err
			}
		}
		return q.PutCustomerSync(ctx, models.PutCustomerSyncParams{LocationID: locationID, Cursor: page.Cursor, SyncedAt: fmtTS(now)})
	})
	return applied, err
}

func dropCustomer(ctx context.Context, q *models.Queries, id string) error {
	if err := q.DeleteCustomerAddresses(ctx, id); err != nil {
		return err
	}
	return q.DeleteCustomer(ctx, id)
}

// writeCustomer upserts a customer and replaces its addresses, keeping the
// points the Pi learned for the addresses it already had.
func writeCustomer(ctx context.Context, q *models.Queries, c terminal.Customer, updatedAt, localAt string) error {
	old, err := q.ListCustomerAddresses(ctx, c.ID)
	if err != nil {
		return err
	}
	points := map[string]models.CustomerAddress{}
	for _, a := range old {
		points[a.ID] = a
	}
	if err := q.UpsertCustomer(ctx, models.UpsertCustomerParams{ID: c.ID, Phone: c.Phone, PhoneRev: terminal.Reverse(c.Phone),
		FirstName: c.FirstName, LastName: c.LastName, Name: c.Name, Notes: c.Notes, Source: c.Source, UpdatedAt: updatedAt,
		LocalAt: localAt}); err != nil {
		return fmt.Errorf("upsert customer: %w", err)
	}
	if err := q.DeleteCustomerAddresses(ctx, c.ID); err != nil {
		return err
	}
	for i, a := range c.Addresses {
		if a.ID == "" {
			continue
		}
		lat, lng := nullF(a.Lat), nullF(a.Lng)
		if p, ok := points[a.ID]; ok && !lat.Valid && p.Lat.Valid && terminal.SameAddress(p.Line1, p.Zip, a.Line1, a.Zip) {
			lat, lng = p.Lat, p.Lng
		}
		if err := q.InsertCustomerAddress(ctx, models.InsertCustomerAddressParams{ID: a.ID, CustomerID: c.ID, Position: int64(i),
			Label: a.Label, Line1: a.Line1, Line2: a.Line2, City: a.City, State: a.State, Zip: a.Zip, Notes: a.Notes,
			IsDefault: a.Default, CreatedAt: a.CreatedAt, Lat: lat, Lng: lng}); err != nil {
			return fmt.Errorf("insert customer address: %w", err)
		}
	}
	return nil
}

func nullF(f *float64) sql.NullFloat64 {
	if f == nil {
		return sql.NullFloat64{}
	}
	return sql.NullFloat64{Float64: *f, Valid: true}
}

func customerOf(ctx context.Context, q *models.Queries, row models.Customer) (terminal.Customer, error) {
	c := terminal.Customer{ID: row.ID, Phone: row.Phone, FirstName: row.FirstName, LastName: row.LastName, Name: row.Name,
		Notes: row.Notes, Source: row.Source, UpdatedAt: row.UpdatedAt, LastOrderAt: row.LastOrderAt, Pending: row.LocalAt != "",
		Addresses: []terminal.CustomerAddress{}}
	rows, err := q.ListCustomerAddresses(ctx, row.ID)
	if err != nil {
		return c, err
	}
	for _, a := range rows {
		ca := terminal.CustomerAddress{ID: a.ID, Label: a.Label, Line1: a.Line1, Line2: a.Line2, City: a.City, State: a.State,
			Zip: a.Zip, Notes: a.Notes, Default: a.IsDefault, CreatedAt: a.CreatedAt}
		if a.Lat.Valid && a.Lng.Valid {
			lat, lng := a.Lat.Float64, a.Lng.Float64
			ca.Lat, ca.Lng = &lat, &lng
		}
		c.Addresses = append(c.Addresses, ca)
	}
	return c, nil
}

// Customer is one customer with its addresses (terminal.ErrCustomerNotFound).
func (r *OrderRepository) Customer(ctx context.Context, id string) (c terminal.Customer, err error) {
	ctx, _, end := dbSpan(ctx, "customers.get")
	defer end(&err)
	q := models.New(r.db)
	row, err := q.GetCustomer(ctx, id)
	if errors.Is(err, sql.ErrNoRows) {
		return c, terminal.ErrCustomerNotFound
	}
	if err != nil {
		return c, err
	}
	return customerOf(ctx, q, row)
}

// CustomerByPhone is the customer with these 10 digits (ErrCustomerNotFound).
func (r *OrderRepository) CustomerByPhone(ctx context.Context, phone string) (c terminal.Customer, err error) {
	ctx, _, end := dbSpan(ctx, "customers.by_phone")
	defer end(&err)
	q := models.New(r.db)
	row, err := q.GetCustomerByPhone(ctx, phone)
	if errors.Is(err, sql.ErrNoRows) {
		return c, terminal.ErrCustomerNotFound
	}
	if err != nil {
		return c, err
	}
	return customerOf(ctx, q, row)
}

// FindCustomers looks digits up (indexed): 10 digits = that phone; fewer =
// numbers starting with them, then numbers ending with them (the last 4),
// newest order first, at most limit.
func (r *OrderRepository) FindCustomers(ctx context.Context, digits string, limit int) (out []terminal.Customer, err error) {
	ctx, _, end := dbSpan(ctx, "customers.find")
	defer end(&err)
	q := models.New(r.db)
	var rows []models.Customer
	if len(digits) == 10 {
		row, err := q.GetCustomerByPhone(ctx, digits)
		if err != nil && !errors.Is(err, sql.ErrNoRows) {
			return nil, err
		}
		if err == nil {
			rows = append(rows, row)
		}
	} else {
		// ':' sorts right after '9': [d, d:) is every string starting with d.
		pre, err := q.FindCustomersByPhonePrefix(ctx, models.FindCustomersByPhonePrefixParams{Lo: digits, Hi: digits + ":", MaxRows: int64(limit)})
		if err != nil {
			return nil, err
		}
		rev := terminal.Reverse(digits)
		suf, err := q.FindCustomersByPhoneSuffix(ctx, models.FindCustomersByPhoneSuffixParams{Lo: rev, Hi: rev + ":", MaxRows: int64(limit)})
		if err != nil {
			return nil, err
		}
		seen := map[string]bool{}
		for _, x := range append(pre, suf...) {
			if !seen[x.ID] {
				seen[x.ID] = true
				rows = append(rows, x)
			}
		}
		sort.SliceStable(rows, func(i, j int) bool {
			if rows[i].LastOrderAt != rows[j].LastOrderAt {
				return rows[i].LastOrderAt > rows[j].LastOrderAt
			}
			return rows[i].UpdatedAt > rows[j].UpdatedAt
		})
		if len(rows) > limit {
			rows = rows[:limit]
		}
	}
	out = make([]terminal.Customer, 0, len(rows))
	for _, row := range rows {
		c, err := customerOf(ctx, q, row)
		if err != nil {
			return nil, err
		}
		out = append(out, c)
	}
	return out, nil
}

// CountCustomers is how many customers the Pi holds.
func (r *OrderRepository) CountCustomers(ctx context.Context) (n int64, err error) {
	ctx, _, end := dbSpan(ctx, "customers.count")
	defer end(&err)
	return models.New(r.db).CountCustomers(ctx)
}

// SaveTerminalCustomer saves a customer a terminal added or changed (the whole
// customer, addresses included), marks it pending, and writes its
// customer.upsert floor event (wire) and the journal entries, in one
// transaction. The phone must not belong to another customer.
func (r *OrderRepository) SaveTerminalCustomer(ctx context.Context, c terminal.Customer, wire map[string]any, journal []terminal.Entry,
	now time.Time) (err error) {
	ctx, _, end := dbSpan(ctx, "customers.save_terminal")
	defer end(&err)
	return r.withTx(ctx, func(q *models.Queries) error {
		if other, err := q.GetCustomerByPhone(ctx, c.Phone); err == nil && other.ID != c.ID {
			return fmt.Errorf("phone %w", terminal.ErrPhoneTaken)
		} else if err != nil && !errors.Is(err, sql.ErrNoRows) {
			return err
		}
		updated := ""
		if cur, err := q.GetCustomer(ctx, c.ID); err == nil {
			updated = cur.UpdatedAt
		}
		if err := writeCustomer(ctx, q, c, updated, fmtTS(now)); err != nil {
			return err
		}
		id := uuid.NewString()
		ev := map[string]any{"event_id": id, "type": terminal.EventCustomerUpsert, "at": now.UTC().Truncate(time.Millisecond)}
		for k, v := range wire {
			ev[k] = v
		}
		if err := insertSubjectEvent(ctx, q, id, "", "customer:"+c.ID, terminal.EventCustomerUpsert, now, ev); err != nil {
			return err
		}
		return insertJournal(ctx, q, now, journal)
	})
}

// TouchCustomerOrder notes an order for the customer with this id or phone.
func (r *OrderRepository) TouchCustomerOrder(ctx context.Context, id, phone string, at time.Time) (err error) {
	ctx, _, end := dbSpan(ctx, "customers.touch_order")
	defer end(&err)
	return models.New(r.db).TouchCustomerOrder(ctx, models.TouchCustomerOrderParams{At: fmtTS(at), ID: id, Phone: phone})
}

// SetCustomerAddressPoint keeps where an address was located.
func (r *OrderRepository) SetCustomerAddressPoint(ctx context.Context, id string, lat, lng float64) (err error) {
	ctx, _, end := dbSpan(ctx, "customers.set_point")
	defer end(&err)
	return models.New(r.db).SetCustomerAddressPoint(ctx, models.SetCustomerAddressPointParams{
		Lat: sql.NullFloat64{Float64: lat, Valid: true}, Lng: sql.NullFloat64{Float64: lng, Valid: true}, ID: id})
}

// LocatedDeliveries are the Pi's located delivery addresses, newest first.
func (r *OrderRepository) LocatedDeliveries(ctx context.Context) (out []terminal.LocatedAddress, err error) {
	ctx, _, end := dbSpan(ctx, "customers.located_deliveries")
	defer end(&err)
	rows, err := models.New(r.db).ListLocatedDeliveries(ctx)
	if err != nil {
		return nil, err
	}
	for _, row := range rows {
		var s struct {
			Fulfillment struct {
				Address *struct {
					Line1      string `json:"line1"`
					PostalCode string `json:"postal_code"`
				} `json:"address"`
			} `json:"fulfillment"`
		}
		if json.Unmarshal([]byte(row.Snapshot), &s) != nil || s.Fulfillment.Address == nil || s.Fulfillment.Address.Line1 == "" {
			continue
		}
		out = append(out, terminal.LocatedAddress{Line1: s.Fulfillment.Address.Line1, Zip: s.Fulfillment.Address.PostalCode,
			Lat: row.Lat.Float64, Lng: row.Lng.Float64})
	}
	return out, nil
}

// OrdersMentioning are the Pi's orders (not POS checks) whose snapshot holds
// text, newest first, at most limit (the caller checks each one).
func (r *OrderRepository) OrdersMentioning(ctx context.Context, text string, limit int) (out []order.StoredOrder, err error) {
	ctx, _, end := dbSpan(ctx, "customers.orders_mentioning")
	defer end(&err)
	q := models.New(r.db)
	rows, err := q.ListOrdersMentioning(ctx, models.ListOrdersMentioningParams{Text: text, MaxRows: int64(limit)})
	if err != nil {
		return nil, err
	}
	return mapRowsGeo(ctx, q, rows)
}
