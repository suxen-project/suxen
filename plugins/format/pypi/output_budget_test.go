package pypi

import (
	"bytes"
	"errors"
	"fmt"
	"html"
	"net/url"
	"strings"
	"testing"

	"github.com/suxen-project/suxen/spi/format"
)

func TestPyPIRewrittenLinkBudgetMatchesURL(t *testing.T) {
	base, err := url.Parse("https://index.example/simple/widget/")
	if err != nil {
		t.Fatal(err)
	}
	for _, link := range []string{"../../packages/widget-1.0.tar.gz?token=a&b=c#sha256=abc", "https://files.example/a%2Fb/%25/é.whl", "https://files.example/widget.whl"} {
		budget := newOutputBudget()
		rewritten, err := rewriteLinkWithBudget(link, base, "https://mirror.example/repository/proxy", kindProject, &budget)
		if err != nil {
			t.Fatal(err)
		}
		if got, want := renderedIndexLimit-budget.remaining, int64(len(rewritten)); got != want {
			t.Fatalf("rewritten URL budget = %d, emitted = %d: %q", got, want, rewritten)
		}
	}
}

func TestPyPIHTMLBudgetMatchesEmittedAnchors(t *testing.T) {
	for _, entry := range []map[string]any{
		{"filename": `a<&'".whl`, "url": `https://files.example/a?x=1&y=2`, "hashes": map[string]any{"sha256": `a&b`}, "gpg-sig": false},
		{"filename": "widget-1.0.tar.gz", "url": "https://files.example/widget-1.0.tar.gz", "requires-python": `>=3.9<&`, "yanked": `bad "release"`, "core-metadata": map[string]any{"sha256": "abc"}, "provenance": "https://attest.example/a?b=1&c=2"},
		{"name": `one<&'"`, "url": `https://index.example/one?x=1&y=2`},
	} {
		kind := kindProject
		if _, root := entry["name"]; root {
			kind = kindRoot
		}
		budget := newOutputBudget()
		if err := htmlAnchorSize(entry, kind, &budget); err != nil {
			t.Fatal(err)
		}
		if got, want := renderedIndexLimit-budget.remaining, int64(len(renderAnchor(entry, kind))); got != want {
			t.Fatalf("anchor budget = %d, emitted = %d: %v", got, want, entry)
		}
	}
	for _, value := range []string{`a<&'"`, "é\u2028", strings.Repeat("&", 200)} {
		budget := newOutputBudget()
		if err := htmlStringSize(value, &budget); err != nil {
			t.Fatal(err)
		}
		if got, want := renderedIndexLimit-budget.remaining, int64(len(html.EscapeString(value))); got != want {
			t.Fatalf("escape budget = %d, emitted = %d", got, want)
		}
	}
}

func TestPyPIRewriteRefusesRepeatedHostExpansion(t *testing.T) {
	const count = 17000
	var project bytes.Buffer
	project.WriteString(`{"name":"widget","files":[`)
	for i := 0; i < count; i++ {
		if i != 0 {
			project.WriteByte(',')
		}
		fmt.Fprintf(&project, `{"filename":"widget-1.0.%d.tar.gz","url":"https://files.example/widget-1.0.%d.tar.gz","hashes":{}}`, i, i)
	}
	project.WriteString(`]}`)
	repository := format.Repository{Upstream: "https://index.example"}
	repositoryURL := "https://" + strings.Repeat("h", 8192) + ".example/repository/proxy"
	for _, asJSON := range []bool{true, false} {
		if _, _, err := rewriteIndex(repository, "simple/widget/", project.Bytes(), jsonContentType, repositoryURL, &asJSON); !errors.Is(err, errIndexBudget) {
			t.Fatalf("project asJSON=%v: expected output budget, got %v", asJSON, err)
		}
	}
	var root bytes.Buffer
	root.WriteString(`{"projects":[`)
	for i := 0; i < count; i++ {
		if i != 0 {
			root.WriteByte(',')
		}
		fmt.Fprintf(&root, `{"name":"widget%d"}`, i)
	}
	root.WriteString(`]}`)
	for _, asJSON := range []bool{true, false} {
		if _, _, err := rewriteRootIndex(repository, root.Bytes(), repositoryURL, &asJSON); !errors.Is(err, errIndexBudget) {
			t.Fatalf("root asJSON=%v: expected output budget, got %v", asJSON, err)
		}
	}
}

func TestPyPINormalizationRefusesRepeatedLargeBase(t *testing.T) {
	const count = 140
	base := "https://files.example/" + strings.Repeat("x", 1<<20) + "/"
	var page bytes.Buffer
	fmt.Fprintf(&page, `<html><head><base href=%q></head><body>`, base)
	for i := 0; i < count; i++ {
		fmt.Fprintf(&page, `<a href="widget-%d.tar.gz">widget-%d.tar.gz</a>`, i, i)
	}
	page.WriteString(`</body></html>`)
	if _, err := (Format{}).NormalizeGroupSource(format.Repository{Upstream: "https://index.example"}, "simple/widget/", page.Bytes()); !errors.Is(err, errIndexBudget) {
		t.Fatalf("project normalization: expected output budget, got %v", err)
	}
	if _, err := (Format{}).NormalizeGroupSource(format.Repository{Upstream: "https://index.example"}, "simple/", page.Bytes()); !errors.Is(err, errIndexBudget) {
		t.Fatalf("root normalization: expected output budget, got %v", err)
	}
}
