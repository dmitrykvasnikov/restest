// Package migrations carries the goose migration files into the binary.
//
// The files live here rather than under internal/ because go:embed cannot
// reach outside the directory of the package that declares it, and the
// migrations are also read directly by the goose CLI (`make migrate`) and by
// sqlc, both of which expect them at a stable path.
package migrations

import "embed"

// FS holds every migration, in the layout goose expects: file names beginning
// with a version number, at the root of the filesystem.
//
//go:embed *.sql
var FS embed.FS
