// Package web embeds the built console assets so the whole thing ships as
// one binary. Run `npm run build` in web/ to refresh dist/.
package web

import "embed"

//go:embed all:dist
var Dist embed.FS
