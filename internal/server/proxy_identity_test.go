package server

import (
	"context"
	"errors"
	"net/http"
	"sync"
	"testing"

	"github.com/suxen-project/suxen/internal/domain"
	"github.com/suxen-project/suxen/internal/store"
)

// TestProxyBlockedFetchDoesNotPublishIntoSameNameRecreation reproduces the
// review's blocked-fetch scenario: a proxy fetch resolves its repository, then
// blocks in the upstream request while the repository is deleted and recreated
// under the same public name. When the fetch is released it must not publish
// its bytes into the replacement, because the replacement is a fresh internal
// identity. The upstream transport is gated on a channel, so the ordering is
// deterministic without sleeps.
func TestProxyBlockedFetchDoesNotPublishIntoSameNameRecreation(t *testing.T) {
	t.Parallel()
	fixture := newServerFixture(t)
	ctx := context.Background()

	entered := make(chan struct{})
	release := make(chan struct{})
	var once sync.Once
	fixture.Handler.setHTTPClient(&http.Client{
		Transport: roundTripFunc(func(request *http.Request) (*http.Response, error) {
			blocked := false
			once.Do(func() {
				blocked = true
				close(entered)
			})
			if blocked {
				<-release
			}
			return testHTTPResponse(request, http.StatusOK, "ORIGINAL"), nil
		}),
	})

	repository := domain.Repository{
		Name:     "mirror",
		Format:   "raw",
		Type:     "proxy",
		Upstream: "https://origin.example/artifacts",
	}
	if err := fixture.Metadata.CreateRepository(ctx, repository); err != nil {
		t.Fatal(err)
	}
	first, err := fixture.Metadata.Repository(ctx, "mirror")
	if err != nil {
		t.Fatal(err)
	}

	// Start the fetch; it blocks in the upstream transport, which is reached
	// before any blob staging or store lease, so the repository can be replaced.
	done := make(chan *http.Response, 1)
	go func() {
		done <- fixture.request(t, http.MethodGet, "/repository/mirror/file", nil, true)
	}()
	<-entered

	// Delete and recreate under the same name: a fresh internal identity.
	if err := fixture.Metadata.DeleteRepository(ctx, "mirror", store.Ownership{Force: true}); err != nil {
		t.Fatal(err)
	}
	if err := fixture.Metadata.CreateRepository(ctx, repository); err != nil {
		t.Fatal(err)
	}
	second, err := fixture.Metadata.Repository(ctx, "mirror")
	if err != nil {
		t.Fatal(err)
	}
	if second.ID == first.ID {
		t.Fatalf("recreated repository reused the internal id %q", second.ID)
	}

	// Release the blocked fetch. Its write is pinned to the original identity,
	// which no longer exists, so publication must fail rather than land in the
	// replacement.
	close(release)
	response := <-done
	response.Body.Close()

	if _, err := fixture.Metadata.Asset(ctx, "mirror", "file"); !errors.Is(err, domain.ErrNotFound) {
		t.Fatalf("stale fetch published into the replacement: %v", err)
	}
}
