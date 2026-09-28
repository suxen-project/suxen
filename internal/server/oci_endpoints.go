package server

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/suxen-project/suxen/internal/domain"
	"github.com/suxen-project/suxen/internal/httpx"
	"github.com/suxen-project/suxen/internal/startup"
)

type listenPortContextKey struct{}

type extraHTTPListeners struct {
	mu      sync.Mutex
	enabled bool
	servers map[int]*http.Server
}

const defaultOCIRepositoryName = "oci"

// HandlerForPort returns the process HTTP handler bound to a listen port so
// `/v2/` routing can select an OCI repository by extra listener.
func (s *Server) HandlerForPort(port int) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		s.httpHandler.ServeHTTP(w, requestWithListenPort(r, port))
	})
}

// extraOCIHandler serves only registry-root and probe routes on extra listen
// ports so a Docker-facing port does not expose the control plane or UI.
func (s *Server) extraOCIHandler(port int) http.Handler {
	inner := s.HandlerForPort(port)
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		path := r.URL.Path
		switch {
		case path == "/healthz", path == "/readyz":
			inner.ServeHTTP(w, r)
		case path == "/v2" || strings.HasPrefix(path, "/v2/"):
			inner.ServeHTTP(w, r)
		default:
			httpx.WriteProblem(w, http.StatusNotFound, "not_found", "route not found")
		}
	})
}

// ExtraListenerErrors reports a fatal extra-listener failure. The channel is
// never closed; callers should select on it alongside the primary server.
func (s *Server) ExtraListenerErrors() <-chan error {
	return s.extraErrors
}

// StartExtraListeners binds additional HTTP servers for the OCI repository
// ports configured at startup that are not the primary SUXEN_LISTEN port. The
// set is fixed for the process lifetime: repository create/update/delete change
// routing but never bind or unbind a listener, so a port added or changed after
// startup opens only on the next restart.
func (s *Server) StartExtraListeners(ctx context.Context) error {
	s.extraListeners.mu.Lock()
	s.extraListeners.enabled = true
	if s.extraListeners.servers == nil {
		s.extraListeners.servers = make(map[int]*http.Server)
	}
	s.extraListeners.mu.Unlock()
	return s.syncExtraListeners(ctx)
}

func requestWithListenPort(r *http.Request, port int) *http.Request {
	if port < 1 {
		return r
	}
	return r.WithContext(context.WithValue(r.Context(), listenPortContextKey{}, port))
}

func listenPortFromRequest(r *http.Request) int {
	port, _ := r.Context().Value(listenPortContextKey{}).(int)
	return port
}

func (s *Server) resolveOCIRootRepository(r *http.Request) (domain.Repository, error) {
	repositories, err := s.repositoryReads().Repositories(r.Context())
	if err != nil {
		return domain.Repository{}, err
	}
	host := domain.CanonicalOCIRequestHost(r.Host)
	if host != "" {
		for _, repository := range repositories {
			if repository.Format != "oci" || repository.Endpoints.Empty() {
				continue
			}
			for _, boundHost := range repository.Endpoints.Hosts {
				if boundHost == host {
					return repository, nil
				}
			}
		}
	}
	port := listenPortFromRequest(r)
	if port > 0 {
		for _, repository := range repositories {
			if repository.Format != "oci" || repository.Endpoints.Empty() {
				continue
			}
			for _, boundPort := range repository.Endpoints.Ports {
				if boundPort == port {
					return repository, nil
				}
			}
		}
	}
	return s.repositoryReads().Repository(r.Context(), defaultOCIRepositoryName)
}

func (s *Server) syncExtraListeners(ctx context.Context) error {
	s.extraListeners.mu.Lock()
	enabled := s.extraListeners.enabled
	s.extraListeners.mu.Unlock()
	if !enabled {
		return nil
	}
	wanted, err := s.extraListenPorts(ctx)
	if err != nil {
		return startup.Retry(err)
	}
	wantedSet := make(map[int]struct{}, len(wanted))
	started := make([]int, 0)
	for _, port := range wanted {
		wantedSet[port] = struct{}{}
		existed := s.hasExtraListener(port)
		if err := s.ensureExtraListener(port); err != nil {
			s.stopExtraListenersOn(started)
			return err
		}
		if !existed {
			started = append(started, port)
		}
	}
	s.extraListeners.mu.Lock()
	unused := make([]*http.Server, 0)
	for port, httpServer := range s.extraListeners.servers {
		if _, keep := wantedSet[port]; keep {
			continue
		}
		unused = append(unused, httpServer)
		delete(s.extraListeners.servers, port)
	}
	s.extraListeners.mu.Unlock()
	for _, httpServer := range unused {
		shutdownHTTPServer(httpServer)
	}
	return nil
}

func (s *Server) extraListenPorts(ctx context.Context) ([]int, error) {
	primaryPort, _ := listenPortFromAddr(s.cfg.Listen)
	repositories, err := s.repositoryReads().Repositories(ctx)
	if err != nil {
		return nil, err
	}
	seen := make(map[int]struct{})
	ports := make([]int, 0)
	for _, repository := range repositories {
		if repository.Endpoints.Empty() {
			continue
		}
		for _, port := range repository.Endpoints.Ports {
			if port == primaryPort {
				continue
			}
			if _, exists := seen[port]; exists {
				continue
			}
			seen[port] = struct{}{}
			ports = append(ports, port)
		}
	}
	return ports, nil
}

func (s *Server) hasExtraListener(port int) bool {
	s.extraListeners.mu.Lock()
	defer s.extraListeners.mu.Unlock()
	_, exists := s.extraListeners.servers[port]
	return exists
}

func (s *Server) stopExtraListenersOn(ports []int) {
	for _, port := range ports {
		s.extraListeners.mu.Lock()
		httpServer := s.extraListeners.servers[port]
		delete(s.extraListeners.servers, port)
		s.extraListeners.mu.Unlock()
		if httpServer != nil {
			shutdownHTTPServer(httpServer)
		}
	}
}

func (s *Server) ensureExtraListener(port int) error {
	s.extraListeners.mu.Lock()
	if _, exists := s.extraListeners.servers[port]; exists {
		s.extraListeners.mu.Unlock()
		return nil
	}
	addr := extraListenAddr(s.cfg.Listen, port)
	listener, err := net.Listen("tcp", addr)
	if err != nil {
		s.extraListeners.mu.Unlock()
		return fmt.Errorf("listen on %s: %w", addr, err)
	}
	httpServer := &http.Server{
		Addr:              addr,
		Handler:           s.extraOCIHandler(port),
		ReadHeaderTimeout: 10 * time.Second,
		ReadTimeout:       s.cfg.ReadTimeout,
		WriteTimeout:      s.cfg.WriteTimeout,
		IdleTimeout:       2 * time.Minute,
	}
	s.extraListeners.servers[port] = httpServer
	s.extraListeners.mu.Unlock()

	go func() {
		s.log.Info("extra OCI listener started", "address", addr, "port", port)
		err := httpServer.Serve(listener)
		if err == nil || errors.Is(err, http.ErrServerClosed) {
			return
		}
		s.log.Error("extra OCI listener failed", "address", addr, "error", err)
		select {
		case s.extraErrors <- fmt.Errorf("listen on %s: %w", addr, err):
		default:
		}
	}()
	return nil
}

func (s *Server) stopExtraListeners() {
	s.extraListeners.mu.Lock()
	s.extraListeners.enabled = false
	servers := s.extraListeners.servers
	s.extraListeners.servers = nil
	s.extraListeners.mu.Unlock()
	for _, httpServer := range servers {
		shutdownHTTPServer(httpServer)
	}
}

func shutdownHTTPServer(httpServer *http.Server) {
	if httpServer == nil {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	_ = httpServer.Shutdown(ctx)
}

func listenPortFromAddr(addr string) (int, error) {
	if addr == "" {
		return 0, errors.New("listen address is empty")
	}
	_, portText, err := net.SplitHostPort(addr)
	if err != nil {
		return 0, err
	}
	port, err := strconv.Atoi(portText)
	if err != nil || port < 1 || port > 65535 {
		return 0, fmt.Errorf("invalid listen port in %q", addr)
	}
	return port, nil
}

func extraListenAddr(primary string, port int) string {
	host, _, err := net.SplitHostPort(primary)
	if err != nil {
		return fmt.Sprintf(":%d", port)
	}
	return net.JoinHostPort(host, strconv.Itoa(port))
}
