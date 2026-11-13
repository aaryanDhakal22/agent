package terminal

import "quiccpos/agent/internal/domain/order"

// VoidableSource says whether a terminal may void (or waste) an order of
// this kind (agent 2.5.3, the owner): a counter order (the store's own, the
// call center's), or an online vendor order that is CASH — nothing paid
// online: never edited and its cash due is the whole total, or edited at a
// terminal from an order that arrived with nothing paid. An online order
// paid online, all or part (a prepaid order with an item added for the
// door too), is ErrPaidOnline; any other source ErrNotCounterOrder.
func VoidableSource(snap order.Snapshot) error {
	switch snap.Source {
	case order.SourceCounter:
		return nil
	case order.SourceOnline:
		if ob := snap.OnlineBase(); ob != nil {
			if ob.PaidCents > 0 {
				return ErrPaidOnline
			}
			return nil
		}
		if snap.CashDue() < snap.Money.Total {
			return ErrPaidOnline
		}
		return nil
	}
	return ErrNotCounterOrder
}
