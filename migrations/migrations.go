// Package migrations embeds the schema.
//
// It is a Go package rather than a bare directory because go:embed cannot
// reach outside its own package, and embedding is what makes `taskapi migrate
// up` work from the distroless image: there is no filesystem to read .sql
// files from, and no shell to copy them in with.
package migrations

import "embed"

// FS holds the versioned migrations, in golang-migrate's naming convention:
// <version>_<name>.<up|down>.sql.
//
//go:embed *.sql
var FS embed.FS
