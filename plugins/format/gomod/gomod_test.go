package gomod

import (
	"context"
	"errors"
	"reflect"
	"strings"
	"testing"

	"github.com/suxen-project/suxen/spi/format"
)

func TestParsePath(t *testing.T) {
	cases := []struct {
		path    string
		module  string
		version string
		kind    pathKind
		ok      bool
	}{
		{"example.com/hello/@v/list", "example.com/hello", "", kindList, true},
		{"example.com/hello/@latest", "example.com/hello", "", kindLatest, true},
		{"example.com/hello/@v/v1.2.3.info", "example.com/hello", "v1.2.3", kindInfo, true},
		{"example.com/hello/@v/v1.2.3.mod", "example.com/hello", "v1.2.3", kindMod, true},
		{"example.com/hello/@v/v1.2.3.zip", "example.com/hello", "v1.2.3", kindZip, true},
		{"github.com/!burnt!sushi/toml/@v/v1.0.0-!r!c1.info", "github.com/BurntSushi/toml", "v1.0.0-RC1", kindInfo, true},
		{"example.com/hello/@v/master.info", "example.com/hello", "master", kindInfo, true},
		{"example.com/hello/@v/v1.2.3.tar.gz", "", "", 0, false},
		{"example.com/hello/@v/", "", "", 0, false},
		{"example.com/hello/@v/.info", "", "", 0, false},
		{"example.com/hello/@v/sub/v1.0.0.info", "", "", 0, false},
		{"example.com/Hello/@v/list", "", "", 0, false},
		{"sumdb/sum.golang.org/supported", "", "", 0, false},
		{"example.com/hello/v1.0.0.zip", "", "", 0, false},
	}
	for _, tc := range cases {
		info, ok := parsePath(tc.path)
		if ok != tc.ok {
			t.Fatalf("parsePath(%q) ok = %v, want %v", tc.path, ok, tc.ok)
		}
		if info.module != tc.module || info.version != tc.version || info.kind != tc.kind {
			t.Fatalf("parsePath(%q) = %+v", tc.path, info)
		}
	}
}

func TestRetentionUnitPaths(t *testing.T) {
	f := Format{}
	for _, kind := range []string{"hosted", "proxy"} {
		repository := format.Repository{Type: kind}
		paths := f.RetentionUnitPaths(repository, "github.com/!burnt!sushi/toml/@v/v1.2.3.info")
		want := []string{
			"github.com/!burnt!sushi/toml/@v/v1.2.3.info",
			"github.com/!burnt!sushi/toml/@v/v1.2.3.mod",
			"github.com/!burnt!sushi/toml/@v/v1.2.3.zip",
		}
		if !reflect.DeepEqual(paths, want) {
			t.Fatalf("%s paths = %v, want %v", kind, paths, want)
		}
		key, ok := f.RetentionGroupKey(repository, format.Asset{Path: paths[2]})
		if !ok || key != "github.com/BurntSushi/toml" {
			t.Fatalf("%s key = %q, %v", kind, key, ok)
		}
	}
	for _, path := range []string{"example.com/widget/@latest", "example.com/widget/@v/list", "example.com/widget/@v/master.info", "sumdb/sum.golang.org/supported"} {
		if paths := f.RetentionUnitPaths(format.Repository{Type: "proxy"}, path); len(paths) != 0 {
			t.Fatalf("%q claimed unit paths %v", path, paths)
		}
	}
}

func TestMutableUpstreamPath(t *testing.T) {
	var f Format
	repository := format.Repository{Type: "proxy"}
	mutable := []string{
		"example.com/hello/@v/list",
		"example.com/hello/@latest",
		"example.com/hello/@v/master.info",
		"example.com/hello/@v/latest.info",
		"example.com/hello/@v/v1.info",
		"example.com/hello/@v/v2.0.0.info",
		"sumdb/sum.golang.org/supported",
		"sumdb/sum.golang.org/lookup/example.com/hello@v1.0.0",
	}
	immutable := []string{
		"example.com/hello/@v/v1.0.0.info",
		"example.com/hello/@v/v1.0.0.mod",
		"example.com/hello/@v/v1.0.0.zip",
		"example.com/hello/@v/v0.0.0-20240101000000-abcdefabcdef.info",
		"example.com/hello/@v/v1.0.0-rc.1.info",
		"example.com/hello/@v/v2.0.0+incompatible.info",
		"example.com/hello/v2/@v/v2.0.0.info",
		"example.com/hello/README.md",
	}
	for _, path := range mutable {
		if !f.MutableUpstreamPath(repository, path) {
			t.Fatalf("%q should be mutable", path)
		}
	}
	for _, path := range immutable {
		if f.MutableUpstreamPath(repository, path) {
			t.Fatalf("%q should be immutable", path)
		}
	}
}

func TestValidateUploadAndRepository(t *testing.T) {
	var f Format
	repository := format.Repository{Type: "hosted"}
	var violation *format.PolicyViolation
	cases := map[string]string{
		"example.com/hello/@v/v1.0.0.info":                            "",
		"example.com/hello/@v/v1.0.0.mod":                             "",
		"example.com/hello/@v/v1.0.0.zip":                             "",
		"example.com/hello/@v/v0.0.0-20240101000000-abcdefabcdef.zip": "",
		"example.com/hello/@v/v2.0.0+incompatible.mod":                "",
		"example.com/hello/v2/@v/v2.0.0.mod":                          "",
		"example.com/hello/@v/v2.0.0.mod":                             "go_invalid_version",
		"example.com/hello/@v/list":                                   "go_derived_path",
		"example.com/hello/@latest":                                   "go_derived_path",
		"example.com/hello/@v/master.info":                            "go_invalid_version",
		"example.com/hello/@v/1.0.0.info":                             "go_invalid_version",
		"example.com/hello/@v/v1.0.info":                              "go_invalid_version",
		"example.com/hello/hello-1.0.0.zip":                           "go_invalid_path",
		"sumdb/sum.golang.org/supported":                              "go_invalid_path",
	}
	for path, wantCode := range cases {
		err := f.ValidateUpload(repository, path)
		if wantCode == "" {
			if err != nil {
				t.Fatalf("ValidateUpload(%q) = %v", path, err)
			}
			continue
		}
		if !errors.As(err, &violation) || violation.Code != wantCode {
			t.Fatalf("ValidateUpload(%q) = %v, want %s", path, err, wantCode)
		}
	}

	if err := f.ValidateRepository(format.Repository{Type: "proxy"}); err != nil {
		t.Fatal(err)
	}
	err := f.ValidateRepository(format.Repository{Type: "proxy", Config: map[string]any{"x": 1}})
	if !errors.As(err, &violation) || violation.Code != "invalid_format_config" {
		t.Fatalf("config err = %v", err)
	}
}

func TestProjectAttributes(t *testing.T) {
	var f Format
	attributes := f.ProjectAttributes(format.Asset{Path: "github.com/!burnt!sushi/toml/@v/v1.0.0.zip"})
	if attributes["module"] != "github.com/BurntSushi/toml" || attributes["version"] != "v1.0.0" {
		t.Fatalf("attributes = %v", attributes)
	}
	attributes = f.ProjectAttributes(format.Asset{Path: "example.com/hello/@v/list"})
	if attributes["module"] != "example.com/hello" || attributes["version"] != nil {
		t.Fatalf("list attributes = %v", attributes)
	}
	if f.ProjectAttributes(format.Asset{Path: "unrelated.txt"}) != nil {
		t.Fatal("unrelated path projected attributes")
	}
}

func TestRenderVersionList(t *testing.T) {
	got := string(renderVersionList("example.com/hello", []string{
		"v1.10.0", "v1.2.0", "v1.2.0", "v0.0.0-20240101000000-abcdefabcdef", "v1.0.0-rc.1", "v1.2", "v2.0.0", "v2.0.0+incompatible", "junk",
	}))
	if got != "v1.0.0-rc.1\nv1.2.0\nv1.10.0\nv2.0.0+incompatible\n" {
		t.Fatalf("list = %q", got)
	}
	if got := renderVersionList("example.com/hello", nil); len(got) != 0 {
		t.Fatalf("empty list = %q", got)
	}
}

func TestMergeGroupContent(t *testing.T) {
	var f Format
	repository := format.Repository{Type: "group"}
	if _, ok := f.GroupMergeSource(repository, "example.com/hello/@v/v1.0.0.zip"); ok {
		t.Fatal("module files must resolve first-match")
	}
	for _, path := range []string{"example.com/hello/@v/list", "example.com/hello/@latest"} {
		if source, ok := f.GroupMergeSource(repository, path); !ok || source != path {
			t.Fatalf("GroupMergeSource(%q) = %q %v", path, source, ok)
		}
	}

	content, contentType, err := f.MergeGroupContent(repository, "example.com/hello/@v/list", [][]byte{
		[]byte("v1.0.0\nv1.1.0\n"),
		[]byte("v1.1.0\nv2.0.0\nv2.0.0+incompatible\nnot-a-version\n"),
		[]byte("<html>oops</html>"),
	})
	if err != nil || contentType != listContentType || string(content) != "v1.0.0\nv1.1.0\nv2.0.0+incompatible\n" {
		t.Fatalf("merged list = %q %q %v", content, contentType, err)
	}

	content, contentType, err = f.MergeGroupContent(repository, "example.com/hello/@latest", [][]byte{
		[]byte(`{"Version":"v1.0.0","Time":"2024-01-01T00:00:00Z"}`),
		[]byte(`{"Version":"v1.2.0","Time":"2024-02-01T00:00:00Z"}`),
		[]byte(`{"Version":"v2.0.0","Time":"2024-03-01T00:00:00Z"}`),
		[]byte(`{"Version":"v2.0.0+incompatible","Time":"2024-03-01T00:00:00Z"}`),
		[]byte(`garbage`),
	})
	if err != nil || contentType != infoContentType || !strings.Contains(string(content), `"v2.0.0+incompatible"`) {
		t.Fatalf("merged latest = %q %q %v", content, contentType, err)
	}
	if _, _, err := f.MergeGroupContent(repository, "example.com/hello/@latest", [][]byte{[]byte("nope")}); err == nil {
		t.Fatal("latest without any valid member succeeded")
	}
}

type fakeAssets struct {
	paths   []string
	content map[string][]byte
}

func (a fakeAssets) VisitAssetPaths(_ context.Context, prefix string, visit func(string) (bool, error)) error {
	for _, path := range a.paths {
		if strings.HasPrefix(path, prefix) {
			more, err := visit(path)
			if err != nil || !more {
				return err
			}
		}
	}
	return nil
}

func (a fakeAssets) ReadAsset(_ context.Context, path string) ([]byte, bool, error) {
	content, found := a.content[path]
	return content, found, nil
}

func TestSynthesizeHosted(t *testing.T) {
	var f Format
	repository := format.Repository{Type: "hosted"}
	assets := fakeAssets{
		paths: []string{
			"example.com/hello/@v/v1.0.0.info",
			"example.com/hello/@v/v1.0.0.mod",
			"example.com/hello/@v/v1.0.0.zip",
			"example.com/hello/@v/v1.1.0.zip",
			"example.com/hello/@v/v0.0.0-20240101000000-abcdefabcdef.zip",
			"example.com/hello/@v/v2.0.0-beta.1.mod",
			"example.com/hello/@v/v2.0.0+incompatible.mod",
			"example.com/hello/sub/@v/v1.9.0.zip",
		},
		content: map[string][]byte{
			"example.com/hello/@v/v1.0.0.info": []byte(`{"Version":"v1.0.0","Time":"2024-01-01T00:00:00Z"}`),
		},
	}
	ctx := context.Background()

	content, contentType, ok, err := f.SynthesizeHosted(ctx, repository, "example.com/hello/@v/list", assets)
	if err != nil || !ok || contentType != listContentType {
		t.Fatalf("list = %v %q %v", ok, contentType, err)
	}
	if string(content) != "v1.0.0\n" {
		t.Fatalf("list content = %q", content)
	}

	// Incomplete versions cannot be downloaded by go, so none may displace
	// the complete v1.0.0 release in the list or @latest.
	content, contentType, ok, err = f.SynthesizeHosted(ctx, repository, "example.com/hello/@latest", assets)
	if err != nil || !ok || contentType != infoContentType || string(content) != `{"Version":"v1.0.0","Time":"2024-01-01T00:00:00Z"}` {
		t.Fatalf("latest = %q %q %v %v", content, contentType, ok, err)
	}

	content, _, ok, err = f.SynthesizeHosted(ctx, repository, "example.com/hello/sub/@latest", assets)
	if err != nil || ok || content != nil {
		t.Fatalf("sub latest = %q %v %v", content, ok, err)
	}

	if _, _, ok, err := f.SynthesizeHosted(ctx, repository, "example.com/other/@v/list", assets); ok || err != nil {
		t.Fatalf("unknown module synthesized: %v %v", ok, err)
	}
	if _, _, ok, _ := f.SynthesizeHosted(ctx, repository, "example.com/hello/@v/v1.0.0.zip", assets); ok {
		t.Fatal("module file synthesized")
	}
}
