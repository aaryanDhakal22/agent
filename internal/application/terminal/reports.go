package terminal

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	orderApp "quiccpos/agent/internal/application/order"
	"quiccpos/agent/internal/domain/order"
	"quiccpos/agent/internal/domain/terminal"
)

// The terminal's Manager tab (contract/agent.md §2.10m): the till report
// (one drawer session or a whole batch, SoftTouch's TILL REPORT), the
// driver reports (list + print on the Pi's receipt printer, like Quibase)
// and the day's close (Close batch), which needs a manager.

// ReportFloor is what the Manager tab needs from the floor (orderApp.Service).
type ReportFloor interface {
	ListBatches(ctx context.Context) ([]order.Batch, error)
	DriverReports(ctx context.Context, batchID int64) ([]order.DriverReport, error)
	PrintDriverReports(ctx context.Context, batchID int64) ([]orderApp.ReportPrint, error)
	PrintBatchDriverReport(ctx context.Context, batchID int64, driverID string) (*orderApp.ReportPrint, error)
	CloseBatch(ctx context.Context, force bool) (*order.Batch, []order.StoredOrder, error)
}

// Report errors.
var (
	// ErrDriverNotAllowed: a driver on a Manager tab route (403
	// driver_not_allowed; drivers have their own report, §2.10b).
	ErrDriverNotAllowed = errors.New("drivers have their own report")
	// ErrReportNotFound: no such drawer / batch (or no longer kept).
	ErrReportNotFound = errors.New("no such drawer or batch on the Pi")
	// ErrReportsOff: the Manager tab's floor isn't wired.
	ErrReportsOff = errors.New("reports aren't available")
	// ErrNotTippable: a tip goes on a card payment only (409 not_tippable).
	ErrNotTippable = terminal.ErrNotTippable
	errNoTipChange = errors.New("the tip is already that")
)

// SetReports wires the Manager tab's floor (cmd/main.go).
func (s *Service) SetReports(f ReportFloor) { s.reports = f }

// SetStoreName wires the store's name for the till report's header.
func (s *Service) SetStoreName(fn func() string) { s.storeName = fn }

// reportStaff: a staff member logged in on the terminal who isn't a driver.
func (s *Service) reportStaff(a Actor) (terminal.Staff, *terminal.Config, error) {
	st, cfg, err := s.staffOf(a)
	if err != nil {
		return st, cfg, err
	}
	if st.Role == terminal.RoleDriver {
		return st, cfg, ErrDriverNotAllowed
	}
	return st, cfg, nil
}

// TillQuery picks the till report: one drawer session, else a batch (0 =
// the open one).
type TillQuery struct {
	DrawerID   string
	TerminalID string // one terminal = one drawer, for the batch (the owner, 2026-11-12)
	BatchID    int64
}

// openBatchID is the open batch's id.
func (s *Service) openBatchID(ctx context.Context) (int64, error) {
	if s.reports == nil {
		return 0, ErrReportsOff
	}
	bs, err := s.reports.ListBatches(ctx)
	if err != nil {
		return 0, err
	}
	for _, b := range bs {
		if b.ClosedAt == nil {
			return b.ID, nil
		}
	}
	return 0, ErrReportNotFound
}

// Till builds the till report (contract §2.10m).
func (s *Service) Till(ctx context.Context, a Actor, q TillQuery) (*terminal.Till, error) {
	_, cfg, err := s.reportStaff(a)
	if err != nil {
		return nil, err
	}
	repo := s.zrepo()
	if repo == nil {
		return nil, ErrReportsOff
	}
	var drawer *terminal.Drawer
	batchID := q.BatchID
	if q.DrawerID != "" {
		dr := s.drawers()
		if dr == nil {
			return nil, ErrReportsOff
		}
		d, err := dr.DrawerByID(ctx, q.DrawerID)
		if err != nil || d == nil {
			return nil, ErrReportNotFound
		}
		drawer, batchID = d, d.BatchID
	} else if batchID == 0 {
		if batchID, err = s.openBatchID(ctx); err != nil {
			return nil, err
		}
	}
	in, err := repo.ZInputs(ctx, batchID)
	if err != nil {
		if strings.Contains(err.Error(), "no rows") {
			return nil, ErrReportNotFound
		}
		return nil, err
	}
	in.Loc = s.orders.StoreLocation()
	in.Now = s.now()
	if drawer == nil && s.zDrivers != nil {
		if in.Drivers, err = s.zDrivers(ctx, batchID); err != nil {
			return nil, fmt.Errorf("driver reports: %w", err)
		}
	}
	names := map[string]string{}
	if cfg != nil {
		for _, st := range cfg.Staff {
			names[st.ID] = st.Name()
		}
	}
	var cats *terminal.CategoryIndex
	if m, _ := s.store.Menu(); m != nil {
		cats = terminal.NewCategoryIndex(m.Pricing.Menu.Categories)
	}
	store := ""
	if s.storeName != nil {
		store = s.storeName()
	}
	var dev *terminal.TillDevice
	if q.TerminalID != "" && drawer == nil {
		if dev = terminalOf(in, q.TerminalID); dev == nil {
			return nil, ErrReportNotFound
		}
	}
	t := terminal.BuildTill(terminal.TillInput{Z: in, Store: store, Drawer: drawer, Terminal: dev, Categories: cats, StaffNames: names})
	return &t, nil
}

// terminalOf is a terminal that worked in the batch (a drawer session, a
// payment or a journal row of it), with its name; nil = none.
func terminalOf(in terminal.ZInput, deviceID string) *terminal.TillDevice {
	dev := &terminal.TillDevice{DeviceID: deviceID}
	seen := false
	for _, d := range in.Drawers {
		if d.DeviceID == deviceID {
			seen, dev.DeviceName = true, d.DeviceName
		}
	}
	for _, p := range in.Payments {
		seen = seen || p.DeviceID == deviceID
	}
	for _, e := range in.Journal {
		if e.DeviceID != deviceID {
			continue
		}
		seen = true
		if dev.DeviceName == "" && e.Action == terminal.ActionLogin {
			var d struct {
				Name string `json:"device_name"`
			}
			if json.Unmarshal(e.Detail, &d) == nil {
				dev.DeviceName = d.Name
			}
		}
	}
	if !seen {
		return nil
	}
	return dev
}

// Batches are the open batch and the kept closed ones, newest first.
func (s *Service) ReportBatches(ctx context.Context, a Actor) ([]order.Batch, error) {
	if _, _, err := s.reportStaff(a); err != nil {
		return nil, err
	}
	if s.reports == nil {
		return nil, ErrReportsOff
	}
	bs, err := s.reports.ListBatches(ctx)
	if bs == nil {
		bs = []order.Batch{}
	}
	return bs, err
}

// resolveBatch is a batch id the Pi still keeps (0 = the open one).
func (s *Service) resolveBatch(ctx context.Context, id int64) (int64, error) {
	if s.reports == nil {
		return 0, ErrReportsOff
	}
	bs, err := s.reports.ListBatches(ctx)
	if err != nil {
		return 0, err
	}
	for _, b := range bs {
		if (id == 0 && b.ClosedAt == nil) || (id != 0 && b.ID == id) {
			return b.ID, nil
		}
	}
	return 0, ErrReportNotFound
}

// DriverReportsView is GET /api/terminal/reports/drivers.
type DriverReportsView struct {
	BatchID int64                `json:"batch_id"`
	Reports []order.DriverReport `json:"reports"`
}

// DriverReports are a batch's driver reports (0 = the open batch, so far).
func (s *Service) DriverReports(ctx context.Context, a Actor, batchID int64) (*DriverReportsView, error) {
	if _, _, err := s.reportStaff(a); err != nil {
		return nil, err
	}
	id, err := s.resolveBatch(ctx, batchID)
	if err != nil {
		return nil, err
	}
	reps, err := s.reports.DriverReports(ctx, id)
	if err != nil {
		return nil, err
	}
	if reps == nil {
		reps = []order.DriverReport{}
	}
	return &DriverReportsView{BatchID: id, Reports: reps}, nil
}

// ReportPrint is one slip's outcome (contract §2.10m).
type ReportPrint struct {
	DriverID     string `json:"driver_id"`
	DriverName   string `json:"driver_name"`
	PrintedOn    string `json:"printed_on"`
	ReroutedFrom string `json:"rerouted_from,omitempty"`
	Error        string `json:"error,omitempty"`
}

// DriverReportsPrinted is POST /api/terminal/reports/drivers/print's answer.
type DriverReportsPrinted struct {
	BatchID int64         `json:"batch_id"`
	Printed []ReportPrint `json:"printed"`
	Failed  []ReportPrint `json:"failed"`
}

// ErrNothingPrinted: no slip printed (502 nothing_printed {failed}).
type ErrNothingPrinted struct{ Result *DriverReportsPrinted }

func (e *ErrNothingPrinted) Error() string { return "no driver report printed" }

// PrintDriverReports prints a batch's driver reports (one driver, or every
// driver + the Unassigned slip like Close batch) on the receipt printer.
func (s *Service) PrintDriverReports(ctx context.Context, a Actor, batchID int64, driverID string) (*DriverReportsPrinted, error) {
	st, _, err := s.reportStaff(a)
	if err != nil {
		return nil, err
	}
	id, err := s.resolveBatch(ctx, batchID)
	if err != nil {
		return nil, err
	}
	var prints []orderApp.ReportPrint
	ids := map[string]string{}
	if driverID != "" {
		p, err := s.reports.PrintBatchDriverReport(ctx, id, driverID)
		if errors.Is(err, order.ErrNotFound) {
			return nil, ErrReportNotFound
		}
		if err != nil {
			return nil, err
		}
		prints = []orderApp.ReportPrint{*p}
		ids[p.DriverName] = driverID
	} else {
		if reps, err := s.reports.DriverReports(ctx, id); err == nil {
			for _, r := range reps {
				ids[r.DriverName] = r.DriverID
			}
		}
		if prints, err = s.reports.PrintDriverReports(ctx, id); err != nil {
			return nil, err
		}
	}
	out := &DriverReportsPrinted{BatchID: id, Printed: []ReportPrint{}, Failed: []ReportPrint{}}
	for _, p := range prints {
		rp := ReportPrint{DriverID: ids[p.DriverName], DriverName: p.DriverName, PrintedOn: p.PrintedOn, ReroutedFrom: p.ReroutedFrom, Error: p.Err}
		if p.Err != "" || p.PrintedOn == "" {
			if rp.Error == "" {
				rp.Error = "printed nowhere"
			}
			out.Failed = append(out.Failed, rp)
		} else {
			out.Printed = append(out.Printed, rp)
		}
	}
	d := map[string]any{"kind": "drivers", "batch_id": id, "printed": len(out.Printed), "failed": len(out.Failed)}
	if driverID != "" {
		d["driver_id"] = driverID
	}
	s.journal(ctx, terminal.Entry{StaffID: st.ID, DeviceID: a.DeviceID, Action: terminal.ActionReportPrint, Detail: detail(d)})
	if len(prints) > 0 && len(out.Printed) == 0 {
		return out, &ErrNothingPrinted{Result: out}
	}
	return out, nil
}

// DayCloseResult is POST /api/terminal/close's answer (the shape of POST
// /api/batch/close minus `opened`, which the transport adds).
type DayCloseResult struct {
	Closed       *order.Batch        `json:"closed"`
	AutoFinished []order.StoredOrder `json:"auto_finished"`
}

// CloseBatch is the day's close from a terminal (the owner, 2026-11-12: a
// manager's): a manager logged in, or anyone with a manager's PIN. The Pi's
// Close batch then runs as from the tablet (driver reports, Z report).
// Journaled batch.close {batch_id, forced, approved_by?}.
func (s *Service) CloseBatch(ctx context.Context, a Actor, force bool, pin string, printsUnpaid bool) (*DayCloseResult, error) {
	st, cfg, err := s.staffOf(a)
	if err != nil {
		return nil, err
	}
	if s.reports == nil {
		return nil, ErrReportsOff
	}
	var approver *terminal.Staff
	if !cfg.LimitsOf(st).Unlimited {
		need := &terminal.ManagerRequired{Action: terminal.NeedCloseBatch}
		if approver, err = s.manager(ctx, a, cfg, pin, need); err != nil {
			return nil, err
		}
		s.journal(ctx, overrideEntry(a, st, approver, need, ""))
	}
	// Who closed: a forced close writes unpaid counter checks off in their
	// name (journal void.waste, order.void staff_id / approved_by).
	by := order.CloseActor{StaffID: st.ID, StaffName: st.Name(), DeviceID: a.DeviceID, DeviceName: a.DeviceName,
		Trigger: "terminal", PrintsHere: printsUnpaid}
	if approver != nil {
		by.ApprovedBy = approver.ID
	}
	closed, finished, err := s.reports.CloseBatch(order.WithCloseActor(order.WithCloseTrigger(ctx, "terminal"), by), force)
	if err != nil {
		return nil, err
	}
	if finished == nil {
		finished = []order.StoredOrder{}
	}
	d := map[string]any{"forced": force}
	if closed != nil {
		d["batch_id"] = closed.ID
		if u := closed.UnpaidAtClose; u != nil {
			d["unpaid_written_off"], d["unpaid_written_off_cents"] = u.Count, u.TotalCents
		}
	}
	if approver != nil {
		d["approved_by"] = approver.ID
	}
	s.journal(ctx, terminal.Entry{StaffID: st.ID, DeviceID: a.DeviceID, Action: terminal.ActionBatchClose, Detail: detail(d)})
	return &DayCloseResult{Closed: closed, AutoFinished: finished}, nil
}
