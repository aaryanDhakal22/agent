// Package rpc is the agent's half of the standardized duplex agent↔main channel.
// The agent receives main→agent COMMANDS as `event: command` SSE frames and
// sends agent→main REPLIES + REPORTS as authenticated POSTs to
// /store/v1/agent/rpc. The Envelope must stay byte-identical to
// main/backend/internal/rpc.Envelope (pinned in contract/agent.md).
package rpc

import "encoding/json"

// Message type constants (mirror main's).
const (
	MsgPing                 = "ping"
	MsgPrinterReport        = "printer.report"         // agent → main, unsolicited push
	MsgPrinterReportRequest = "printer.report.request" // main → agent, pull
	MsgDiagReport           = "diag.report"            // agent → main, a tablet's diagnostics report (Send: main replies {id, stored, duplicate})
	MsgOrderGeo             = "order.geo"              // main → agent, where a delivery address is + drive times (reply {ok} | {ok:false, code})
)

// Envelope is the wire shape for every duplex message, both directions.
type Envelope struct {
	Type    string          `json:"type"`
	ID      string          `json:"id"`
	Payload json.RawMessage `json:"payload,omitempty"`
}
