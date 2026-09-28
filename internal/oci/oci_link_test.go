package oci

import (
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"

	"github.com/suxen-project/suxen/internal/content"
	"github.com/suxen-project/suxen/internal/domain"
	"github.com/suxen-project/suxen/internal/ocimodel"
)

type ociLinkTransport func(*http.Request) (*http.Response, error)

func (fn ociLinkTransport) RoundTrip(request *http.Request) (*http.Response, error) {
	return fn(request)
}

func TestParseOCINextLinkHeaders(t *testing.T) {
	const target = "/v2/app/tags/list?cursor=one,two;three"
	for _, test := range []struct {
		name    string
		headers []string
		want    string
	}{
		{"URI delimiters", []string{`<` + target + `>; rel="next"`}, target},
		{"relation list", []string{`<` + target + `>; rel="prev NeXt"`}, target},
		{"whitespace and parameter case", []string{`<` + target + `> ; ReL = "next"`}, target},
		{"other quoted delimiters", []string{`<` + target + `>; title="a, b; rel=other"; rel=next`}, target},
		{"multiple links", []string{`</first>; rel="prev", <` + target + `>; rel="next"`}, target},
		{"repeated fields", []string{`</first>; rel=prev`, `<` + target + `>; rel=next`}, target},
		{"flag before relation", []string{`<` + target + `>; foo; rel=next`}, target},
		{"flag after relation", []string{`<` + target + `>; rel=next; foo`}, target},
		{"escaped quote", []string{`<` + target + `>; title="a\"; rel=other, b"; rel=next`}, target},
		{"unrelated parameter", []string{`</first>; title="rel=next"`}, ""},
		{"unrelated relation", []string{`</first>; rel="next-page"`}, ""},
		{"unrelated name", []string{`</first>; xrel=next`}, ""},
		{"duplicate relation", []string{`</first>; rel=prev; rel=next`}, ""},
		{"anchor before relation", []string{`</first>; anchor="/elsewhere"; rel=next`}, ""},
		{"anchor after relation", []string{`</first>; rel=next; anchor="/elsewhere"`}, ""},
		{"malformed suffix", []string{`</first>; rel=next; title="unterminated`}, ""},
		{"missing separator", []string{`</first>; rel=next title=other`}, ""},
		{"malformed target", []string{`/first; rel=next`}, ""},
	} {
		t.Run(test.name, func(t *testing.T) {
			got, ok := parseOCINextLinkHeaders(test.headers)
			if got != test.want || ok != (test.want != "") {
				t.Fatalf("next link = %q, %v; want %q", got, ok, test.want)
			}
		})
	}
}

func TestProxyOCIPaginationLinkForms(t *testing.T) {
	const digest = "sha256:0123456789abcdef"
	for _, kind := range []string{"catalog", "tags", "referrers"} {
		t.Run(kind, func(t *testing.T) {
			path, firstBody, secondBody := "v2/_catalog", `{"repositories":["first"]}`, `{"repositories":["second"]}`
			switch kind {
			case "tags":
				path, firstBody, secondBody = "v2/app/tags/list", `{"tags":["first"]}`, `{"tags":["second"]}`
			case "referrers":
				path = "v2/app/referrers/" + digest
				firstBody = `{"schemaVersion":2,"manifests":[{"mediaType":"application/vnd.oci.image.manifest.v1+json","digest":"sha256:first","size":1}]}`
				secondBody = `{"schemaVersion":2,"manifests":[{"mediaType":"application/vnd.oci.image.manifest.v1+json","digest":"sha256:second","size":1}]}`
			}
			requests := 0
			runtime := content.New(content.Options{HTTPClient: &http.Client{Transport: ociLinkTransport(func(request *http.Request) (*http.Response, error) {
				requests++
				if request.URL.Path != "/source/"+path {
					t.Errorf("upstream path = %q", request.URL.Path)
				}
				header := make(http.Header)
				body := secondBody
				if requests == 1 {
					body = firstBody
					switch kind {
					case "catalog":
						header.Add("Link", `</source/`+path+`?cursor=one,two;three>; title="a, b; rel=prev"; rel="prev NEXT"`)
					case "tags":
						header.Add("Link", `</source/`+path+`?cursor=wrong>; rel=prev`)
						header.Add("Link", `</source/`+path+`?cursor=one,two;three> ; ReL = "next"`)
					case "referrers":
						header.Add("Link", `</source/`+path+`?cursor=wrong>; title="rel=next"; rel=prev, </source/`+path+`?cursor=one,two;three>; rel=next`)
					}
				} else if request.URL.RawQuery != "cursor=one,two;three" {
					t.Errorf("continuation query = %q", request.URL.RawQuery)
				}
				return &http.Response{StatusCode: http.StatusOK, Header: header, Body: io.NopCloser(strings.NewReader(body))}, nil
			})}})
			handler := New(runtime)
			request := httptest.NewRequest(http.MethodGet, "http://local/"+path, nil)
			repository := domain.Repository{Format: "oci", Type: "proxy", Upstream: "https://registry.example/source"}
			var got []string
			var err error
			switch kind {
			case "catalog":
				got, err = handler.fetchProxyOCICatalog(request, repository)
			case "tags":
				got, err = handler.fetchProxyOCITags(request, repository, "app")
			case "referrers":
				var descriptors []ocimodel.Descriptor
				descriptors, err = handler.fetchProxyOCIReferrers(request, repository, "app", digest)
				for _, descriptor := range descriptors {
					got = append(got, strings.TrimPrefix(descriptor.Digest, "sha256:"))
				}
			}
			if err != nil || strings.Join(got, ",") != "first,second" || requests != 2 {
				t.Fatalf("items=%v requests=%d err=%v", got, requests, err)
			}
		})
	}
}

func TestProxyOCINextLinkConfinementAndPageLimit(t *testing.T) {
	for _, link := range []string{
		`<https://elsewhere.example/source/v2/_catalog?cursor=1>; rel=next`,
		`</other/v2/_catalog?cursor=1>; rel=next`,
	} {
		t.Run(link, func(t *testing.T) {
			runtime := content.New(content.Options{HTTPClient: &http.Client{Transport: ociLinkTransport(func(*http.Request) (*http.Response, error) {
				header := http.Header{"Link": {link}}
				return &http.Response{StatusCode: http.StatusOK, Header: header, Body: io.NopCloser(strings.NewReader(`{"repositories":[]}`))}, nil
			})}})
			_, err := New(runtime).fetchProxyOCICatalog(httptest.NewRequest(http.MethodGet, "http://local/v2/_catalog", nil), domain.Repository{Type: "proxy", Upstream: "https://registry.example/source"})
			if err == nil || !strings.Contains(err.Error(), "leaves the configured upstream endpoint") {
				t.Fatalf("escape accepted: %v", err)
			}
		})
	}
	requests := 0
	runtime := content.New(content.Options{HTTPClient: &http.Client{Transport: ociLinkTransport(func(*http.Request) (*http.Response, error) {
		requests++
		header := http.Header{"Link": {`</source/v2/_catalog?cursor=` + fmt.Sprint(requests) + `>; rel=next`}}
		return &http.Response{StatusCode: http.StatusOK, Header: header, Body: io.NopCloser(strings.NewReader(`{"repositories":[]}`))}, nil
	})}})
	_, err := New(runtime).fetchProxyOCICatalog(httptest.NewRequest(http.MethodGet, "http://local/v2/_catalog", nil), domain.Repository{Type: "proxy", Upstream: "https://registry.example/source"})
	if err == nil || !strings.Contains(err.Error(), "page limit") || requests != maxOCIUpstreamPages {
		t.Fatalf("page limit: requests=%d err=%v", requests, err)
	}
}

func FuzzOCINextLinkParser(f *testing.F) {
	f.Add("one,two;three", `a, b; "quote"`)
	f.Add("next", "")
	f.Fuzz(func(t *testing.T, cursor, title string) {
		if len(cursor) > 1000 || len(title) > 1000 {
			return
		}
		target := "/v2/app/tags/list?cursor=" + url.QueryEscape(cursor)
		quotedTitle := strings.NewReplacer(`\`, `\\`, `"`, `\"`).Replace(title)
		header := `</old>; rel=prev, <` + target + `>; title="` + quotedTitle + `"; rel="prev next"`
		got, ok := parseOCINextLink(header)
		if !ok || got != target {
			t.Fatalf("next link = %q, %v; want %q", got, ok, target)
		}
	})
}
