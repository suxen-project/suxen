//go:build noui

package server

import (
	"github.com/suxen-project/suxen/internal/httpx"
)

import "net/http"

// uiEnabled reports whether this binary contains the administration UI.
const uiEnabled = false

// handleUI exists in UI-free builds so both variants retain the same internal server
// shape. The router normally rejects UI paths before reaching this handler.
func (s *Server) handleUI(w http.ResponseWriter, _ *http.Request) {
	httpx.WriteProblem(w, http.StatusNotFound, "not_found", "route not found")
}
