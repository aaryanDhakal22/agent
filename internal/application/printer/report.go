package printer

import "time"

// PrinterReport is the agent→main printer-roster payload (duplex rpc
// "printer.report"). It's the live reachability view main caches + shows the
// manager next to their configured printers.
type PrinterReport struct {
	ReportedAt   time.Time        `json:"reported_at"`
	AgentVersion string           `json:"agent_version"`
	Printers     []StatusSnapshot `json:"printers"`
	// Found lists printers the Pi saw on its LAN (TCP 9100) that are not in
	// the roster — main offers them to the manager to add.
	Found []FoundAddr `json:"found"`
}

// FoundAddr is an unknown printer seen on the LAN.
type FoundAddr struct {
	IP  string `json:"ip"`
	MAC string `json:"mac"`
}

// BuildReport snapshots the current roster + reachability from the registry,
// plus the unknown printers found on the LAN (nil = none).
func BuildReport(reg *Registry, version string, found []FoundAddr) PrinterReport {
	if found == nil {
		found = []FoundAddr{}
	}
	return PrinterReport{
		ReportedAt:   time.Now().UTC(),
		AgentVersion: version,
		Printers:     reg.All(),
		Found:        found,
	}
}
