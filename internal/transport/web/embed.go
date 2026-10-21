// Package web embeds the agent's static web UI into the binary so the agent
// serves it directly (no nginx). It's a single self-contained index.html.
package web

import "embed"

//go:embed index.html
var FS embed.FS
