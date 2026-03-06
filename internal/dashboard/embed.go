package dashboard

import (
	"embed"
	"io/fs"
)

//go:embed web/dist
var webDistEmbed embed.FS

// WebDist is the embedded Vite build rooted at web/dist.
var WebDist fs.FS = func() fs.FS {
	sub, err := fs.Sub(webDistEmbed, "web/dist")
	if err != nil {
		panic("dashboard: embed.go: " + err.Error())
	}
	return sub
}()
