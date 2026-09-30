// Package migrations embeds chain-watcher's versioned SQL migrations (golang-migrate).
package migrations

import "embed"

//go:embed *.sql
var FS embed.FS
