package webdash

import (
	"embed"
	"io/fs"
	"net/http"
)

//go:embed all:build
var buildFS embed.FS

var fsys = func() fs.FS {
	sub, err := fs.Sub(buildFS, "build")
	if err != nil {
		panic(err)
	}
	return sub
}()

// FileServer provides access to the bundled web assets (HTML, CSS, etc)
// via an http.Handler
func FileServer() http.Handler {
	return http.FileServer(http.FS(fsys))
}

// RootHandler returns an http handler which always responds with the /index.html file.
func RootHandler() http.Handler {
	index, err := fs.ReadFile(fsys, "index.html")
	if err != nil {
		panic("webdash: fs.ReadFile(index.html): " + err.Error())
	}

	return http.HandlerFunc(func(resp http.ResponseWriter, req *http.Request) {
		_, err := resp.Write(index)
		if err != nil {
			return
		}
	})
}
