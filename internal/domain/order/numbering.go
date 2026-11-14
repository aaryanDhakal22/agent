package order

// Ticket numbers (contract/agent.md §2.10ae, the owner, final 2026-11-14):
//
//   - ONE sequence for every order: terminal, call-center, website and
//     online orders all take the next number from settings.counter_next,
//     starting at main's counter_start (default 100). No more kitchen
//     numbers from 1100.
//   - Every Close batch restarts it at the start (a second close the same
//     day too). The 2026-11-13 business-day rule is gone.
//   - A future order holds a random 5-digit placeholder (PlaceholderMin -
//     PlaceholderMax, unique among open future orders) from when it is
//     scheduled until it is placed (its ring time, MAKE NOW); then it takes
//     the next number of the sequence.

// DefaultNumberStart is where the store's one ticket-number sequence starts
// when main's terminal config doesn't say (main's counter_start default).
const DefaultNumberStart = 100

// MaxNumberStart bounds counter_start: the real numbers stay below the
// placeholders (main's MaxCounterStart).
const MaxNumberStart = 9_999

// A future order's placeholder number: 5 digits.
const (
	PlaceholderMin = 10_000
	PlaceholderMax = 99_999
)

// Numbering is what taking a ticket number needs: where the sequence
// starts (main's counter_start).
type Numbering struct {
	Start int64
}

// StartOrDefault is n's start, or DefaultNumberStart when unset / out of range.
func (n Numbering) StartOrDefault() int64 {
	if n.Start < 1 || n.Start > MaxNumberStart {
		return DefaultNumberStart
	}
	return n.Start
}

// PickPlaceholder draws a placeholder from rnd (a value in [0, n)) that
// taken doesn't hold. It tries a handful of random draws, then walks the
// range from a random point, so it always finds a free one while fewer than
// 90,000 future orders are open. ok false = every placeholder is taken.
func PickPlaceholder(rnd func(n int64) int64, taken func(int64) (bool, error)) (int64, bool, error) {
	span := int64(PlaceholderMax - PlaceholderMin + 1)
	for range 16 {
		c := PlaceholderMin + rnd(span)
		used, err := taken(c)
		if err != nil {
			return 0, false, err
		}
		if !used {
			return c, true, nil
		}
	}
	from := rnd(span)
	for i := range span {
		c := PlaceholderMin + (from+i)%span
		used, err := taken(c)
		if err != nil {
			return 0, false, err
		}
		if !used {
			return c, true, nil
		}
	}
	return 0, false, nil
}

// IsPlaceholder: a number in the placeholder range.
func IsPlaceholder(n int64) bool { return n >= PlaceholderMin && n <= PlaceholderMax }
