// Package web embeds the built SPA (web/dist). `npm run build` replaces the
// committed placeholder index.html.
package web

import "embed"

//go:embed all:dist
var Dist embed.FS
