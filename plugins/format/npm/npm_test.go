package npm

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"github.com/suxen-project/suxen/spi/format"
)

func TestParsePath(t *testing.T) {
	cases := []struct {
		path    string
		kind    pathKind
		name    string
		version string
		ok      bool
	}{
		{"hello", kindPackument, "hello", "", true},
		{"@acme/hello", kindPackument, "@acme/hello", "", true},
		{"hello/-/hello-1.0.0.tgz", kindTarball, "hello", "1.0.0", true},
		{"@acme/hello/-/hello-1.0.0-beta.2.tgz", kindTarball, "@acme/hello", "1.0.0-beta.2", true},
		{"left-pad/-/left-pad-1.3.0.tgz", kindTarball, "left-pad", "1.3.0", true},
		{"hello/-/other-1.0.0.tgz", 0, "", "", false},
		{"hello/-/hello-.tgz", 0, "", "", false},
		{"hello/dist/index.js", 0, "", "", false},
		{"@acme", 0, "", "", false},
		{"Hello", kindPackument, "Hello", "", true},
		{"../etc", 0, "", "", false},
		{"-/v1/search", 0, "", "", false},
	}
	for _, tc := range cases {
		info, ok := parsePath(tc.path)
		if ok != tc.ok {
			t.Fatalf("parsePath(%q) ok = %v, want %v", tc.path, ok, tc.ok)
		}
		if info.kind != tc.kind || info.name != tc.name || info.version != tc.version {
			t.Fatalf("parsePath(%q) = %+v", tc.path, info)
		}
	}
}

func TestPoliciesAndAttributes(t *testing.T) {
	var f Format
	repository := format.Repository{Type: "proxy"}
	if !f.MutableUpstreamPath(repository, "hello") || !f.MutableUpstreamPath(repository, "@acme/hello") {
		t.Fatal("packuments should be mutable")
	}
	if f.MutableUpstreamPath(repository, "hello/-/hello-1.0.0.tgz") || f.MutableUpstreamPath(repository, "-/v1/search") {
		t.Fatal("tarballs and unknown paths should be immutable")
	}
	attributes := f.ProjectAttributes(format.Asset{Path: "@acme/hello/-/hello-1.0.0.tgz"})
	if attributes["name"] != "@acme/hello" || attributes["version"] != "1.0.0" {
		t.Fatalf("attributes = %v", attributes)
	}
	if attributes := f.ProjectAttributes(format.Asset{Path: "hello"}); attributes["name"] != "hello" || attributes["version"] != nil {
		t.Fatalf("packument attributes = %v", attributes)
	}

	var violation *format.PolicyViolation
	if err := f.ValidateRepository(format.Repository{Type: "hosted"}); err != nil {
		t.Fatalf("hosted err = %v", err)
	}
	if err := f.ValidateRepository(format.Repository{Type: "proxy", Config: map[string]any{"x": 1}}); !errors.As(err, &violation) || violation.Code != "invalid_format_config" {
		t.Fatalf("config err = %v", err)
	}
	if err := f.ValidateRepository(format.Repository{Type: "group"}); err != nil {
		t.Fatal(err)
	}
}

func TestHostedWireAction(t *testing.T) {
	var f Format
	hosted := format.Repository{Type: "hosted"}
	cases := []struct {
		repo   format.Repository
		method string
		path   string
		action string
		ok     bool
	}{
		{hosted, "PUT", "widget", "write", true},
		{hosted, "GET", "widget", "read", true},
		{hosted, "GET", "@acme/widget", "read", true},
		{hosted, "HEAD", "widget", "read", true},
		{hosted, "DELETE", "widget", "", false},
		{hosted, "GET", "widget/-/widget-1.0.0.tgz", "", false},        // tarballs ride the pipeline
		{hosted, "GET", "widget/-/metadata/1.0.0.json", "", false},     // metadata assets ride the pipeline
		{format.Repository{Type: "proxy"}, "PUT", "widget", "", false}, // proxy is never claimed
		{format.Repository{Type: "group"}, "GET", "widget", "", false},
	}
	for _, tc := range cases {
		action, ok := f.WireAction(tc.repo, tc.method, tc.path, nil)
		if action != tc.action || ok != tc.ok {
			t.Fatalf("WireAction(%s %s %s) = (%q,%v), want (%q,%v)", tc.repo.Type, tc.method, tc.path, action, ok, tc.action, tc.ok)
		}
	}
}

func TestVersionFromMetadataPath(t *testing.T) {
	cases := []struct {
		name    string
		path    string
		version string
		ok      bool
	}{
		{"widget", "widget/-/metadata/1.0.0.json", "1.0.0", true},
		{"@acme/widget", "@acme/widget/-/metadata/2.0.0.json", "2.0.0", true},
		{"widget", "widget/-/widget-1.0.0.tgz", "", false},
		{"widget", "widget/-/metadata/nested/1.0.0.json", "", false},
		{"widget", "widget/-/metadata/.json", "", false},
	}
	for _, tc := range cases {
		version, ok := versionFromMetadataPath(tc.name, tc.path)
		if version != tc.version || ok != tc.ok {
			t.Fatalf("versionFromMetadataPath(%q,%q) = (%q,%v), want (%q,%v)", tc.name, tc.path, version, ok, tc.version, tc.ok)
		}
	}
}

const packument = `{"name":"@acme/hello","dist-tags":{"latest":"1.1.0"},"versions":{
"1.0.0":{"name":"@acme/hello","version":"1.0.0","dist":{"tarball":"https://cdn.example/@acme/hello/-/hello-1.0.0.tgz","integrity":"sha512-RTrxp0o8VHVGL/kkZ+65AsWdHzC/PWfGbegvbmRUIQveapELafl+SYPEAmblYO/c0Qg8/H6yer5T6GfGHEua1A==","shasum":"0f5e573fa2cadb43bbf3a3bce1063ed77eb69be2"}},
"1.1.0":{"name":"@acme/hello","version":"1.1.0","dist":{"tarball":"https://registry.example/@acme%2fhello/-/hello-1.1.0.tgz","integrity":"sha512-LefmkNVQHiFMZ/p83Uz2FbMa3DYouHX/Aq86EqZpRMhbeZidjA5XwxzlyPtVb6RVsX6U2xoyWu1iagg6cb5p/w=="}}},
"time":{"1.0.0":"2024-01-01T00:00:00.000Z"}}`

func TestRewriteIndex(t *testing.T) {
	var f Format
	repository := format.Repository{Type: "proxy"}
	rewritten, contentType, err := f.RewriteIndex(repository, "@acme/hello", []byte(packument), "application/json", "https://mirror.example/repository/npm")
	if err != nil || contentType != packumentContentType {
		t.Fatalf("rewrite: %v %q", err, contentType)
	}
	var document struct {
		Versions map[string]struct {
			Dist map[string]string `json:"dist"`
		} `json:"versions"`
		Time map[string]string `json:"time"`
	}
	if err := json.Unmarshal(rewritten, &document); err != nil {
		t.Fatal(err)
	}
	if got := document.Versions["1.0.0"].Dist["tarball"]; got != "https://mirror.example/repository/npm/@acme/hello/-/hello-1.0.0.tgz" {
		t.Fatalf("1.0.0 tarball = %q", got)
	}
	if got := document.Versions["1.1.0"].Dist["tarball"]; got != "https://mirror.example/repository/npm/@acme/hello/-/hello-1.1.0.tgz" {
		t.Fatalf("1.1.0 tarball = %q", got)
	}
	if document.Versions["1.0.0"].Dist["integrity"] != "sha512-RTrxp0o8VHVGL/kkZ+65AsWdHzC/PWfGbegvbmRUIQveapELafl+SYPEAmblYO/c0Qg8/H6yer5T6GfGHEua1A==" || document.Versions["1.0.0"].Dist["shasum"] != "0f5e573fa2cadb43bbf3a3bce1063ed77eb69be2" {
		t.Fatal("integrity fields were touched")
	}
	if document.Time["1.0.0"] == "" {
		t.Fatal("other fields dropped")
	}

	tarball := []byte("gzip bytes")
	passthrough, passType, err := f.RewriteIndex(repository, "@acme/hello/-/hello-1.0.0.tgz", tarball, "application/octet-stream", "https://x")
	if err != nil || string(passthrough) != string(tarball) || passType != "application/octet-stream" {
		t.Fatal("tarball was rewritten")
	}
	if _, _, err := f.RewriteIndex(repository, "hello", []byte("<html>"), "", "https://x"); err == nil {
		t.Fatal("invalid packument accepted")
	}
}

type stored map[string][]byte

func (s stored) VisitAssetPaths(context.Context, string, func(string) (bool, error)) error {
	return nil
}

func (s stored) ReadAsset(_ context.Context, path string) ([]byte, bool, error) {
	content, found := s[path]
	return content, found, nil
}

func TestResolveProxyRequest(t *testing.T) {
	var f Format
	repository := format.Repository{Type: "proxy", Upstream: "https://registry.example"}
	ctx := context.Background()
	cache := stored{"@acme/hello": []byte(packument)}

	got, err := f.ResolveProxyRequest(ctx, repository, "@acme/hello/-/hello-1.0.0.tgz", "", cache)
	if err != nil || got.UpstreamURL != "https://cdn.example/@acme/hello/-/hello-1.0.0.tgz" ||
		!strings.HasPrefix(got.CachePath, "@acme/hello/-/hello-1.0.0.tgz"+proxyIdentityMarker) ||
		len(got.CachePath) != len("@acme/hello/-/hello-1.0.0.tgz"+proxyIdentityMarker)+64 ||
		len(got.ExpectedDigests) != 1 || !strings.HasPrefix(got.ExpectedDigests[0], "sha512:") {
		t.Fatalf("resolved = %+v %v", got, err)
	}
	got, err = f.ResolveProxyRequest(ctx, repository, "@acme/hello/-/hello-1.1.0.tgz", "", cache)
	if err != nil || got.UpstreamURL != "https://registry.example/@acme%2fhello/-/hello-1.1.0.tgz" ||
		!strings.HasPrefix(got.CachePath, "@acme/hello/-/hello-1.1.0.tgz"+proxyIdentityMarker) ||
		len(got.CachePath) != len("@acme/hello/-/hello-1.1.0.tgz"+proxyIdentityMarker)+64 {
		t.Fatalf("encoded-scope resolved = %+v %v", got, err)
	}
	if got, err := f.ResolveProxyRequest(ctx, repository, "@acme/hello/-/hello-9.9.9.tgz", "", cache); got.UpstreamURL != "" || err != nil {
		t.Fatalf("unknown version resolved: %+v %v", got, err)
	}
	if got, err := f.ResolveProxyRequest(ctx, repository, "hello/-/hello-1.0.0.tgz", "", cache); got.UpstreamURL != "" || err != nil {
		t.Fatalf("uncached packument resolved: %+v %v", got, err)
	}
	if got, err := f.ResolveProxyRequest(ctx, repository, "@acme/hello", "", cache); got.UpstreamURL != "" || err != nil {
		t.Fatalf("packument path resolved: %+v %v", got, err)
	}
}

func TestMergeGroupContent(t *testing.T) {
	var f Format
	repository := format.Repository{Type: "group"}
	if _, ok := f.GroupMergeSource(repository, "hello/-/hello-1.0.0.tgz"); ok {
		t.Fatal("tarballs must not be merged as indexes")
	}
	if source, ok := f.GroupMergeSource(repository, "@acme/hello"); !ok || source != "@acme/hello" {
		t.Fatalf("GroupMergeSource = %q %v", source, ok)
	}

	first := `{"name":"hello","description":"first","dist-tags":{"latest":"1.0.0","next":"2.0.0-rc.1"},"versions":{"1.0.0":{"dist":{"tarball":"a"}},"2.0.0-rc.1":{"dist":{"tarball":"rc"}}},"time":{"1.0.0":"t1"}}`
	second := `{"name":"hello","description":"second","dist-tags":{"latest":"1.2.0"},"versions":{"1.0.0":{"dist":{"tarball":"b"}},"1.2.0":{"dist":{"tarball":"c"}}},"time":{"1.2.0":"t2"},"homepage":"https://h"}`
	merged, contentType, err := f.MergeGroupContent(repository, "hello", [][]byte{[]byte(first), []byte(second)})
	if err != nil || contentType != packumentContentType {
		t.Fatalf("merge: %v %q", err, contentType)
	}
	var document map[string]any
	if err := json.Unmarshal(merged, &document); err != nil {
		t.Fatal(err)
	}
	versions := document["versions"].(map[string]any)
	if len(versions) != 3 || versions["1.0.0"].(map[string]any)["dist"].(map[string]any)["tarball"] != "a" {
		t.Fatalf("versions = %v", versions)
	}
	tags := document["dist-tags"].(map[string]any)
	if tags["latest"] != "1.2.0" || tags["next"] != "2.0.0-rc.1" {
		t.Fatalf("dist-tags = %v", tags)
	}
	if document["description"] != "first" || document["homepage"] != "https://h" {
		t.Fatalf("metadata = %v %v", document["description"], document["homepage"])
	}
	if document["time"].(map[string]any)["1.2.0"] != "t2" {
		t.Fatalf("time = %v", document["time"])
	}
	if _, _, err := f.MergeGroupContent(repository, "hello", [][]byte{[]byte("junk")}); err == nil {
		t.Fatal("merge without a valid member succeeded")
	}
	if _, _, err := f.MergeGroupContent(repository, "hello", [][]byte{[]byte(first), []byte("junk"), []byte(second)}); err == nil {
		t.Fatal("merge with malformed member succeeded")
	}

	if got := highestRelease(map[string]any{"1.0.0-beta.1": nil, "1.0.0-beta.2": nil}); got != "1.0.0-beta.2" {
		t.Fatalf("prerelease-only latest = %q", got)
	}
	if got := highestRelease(map[string]any{"1.10.0": nil, "1.9.0": nil, "2.0.0-rc.1": nil, "junk": nil}); got != "1.10.0" {
		t.Fatalf("latest = %q", got)
	}
}

func TestMergeGroupContentRejectsNullMember(t *testing.T) {
	f := Format{}
	repository := format.Repository{Type: "group"}
	valid := []byte(`{"name":"hello","versions":{"1.0.0":{}}}`)
	if _, _, err := f.MergeGroupContent(repository, "hello", [][]byte{[]byte("null"), valid}); err == nil {
		t.Fatal("null member was accepted")
	}
	if _, _, err := f.MergeGroupContent(repository, "hello", [][]byte{[]byte("null")}); err == nil {
		t.Fatal("null-only group produced a packument")
	}
}
