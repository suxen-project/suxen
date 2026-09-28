package content

import (
	"context"
	"io"
	"net/http"
	"net/url"
	"strings"
	"testing"

	"github.com/suxen-project/suxen/internal/domain"
)

type proxyTargetRoundTripFunc func(*http.Request) (*http.Response, error)

func (fn proxyTargetRoundTripFunc) RoundTrip(request *http.Request) (*http.Response, error) {
	return fn(request)
}

func TestProxyRequestCallersPreservePathAndProtocolQuery(t *testing.T) {
	repository := domain.Repository{Type: "proxy", Upstream: "https://upstream.example/root"}
	var gotPath, gotQuery string
	runtime := &Runtime{http: &http.Client{Transport: proxyTargetRoundTripFunc(func(request *http.Request) (*http.Response, error) {
		gotPath, gotQuery = request.URL.Path, request.URL.RawQuery
		return &http.Response{StatusCode: http.StatusOK, Body: io.NopCloser(strings.NewReader("ok")), Header: http.Header{}}, nil
	})}}
	request := &http.Request{Header: http.Header{}}
	check := func(response *http.Response, err error, path, query string) {
		t.Helper()
		if err != nil {
			t.Fatal(err)
		}
		defer response.Body.Close()
		if gotPath != path || gotQuery != query {
			t.Fatalf("upstream path=%q query=%q; want path=%q query=%q", gotPath, gotQuery, path, query)
		}
	}
	response, err := runtime.doUpstreamRequest(request, repository, http.MethodGet, "file?build=1", nil, resolvedUpstream{})
	check(response, err, "/root/file?build=1", "")
	response, err = runtime.DoUpstreamRequest(request, repository, http.MethodGet, "v2/image/referrers/sha256:digest?artifactType=example", nil)
	check(response, err, "/root/v2/image/referrers/sha256:digest", "artifactType=example")
	response, err = (wireTools{runtime: runtime, repository: repository}).Upstream(context.Background(), http.MethodGet, "repo.git/info/refs?service=git-upload-pack", nil, nil)
	check(response, err, "/root/repo.git/info/refs", "service=git-upload-pack")
}

func TestProxyURLPreservesDecodedAssetPath(t *testing.T) {
	requestURL, _, err := proxyURL("https://upstream.example/root", "file?build=1")
	if err != nil {
		t.Fatal(err)
	}
	parsed, err := url.Parse(requestURL)
	if err != nil {
		t.Fatal(err)
	}
	if parsed.Path != "/root/file?build=1" || parsed.RawQuery != "" || parsed.EscapedPath() != "/root/file%3Fbuild=1" {
		t.Fatalf("proxy URL path=%q escaped=%q query=%q", parsed.Path, parsed.EscapedPath(), parsed.RawQuery)
	}
}

func TestProxyURLWithProtocolQuery(t *testing.T) {
	for _, requestTarget := range []string{
		"repo.git/info/refs?service=git-upload-pack",
		"v2/image/referrers/sha256:digest?artifactType=application%2Fvnd.example",
	} {
		requestURL, _, err := proxyURLWithQuery("https://upstream.example/root", requestTarget)
		if err != nil {
			t.Fatal(err)
		}
		parsed, err := url.Parse(requestURL)
		if err != nil {
			t.Fatal(err)
		}
		path, query, _ := strings.Cut(requestTarget, "?")
		if parsed.Path != "/root/"+path || parsed.RawQuery != query {
			t.Fatalf("protocol URL path=%q query=%q for %q", parsed.Path, parsed.RawQuery, requestTarget)
		}
	}
}

func TestInvalidResolvedUpstreamDoesNotExposeQuery(t *testing.T) {
	repository := domain.Repository{Upstream: "https://upstream.example"}
	for _, target := range []string{
		"ftp://other.example/file?token=secret",
		"https://user:secret@other.example/file",
		"https://other.example/%zz-secret?token=secret",
		"http:/file?token=secret",
	} {
		_, _, err := validatedResolvedUpstream(repository, target, upstreamCredentials{})
		if err == nil || strings.Contains(err.Error(), "secret") {
			t.Fatalf("target %q returned unsafe error %v", target, err)
		}
	}
}
