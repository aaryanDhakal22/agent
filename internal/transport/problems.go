package transport

import "context"

// AddProblems registers a source of extra GET /api/diag problem codes (the
// Pi page's Needs attention): the hardening round's z_unbalanced (a Z that
// doesn't balance, M-9), main_auth_failing (F-4), pin_lockout (X-2).
func (s *Server) AddProblems(fn func(ctx context.Context) []string) {
	s.problemHooks = append(s.problemHooks, fn)
}
