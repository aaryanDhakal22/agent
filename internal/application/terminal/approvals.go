package terminal

import (
	"context"
	"sync"
	"time"

	"quiccpos/agent/internal/domain/terminal"

	"github.com/google/uuid"
)

// Single-use manager approvals (HARDENING-PLAN X-5, contract 2.10s §9):
// POST /api/terminal/manager-check answers an approval_id bound to {the
// terminal, the action, the amount, 2 minutes}; a request carries
// `approvals: [id]` instead of the raw manager_pin, and each one approves one
// manager need, once. manager_pin still works (older terminals).

// ApprovalTTL is how long an approval may be used.
const ApprovalTTL = 2 * time.Minute

type approval struct {
	id      string
	device  string
	action  string
	amount  int64 // 0 = any amount
	manager terminal.Staff
	expires time.Time
}

type approvalBook struct {
	mu   sync.Mutex
	byID map[string]approval
}

func (b *approvalBook) add(ap approval) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.byID == nil {
		b.byID = map[string]approval{}
	}
	for id, x := range b.byID { // drop expired ones
		if time.Now().After(x.expires) && x.expires.Before(ap.expires) {
			delete(b.byID, id)
		}
	}
	b.byID[ap.id] = ap
}

// take consumes the first of ids that approves need on device at now.
func (b *approvalBook) take(ids []string, device string, need *terminal.ManagerRequired, now time.Time) (*terminal.Staff, bool) {
	b.mu.Lock()
	defer b.mu.Unlock()
	for _, id := range ids {
		ap, ok := b.byID[id]
		if !ok || ap.device != device || now.After(ap.expires) || ap.action != need.Action {
			continue
		}
		if ap.amount > 0 && need.Requested > ap.amount {
			continue
		}
		delete(b.byID, id)
		m := ap.manager
		return &m, true
	}
	return nil, false
}

type approvalsKey struct{}

// WithApprovals puts a request's approval ids on ctx (manager() reads them).
func WithApprovals(ctx context.Context, ids []string) context.Context {
	if len(ids) == 0 {
		return ctx
	}
	return context.WithValue(ctx, approvalsKey{}, ids)
}

func approvalsOf(ctx context.Context) []string {
	ids, _ := ctx.Value(approvalsKey{}).([]string)
	return ids
}

// newApproval mints an approval for a manager's checked PIN.
func (s *Service) newApproval(a Actor, mgr terminal.Staff, action string, amount int64) string {
	id := uuid.NewString()
	s.approvals.add(approval{id: id, device: a.DeviceID, action: action, amount: max(amount, 0), manager: mgr, expires: s.now().Add(ApprovalTTL)})
	return id
}
