package printer

import "time"

// NetAddress is what the Pi learned about a roster printer on the LAN: its
// hardware (MAC) address and the IP it last answered on. The MAC is what lets
// the agent find a printer again after the router hands it a new address.
type NetAddress struct {
	PrinterID string
	MAC       string
	IP        string
	SeenAt    time.Time
}
