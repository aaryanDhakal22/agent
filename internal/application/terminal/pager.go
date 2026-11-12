package terminal

import (
	"context"
	"fmt"
	"time"
)

// Pager sends the owner a Pushover (notify.Notifier.Send).
type Pager interface {
	Send(ctx context.Context, message, sound string) error
}

// SetPager wires the owner's pages (an unbalanced Z, PIN lockouts; nil =
// log only).
func (s *Service) SetPager(p Pager) { s.pager = p }

// page sends the owner a Pushover in the background (never blocks a
// terminal request).
func (s *Service) page(ctx context.Context, msg string) {
	if s.pager == nil {
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
// (a PIN pad paused in the last hour, X-2).
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
	return out
}
