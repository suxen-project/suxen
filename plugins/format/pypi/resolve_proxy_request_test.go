package pypi_test

import (
	"context"
	"errors"
	"strings"
	"testing"

	pypi "github.com/suxen-project/suxen/plugins/format/pypi"
	"github.com/suxen-project/suxen/spi/format"
)

// storedIndexes is a StoredAssets stub backed by a fixed set of cached indexes.
type storedIndexes struct {
	paths  []string
	bodies map[string][]byte
}

func (s storedIndexes) VisitAssetPaths(_ context.Context, prefix string, visit func(string) (bool, error)) error {
	for _, path := range s.paths {
		if strings.HasPrefix(path, prefix) {
			more, err := visit(path)
			if err != nil || !more {
				return err
			}
		}
	}
	return nil
}

func (s storedIndexes) ReadAsset(_ context.Context, assetPath string) ([]byte, bool, error) {
	body, ok := s.bodies[assetPath]
	return body, ok, nil
}

// TestResolveProxyRequest checks the single proxy resolution contract.
func TestResolveProxyRequest(t *testing.T) {
	plugin := pypi.Format{}
	repository := format.Repository{Name: "proxy", Format: "pypi", Type: "proxy", Upstream: "https://upstream.example"}
	advertised := "https://files.example/packages/w/widget-1.0-py3-none-any.whl"
	stored := storedIndexes{
		paths: []string{"simple/widget/"},
		bodies: map[string][]byte{
			"simple/widget/": []byte(`<html><a href="` + advertised + `">widget-1.0-py3-none-any.whl</a></html>`),
		},
	}
	ctx := context.Background()

	t.Run("advertised file", func(t *testing.T) {
		assetPath := "files/https/files.example/packages/w/widget-1.0-py3-none-any.whl"
		resolved, err := plugin.ResolveProxyRequest(ctx, repository, assetPath, "", stored)
		if err != nil {
			t.Fatalf("ResolveProxyRequest error: %v", err)
		}
		if resolved.CachePath == assetPath || strings.Contains(resolved.CachePath, "secret") || resolved.UpstreamURL != advertised {
			t.Errorf("resolution = %+v, want opaque key and %q", resolved, advertised)
		}
	})

	t.Run("unadvertised file is rejected", func(t *testing.T) {
		assetPath := "files/https/files.example/packages/e/evil-9.9-py3-none-any.whl"
		_, err := plugin.ResolveProxyRequest(ctx, repository, assetPath, "", stored)
		var violation *format.PolicyViolation
		if !errors.As(err, &violation) || violation.Code != "pypi_unadvertised_file" {
			t.Fatalf("ResolveProxyRequest error = %v, want pypi_unadvertised_file violation", err)
		}
	})

	t.Run("index path uses default upstream", func(t *testing.T) {
		assetPath := "simple/widget/"
		resolved, err := plugin.ResolveProxyRequest(ctx, repository, assetPath, "", stored)
		if err != nil {
			t.Fatalf("ResolveProxyRequest error: %v", err)
		}
		if resolved.CachePath != assetPath {
			t.Errorf("CachePath = %q, want %q", resolved.CachePath, assetPath)
		}
		if resolved.UpstreamURL != "" {
			t.Errorf("UpstreamURL = %q, want empty (host default) for an index path", resolved.UpstreamURL)
		}
	})
}

func TestResolveProxyRequestRetainsExactCachedDistribution(t *testing.T) {
	plugin := pypi.Format{}
	repository := format.Repository{Type: "proxy", Upstream: "https://upstream.example"}
	path := "files/https/files.example/packages/a%2Fb/widget-1.0.whl"
	query := "token=first"
	// Derive the opaque key from an advertised request, then remove the
	// project index to model a retained distribution after index deletion.
	advertised := storedIndexes{paths: []string{"simple/widget/"}, bodies: map[string][]byte{
		"simple/widget/": []byte(`<a href="https://files.example/packages/a%2Fb/widget-1.0.whl?token=first">wheel</a>`),
	}}
	warm, err := plugin.ResolveProxyRequest(context.Background(), repository, path, query, advertised)
	if err != nil {
		t.Fatal(err)
	}
	stored := storedIndexes{paths: []string{warm.CachePath}, bodies: map[string][]byte{}}
	got, err := plugin.ResolveProxyRequest(context.Background(), repository, path, query, stored)
	if err != nil || got.CachePath != warm.CachePath || !got.CacheOnly || got.UpstreamURL != "" {
		t.Fatalf("retained distribution = %+v, %v", got, err)
	}
	// Indexes are mutable but downloaded distributions are immutable. Removing
	// a link from a refreshed page does not silently revoke the cached file.
	stored.paths = append(stored.paths, "simple/widget/")
	stored.bodies["simple/widget/"] = []byte(`<a href="https://files.example/other.whl">other</a>`)
	got, err = plugin.ResolveProxyRequest(context.Background(), repository, path, query, stored)
	if err != nil || !got.CacheOnly || got.CachePath != warm.CachePath {
		t.Fatalf("retained distribution after index change = %+v, %v", got, err)
	}
	for _, test := range []struct {
		name, path, query string
		paths             []string
	}{
		{"different query", path, "token=second", []string{warm.CachePath}},
		{"different escaped path", "files/https/files.example/packages/a/b/widget-1.0.whl", query, []string{warm.CachePath}},
		{"prefix is not an exact row", path, query, []string{warm.CachePath + ".other"}},
		{"cold request", path, query, nil},
	} {
		t.Run(test.name, func(t *testing.T) {
			_, err := plugin.ResolveProxyRequest(context.Background(), repository, test.path, test.query,
				storedIndexes{paths: test.paths})
			var violation *format.PolicyViolation
			if !errors.As(err, &violation) || violation.Code != "pypi_unadvertised_file" {
				t.Fatalf("error = %v, want pypi_unadvertised_file", err)
			}
		})
	}
}
