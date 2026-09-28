package server

import (
	"net/http"
	"strings"
	"testing"

	"github.com/suxen-project/suxen/internal/config"
	"github.com/suxen-project/suxen/internal/domain"
	_ "github.com/suxen-project/suxen/plugins/format/pypi"
)

func TestPyPIProxyDistinguishesEscapedSlashFromPathSeparator(t *testing.T) {
	fixture := newServerFixture(t)
	fixture.Handler.updateConfig(func(cfg *config.Config) { cfg.ProxyManifestTTL = 0 })
	escaped := true
	fileCalls := 0
	fixture.Handler.setHTTPClient(&http.Client{Transport: roundTripFunc(func(request *http.Request) (*http.Response, error) {
		switch request.URL.Host {
		case "index.example":
			link := "https://files.example/files/a%2Fb.whl"
			if !escaped {
				link = "https://files.example/files/a/b.whl"
			}
			return testHTTPResponse(request, http.StatusOK, `<a href="`+link+`">b.whl</a>`), nil
		case "files.example":
			fileCalls++
			body := "literal slash"
			if strings.Contains(request.URL.EscapedPath(), "%2F") {
				body = "escaped slash"
			}
			return testHTTPResponse(request, http.StatusOK, body), nil
		default:
			return testHTTPResponse(request, http.StatusNotFound, ""), nil
		}
	})})
	createTestRepository(t, fixture, domain.Repository{
		Name: "pypi-escaped", Format: "pypi", Type: "proxy", Upstream: "https://index.example",
	})
	indexPath := "/repository/pypi-escaped/simple/demo/"
	index := fixture.request(t, http.MethodGet, indexPath, nil, true)
	assertStatus(t, index, http.StatusOK)
	if body := readBody(t, index); !strings.Contains(body, "a%252Fb.whl") {
		t.Fatalf("rewritten escaped file link = %q", body)
	}
	first := fixture.request(t, http.MethodGet, "/repository/pypi-escaped/files/https/files.example/files/a%252Fb.whl", nil, true)
	assertStatus(t, first, http.StatusOK)
	assertBody(t, first, []byte("escaped slash"))

	escaped = false
	index = fixture.request(t, http.MethodGet, indexPath, nil, true)
	assertStatus(t, index, http.StatusOK)
	if body := readBody(t, index); !strings.Contains(body, "/a/b.whl") {
		t.Fatalf("rewritten literal file link = %q", body)
	}
	second := fixture.request(t, http.MethodGet, "/repository/pypi-escaped/files/https/files.example/files/a/b.whl", nil, true)
	assertStatus(t, second, http.StatusOK)
	assertBody(t, second, []byte("literal slash"))
	if fileCalls != 2 {
		t.Fatalf("upstream file requests = %d, want two distinct targets", fileCalls)
	}
}
