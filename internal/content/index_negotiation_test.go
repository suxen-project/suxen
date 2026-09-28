package content

import (
	"testing"

	spiformat "github.com/suxen-project/suxen/spi/format"
)

type legacyIndexRewriter struct{}

func (legacyIndexRewriter) RewriteIndex(_ spiformat.Repository, _ string, _ []byte, _ string, _ string) ([]byte, string, error) {
	return []byte("legacy"), "text/plain", nil
}

type negotiatedIndexRewriter struct{ legacyIndexRewriter }

func (negotiatedIndexRewriter) RewriteIndexForAccept(_ spiformat.Repository, _ string, _ []byte, _ string, _ string, accept string) ([]byte, string, error) {
	return []byte(accept), "application/json", nil
}

func TestRewriteIndexContentPrefersNegotiationAndPreservesLegacyFallback(t *testing.T) {
	for _, test := range []struct {
		name      string
		rewriter  spiformat.IndexRewriter
		wantBody  string
		wantVary  bool
		wantMedia string
	}{
		{"negotiated", negotiatedIndexRewriter{}, "application/json", true, "application/json"},
		{"legacy", legacyIndexRewriter{}, "legacy", false, "text/plain"},
	} {
		t.Run(test.name, func(t *testing.T) {
			body, mediaType, vary, err := rewriteIndexContent(
				test.rewriter, spiformat.Repository{}, "index", []byte("source"), "application/xml", "https://registry.example", "application/json",
			)
			if err != nil || string(body) != test.wantBody || mediaType != test.wantMedia || vary != test.wantVary {
				t.Fatalf("rewritten = %q %q vary=%v err=%v", body, mediaType, vary, err)
			}
		})
	}
}
