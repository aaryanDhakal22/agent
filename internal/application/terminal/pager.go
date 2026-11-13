package terminal

import (
	"context"
	"fmt"
	"time"

	"quiccpos/agent/internal/domain/order"
	"quiccpos/agent/internal/domain/terminal"
	"quiccpos/agent/internal/infra/notify"
)

// Pager sends the owner a Pushover (notify.Notifier.Send).
type Pager interface {
	Send(ctx context.Context, message, sound string) error
}

// SetPager wires the owner's pages (an unbalanced Z, PIN lockouts; nil =
// log only).
func (s *Service) SetPager(p Pager) {
	s.pager = p
	s.pageCool = notify.NewCooldown(notify.AdminCooldown)
}

// page sends the owner a Pushover in the background (never blocks a
// terminal request).
//
// 2.5.2: the same text at most once per 15 min (a terminal retrying the
// same refused order would page on every try).
func (s *Service) page(ctx context.Context, msg string) {
	if s.pager == nil || !s.pageCool.Allow(msg, time.Now()) {
		return
	}
	p := s.pager
	go func() {
		ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 30*time.Second)
		defer cancel()
		if err := p.Send(ctx, msg, "siren"); err != nil {
			s.logger.Warn().Ctx(ctx).Err(err).Msg("terminal page: Pushover failed (non-fatal)")
		}
	}()
}

func centsText(c int64) string {
	sign := ""
	if c < 0 {
		sign, c = "-", -c
	}
	return fmt.Sprintf("%s$%d.%02d", sign, c/100, c%100)
}

// FlaggedZRepo counts unbalanced Z reports (repositories.OrderRepository).
type FlaggedZRepo interface {
	FlaggedZReports(ctx context.Context) (int, error)
}

// Problems are the Pi page's "Needs attention" codes from the terminal
// side: z_unbalanced (a kept Z that doesn't balance, M-9) and pin_lockout
// (a PIN pad paused in the last hour, X-2), pin_locked (the daily ceiling
// of wrong PINs: every PIN check refused until a manager unlocks).
func (s *Service) Problems(ctx context.Context) []string {
	var out []string
	if fz, ok := s.repo.(FlaggedZRepo); ok {
		if n, err := fz.FlaggedZReports(ctx); err == nil && n > 0 {
			out = append(out, "z_unbalanced")
		}
	}
	if s.staff.lockedRecently(s.now()) {
		out = append(out, "pin_lockout")
	}
	if s.PinGuard().Locked { // the daily ceiling: locked until a manager unlocks (H3)
		out = append(out, "pin_locked")
	}
	return out
}

// RecordPi journals a Pi action outside a terminal request (the pairing
// codes, X-1): device_id = the minter.
func (s *Service) RecordPi(ctx context.Context, deviceID, action string, d map[string]any) {
	s.journal(ctx, terminal.Entry{DeviceID: deviceID, Action: action, Detail: detail(d)})
}

// checkMoney is M-13's identity check before an order's money is stored: a
// mismatch is refused (500) and paged.
func (s *Service) checkMoney(ctx context.Context, m order.Money, items []order.LineItem) error {
	err := terminal.CheckMoney(m, items)
	if err != nil {
		s.logger.Error().Ctx(ctx).Err(err).Int64("total_cents", m.Total).Msg("Counter order money doesn't add up: refused")
		s.page(ctx, "A counter order was refused: its money didn't add up ("+err.Error()+"). Tell the developer.")
	}
	return err
}
