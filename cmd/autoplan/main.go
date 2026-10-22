// Command autoplan runs the AUTO-assign optimizer on one request: a
// dispatch.Input JSON on stdin, the dispatch.Output JSON on stdout. It is the
// exact code the agent runs, for the test harness and the dispatch simulator.
//
//	autoplan [-pretty] < input.json > plan.json
package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"os"

	"quiccpos/agent/internal/application/dispatch"
)

func main() {
	pretty := flag.Bool("pretty", false, "indent the output JSON")
	flag.Usage = func() {
		fmt.Fprintln(flag.CommandLine.Output(), "usage: autoplan [-pretty] < input.json > plan.json")
		flag.PrintDefaults()
	}
	flag.Parse()
	if err := run(os.Stdin, os.Stdout, *pretty); err != nil {
		fmt.Fprintln(os.Stderr, "autoplan:", err)
		os.Exit(1)
	}
}

func run(r io.Reader, w io.Writer, pretty bool) error {
	var in dispatch.Input
	dec := json.NewDecoder(r)
	if err := dec.Decode(&in); err != nil {
		return fmt.Errorf("decode input: %w", err)
	}
	enc := json.NewEncoder(w)
	if pretty {
		enc.SetIndent("", "  ")
	}
	return enc.Encode(dispatch.Plan(in))
}
