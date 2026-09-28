package git

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/suxen-project/suxen/spi/format"
)

type failingWireTools struct {
	format.WireTools
	err      error
	reader   io.ReadCloser
	reported []error
}

func (tools *failingWireTools) StatAsset(context.Context, string) (format.Asset, bool, error) {
	return format.Asset{}, false, tools.err
}

func (tools *failingWireTools) OpenAsset(context.Context, string) (io.ReadCloser, format.Asset, bool, error) {
	return tools.reader, format.Asset{ContentType: contentTypePackfile + "; shallow=" + strings.Repeat("a", 40)}, true, nil
}

func (tools *failingWireTools) OpenMetadataAsset(context.Context, string) (io.ReadCloser, format.Asset, bool, error) {
	return nil, format.Asset{}, false, nil
}

func (tools *failingWireTools) ReportWireError(_ context.Context, _ string, err error) {
	tools.reported = append(tools.reported, err)
}

func TestLsRefsHidesInternalFailure(t *testing.T) {
	const secret = "upstream-credential-must-not-leak"
	tools := &failingWireTools{err: errors.New(secret)}
	server := protocolServer{tools: tools, route: route{key: "acme/widget"}}
	response := httptest.NewRecorder()
	server.serveLsRefs(response, context.Background(), commandRequest{command: "ls-refs"})
	if response.Code != http.StatusOK || strings.Contains(response.Body.String(), secret) || !strings.Contains(response.Body.String(), "snapshot temporarily unavailable") {
		t.Fatalf("response = %d %q", response.Code, response.Body.String())
	}
	if len(tools.reported) != 1 || !errors.Is(tools.reported[0], tools.err) {
		t.Fatalf("reported = %v", tools.reported)
	}
}

func TestGitPolicyFailureRemainsActionable(t *testing.T) {
	tools := &failingWireTools{}
	server := protocolServer{tools: tools}
	response := httptest.NewRecorder()
	server.writeBackendError(response, context.Background(), "open snapshot", format.ErrPolicyRejected)
	if response.Code != http.StatusOK || !strings.Contains(response.Body.String(), "snapshot rejected by repository policy") || len(tools.reported) != 0 {
		t.Fatalf("response = %d %q, reports %v", response.Code, response.Body.String(), tools.reported)
	}
}

type failingReader struct{ err error }

func (reader failingReader) Read([]byte) (int, error) { return 0, reader.err }

func TestSnapshotStreamHidesInternalFailure(t *testing.T) {
	const secret = "blob-path-must-not-leak"
	tools := &failingWireTools{reader: io.NopCloser(failingReader{err: errors.New(secret)})}
	server := protocolServer{tools: tools, route: route{key: "acme/widget"}}
	response := httptest.NewRecorder()
	server.serveFetch(response, context.Background(), commandRequest{args: []string{"want " + strings.Repeat("a", 40), "done"}})
	if response.Code != http.StatusOK || strings.Contains(response.Body.String(), secret) || !strings.Contains(response.Body.String(), "snapshot stream failed") {
		t.Fatalf("response = %d %q", response.Code, response.Body.String())
	}
	if len(tools.reported) != 1 || tools.reported[0].Error() != secret {
		t.Fatalf("reported = %v", tools.reported)
	}
}
