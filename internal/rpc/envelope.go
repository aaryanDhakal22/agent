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
	// The call center's remote terminal (contract §2.10ab).
	MsgTerminalsUpdateNow = "terminals.update_now" // main → agent {requested_by} (2.5.3): get the newest terminal release cached and tell the terminals to install it at their next quiet moment; reply {ok, code?, release, terminals[]}
	MsgTerminalRelay      = "terminal.relay"       // main → agent, one terminal call {device, staff_id, method, path, query, headers, body} (reply {status, headers, body}); runs off the SSE reader
	MsgTerminalEventsWant = "terminal.events.want" // main → agent, forward the terminals' events for {seconds} (<= 300); no reply
	MsgTerminalEvents     = "terminal.events"      // agent → main, {events: [{event, data, at}]} while wanted
)

// Envelope is the wire shape for every duplex message, both directions.
type Envelope struct {
	Type    string          `json:"type"`
	ID      string          `json:"id"`
	Payload json.RawMessage `json:"payload,omitempty"`
}
