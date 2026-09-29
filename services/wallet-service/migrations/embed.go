// Package migrations embeds wallet-service's versioned SQL migrations
// (golang-migrate format) so the binary can apply them on startup.
package migrations

import "embed"

//go:embed *.sql
var FS embed.FS
