package content

import (
	"bytes"
	"errors"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/suxen-project/suxen/internal/domain"
	"github.com/suxen-project/suxen/internal/httpx"
	spiformat "github.com/suxen-project/suxen/spi/format"
)

func TestWireErrorReporterIncludesRequestContext(t *testing.T) {
	const secret = "database-detail-must-stay-in-log"
	var output bytes.Buffer
	runtime := &Runtime{Log: slog.New(slog.NewTextHandler(&output, nil))}
	tools := runtime.WireTools(domain.Repository{Name: "packages", Format: "npm"})
	request := httptest.NewRequest(http.MethodGet, "/repository/packages/widget", nil)
	request = httpx.WithRequestLog(request, &httpx.RequestLog{RequestID: "request-456", Subject: "alice"})
	spiformat.ReportWireError(tools, request.Context(), "list versions", errors.New(secret))
	for _, expected := range []string{secret, "request-456", "alice", "packages", "npm", "list versions"} {
		if !strings.Contains(output.String(), expected) {
			t.Fatalf("log missing %q: %s", expected, output.String())
		}
	}
}
