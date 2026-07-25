// Package migrations embeds tgfile's versioned SQLite schema migrations.
package migrations

import "embed"

// FS contains every migration and recognized legacy schema profile shipped
// with the current binary. Only SQL files at the filesystem root are applied
// as migrations.
//
//go:embed *.sql legacy/*.sql
var FS embed.FS
