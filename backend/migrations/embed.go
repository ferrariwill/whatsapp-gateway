// Package migrations embute os arquivos SQL para golang-migrate no binário final (Render).
package migrations

import "embed"

//go:embed *.sql
var FS embed.FS
