// Package web serves the single-page library manager.
package web

import (
	_ "embed"
	"net/http"
)

//go:embed index.html
var index []byte

// Handler serves the embedded interface. It holds no data: the page talks to
// the JSON API, so it can be served without the API key (the page asks for it).
func Handler() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		h := w.Header()
		h.Set("Content-Type", "text/html; charset=utf-8")
		h.Set("Cache-Control", "no-cache")
		h.Set("Content-Security-Policy", "default-src 'self'; style-src 'unsafe-inline'; script-src 'unsafe-inline'; img-src 'self' blob:; frame-ancestors 'none'")
		h.Set("X-Content-Type-Options", "nosniff")
		_, _ = w.Write(index)
	})
}
