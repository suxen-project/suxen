package oci

import (
	"net/http"
	"net/url"
	"strings"

	"github.com/suxen-project/suxen/internal/content"
	"github.com/suxen-project/suxen/internal/httpx"
	"github.com/suxen-project/suxen/internal/identity"
	spiformat "github.com/suxen-project/suxen/spi/format"
)

// Format dispatches the OCI Distribution API through the format SPI's
// WireProtocol hook, so the host routes OCI like any wire format. OCI stays a
// core format: it is compiled into the server, reaches the runtime through
// content.WireHost, and keeps the token endpoint, upload sessions, and
// bound-port listeners host-owned.
type Format struct{}

var _ spiformat.WireProtocol = Format{}

// Name returns the format identifier.
func (Format) Name() string { return "oci" }

// WireAction claims every path of an OCI repository. The token endpoint is
// dispatched without a privilege check because it authenticates clients
// itself; other requests map the method to the repository action. Methods
// outside the Distribution API are dispatched unchecked too, so ServeWire
// can answer 405 before authentication, as the Distribution API expects.
func (Format) WireAction(
	_ spiformat.Repository,
	method string,
	requestPath string,
	_ url.Values,
) (string, bool) {
	if ociRoute, distributionPath := splitOCIRequestPath(requestPath); ociRoute &&
		identity.DistributionAuthPath(distributionPath) {
		return "", true
	}
	action, _ := distributionAction(method)
	return action, true
}

// distributionAction maps a Distribution API method to the repository
// privilege it needs.
func distributionAction(method string) (string, bool) {
	switch method {
	case http.MethodGet, http.MethodHead:
		return "read", true
	case http.MethodPost, http.MethodPatch, http.MethodPut:
		return "write", true
	case http.MethodDelete:
		return "delete", true
	default:
		return "", false
	}
}

// PrepareWireResponse switches the error response format to the
// Distribution error envelope before the host writes any problem, so an
// authentication failure on a /v2 route reads as an OCI error.
func (Format) PrepareWireResponse(w http.ResponseWriter, requestPath string) {
	if ociRoute, _ := splitOCIRequestPath(requestPath); ociRoute {
		httpx.SelectErrorResponseFormat(w, httpx.ErrorResponseFormatOCI)
	}
}

// ServeWire serves one request of an OCI repository.
func (Format) ServeWire(
	w http.ResponseWriter,
	r *http.Request,
	_ spiformat.Repository,
	requestPath string,
	tools spiformat.WireTools,
) {
	host, ok := tools.(content.WireHost)
	if !ok {
		httpx.WriteProblem(
			w,
			http.StatusInternalServerError,
			"oci_host_unavailable",
			"OCI requires the in-process runtime",
		)
		return
	}
	ociRoute, distributionPath := splitOCIRequestPath(requestPath)
	if _, supported := distributionAction(r.Method); !supported &&
		!(ociRoute && identity.DistributionAuthPath(distributionPath)) {
		httpx.MethodNotAllowed(
			w,
			http.MethodGet,
			http.MethodHead,
			http.MethodPost,
			http.MethodPatch,
			http.MethodPut,
			http.MethodDelete,
		)
		return
	}
	if !ociRoute {
		httpx.WriteProblem(
			w,
			http.StatusBadRequest,
			"format_mismatch",
			"OCI repositories must use the v2 endpoint",
		)
		return
	}
	New(host.Runtime()).Handle(w, r, host.Repository(), distributionPath)
}

// splitOCIRequestPath recognizes the repository-relative v2 prefix and
// returns the Distribution API path below it.
func splitOCIRequestPath(requestPath string) (bool, string) {
	requestPath = strings.TrimPrefix(requestPath, "/")
	if requestPath == "v2" || strings.HasPrefix(requestPath, "v2/") {
		return true, strings.TrimPrefix(requestPath, "v2")
	}
	return false, ""
}
