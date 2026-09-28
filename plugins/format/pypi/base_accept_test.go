package pypi

import (
	"strings"
	"testing"

	"github.com/suxen-project/suxen/spi/format"
)

func TestPyPIAcceptQualityZero(t *testing.T) {
	if acceptWantsJSON("text/html, application/vnd.pypi.simple.v1+json;q=0") {
		t.Fatal("selected explicitly unacceptable JSON")
	}
	if acceptWantsJSON("application/vnd.pypi.simple.v1+html, application/example+json") {
		t.Fatal("selected JSON for an unrelated media type")
	}
}

func TestPyPIHTMLBaseChangesRelativeFileURL(t *testing.T) {
	body := []byte(`<html><head><base href="https://cdn.example/packages/"></head><body><a href="widget-1.0.whl#sha256=abc">widget-1.0.whl</a></body></html>`)
	output, _, err := (Format{}).RewriteIndex(format.Repository{Type: "proxy", Upstream: "https://index.example"}, "simple/widget/", body, "text/html", "https://mirror.example/repository/proxy")
	if err != nil {
		t.Fatal(err)
	}
	want := "https://mirror.example/repository/proxy/files/https/cdn.example/packages/widget-1.0.whl#sha256=abc"
	if !strings.Contains(string(output), want) {
		t.Fatalf("base URL ignored: %s", output)
	}
}

func TestPyPINegotiationCases(t *testing.T) {
	cases := []struct {
		header string
		json   bool
	}{
		{"", false},
		{"*/*", false},
		{"application/vnd.pypi.simple.v1+json", true},
		{"application/vnd.pypi.simple.latest+json", true},
		{"application/vnd.pypi.simple.v1+json;q=0.8, text/html;q=0.2", true},
		{"application/vnd.pypi.simple.v1+json;q=0, */*", false},
		{"application/vnd.pypi.simple.v1+json;q=0.1, text/html;q=0.9", false},
		{"application/vnd.pypi.simple.v1+html, application/example+json", false},
		{"application/example+json", false},
		{"application/example+json, application/vnd.pypi.simple.v1+json;q=0", false},
		{"application/vnd.pypi.simple.v1+json;q=NaN, text/html", false},
		{"application/*;q=0, */*", false},
	}
	for _, tc := range cases {
		if got := acceptWantsJSON(tc.header); got != tc.json {
			t.Errorf("Accept %q: json=%v, want %v", tc.header, got, tc.json)
		}
	}
}

func TestPyPIHTMLBaseFirstHrefAndSelfClosing(t *testing.T) {
	repo := format.Repository{Type: "proxy", Upstream: "https://index.example"}
	for _, tc := range []struct{ name, body, want string }{
		{"self closing", `<html><head><base href="https://cdn.example/packages/"/></head><body><a href="widget-1.0.whl">widget-1.0.whl</a></body></html>`, `/files/https/cdn.example/packages/widget-1.0.whl`},
		{"first wins", `<html><head><base href="https://first.example/files/"><base href="https://second.example/files/"></head><body><a href="widget-1.0.whl">widget-1.0.whl</a></body></html>`, `/files/https/first.example/files/widget-1.0.whl`},
		{"invalid first falls back", `<html><head><base href="javascript:alert(1)"><base href="https://second.example/files/"></head><body><a href="widget-1.0.whl">widget-1.0.whl</a></body></html>`, `/files/https/index.example/simple/widget/widget-1.0.whl`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			output, _, err := (Format{}).RewriteIndex(repo, "simple/widget/", []byte(tc.body), "text/html", "https://mirror.example/repository/proxy")
			if err != nil {
				t.Fatal(err)
			}
			if !strings.Contains(string(output), tc.want) {
				t.Fatalf("got %s, want %s", output, tc.want)
			}
		})
	}
	root := []byte(`<html><head><base href="https://cdn.example/projects/"/></head><body><a href="widget/">widget</a></body></html>`)
	normalized, err := (Format{}).NormalizeGroupSource(repo, "simple/", root)
	if err != nil || !strings.Contains(string(normalized), `href="https://cdn.example/projects/widget/"`) {
		t.Fatalf("root base ignored: %s %v", normalized, err)
	}
}
