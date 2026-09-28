package exampleplugin

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"testing"

	"github.com/suxen-project/suxen/spi/api"
	"github.com/suxen-project/suxen/spi/format"
)

func TestExternalFormatCanInspectPublicationCategories(t *testing.T) {
	for _, category := range []error{format.ErrConflict, format.ErrPolicyRejected, format.ErrUploadLimit} {
		if !errors.Is(fmt.Errorf("host publication: %w", category), category) {
			t.Fatalf("wrapped public category %v was lost", category)
		}
	}
}

func TestExternalRegistrations(t *testing.T) {
	registeredFormat, found := format.Lookup("exampletext")
	if !found || registeredFormat.Name() != "exampletext" {
		t.Fatalf("format lookup = %v, %v", registeredFormat, found)
	}

	pluginID, route, _, found := api.Match([]string{"plugins", "exampleplugin", "status"})
	if !found || pluginID != "exampleplugin" || !route.Allows(http.MethodGet) {
		t.Fatalf("API match = %q, %+v, %v", pluginID, route, found)
	}
}

func TestExternalProxyResolverKeepsQueryOutOfCacheKey(t *testing.T) {
	resolver := textFormat{}
	repository := format.Repository{Type: "proxy", Upstream: "https://upstream.example"}
	resolved, err := resolver.ResolveProxyRequest(context.Background(), repository, "file.txt", "token=secret", nil)
	if err != nil || resolved.CachePath == "file.txt" || strings.Contains(resolved.CachePath, "secret") ||
		resolved.UpstreamURL != "https://upstream.example/file.txt?token=secret" {
		t.Fatalf("resolved request = %+v, %v", resolved, err)
	}
}

func TestExternalRetentionGrouping(t *testing.T) {
	plugin := textFormat{}
	repository := format.Repository{Type: "hosted"}
	for _, version := range []string{"1.0", "2.0"} {
		path := "pkg/widget/" + version + "/artifact.txt"
		if key, ok := plugin.RetentionGroupKey(repository, format.Asset{Path: path}); !ok || key != "pkg/widget" {
			t.Fatalf("group %s = %q, %v", path, key, ok)
		}
		companions := plugin.CompanionPaths(repository, path)
		if len(companions) != 1 || companions[0] != "pkg/widget/"+version+"/meta.json" {
			t.Fatalf("companions for %s = %v", path, companions)
		}
	}
	if _, ok := plugin.RetentionGroupKey(repository, format.Asset{Path: "pkg/widget/1.0/meta.json"}); ok {
		t.Fatal("metadata must use the host fallback, not count as an artifact release")
	}
	if companions := plugin.CompanionPaths(repository, "pkg/widget/1.0/meta.json"); len(companions) != 0 {
		t.Fatalf("metadata declares companions: %v", companions)
	}
}
