// Package db embeds the SQL migrations applied at boot.
package db

import (
	"embed"
	"io/fs"
)

//go:embed migrations/*.sql
var migrationsFS embed.FS

// Migrations returns the embedded migrations directory as an fs.FS.
func Migrations() fs.FS {
	sub, err := fs.Sub(migrationsFS, "migrations")
	if err != nil {
		panic("db: migrations sub: " + err.Error())
	}
	return sub
}
