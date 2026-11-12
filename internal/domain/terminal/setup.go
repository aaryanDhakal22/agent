package terminal

// Pi journal actions of the setup wizard (contract §2.10y). The Pi writes
// them itself; a terminal can't post them.
const (
	// ActionPairingAdminCode: a terminal paired with the admin pairing code
	// {device_id, device_name} (never the code).
	ActionPairingAdminCode = "pairing.admin_code"
	// ActionTerminalPrint: raw bytes a terminal sent to one of the Pi's
	// printers {kind, printer, printer_id, bytes, order_id?, ok, error?}.
	ActionTerminalPrint = "terminal_print"
	// ActionSetupCardRefund: the setup wizard's test charge refunded through
	// the Pi {square_payment_id, amount_cents, refund_id?, status?, ok, code?}.
	// (The terminal journals its own setup.card_test.)
	ActionSetupCardRefund = "setup.card_refund"
	// ActionSquareKeyed: a typed card's payment created through main
	// (contract 2.10r §2) {amount_cents, tip_cents, reference_id,
	// idempotency_key, staff_id, ok, square_payment_id?, code?, square_code?}.
	// Never the card or the nonce.
	ActionSquareKeyed = "square.keyed_payment"
	// ActionSquareSession: a terminal asked for the reader's Square sign-in
	// (§2.10r §1) {ok, code?} (never the token).
	ActionSquareSession = "square.session"
)
