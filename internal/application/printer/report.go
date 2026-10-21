package printer

import "time"

// PrinterReport is the agent→main printer-roster payload (duplex rpc
// "printer.report"). It's the live reachability view main caches + shows the
// manager next to their configured printers.
type PrinterReport struct {
	ReportedAt   time.Time        `json:"reported_at"`
	AgentVersion string           `json:"agent_version"`
	Printers     []StatusSnapshot `json:"printers"`
}

// BuildReport snapshots the current roster + reachability from the registry.
func BuildReport(reg *Registry, version string) PrinterReport {
	return PrinterReport{
		ReportedAt:   time.Now().UTC(),
		AgentVersion: version,
		Printers:     reg.All(),
	}
}
