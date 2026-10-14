package talonflame

import "embed"

// Files contains Talonflame's independent, forward-only schema migrations.
// It must not be passed to Fletchling's legacy schema_migrations runner.
//
//go:embed *.sql
var Files embed.FS
