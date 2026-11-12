// Package phonepage holds the cashier phone's pages for "Send to phone"
// (contract/agent.md §2.10z): /pay (pair, wait, CHARGE in Square's app),
// /pay/back (Square's answer, through the HTTPS bounce page) and the fake
// Square POS for dev / staging. Plain HTML + a little JS, no framework, both
// themes; the Pi serves them on the LAN.
package phonepage

import "embed"

//go:embed pay.html back.html fake.html
var FS embed.FS
