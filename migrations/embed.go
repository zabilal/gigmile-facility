// Package migrations embeds the SQL migrations into the binary so that applying
// them needs no external CLI on the reviewer's machine.
package migrations

import "embed"

//go:embed *.sql
var FS embed.FS
