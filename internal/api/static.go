package api

import (
	"bytes"
	"io/fs"
	"mime"
	"net/http"
	"path"
	"strings"
	"time"

	"github.com/systemgroup/redmine-coding-agent/internal/web"
)

// newStaticHandler serves the embedded SPA, with immutable caching for hashed
// assets and an index.html fallback for client-side routes.
func newStaticHandler() http.Handler {
	dist, err := fs.Sub(web.Dist(), "dist")
	if err != nil {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			http.Error(w, "embedded web assets are unavailable", http.StatusInternalServerError)
		})
	}
	index, err := fs.ReadFile(dist, "index.html")
	if err != nil {
		index = []byte("<!doctype html><title>RedmineCodingAgent</title>")
	}
	return &staticHandler{assets: dist, index: index}
}

type staticHandler struct {
	assets fs.FS
	index  []byte
}

func (h *staticHandler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet && r.Method != http.MethodHead {
		w.Header().Set("Allow", "GET, HEAD")
		http.Error(w, http.StatusText(http.StatusMethodNotAllowed), http.StatusMethodNotAllowed)
		return
	}

	cleaned := path.Clean("/" + r.URL.Path)
	if cleaned == "/" {
		h.serveIndex(w, r)
		return
	}
	name := strings.TrimPrefix(cleaned, "/")
	if fs.ValidPath(name) {
		if info, err := fs.Stat(h.assets, name); err == nil && !info.IsDir() {
			h.serveAsset(w, r, name, info)
			return
		}
	}
	// Unknown non-/api path: let the SPA router handle it.
	h.serveIndex(w, r)
}

func (h *staticHandler) serveAsset(w http.ResponseWriter, r *http.Request, name string, info fs.FileInfo) {
	content, err := fs.ReadFile(h.assets, name)
	if err != nil {
		h.serveIndex(w, r)
		return
	}
	if contentType := mime.TypeByExtension(path.Ext(name)); contentType != "" {
		w.Header().Set("Content-Type", contentType)
	}
	if strings.HasPrefix(name, "_app/immutable/") {
		w.Header().Set("Cache-Control", "public, max-age=31536000, immutable")
	} else {
		w.Header().Set("Cache-Control", "no-cache")
	}
	http.ServeContent(w, r, name, info.ModTime().UTC(), bytes.NewReader(content))
}

func (h *staticHandler) serveIndex(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Cache-Control", "no-cache")
	http.ServeContent(w, r, "index.html", time.Time{}, bytes.NewReader(h.index))
}
