// Package web embeds the admin console.
package web

import (
	"embed"
	"io/fs"
)

//go:embed all:static
var files embed.FS

// FS returns the console's static files.
func FS() fs.FS {
	sub, err := fs.Sub(files, "static")
	if err != nil {
		panic(err)
	}
	return sub
}
