package content

import (
	"context"
	"net/http"
	"net/http/httptest"
	"net/url"
	"testing"

	"github.com/suxen-project/suxen/internal/domain"
	_ "github.com/suxen-project/suxen/plugins/format/npm"
	spiformat "github.com/suxen-project/suxen/spi/format"
)

func TestBufferedFormatContentLimits(t *testing.T) {
	tests := []struct {
		format, path string
		want         int64
	}{
		{"npm", "typescript", npmPackumentSourceLimit},
		{"npm", "@prisma/client", npmPackumentSourceLimit},
		{"npm", "@prisma/client/-/client-1.0.0.tgz", formatContentSourceLimit},
		{"npm", "invalid/more/segments", formatContentSourceLimit},
		{"pypi", "simple", pypiRootSourceLimit},
		{"pypi", "simple/requests", formatContentSourceLimit},
		{"cargo", "se/rd/serde", formatContentSourceLimit},
	}
	for _, test := range tests {
		t.Run(test.format+"/"+test.path, func(t *testing.T) {
			if got := formatContentLimit(test.format, test.path); got != test.want {
				t.Fatalf("limit = %d, want %d", got, test.want)
			}
		})
	}
}

func TestBufferedFormatRejectsOversizeBeforeOpeningBlob(t *testing.T) {
	rt := &Runtime{}
	for _, test := range []struct {
		format, path string
		limit        int64
	}{
		{"npm", "typescript", npmPackumentSourceLimit},
		{"npm", "@prisma/client", npmPackumentSourceLimit},
		{"npm", "typescript/-/typescript-1.0.0.tgz", formatContentSourceLimit},
		{"pypi", "simple", pypiRootSourceLimit},
	} {
		_, err := rt.readAssetContentForFormat(context.Background(), domain.Asset{
			Path: test.path, Size: test.limit + 1,
		}, test.format)
		if err == nil {
			t.Errorf("%s %s: expected size rejection", test.format, test.path)
		}
	}
}

func TestLargeMetadataGroupAndOutputBudgets(t *testing.T) {
	for _, test := range []struct {
		format, path string
		want         int64
		wantError    error
	}{
		{"npm", "typescript", npmGroupSourcesLimit, errNpmGroupSourcesLimit},
		{"npm", "typescript/-/typescript-1.0.0.tgz", 0, nil},
		{"pypi", "simple", pypiGroupSourcesLimit, errPyPIGroupSourcesLimit},
		{"pypi", "simple/requests/", pypiGroupSourcesLimit, errPyPIGroupSourcesLimit},
		{"pypi", "packages/requests/requests.whl", 0, nil},
		{"cargo", "se/rd/serde", 0, nil},
	} {
		budget, err := formatGroupSourceBudget(test.format, test.path)
		if budget != test.want || err != test.wantError {
			t.Errorf("%s %s: budget/error = %d/%v, want %d/%v", test.format, test.path, budget, err, test.want, test.wantError)
		}
	}
	if npmRenderedContentTooLarge("npm", "typescript", npmRenderedContentLimit) {
		t.Fatal("exact npm output limit rejected")
	}
	if !npmRenderedContentTooLarge("npm", "typescript", npmRenderedContentLimit+1) {
		t.Fatal("oversized npm output accepted")
	}
	if npmRenderedContentTooLarge("npm", "typescript/-/typescript-1.0.0.tgz", npmRenderedContentLimit+1) {
		t.Fatal("tarball treated as packument")
	}
}

func TestLargeMetadataRenderSlotHonorsCancellation(t *testing.T) {
	rt := &Runtime{largeIndexSlots: make(chan struct{}, 1)}
	release, err := rt.acquireLargeIndex(context.Background(), "npm", "typescript")
	if err != nil {
		t.Fatal(err)
	}
	defer release()
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := rt.acquireLargeIndex(ctx, "npm", "typescript"); err != context.Canceled {
		t.Fatalf("contended npm render slot: %v, want context canceled", err)
	}
	for _, path := range []string{"simple", "simple/requests/"} {
		if _, err := rt.acquireLargeIndex(ctx, "pypi", path); err != context.Canceled {
			t.Fatalf("contended PyPI %s render slot: %v, want context canceled", path, err)
		}
	}
	if _, err := rt.acquireLargeIndex(ctx, "pypi", "packages/requests/requests.whl"); err != nil {
		t.Fatalf("distribution unexpectedly needed render slot: %v", err)
	}
	if _, err := rt.acquireLargeIndex(ctx, "npm", "typescript/-/typescript-1.0.0.tgz"); err != nil {
		t.Fatalf("ordinary path unexpectedly needed render slot: %v", err)
	}
}

// conditionalWireTestFormat honors a client's If-None-Match the way a real
// hosted index handler would: it answers 304 when the precondition is present.
// Internal group synthesis must never let that precondition through, because a
// 304 has no mergeable body.
type conditionalWireTestFormat struct{}

func init() {
	spiformat.Register(conditionalWireTestFormat{})
}

func (conditionalWireTestFormat) Name() string { return "conditional-wire-test" }

func (conditionalWireTestFormat) WireAction(
	_ spiformat.Repository,
	method string,
	_ string,
	_ url.Values,
) (string, bool) {
	if method == http.MethodGet {
		return "read", true
	}
	return "", false
}

func (conditionalWireTestFormat) ServeWire(
	w http.ResponseWriter,
	r *http.Request,
	_ spiformat.Repository,
	_ string,
	_ spiformat.WireTools,
) {
	if r.Header.Get("If-None-Match") != "" {
		w.WriteHeader(http.StatusNotModified)
		return
	}
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write([]byte("INDEX"))
}

func TestSynthesizeHostedWireReadStripsConditional(t *testing.T) {
	rt := &Runtime{}
	repo := domain.Repository{
		Name:   "member",
		Format: "conditional-wire-test",
		Type:   "hosted",
	}
	req := httptest.NewRequest(http.MethodGet, "/repository/group/index", nil)
	req.Header.Set("If-None-Match", `"deadbeef"`)

	body, ok, err := rt.synthesizeHostedWireRead(req, repo, "index")
	if err != nil {
		t.Fatalf("synthesizeHostedWireRead returned error: %v", err)
	}
	if !ok {
		t.Fatalf("synthesizeHostedWireRead did not resolve the source")
	}
	if string(body) != "INDEX" {
		t.Fatalf("body = %q, want INDEX (conditional header should have been stripped)", body)
	}
}

func TestStripConditionalRangeHeaders(t *testing.T) {
	header := http.Header{}
	for _, name := range []string{
		"If-None-Match", "If-Modified-Since", "If-Match",
		"If-Unmodified-Since", "If-Range", "Range",
	} {
		header.Set(name, "x")
	}
	header.Set("Accept", "application/json")

	stripConditionalRangeHeaders(header)

	for name := range header {
		if name != "Accept" {
			t.Errorf("header %q survived stripping", name)
		}
	}
	if header.Get("Accept") != "application/json" {
		t.Errorf("Accept header was removed")
	}
}
