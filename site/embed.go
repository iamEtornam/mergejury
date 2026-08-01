// Package site embeds the landing page and its assets so the site server
// ships as one self-contained binary with no filesystem dependencies.
package site

import "embed"

// Files lists assets explicitly rather than using a wildcard: a new asset
// should be a deliberate addition, and `_headers` would be skipped by a
// pattern anyway (go:embed ignores names starting with underscore).
//
//go:embed index.html install robots.txt sitemap.xml _headers
//go:embed favicon.svg og.png apple-touch-icon.png gambarino.woff2
var Files embed.FS
