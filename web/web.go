// Package web embeds the static files served at /app.
package web

import "embed"

// WebFS holds the embedded web/ directory content.
// main.go uses http.FS(web.WebFS) to serve static assets.
//
//go:embed index.html
var WebFS embed.FS
