// Package web embeds the browser UI (plain ES modules, no build step).
package web

import (
	"embed"
	"io/fs"
)

//go:embed static
var files embed.FS

// Static returns the UI file system rooted at static/.
func Static() fs.FS {
	sub, err := fs.Sub(files, "static")
	if err != nil {
		panic(err)
	}
	return sub
}
