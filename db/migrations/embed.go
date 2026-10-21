// Package migrations embeds the plain-SQL goose migrations so that the
// migrate binary is self-contained and the schema ships with the code that
// depends on it.
package migrations

import "embed"

// FS holds every *.sql migration in this directory.
//
//go:embed *.sql
var FS embed.FS
