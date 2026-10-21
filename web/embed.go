package web

import "embed"

// FS contains every front-end asset. Everything is embedded so deployment is a
// single binary and no CDN or build step is required.
//
//go:embed *.html *.js *.css i18n/*.json
var FS embed.FS
