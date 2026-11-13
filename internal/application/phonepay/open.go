package phonepay

import (
	"context"

	pp "quiccpos/agent/internal/domain/phonepay"
)

// OpenCount is how many phone charges are still open (sent to a phone or in
// Square's app): the store update round waits for none (contract §2.10ad).
// An error counts as open: better a later round than a lost charge.
func (s *Service) OpenCount(ctx context.Context) int {
	open, err := s.d.Repo.OpenFor(ctx, "")
	if err != nil {
		return 1
	}
	n := 0
	for _, c := range open {
		if pp.Open(c.Status) {
			n++
		}
	}
	return n
}
