package terminal

// A mod's price set at the counter (the owner, 2026-11-14, #11; contract
// §2.10an, money-rules mod_prices): any cashier or manager, up or down, to
// $0 too, no PIN, never a comp; a driver lowering one needs a manager.
const (
	// ActionModPrice journals a mod price change {line, pick, option_id,
	// name, from_cents, to_cents} and names a driver's manager need.
	ActionModPrice = "mod_price"
	// ActionModPriceChange is the journal entry's action.
	ActionModPriceChange = "mod_price_change"
)

// ModPriceNeed is what a manager must cover for staff member st's mod price
// changes (from → to, cents; nil = nothing): a driver lowering any (the cents
// lowered summed); everyone else, and any raise, is free.
func ModPriceNeed(c *Config, st Staff, changes [][2]int64) *ManagerRequired {
	if st.Role != RoleDriver || c.LimitsOf(st).Unlimited {
		return nil
	}
	var need *ManagerRequired
	for _, ch := range changes {
		if ch[1] >= ch[0] {
			continue
		}
		if need == nil {
			need = &ManagerRequired{Action: ActionModPrice}
		}
		need.Requested += ch[0] - ch[1]
	}
	return need
}
