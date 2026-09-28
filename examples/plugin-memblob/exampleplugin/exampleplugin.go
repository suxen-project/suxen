// Package exampleplugin is a compile-time contract consumer for the public
// format and control-plane API SPIs. It lives in the separate example module so
// make check-example catches source-incompatible SPI changes for out-of-tree
// plugins.
package exampleplugin

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"net/http"
	"strings"

	"github.com/suxen-project/suxen/spi/api"
	"github.com/suxen-project/suxen/spi/format"
)

type textFormat struct{}

func (textFormat) Name() string { return "exampletext" }

func (textFormat) MutableUpstreamPath(_ format.Repository, assetPath string) bool {
	return assetPath == "index.txt"
}

// ResolveProxyRequest keeps query-selected responses apart without storing
// raw query bytes in metadata. The host still validates the outbound target.
func (textFormat) ResolveProxyRequest(
	_ context.Context, repository format.Repository, assetPath, rawQuery string, _ format.StoredAssets,
) (format.ResolvedProxyRequest, error) {
	if rawQuery == "deny=1" {
		return format.ResolvedProxyRequest{}, &format.PolicyViolation{Code: "exampletext_denied", Message: "request denied"}
	}
	resolved := format.ResolvedProxyRequest{CachePath: assetPath}
	if rawQuery == "" {
		return resolved, nil
	}
	digest := sha256.Sum256([]byte(rawQuery))
	resolved.CachePath += ".query-" + hex.EncodeToString(digest[:])
	resolved.UpstreamURL = strings.TrimSuffix(repository.Upstream, "/") + "/" + assetPath + "?" + rawQuery
	return resolved, nil
}

func (textFormat) ValidateUpload(_ format.Repository, assetPath string) error {
	if !strings.HasSuffix(assetPath, ".txt") {
		return &format.PolicyViolation{
			Code:    "exampletext_path",
			Message: "exampletext assets must end in .txt",
		}
	}
	return nil
}

func init() {
	format.Register(textFormat{})
	api.Register("exampleplugin", api.Route{
		Path:    "status",
		Methods: []string{http.MethodGet},
		Handle: func(ctx api.Context) {
			ctx.WriteJSON(http.StatusOK, map[string]string{"status": "ready"})
		},
		Operations: map[string]api.Operation{
			http.MethodGet: {
				"summary": "Read example plugin status",
				"responses": map[string]any{
					"200": map[string]any{"description": "Plugin status", "content": map[string]any{
						"application/json": map[string]any{"schema": map[string]any{
							"type": "object", "properties": map[string]any{"status": map[string]any{"type": "string"}},
						}},
					}},
				},
			},
		},
	})
}

var _ format.Format = textFormat{}
var _ format.UploadPolicy = textFormat{}
var _ format.ProxyRequestResolver = textFormat{}
var _ format.ProxyPolicy = textFormat{}
