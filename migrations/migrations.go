// Package migrations embeds the ordered SQL schema migrations.
//
// Files are named NNNN_description.sql and applied in lexical order, each in
// its own transaction. Never edit an applied migration; add a new one.
package migrations

import "embed"

// FS holds every *.sql migration.
//
//go:embed *.sql
var FS embed.FS
