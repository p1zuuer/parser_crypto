// Package web embeds the Mini App static assets served at /app.
package web

import "embed"

//go:embed index.html
var WebFS embed.FS
