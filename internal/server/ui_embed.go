//go:build !noui

package server

import (
	"embed"
	"errors"
	"github.com/suxen-project/suxen/internal/httpx"
	"io/fs"
	"net/http"
	"path"
	"strconv"
	"strings"
)

// uiEnabled reports whether this binary contains the administration UI.
const uiEnabled = true

// adminUI contains the dependency-free administration application shipped in the
// standard server binary.
//
//go:embed ui/*
var adminUI embed.FS

func (s *Server) handleUI(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet && r.Method != http.MethodHead {
		httpx.MethodNotAllowed(w, http.MethodGet, http.MethodHead)
		return
	}

	assetPath := strings.TrimPrefix(r.URL.Path, "/ui/")
	if r.URL.Path == "/" || r.URL.Path == "/ui" || assetPath == "" {
		assetPath = "index.html"
	}
	assetPath = path.Clean(assetPath)
	if assetPath == "." || strings.HasPrefix(assetPath, "../") {
		httpx.WriteProblem(w, http.StatusNotFound, "not_found", "UI asset not found")
		return
	}

	content, err := fs.ReadFile(adminUI, "ui/"+assetPath)
	if errors.Is(err, fs.ErrNotExist) {
		httpx.WriteProblem(w, http.StatusNotFound, "not_found", "UI asset not found")
		return
	}
	if err != nil {
		httpx.WriteServerProblem(
			w,
			http.StatusInternalServerError,
			"ui_error",
			"load UI asset failed",
			err,
		)
		return
	}

	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set(
		"Content-Security-Policy",
		"default-src 'self'; connect-src 'self'; img-src 'self' data:; "+
			"script-src 'self'; style-src 'self'; base-uri 'none'; frame-ancestors 'none'",
	)
	switch path.Ext(assetPath) {
	case ".css":
		w.Header().Set("Content-Type", "text/css; charset=utf-8")
	case ".js":
		w.Header().Set("Content-Type", "text/javascript; charset=utf-8")
	default:
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
	}
	w.Header().Set("Content-Length", strconv.Itoa(len(content)))
	w.WriteHeader(http.StatusOK)
	if r.Method == http.MethodGet {
		_, _ = w.Write(content)
	}
}
