package oci

import (
	"net/http"
	"testing"

	spiformat "github.com/suxen-project/suxen/spi/format"
)

func TestFormatWireAction(t *testing.T) {
	var f Format
	repository := spiformat.Repository{Format: "oci", Type: "hosted"}
	cases := []struct {
		method  string
		path    string
		action  string
		claimed bool
	}{
		{http.MethodGet, "v2/", "read", true},
		{http.MethodGet, "v2", "read", true},
		{http.MethodHead, "v2/acme/widget/manifests/latest", "read", true},
		{http.MethodPost, "v2/acme/widget/blobs/uploads/", "write", true},
		{http.MethodPatch, "v2/acme/widget/blobs/uploads/abc", "write", true},
		{http.MethodPut, "v2/acme/widget/manifests/latest", "write", true},
		{http.MethodDelete, "v2/acme/widget/manifests/latest", "delete", true},
		{http.MethodGet, "v2/token", "", true},
		{http.MethodPost, "v2/token/", "", true},
		// Non-v2 paths are still claimed so the format can answer
		// format_mismatch after the privilege check, as the host did before.
		{http.MethodGet, "acme/widget.tar", "read", true},
		// Unsupported methods are dispatched unchecked so the 405 keeps
		// preceding authentication.
		{http.MethodOptions, "v2/", "", true},
		{"PROPFIND", "v2/acme/widget/tags/list", "", true},
	}
	for _, tc := range cases {
		action, claimed := f.WireAction(repository, tc.method, tc.path, nil)
		if action != tc.action || claimed != tc.claimed {
			t.Fatalf("WireAction(%s %s) = %q %v, want %q %v", tc.method, tc.path, action, claimed, tc.action, tc.claimed)
		}
	}
}

func TestSplitOCIRequestPath(t *testing.T) {
	cases := []struct {
		path      string
		ociRoute  bool
		remainder string
	}{
		{"v2", true, ""},
		{"v2/", true, "/"},
		{"/v2/acme/widget/blobs/sha256:abc", true, "/acme/widget/blobs/sha256:abc"},
		{"v2x/acme", false, ""},
		{"acme/v2/widget", false, ""},
	}
	for _, tc := range cases {
		ociRoute, remainder := splitOCIRequestPath(tc.path)
		if ociRoute != tc.ociRoute || remainder != tc.remainder {
			t.Fatalf("splitOCIRequestPath(%q) = %v %q", tc.path, ociRoute, remainder)
		}
	}
}
