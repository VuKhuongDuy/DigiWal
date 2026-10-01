// Package migrations embeds withdrawal-service's versioned SQL migrations (golang-migrate).
package migrations

import "embed"

//go:embed *.sql
var FS embed.FS
