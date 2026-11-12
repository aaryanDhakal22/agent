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
)
