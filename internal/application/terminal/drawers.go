package terminal

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"time"
	"unicode/utf8"

	"quiccpos/agent/internal/domain/terminal"

	"github.com/google/uuid"
)

// Cash drawers (contract/agent.md §2.10k): one session per terminal (its
// drawer) and cashier shift - count in, pay-ins / pay-outs, count out with
// expected vs counted. Synced to main as drawer.opened / drawer.movement /
// drawer.closed.

// SSE event on /api/events for every drawer change.
const EventDrawerChanged = "terminal.drawer.changed"

// ErrDrawersUnavailable: the storage has no drawers (never in the agent).
var ErrDrawersUnavailable = errors.New("cash drawers aren't available")

// DrawerRepo is the drawers' storage (repositories.OrderRepository).
type DrawerRepo interface {
	DrawerByDevice(ctx context.Context, deviceID string) (*terminal.Drawer, error)
	DrawerByStaff(ctx context.Context, staffID string) (*terminal.Drawer, error)
	DrawerByID(ctx context.Context, id string) (*terminal.Drawer, error)
	DrawersByBatch(ctx context.Context, batchID int64) ([]terminal.Drawer, error)
	OpenDrawer(ctx context.Context, d terminal.Drawer, journal []terminal.Entry) (terminal.Drawer, error)
	AddDrawerMovement(ctx context.Context, deviceID string, m terminal.Movement,
		check func(d terminal.Drawer) (terminal.Movement, []terminal.Entry, error)) (terminal.Drawer, terminal.Movement, bool, error)
	CloseDrawer(ctx context.Context, c terminal.DrawerClose, check func(d terminal.Drawer) error) (terminal.Drawer, error)
	HandoverDrawer(ctx context.Context, deviceID string, c terminal.DrawerClose, next terminal.Drawer,
		journal []terminal.Entry) (closed, opened terminal.Drawer, err error)
}

func (s *Service) drawers() DrawerRepo {
	if d, ok := s.repo.(DrawerRepo); ok {
		return d
	}
	return nil
}

// DrawerView is GET /api/terminal/drawer: this terminal's open drawer, and
// the logged-in staff member's open drawer when it is on another terminal.
type DrawerView struct {
	Drawer   *terminal.Drawer `json:"drawer"`
	Mine     *terminal.Drawer `json:"mine"`
	DeviceID string           `json:"device_id"`
	Staff    *terminal.Public `json:"staff"`
	// PayoutLimit: what the logged-in staff member may pay out alone (null =
	// no limit).
	PayoutLimitCents *int64 `json:"payout_limit_cents"`
}

// Drawer is GET /api/terminal/drawer (no login needed; Mine needs one).
func (s *Service) Drawer(ctx context.Context, a Actor) (*DrawerView, error) {
	repo := s.drawers()
	if repo == nil {
		return nil, ErrDrawersUnavailable
	}
	d, err := repo.DrawerByDevice(ctx, a.DeviceID)
	if err != nil {
		return nil, err
	}
	v := &DrawerView{Drawer: d, DeviceID: a.DeviceID}
	if st, cfg, err := s.staffOf(a); err == nil {
		p := st.Public()
		v.Staff = &p
		if lim, unlimited := cfg.PayoutLimit(st); !unlimited {
			v.PayoutLimitCents = &lim
		}
		if mine, err := repo.DrawerByStaff(ctx, st.ID); err == nil && mine != nil && (d == nil || mine.ID != d.ID) {
			v.Mine = mine
		}
	}
	return v, nil
}

// Drawers is GET /api/terminal/drawers: a batch's sessions (0 = the open one).
func (s *Service) Drawers(ctx context.Context, batchID int64) ([]terminal.Drawer, error) {
	repo := s.drawers()
	if repo == nil {
		return nil, ErrDrawersUnavailable
	}
	return repo.DrawersByBatch(ctx, batchID)
}

// DrawerOpenIn is POST /api/terminal/drawer/open (count in).
type DrawerOpenIn struct {
	StartCents    *int64                  `json:"start_cents"`
	Denominations []terminal.Denomination `json:"denominations"`
}

func (s *Service) drawerStaff(a Actor) (terminal.Staff, *terminal.Config, DrawerRepo, error) {
	repo := s.drawers()
	if repo == nil {
		return terminal.Staff{}, nil, nil, ErrDrawersUnavailable
	}
	st, cfg, err := s.staffOf(a)
	if err != nil {
		return st, cfg, nil, err
	}
	if !terminal.UsesDrawer(st) {
		return st, cfg, nil, terminal.ErrDrawerNotAllowed
	}
	return st, cfg, repo, nil
}

func countIn(counted *int64, denoms []terminal.Denomination) (*int64, error) {
	n, err := terminal.CountOf(counted, denoms)
	if err != nil {
		return nil, badRequest(err.Error())
	}
	return n, nil
}

// OpenDrawer counts the logged-in cashier's drawer in on this terminal.
func (s *Service) OpenDrawer(ctx context.Context, a Actor, in DrawerOpenIn) (*terminal.Drawer, error) {
	st, _, repo, err := s.drawerStaff(a)
	if err != nil {
		return nil, err
	}
	start, err := countIn(in.StartCents, in.Denominations)
	if err != nil {
		return nil, err
	}
	if start == nil {
		return nil, badRequest("start_cents (or denominations) is required")
	}
	now := s.now().UTC().Truncate(time.Millisecond)
	d := terminal.Drawer{ID: uuid.Must(uuid.NewV7()).String(), DeviceID: a.DeviceID, DeviceName: a.DeviceName, StaffID: st.ID,
		StaffName: st.Name(), OpenedAt: now, StartCents: *start, Denominations: in.Denominations}
	journal := []terminal.Entry{{StaffID: st.ID, DeviceID: a.DeviceID, Action: terminal.ActionDrawerOpen,
		Detail: detail(map[string]any{"drawer_id": d.ID, "start_cents": d.StartCents})}}
	out, err := repo.OpenDrawer(ctx, d, journal)
	if err != nil {
		return nil, err
	}
	s.orders.JournalWritten()
	s.publish(EventDrawerChanged, map[string]any{"drawer": out, "device_id": a.DeviceID})
	s.logger.Info().Ctx(ctx).Str("drawer_id", out.ID).Str("staff_id", st.ID).Str("staff_name", st.Name()).Str("device_id", a.DeviceID).
		Int64("amount_cents", out.StartCents).Msg("Drawer opened")
	return &out, nil
}

// DrawerMoveIn is POST /api/terminal/drawer/pay-in | pay-out.
type DrawerMoveIn struct {
	AmountCents int64  `json:"amount_cents"`
	Reason      string `json:"reason"`
	ClientKey   string `json:"client_key"`
	ManagerPIN  string `json:"manager_pin"`
}

// DrawerMoveResult is a pay-in / pay-out's answer.
type DrawerMoveResult struct {
	Drawer    terminal.Drawer   `json:"drawer"`
	Movement  terminal.Movement `json:"movement"`
	Duplicate bool              `json:"duplicate"`
}

// MoveDrawer puts cash in (pay-in) or takes it out (pay-out) of this
// terminal's open drawer. A cashier's pay-out over cashier_payout_cents needs
// a manager's PIN; a pay-out can't take more than the drawer holds.
func (s *Service) MoveDrawer(ctx context.Context, a Actor, kind string, in DrawerMoveIn) (*DrawerMoveResult, error) {
	st, cfg, repo, err := s.drawerStaff(a)
	if err != nil {
		return nil, err
	}
	if in.AmountCents < 1 || in.AmountCents > 1_000_000 {
		return nil, badRequest("amount_cents is 1-1000000")
	}
	reason := strings.TrimSpace(in.Reason)
	if reason == "" || utf8.RuneCountInString(reason) > 140 {
		return nil, badRequest("a reason is required (1-140 characters)")
	}
	if in.ClientKey != "" && !keyRe.MatchString(in.ClientKey) {
		return nil, badRequest("client_key is 8-64 letters, digits, _ . : -")
	}
	var approver *terminal.Staff
	var need *terminal.ManagerRequired
	if kind == terminal.MovePayOut {
		if lim, unlimited := cfg.PayoutLimit(st); !unlimited && in.AmountCents > lim {
			need = &terminal.ManagerRequired{Action: "pay_out", Limit: lim, Requested: in.AmountCents}
			if approver, err = s.manager(ctx, a, cfg, in.ManagerPIN, need); err != nil {
				return nil, err
			}
		}
	}
	now := s.now().UTC().Truncate(time.Millisecond)
	m := terminal.Movement{ID: uuid.Must(uuid.NewV7()).String(), Kind: kind, AmountCents: in.AmountCents, Reason: reason,
		StaffID: st.ID, ClientKey: in.ClientKey, At: now}
	if approver != nil {
		m.ApprovedBy = approver.ID
	}
	d, got, dup, err := repo.AddDrawerMovement(ctx, a.DeviceID, m, func(cur terminal.Drawer) (terminal.Movement, []terminal.Entry, error) {
		if kind == terminal.MovePayOut && in.AmountCents > cur.ExpectedCents {
			return m, nil, ErrNotEnoughCash{Expected: cur.ExpectedCents}
		}
		m.DrawerID = cur.ID
		action := terminal.ActionDrawerPayIn
		if kind == terminal.MovePayOut {
			action = terminal.ActionDrawerPayOut
		}
		dd := map[string]any{"drawer_id": cur.ID, "movement_id": m.ID, "amount_cents": m.AmountCents, "reason": reason}
		if approver != nil {
			dd["approved_by"] = approver.ID
		}
		entries := []terminal.Entry{{StaffID: st.ID, DeviceID: a.DeviceID, Action: action, Detail: detail(dd)}}
		if approver != nil {
			entries = append(entries, overrideEntry(a, st, approver, need, ""))
		}
		return m, entries, nil
	})
	if err != nil {
		return nil, err
	}
	if !dup {
		s.orders.JournalWritten()
		s.publish(EventDrawerChanged, map[string]any{"drawer": d, "device_id": a.DeviceID})
		s.logger.Info().Ctx(ctx).Str("drawer_id", d.ID).Str("staff_id", st.ID).Str("staff_name", st.Name()).Str("device_id", a.DeviceID).
			Str("kind", kind).Int64("amount_cents", in.AmountCents).Str("approver_id", got.ApprovedBy).Str("reason", in.Reason).Msg("Drawer cash moved")
	}
	return &DrawerMoveResult{Drawer: d, Movement: got, Duplicate: dup}, nil
}

// ErrNotEnoughCash: a pay-out over what the drawer should hold (409
// not_enough_cash {expected_cents}).
type ErrNotEnoughCash struct{ Expected int64 }

func (e ErrNotEnoughCash) Error() string { return terminal.ErrNotEnoughCash.Error() }

// DrawerCloseIn is POST /api/terminal/drawer/close (count out) and
// /handover.
type DrawerCloseIn struct {
	DrawerID      string                  `json:"drawer_id"`
	CountedCents  *int64                  `json:"counted_cents"`
	Denominations []terminal.Denomination `json:"denominations"`
	ManagerPIN    string                  `json:"manager_pin"`
}

// CloseDrawer counts a drawer out: this terminal's open one, or drawer_id
// (another terminal's: a cashier switching terminals closes the one they
// left). Its own cashier or a manager closes it; anyone else needs a
// manager's PIN. Answers expected vs counted (over / short).
func (s *Service) CloseDrawer(ctx context.Context, a Actor, in DrawerCloseIn) (*terminal.Drawer, error) {
	st, cfg, repo, err := s.drawerStaff(a)
	if err != nil {
		return nil, err
	}
	counted, err := countIn(in.CountedCents, in.Denominations)
	if err != nil {
		return nil, err
	}
	if counted == nil {
		return nil, badRequest("counted_cents (or denominations) is required")
	}
	id := strings.TrimSpace(in.DrawerID)
	if id == "" {
		d, err := repo.DrawerByDevice(ctx, a.DeviceID)
		if err != nil {
			return nil, err
		}
		if d == nil {
			return nil, terminal.ErrNoDrawer
		}
		id = d.ID
	}
	cur, err := repo.DrawerByID(ctx, id)
	if err != nil {
		return nil, err
	}
	var approver *terminal.Staff
	var need *terminal.ManagerRequired
	if cur.StaffID != st.ID && st.Role != terminal.RoleManager && !cfg.LimitsOf(st).Unlimited {
		need = &terminal.ManagerRequired{Action: "drawer_close", Requested: 0}
		if approver, err = s.manager(ctx, a, cfg, in.ManagerPIN, need); err != nil {
			return nil, err
		}
	}
	now := s.now().UTC().Truncate(time.Millisecond)
	dd := map[string]any{"drawer_id": id, "reason": terminal.DrawerClosedCounted, "counted_cents": *counted}
	if approver != nil {
		dd["approved_by"] = approver.ID
	}
	journal := []terminal.Entry{{StaffID: st.ID, DeviceID: a.DeviceID, Action: terminal.ActionDrawerClose, Detail: detail(dd)}}
	if approver != nil {
		journal = append(journal, overrideEntry(a, st, approver, need, ""))
	}
	out, err := repo.CloseDrawer(ctx, terminal.DrawerClose{DrawerID: id, By: st.ID, Reason: terminal.DrawerClosedCounted,
		CountedCents: counted, Denominations: in.Denominations, At: now, Journal: journal}, nil)
	if err != nil {
		return nil, err
	}
	s.orders.JournalWritten()
	s.publish(EventDrawerChanged, map[string]any{"drawer": out, "device_id": out.DeviceID})
	l := s.logger.Info().Ctx(ctx).Str("drawer_id", out.ID).Str("staff_id", st.ID).Str("staff_name", st.Name()).Str("device_id", out.DeviceID).
		Int64("expected_cents", out.ExpectedCents)
	if out.CountedCents != nil {
		l = l.Int64("amount_cents", *out.CountedCents)
	}
	if out.OverShortCents != nil {
		l = l.Int64("over_short_cents", *out.OverShortCents)
	}
	l.Msg("Drawer closed")
	return &out, nil
}

// HandoverResult is POST /api/terminal/drawer/handover's answer.
type HandoverResult struct {
	Closed terminal.Drawer `json:"closed"`
	Drawer terminal.Drawer `json:"drawer"`
}

// HandoverDrawer: the logged-in cashier takes over this terminal's drawer
// from the cashier before them. The drawer is counted (in front of both):
// the old session closes with the count (reason handover, over / short
// against its expected) and a new one opens for the new cashier starting
// from the count.
func (s *Service) HandoverDrawer(ctx context.Context, a Actor, in DrawerCloseIn) (*HandoverResult, error) {
	st, _, repo, err := s.drawerStaff(a)
	if err != nil {
		return nil, err
	}
	counted, err := countIn(in.CountedCents, in.Denominations)
	if err != nil {
		return nil, err
	}
	if counted == nil {
		return nil, badRequest("counted_cents (or denominations) is required")
	}
	now := s.now().UTC().Truncate(time.Millisecond)
	next := terminal.Drawer{ID: uuid.Must(uuid.NewV7()).String(), DeviceID: a.DeviceID, DeviceName: a.DeviceName, StaffID: st.ID,
		StaffName: st.Name(), OpenedAt: now}
	closeJournal := []terminal.Entry{{StaffID: st.ID, DeviceID: a.DeviceID, Action: terminal.ActionDrawerClose}}
	openJournal := []terminal.Entry{{StaffID: st.ID, DeviceID: a.DeviceID, Action: terminal.ActionDrawerHandover,
		Detail: detail(map[string]any{"drawer_id": next.ID, "start_cents": *counted})}}
	closed, opened, err := repo.HandoverDrawer(ctx, a.DeviceID, terminal.DrawerClose{By: st.ID, Reason: terminal.DrawerClosedHandover,
		CountedCents: counted, Denominations: in.Denominations, At: now, Journal: closeJournal}, next, openJournal)
	if err != nil {
		return nil, err
	}
	s.orders.JournalWritten()
	s.publish(EventDrawerChanged, map[string]any{"drawer": opened, "closed": closed, "device_id": a.DeviceID})
	return &HandoverResult{Closed: closed, Drawer: opened}, nil
}

// noSaleDetail adds the terminal's open drawer to a no_sale journal row (the
// drawer's no-sale count reads the terminal + time anyway).
func (s *Service) noSaleDetail(ctx context.Context, a Actor, d json.RawMessage) json.RawMessage {
	repo := s.drawers()
	if repo == nil {
		return d
	}
	dr, err := repo.DrawerByDevice(ctx, a.DeviceID)
	if err != nil || dr == nil {
		return d
	}
	var obj map[string]any
	if json.Unmarshal(d, &obj) != nil {
		return d
	}
	if _, ok := obj["drawer_id"]; !ok {
		obj["drawer_id"] = dr.ID
	}
	return detail(obj)
}
