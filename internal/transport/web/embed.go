// Package web embeds the agent's web UI into the binary so the agent serves it
// directly (no nginx, no build step): index.html (markup) plus static/ — our
// app.js + app.css, the vendored Alpine.js and the Inter / JetBrains Mono
// fonts (third-party notices in static/LICENSES.md).
package web

import (
	"embed"
	"io/fs"
)

//go:embed index.html static
var FS embed.FS

// Static is the static/ subtree, served at /static/.
var Static = mustSub(FS, "static")

func mustSub(fsys fs.FS, dir string) fs.FS {
	sub, err := fs.Sub(fsys, dir)
	if err != nil {
		panic(err) // a fixed, valid path: only a broken build gets here
	}
	return sub
}
