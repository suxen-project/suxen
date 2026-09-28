package cargo

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
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
		{"config.json", kindConfig, "", "", true},
		{"1/a", kindIndex, "a", "", true},
		{"2/ab", kindIndex, "ab", "", true},
		{"3/a/abc", kindIndex, "abc", "", true},
		{"he/ll/hello", kindIndex, "hello", "", true},
		{"se/rd/serde_json", kindIndex, "serde_json", "", true},
		{"dl/hello/0.1.0/download", kindCrate, "hello", "0.1.0", true},
		{"dl/serde_json/1.0.0-beta.1/download", kindCrate, "serde_json", "1.0.0-beta.1", true},
		{"3/b/abc", 0, "", "", false},
		{"he/lp/hello", 0, "", "", false},
		{"hello", 0, "", "", false},
		{"dl/hello/download", 0, "", "", false},
		{"dl/hello/0.1.0/hello.crate", 0, "", "", false},
		{"dl/he llo/0.1.0/download", 0, "", "", false},
		{"index/config.json", 0, "", "", false},
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
	for _, path := range []string{"config.json", "he/ll/hello", "1/a"} {
		if !f.MutableUpstreamPath(repository, path) {
			t.Fatalf("%q should be mutable", path)
		}
	}
	for _, path := range []string{"dl/hello/0.1.0/download", "unrelated.txt"} {
		if f.MutableUpstreamPath(repository, path) {
			t.Fatalf("%q should be immutable", path)
		}
	}
	attributes := f.ProjectAttributes(format.Asset{Path: "dl/hello/0.1.0/download"})
	if attributes["name"] != "hello" || attributes["version"] != "0.1.0" {
		t.Fatalf("crate attributes = %v", attributes)
	}
	if attributes := f.ProjectAttributes(format.Asset{Path: "he/ll/hello"}); attributes["name"] != "hello" {
		t.Fatalf("index attributes = %v", attributes)
	}
	if f.ProjectAttributes(format.Asset{Path: "config.json"}) != nil {
		t.Fatal("config projected attributes")
	}

	var violation *format.PolicyViolation
	if err := f.ValidateRepository(format.Repository{Type: "hosted"}); err != nil {
		t.Fatalf("hosted err = %v", err)
	}
	if err := f.ValidateRepository(format.Repository{Type: "group"}); err != nil {
		t.Fatalf("group err = %v", err)
	}
	if err := f.ValidateRepository(format.Repository{Type: "proxy", Config: map[string]any{"x": 1}}); !errors.As(err, &violation) || violation.Code != "invalid_format_config" {
		t.Fatalf("config err = %v", err)
	}
}

func TestRewriteIndex(t *testing.T) {
	var f Format
	repository := format.Repository{Type: "proxy"}
	body := []byte(`{"dl":"https://static.crates.io/crates","api":"https://crates.io","auth-required":false}`)
	rewritten, contentType, err := f.RewriteIndex(repository, "config.json", body, "application/json", "https://mirror.example/repository/crates")
	if err != nil || contentType != configContentType {
		t.Fatalf("rewrite: %v %q", err, contentType)
	}
	var config map[string]any
	if err := json.Unmarshal(rewritten, &config); err != nil {
		t.Fatal(err)
	}
	if config["dl"] != "https://mirror.example/repository/crates/dl/{crate}/{version}/download" {
		t.Fatalf("dl = %v", config["dl"])
	}
	if _, present := config["api"]; present {
		t.Fatal("api kept")
	}
	if config["auth-required"] != true {
		t.Fatal("other entries not preserved")
	}

	index := []byte(`{"name":"hello","vers":"0.1.0"}` + "\n")
	passthrough, passType, err := f.RewriteIndex(repository, "he/ll/hello", index, "text/plain", "https://x")
	if err != nil || string(passthrough) != string(index) || passType != "text/plain" {
		t.Fatalf("index passthrough = %q %q %v", passthrough, passType, err)
	}
	if _, _, err := f.RewriteIndex(repository, "config.json", []byte("not json"), "", "https://x"); err == nil {
		t.Fatal("invalid config accepted")
	}
	for _, invalid := range []string{"null", "[]", `"config"`} {
		if _, _, err := f.RewriteIndex(repository, "config.json", []byte(invalid), "", "https://x"); err == nil {
			t.Fatalf("non-object config %s accepted", invalid)
		}
	}
}

type storedConfig map[string][]byte

func (s storedConfig) VisitAssetPaths(_ context.Context, prefix string, visit func(string) (bool, error)) error {
	for path := range s {
		if strings.HasPrefix(path, prefix) {
			more, err := visit(path)
			if err != nil || !more {
				return err
			}
		}
	}
	return nil
}

func (s storedConfig) ReadAsset(_ context.Context, path string) ([]byte, bool, error) {
	content, found := s[path]
	return content, found, nil
}

func TestResolveProxyRequest(t *testing.T) {
	var f Format
	repository := format.Repository{Type: "proxy", Upstream: "https://index.crates.io"}
	ctx := context.Background()

	if got, err := f.ResolveProxyRequest(ctx, repository, "config.json", "", storedConfig{}); err != nil || got.CachePath != "config.json" || got.UpstreamURL != "" {
		t.Fatalf("config resolved: %+v %v", got, err)
	}
	if got, err := f.ResolveProxyRequest(ctx, repository, "he/ll/hello", "", storedConfig{}); err != nil || got.CachePath != "he/ll/hello" || got.UpstreamURL != "" {
		t.Fatalf("index resolved: %+v %v", got, err)
	}
	if _, err := f.ResolveProxyRequest(ctx, repository, "dl/hello/0.1.0/download", "", storedConfig{}); err == nil {
		t.Fatal("missing config accepted")
	}
	cratePath := "dl/hello/0.1.0/download"
	if got, err := f.ResolveProxyRequest(ctx, repository, cratePath, "", storedConfig{cratePath: []byte("cached crate")}); err != nil || got.CachePath != cratePath || got.UpstreamURL != "" || !got.CacheOnly {
		t.Fatalf("retained crate without config resolved: %+v %v", got, err)
	}
	if _, err := f.ResolveProxyRequest(ctx, repository, cratePath, "", storedConfig{cratePath + ".old": []byte("other crate")}); err == nil {
		t.Fatal("prefix match accepted as cached crate")
	}
	if _, err := f.ResolveProxyRequest(ctx, repository, cratePath, "", storedConfig{cratePath: []byte("cached crate"), configPath: []byte("bad json")}); err == nil {
		t.Fatal("malformed cached config accepted")
	}

	cases := map[string]string{
		`{"dl":"https://static.crates.io/crates"}`:                              "https://static.crates.io/crates/hello/0.1.0/download",
		`{"dl":"https://static.crates.io/crates/"}`:                             "https://static.crates.io/crates/hello/0.1.0/download",
		`{"dl":"https://cdn.example/{crate}/{crate}-{version}.crate"}`:          "https://cdn.example/hello/hello-0.1.0.crate",
		`{"dl":"https://cdn.example/{prefix}/{lowerprefix}/{crate}/{version}"}`: "https://cdn.example/he/ll/he/ll/hello/0.1.0",
	}
	for config, want := range cases {
		got, err := f.ResolveProxyRequest(ctx, repository, "dl/hello/0.1.0/download", "", storedConfig{"config.json": []byte(config)})
		if err != nil || got.CachePath != "dl/hello/0.1.0/download" || got.UpstreamURL != want {
			t.Fatalf("ResolveProxyRequest with %s = %+v %v, want %q", config, got, err, want)
		}
	}
	if _, err := f.ResolveProxyRequest(ctx, repository, "dl/hello/0.1.0/download", "", storedConfig{"config.json": []byte(`{"dl":"https://x/{sha256-checksum}"}`)}); err == nil {
		t.Fatal("sha256-checksum template accepted")
	}
	if got, _ := expandDownloadTemplate("https://x/{prefix}/{crate}", "Ab", "1"); got != "https://x/2/Ab" {
		t.Fatalf("two-letter prefix = %q", got)
	}
	if got, _ := expandDownloadTemplate("https://x/{prefix}/{crate}", "AbC", "1"); got != "https://x/3/A/AbC" {
		t.Fatalf("three-letter prefix = %q", got)
	}
}

func TestResolveProxyRequestBindsAdvertisedCrate(t *testing.T) {
	f := Format{}
	path := "dl/hello/0.1.0/download"
	index := indexPath("hello")
	makeIndex := func(payload string) []byte {
		return []byte(fmt.Sprintf(`{"name":"hello","vers":"0.1.0","cksum":"%x"}`+"\n", sha256.Sum256([]byte(payload))))
	}
	cache := storedConfig{configPath: []byte(`{"dl":"https://cdn.example/{crate}/{version}"}`), index: makeIndex("first")}
	resolve := func() format.ResolvedProxyRequest {
		t.Helper()
		got, err := f.ResolveProxyRequest(context.Background(), format.Repository{Type: "proxy"}, path, "", cache)
		if err != nil {
			t.Fatal(err)
		}
		return got
	}
	first := resolve()
	if first.CacheOnly || first.UpstreamURL != "https://cdn.example/hello/0.1.0" ||
		len(first.ExpectedDigests) != 1 || first.ExpectedDigests[0] != fmt.Sprintf("sha256:%x", sha256.Sum256([]byte("first"))) ||
		!isCargoProxyIdentity(first.CachePath, path+proxyIdentityMarker) {
		t.Fatalf("first = %+v", first)
	}
	cache[index] = makeIndex("second")
	second := resolve()
	if second.CachePath == first.CachePath || second.ExpectedDigests[0] == first.ExpectedDigests[0] {
		t.Fatalf("changed checksum reused identity: %+v %+v", first, second)
	}
	cache[configPath] = []byte(`{"dl":"https://other.example/{crate}/{version}"}`)
	third := resolve()
	if third.CachePath == second.CachePath || third.ExpectedDigests[0] != second.ExpectedDigests[0] {
		t.Fatalf("changed URL reused identity: %+v %+v", second, third)
	}
	cache[index] = []byte(`{"name":"hello","vers":"0.1.0","cksum":"bad"}`)
	if _, err := f.ResolveProxyRequest(context.Background(), format.Repository{Type: "proxy"}, path, "", cache); err == nil {
		t.Fatal("malformed advertised checksum accepted")
	}
	cache[index] = nil
	cache[first.CachePath] = []byte("first")
	retained := resolve()
	if !retained.CacheOnly || retained.CachePath != first.CachePath {
		t.Fatalf("retained = %+v", retained)
	}
	cache[second.CachePath] = []byte("second")
	ambiguous := resolve()
	if !ambiguous.CacheOnly || ambiguous.CachePath == first.CachePath || ambiguous.CachePath == second.CachePath {
		t.Fatalf("ambiguous retained identities = %+v", ambiguous)
	}
	cache[path] = []byte("legacy")
	ambiguous = resolve()
	if !ambiguous.CacheOnly || ambiguous.CachePath == path || ambiguous.CachePath == first.CachePath || ambiguous.CachePath == second.CachePath {
		t.Fatalf("legacy plus keyed identities resolved = %+v", ambiguous)
	}
}

func TestMergeGroupContent(t *testing.T) {
	var f Format
	repository := format.Repository{Type: "group"}
	if _, ok := f.GroupMergeSource(repository, "dl/hello/0.1.0/download"); ok {
		t.Fatal("crate files must resolve first-match")
	}
	for _, path := range []string{"config.json", "he/ll/hello"} {
		if source, ok := f.GroupMergeSource(repository, path); !ok || source != path {
			t.Fatalf("GroupMergeSource(%q) = %q %v", path, source, ok)
		}
	}

	merged, contentType, err := f.MergeGroupContent(repository, "he/ll/hello", [][]byte{
		[]byte(`{"name":"hello","vers":"0.1.0","cksum":"` + strings.Repeat("a", 64) + `"}` + "\n" + `{"name":"hello","vers":"0.2.0","cksum":"` + strings.Repeat("b", 64) + `"}` + "\n"),
		[]byte(`{"name":"hello","vers":"0.2.0","cksum":"` + strings.Repeat("d", 64) + `"}` + "\n" + `{"name":"hello","vers":"0.3.0","cksum":"` + strings.Repeat("c", 64) + `"}` + "\nnot json\n"),
	})
	if err != nil || contentType != indexContentType {
		t.Fatalf("merge: %v %q", err, contentType)
	}
	lines := strings.Split(strings.TrimSpace(string(merged)), "\n")
	if len(lines) != 3 || !strings.Contains(lines[1], `"cksum":"`+strings.Repeat("b", 64)+`"`) || !strings.Contains(lines[2], "0.3.0") {
		t.Fatalf("merged index = %q", merged)
	}
	merged, _, err = f.MergeGroupContent(repository, "he/ll/hello", [][]byte{
		[]byte(`{"name":"hello","vers":"1.0.0+first","cksum":"` + strings.Repeat("a", 64) + `"}` + "\n"),
		[]byte(`{"name":"hello","vers":"1.0.0+second","cksum":"` + strings.Repeat("b", 64) + `"}` + "\n"),
	})
	if err != nil || strings.Count(string(merged), "\n") != 1 || !strings.Contains(string(merged), `"cksum":"`+strings.Repeat("a", 64)+`"`) {
		t.Fatalf("build metadata merge = %q %v", merged, err)
	}

	config, contentType, err := f.MergeGroupContent(repository, "config.json", [][]byte{[]byte("junk"), []byte("null"), []byte("[]"), []byte(`{"dl":"x"}`)})
	if err != nil || contentType != configContentType || string(config) != `{"dl":"x"}` {
		t.Fatalf("merged config = %q %q %v", config, contentType, err)
	}
	if _, _, err := f.MergeGroupContent(repository, "config.json", [][]byte{[]byte("junk")}); err == nil {
		t.Fatal("config without a valid member succeeded")
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
		{hosted, "PUT", "api/v1/crates/new", "write", true},
		{hosted, "GET", "config.json", "read", true},
		{hosted, "GET", "he/ll/hello", "read", true},
		{hosted, "HEAD", "config.json", "read", true},
		{hosted, "GET", "dl/hello/1.0.0/download", "", false}, // downloads ride the pipeline
		{hosted, "PUT", "config.json", "", false},
		{format.Repository{Type: "proxy"}, "PUT", "api/v1/crates/new", "", false},
		{format.Repository{Type: "group"}, "GET", "config.json", "", false},
	}
	for _, tc := range cases {
		action, ok := f.WireAction(tc.repo, tc.method, tc.path, nil)
		if action != tc.action || ok != tc.ok {
			t.Fatalf("WireAction(%s %s %q) = (%q,%v), want (%q,%v)", tc.repo.Type, tc.method, tc.path, action, ok, tc.action, tc.ok)
		}
	}
}

func TestBuildIndexEntryMapsRenamedDep(t *testing.T) {
	rename := "renamed"
	meta := publishMeta{
		Name: "widget", Vers: "1.0.0",
		Deps: []publishDep{
			{Name: "serde", VersionReq: "^1.0", Kind: "", DefaultFeatures: true},
			{Name: "real-crate", VersionReq: "^2.0", Kind: "dev", ExplicitNameInToml: &rename},
		},
	}
	entry := buildIndexEntry(meta, "abc123")
	if entry.Cksum != "abc123" || entry.Yanked || entry.Features == nil {
		t.Fatalf("entry = %+v", entry)
	}
	if entry.Deps[0].Req != "^1.0" || entry.Deps[0].Kind != "normal" || entry.Deps[0].Features == nil || entry.Deps[0].Package != nil {
		t.Fatalf("dep0 = %+v", entry.Deps[0])
	}
	if entry.Deps[1].Name != "renamed" || entry.Deps[1].Package == nil || *entry.Deps[1].Package != "real-crate" || entry.Deps[1].Kind != "dev" {
		t.Fatalf("renamed dep = %+v", entry.Deps[1])
	}
}
