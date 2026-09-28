package server

import (
	"log/slog"
	"net/http"

	"github.com/suxen-project/suxen/internal/config"
	"github.com/suxen-project/suxen/internal/store"
)

// Production code builds the identity service and the content runtime once
// from the constructor arguments. Tests that adjust a fixture after
// Bootstrap must push the change to every component that received a copy,
// which these helpers do so no test reaches into a component directly.

func (s *Server) setHTTPClient(client *http.Client) {
	s.identity.SetHTTPClient(client)
	s.content.SetHTTPClient(client)
}

func (s *Server) setMetadata(metadata store.Store) {
	s.metadata = metadata
	s.identity.SetMetadata(metadata)
	s.content.SetMetadata(metadata)
}

func (s *Server) setLogger(log *slog.Logger) {
	s.log = log
	s.identity.SetLogger(log)
	s.content.SetLogger(log)
}

func (s *Server) updateConfig(mutate func(*config.Config)) {
	mutate(&s.cfg)
	s.identity.SetConfig(s.cfg)
	s.content.SetConfig(s.cfg)
}
