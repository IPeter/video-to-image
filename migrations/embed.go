// Package migrations embeds the goose SQL migrations so `v2i migrate` needs no files
// on disk (it runs as a Kubernetes init Job from the same image).
package migrations

import "embed"

//go:embed *.sql
var FS embed.FS
