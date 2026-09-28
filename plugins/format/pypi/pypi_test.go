package pypi

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/suxen-project/suxen/spi/format"
)

func TestParsePath(t *testing.T) {
	cases := []struct {
		path    string
		kind    pathKind
		project string
		host    string
		file    string
		ok      bool
	}{
		{"simple", kindRoot, "", "", "", true},
		{"simple/", kindRoot, "", "", "", true},
		{"simple/hello", kindProject, "hello", "", "", true},
		{"simple/hello/", kindProject, "hello", "", "", true},
		{"simple/Django/", kindProject, "Django", "", "", true},
		{"simple/typing_extensions/", kindProject, "typing_extensions", "", "", true},
		{"files/https/files.pythonhosted.org/packages/ab/cd/hash/hello-1.0.0-py3-none-any.whl", kindFile, "", "files.pythonhosted.org", "hello-1.0.0-py3-none-any.whl", true},
		{"files/http/mirror:8080/packages/hello-1.0.0.tar.gz.metadata", kindFile, "", "mirror:8080", "hello-1.0.0.tar.gz.metadata", true},
		{"simple/../etc", 0, "", "", "", false},
		{"simple/he llo/", 0, "", "", "", false},
		{"files/ftp/host/x", 0, "", "", "", false},
		{"files/https/host", 0, "", "", "", false},
		{"files/https/host/a/../b", 0, "", "", "", false},
		{"pypi/hello/json", 0, "", "", "", false},
	}
	for _, tc := range cases {
		info, ok := parsePath(tc.path)
		if ok != tc.ok {
			t.Fatalf("parsePath(%q) ok = %v, want %v", tc.path, ok, tc.ok)
		}
		if info.kind != tc.kind || info.project != tc.project || info.host != tc.host || info.filename != tc.file {
			t.Fatalf("parsePath(%q) = %+v", tc.path, info)
		}
	}
}

func TestDistributionFilenames(t *testing.T) {
	cases := map[string][2]string{
		"hello-1.0.0-py3-none-any.whl":                 {"hello", "1.0.0"},
		"Typing_Extensions-4.12.2-py3-none-any.whl":    {"typing-extensions", "4.12.2"},
		"hello-1.0.0.tar.gz":                           {"hello", "1.0.0"},
		"my.pkg-2.0rc1.zip":                            {"my-pkg", "2.0rc1"},
		"numpy-2.1.0-cp312-cp312-manylinux_x86_64.whl": {"numpy", "2.1.0"},
	}
	for filename, want := range cases {
		name, version, ok := parseDistributionFilename(filename)
		if !ok || name != want[0] || version != want[1] {
			t.Fatalf("parseDistributionFilename(%q) = %q %q %v", filename, name, version, ok)
		}
	}
	for _, filename := range []string{"hello-1.0.0-py3-none-any.whl.metadata", "hello.whl", "notes.txt", "-1.0.tar.gz"} {
		if _, _, ok := parseDistributionFilename(filename); ok {
			t.Fatalf("parseDistributionFilename(%q) accepted", filename)
		}
	}

	var f Format
	attributes := f.ProjectAttributes(format.Asset{Path: "files/https/h/packages/x/Hello_World-1.2.tar.gz"})
	if attributes["name"] != "hello-world" || attributes["version"] != "1.2" {
		t.Fatalf("attributes = %v", attributes)
	}
	if attributes := f.ProjectAttributes(format.Asset{Path: "simple/hello/"}); attributes["name"] != "hello" {
		t.Fatalf("project attributes = %v", attributes)
	}
	if f.ProjectAttributes(format.Asset{Path: "simple/"}) != nil || f.ProjectAttributes(format.Asset{Path: "files/https/h/x.metadata"}) != nil {
		t.Fatal("root or metadata projected attributes")
	}
}

func TestPolicies(t *testing.T) {
	var f Format
	repository := format.Repository{Type: "proxy"}
	for _, path := range []string{"simple/", "simple/hello/"} {
		if !f.MutableUpstreamPath(repository, path) {
			t.Fatalf("%q should be mutable", path)
		}
	}
	for _, path := range []string{"files/https/h/packages/x/hello-1.0.0.tar.gz", "pypi/hello/json"} {
		if f.MutableUpstreamPath(repository, path) {
			t.Fatalf("%q should be immutable", path)
		}
	}
	if err := f.ValidateRepository(format.Repository{Type: "hosted"}); err != nil {
		t.Fatalf("hosted err = %v", err)
	}
	if err := f.ValidateRepository(format.Repository{Type: "group"}); err != nil {
		t.Fatal(err)
	}
	resolved, err := f.ResolveProxyRequest(context.Background(), repository, "simple/hello/", "", nil)
	if err != nil || resolved.CachePath != "simple/hello/" || resolved.UpstreamURL != "" {
		t.Fatalf("project page resolution = %+v %v", resolved, err)
	}
}

const jsonPage = `{"meta":{"api-version":"1.1"},"name":"hello","files":[
{"filename":"hello-1.0.0-py3-none-any.whl","url":"https://files.pythonhosted.org/packages/ab/cd/h1/hello-1.0.0-py3-none-any.whl","hashes":{"sha256":"aaa"},"requires-python":">=3.8","core-metadata":{"sha256":"mmm"}},
{"filename":"hello-1.0.0.tar.gz","url":"../../packages/ef/hello-1.0.0.tar.gz","hashes":{"sha256":"bbb"},"yanked":"broken"}],
"versions":["1.0.0"]}`

const htmlPage = `<!DOCTYPE html><html><body>
<a href="https://files.pythonhosted.org/packages/ab/cd/h1/hello-1.0.0-py3-none-any.whl#sha256=aaa" data-requires-python="&gt;=3.8">hello-1.0.0-py3-none-any.whl</a>
<a href='../../packages/ef/hello-1.1.0.tar.gz#sha256=ccc' data-yanked="">hello-1.1.0.tar.gz</a>
</body></html>`

func TestRewriteIndex(t *testing.T) {
	var f Format
	repository := format.Repository{Type: "proxy", Upstream: "https://pypi.org/"}
	const repositoryURL = "https://mirror.example/repository/pypi"

	rewritten, contentType, err := f.RewriteIndex(repository, "simple/hello/", []byte(jsonPage), jsonContentType, repositoryURL)
	if err != nil || contentType != jsonContentType {
		t.Fatalf("json rewrite: %v %q", err, contentType)
	}
	var document struct {
		Files []struct {
			URL      string            `json:"url"`
			Hashes   map[string]string `json:"hashes"`
			Requires string            `json:"requires-python"`
		} `json:"files"`
		Versions []string `json:"versions"`
	}
	if err := json.Unmarshal(rewritten, &document); err != nil {
		t.Fatal(err)
	}
	if document.Files[0].URL != repositoryURL+"/files/https/files.pythonhosted.org/packages/ab/cd/h1/hello-1.0.0-py3-none-any.whl" {
		t.Fatalf("absolute file url = %q", document.Files[0].URL)
	}
	if document.Files[1].URL != repositoryURL+"/files/https/pypi.org/packages/ef/hello-1.0.0.tar.gz" {
		t.Fatalf("relative file url = %q", document.Files[1].URL)
	}
	if document.Files[0].Hashes["sha256"] != "aaa" || document.Files[0].Requires != ">=3.8" || len(document.Versions) != 1 {
		t.Fatal("json metadata lost")
	}

	rewrittenHTML, contentType, err := f.RewriteIndex(repository, "simple/hello/", []byte(htmlPage), "text/html", repositoryURL)
	if err != nil || contentType != htmlContentType {
		t.Fatalf("html rewrite: %v %q", err, contentType)
	}
	page := string(rewrittenHTML)
	for _, want := range []string{
		`href="` + repositoryURL + `/files/https/files.pythonhosted.org/packages/ab/cd/h1/hello-1.0.0-py3-none-any.whl#sha256=aaa" data-requires-python="&gt;=3.8"`,
		`href="` + repositoryURL + `/files/https/pypi.org/packages/ef/hello-1.1.0.tar.gz#sha256=ccc" data-yanked=""`,
	} {
		if !strings.Contains(page, want) {
			t.Fatalf("html rewrite lacks %s:\n%s", want, page)
		}
	}

	root := `<a href="/simple/hello/">hello</a><a href="https://pypi.org/simple/other-pkg/">other-pkg</a><a href="weird/">weird</a>`
	rewrittenRoot, _, err := f.RewriteIndex(repository, "simple/", []byte(root), "text/html", repositoryURL)
	if err != nil {
		t.Fatal(err)
	}
	if got := string(rewrittenRoot); !strings.Contains(got, `href="`+repositoryURL+`/simple/hello/"`) ||
		!strings.Contains(got, `href="`+repositoryURL+`/simple/other-pkg/"`) ||
		!strings.Contains(got, `href="`+repositoryURL+`/simple/weird/"`) {
		t.Fatalf("root rewrite = %s", got)
	}

	file := []byte("wheel bytes")
	if passthrough, _, _ := f.RewriteIndex(repository, "files/https/h/packages/x.whl", file, "application/octet-stream", repositoryURL); string(passthrough) != "wheel bytes" {
		t.Fatal("file rewritten")
	}
	if _, _, err := f.RewriteIndex(repository, "simple/hello/", []byte("{not json"), jsonContentType, repositoryURL); err == nil {
		t.Fatal("invalid json accepted")
	}
}

func TestRewriteHTMLIndexRegeneratesSafeDocument(t *testing.T) {
	repository := format.Repository{Type: "proxy", Upstream: "https://pypi.example"}
	body := []byte(`<!doctype html><html><head><style>body{display:none}</style></head><body onload="alert(1)">
		<script>fetch('/api/v1/users',{credentials:'include'})</script>
		<iframe src="/api/v1/users"></iframe>
		<a href="javascript:alert(2)" onclick="alert(3)">bad.whl</a>
		<a href="files/good.whl#sha256=abc" data-requires-python="&gt;=3.8">good.whl</a>
	</body></html>`)

	rewritten, contentType, err := (Format{}).RewriteIndex(
		repository, "simple/example/", body, "text/html", "https://mirror.example/repository/pypi",
	)
	if err != nil {
		t.Fatal(err)
	}
	if contentType != htmlContentType {
		t.Fatalf("content type = %q", contentType)
	}
	page := string(rewritten)
	for _, forbidden := range []string{"<script", "fetch(", "<iframe", "<style", "onload", "onclick", "javascript:"} {
		if strings.Contains(strings.ToLower(page), forbidden) {
			t.Fatalf("regenerated index contains %q:\n%s", forbidden, page)
		}
	}
	if !strings.Contains(page, `href="https://mirror.example/repository/pypi/files/https/pypi.example/simple/example/files/good.whl#sha256=abc"`) {
		t.Fatalf("safe package link was not preserved:\n%s", page)
	}
}

func TestMergeGroupContent(t *testing.T) {
	var f Format
	repository := format.Repository{Type: "group"}
	if _, ok := f.GroupMergeSource(repository, "files/https/h/packages/x.whl"); ok {
		t.Fatal("files must resolve first-match")
	}
	if source, ok := f.GroupMergeSource(repository, "simple/hello/"); !ok || source != "simple/hello/" {
		t.Fatalf("GroupMergeSource = %q %v", source, ok)
	}

	// JSON first: HTML member entries are converted and appended.
	merged, contentType, err := f.MergeGroupContent(repository, "simple/hello/", [][]byte{[]byte(jsonPage), []byte("garbage"), []byte(htmlPage)})
	if err != nil || contentType != jsonContentType {
		t.Fatalf("json-first merge: %v %q", err, contentType)
	}
	var document struct {
		Name  string `json:"name"`
		Files []struct {
			Filename string            `json:"filename"`
			URL      string            `json:"url"`
			Hashes   map[string]string `json:"hashes"`
			Yanked   any               `json:"yanked"`
		} `json:"files"`
		Versions []string `json:"versions"`
	}
	if err := json.Unmarshal(merged, &document); err != nil {
		t.Fatal(err)
	}
	if document.Name != "hello" || len(document.Files) != 3 || len(document.Versions) != 2 {
		t.Fatalf("merged = %s", merged)
	}
	third := document.Files[2]
	if third.Filename != "hello-1.1.0.tar.gz" || third.URL != "../../packages/ef/hello-1.1.0.tar.gz" || third.Hashes["sha256"] != "ccc" || third.Yanked != true {
		t.Fatalf("html-derived entry = %+v", third)
	}

	// HTML first: the intermediate merge remains JSON until the final
	// request negotiation, retaining metadata from later JSON members.
	mergedJSON, contentType, err := f.MergeGroupContent(repository, "simple/hello/", [][]byte{[]byte(htmlPage), []byte(jsonPage)})
	if err != nil || contentType != jsonContentType {
		t.Fatalf("html-first merge: %v %q", err, contentType)
	}
	mergedPage, err := parsePage(mergedJSON, kindProject)
	if err != nil {
		t.Fatal(err)
	}
	mergedPage.asJSON = false
	mergedHTML, contentType, err := mergedPage.render()
	if err != nil || contentType != htmlContentType {
		t.Fatalf("html-first negotiated merge: %v %q", err, contentType)
	}
	page := string(mergedHTML)
	// The wheel is present in both members; the HTML member's entry wins, so
	// only the JSON member's sdist is added, with its attributes rendered.
	if strings.Count(page, "<a ") != 3 ||
		!strings.Contains(page, `href="../../packages/ef/hello-1.0.0.tar.gz#sha256=bbb" data-yanked="broken"`) ||
		strings.Contains(page, "core-metadata") {
		t.Fatalf("html-first merged page:\n%s", page)
	}
	anchor := renderAnchor(map[string]any{
		"filename": "x-1.0-py3-none-any.whl", "url": "https://h/x.whl",
		"hashes": map[string]any{"sha256": "d"}, "core-metadata": map[string]any{"sha256": "mmm"}, "yanked": true,
	}, kindProject)
	if anchor != `<a href="https://h/x.whl#sha256=d" data-yanked="" data-core-metadata="sha256=mmm">x-1.0-py3-none-any.whl</a>`+"\n" {
		t.Fatalf("rendered anchor = %q", anchor)
	}

	rootMerged, _, err := f.MergeGroupContent(repository, "simple/", [][]byte{
		[]byte(`{"meta":{"api-version":"1.0"},"projects":[{"name":"hello"}]}`),
		[]byte(`<a href="/simple/hello/">Hello</a><a href="/simple/other/">other</a>`),
	})
	if err != nil || !strings.Contains(string(rootMerged), `"other"`) || strings.Count(string(rootMerged), `"name"`) != 2 {
		t.Fatalf("root merge = %s %v", rootMerged, err)
	}
	if _, _, err := f.MergeGroupContent(repository, "simple/hello/", [][]byte{[]byte("garbage")}); err == nil {
		t.Fatal("merge without a valid member succeeded")
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
		{hosted, "POST", "", "write", true},
		{hosted, "POST", "/", "write", true},
		{hosted, "POST", "legacy/", "write", true},
		{hosted, "GET", "simple/", "read", true},
		{hosted, "GET", "simple/hello/", "read", true},
		{hosted, "HEAD", "simple/", "read", true},
		{hosted, "GET", "packages/hello/hello-1.0.0-py3-none-any.whl", "", false}, // files ride the pipeline
		{hosted, "POST", "simple/hello/", "", false},
		{format.Repository{Type: "proxy"}, "POST", "", "", false},
		{format.Repository{Type: "group"}, "GET", "simple/", "", false},
	}
	for _, tc := range cases {
		action, ok := f.WireAction(tc.repo, tc.method, tc.path, nil)
		if action != tc.action || ok != tc.ok {
			t.Fatalf("WireAction(%s %s %q) = (%q,%v), want (%q,%v)", tc.repo.Type, tc.method, tc.path, action, ok, tc.action, tc.ok)
		}
	}
}

func TestParsePathHostedFile(t *testing.T) {
	info, ok := parsePath("packages/hello/hello-1.0.0-py3-none-any.whl")
	if !ok || info.kind != kindHostedFile || info.project != "hello" || info.filename != "hello-1.0.0-py3-none-any.whl" {
		t.Fatalf("parsePath hosted file = %+v ok=%v", info, ok)
	}
	attributes := Format{}.ProjectAttributes(format.Asset{Path: "packages/hello/hello-1.0.0-py3-none-any.whl"})
	if attributes["name"] != "hello" || attributes["version"] != "1.0.0" {
		t.Fatalf("hosted file attributes = %v", attributes)
	}
	for _, bad := range []string{"packages/hello/", "packages//x.whl", "packages/hello/sub/x.whl", "packages/hello/.."} {
		if _, ok := parsePath(bad); ok {
			t.Fatalf("parsePath(%q) accepted", bad)
		}
	}
}
