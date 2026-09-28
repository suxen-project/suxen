package pypi

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/suxen-project/suxen/spi/format"
)

func TestPyPIVersionUnionWithHTMLFirst(t *testing.T) {
	first := []byte(`<html><a href="https://files.example/widget-1.0.tar.gz">widget-1.0.tar.gz</a></html>`)
	later := []byte(`{"meta":{"api-version":"1.1"},"name":"widget","versions":["2.0","3.0"],"files":[{"filename":"widget-2.0.tar.gz","url":"https://files.example/widget-2.0.tar.gz","hashes":{}}]}`)
	normalized, err := (Format{}).NormalizeGroupSource(format.Repository{Upstream: "https://index.example"}, "simple/widget/", first)
	if err != nil {
		t.Fatal(err)
	}
	output, _, err := (Format{}).MergeGroupContent(format.Repository{}, "simple/widget/", [][]byte{normalized, later})
	if err != nil {
		t.Fatal(err)
	}
	var document struct {
		Meta     map[string]string `json:"meta"`
		Versions []string          `json:"versions"`
	}
	if err := json.Unmarshal(output, &document); err != nil {
		t.Fatal(err)
	}
	if strings.Join(document.Versions, ",") != "2.0,3.0,1.0" {
		t.Fatalf("union = %s", output)
	}
	if document.Meta["api-version"] != "1.0" {
		t.Fatalf("version = %s", output)
	}
}

func TestPyPIPreservesVersionWithoutFiles(t *testing.T) {
	input := []byte(`{"meta":{"api-version":"1.1"},"name":"widget","versions":["1.0","2.0"],"files":[{"filename":"widget-1.0.tar.gz","url":"https://files.example/widget-1.0.tar.gz","hashes":{}}]}`)
	page, err := parsePage(input, kindProject)
	if err != nil {
		t.Fatal(err)
	}
	output, _, err := page.render()
	if err != nil {
		t.Fatal(err)
	}
	var document struct {
		Versions []string `json:"versions"`
	}
	if err := json.Unmarshal(output, &document); err != nil {
		t.Fatal(err)
	}
	if strings.Join(document.Versions, ",") != "1.0,2.0" {
		t.Fatalf("fileless version lost: %s", output)
	}
}

func TestPyPIEmptyVersionsRemainArrayAndMajorVersionPreserved(t *testing.T) {
	for _, version := range []string{"1.1", "2.0"} {
		input := []byte(`{"meta":{"api-version":"` + version + `"},"name":"widget","versions":[],"files":[]}`)
		page, err := parsePage(input, kindProject)
		if err != nil {
			t.Fatal(err)
		}
		output, _, err := page.render()
		if err != nil {
			t.Fatal(err)
		}
		if !strings.Contains(string(output), `"versions":[]`) || !strings.Contains(string(output), `"api-version":"`+version+`"`) {
			t.Fatalf("empty versions or supplied major version lost: %s", output)
		}
	}
}

func TestPyPIStatusAndProvenanceConversions(t *testing.T) {
	htmlPage := []byte(`<!DOCTYPE html><html><head><meta name="pypi:repository-version" content="1.4"><meta name="pypi:project-status" content="archived"><meta name="pypi:project-status-reason" content="a &amp; b"></head><body><a href="https://files.example/widget-1.0.tar.gz" data-provenance="https://attest.example/widget-1.0.tar.gz.provenance" data-gpg-sig="true">widget-1.0.tar.gz</a></body></html>`)
	jsonOutput, _, err := rewriteIndex(format.Repository{Upstream: "https://index.example"}, "simple/widget/", htmlPage, "text/html", "https://local.example/repository/proxy", boolPtr(true))
	if err != nil {
		t.Fatal(err)
	}
	var document struct {
		Meta          map[string]string `json:"meta"`
		ProjectStatus map[string]string `json:"project-status"`
		Files         []map[string]any  `json:"files"`
	}
	if err := json.Unmarshal(jsonOutput, &document); err != nil {
		t.Fatal(err)
	}
	if document.Meta["api-version"] != "1.0" || document.ProjectStatus["status"] != "archived" || document.ProjectStatus["reason"] != "a & b" {
		t.Fatalf("status lost: %s", jsonOutput)
	}
	if document.Files[0]["provenance"] != "https://attest.example/widget-1.0.tar.gz.provenance" || document.Files[0]["gpg-sig"] != true {
		t.Fatalf("file metadata lost: %s", jsonOutput)
	}
	htmlOutput, _, err := rewriteIndex(format.Repository{Upstream: "https://index.example"}, "simple/widget/", jsonOutput, "application/vnd.pypi.simple.v1+json", "https://local.example/repository/proxy", boolPtr(false))
	if err != nil {
		t.Fatal(err)
	}
	for _, snippet := range []string{`content="1.4"`, `pypi:project-status" content="archived"`, `pypi:project-status-reason" content="a &amp; b"`, `data-gpg-sig="true"`, `data-provenance="https://attest.example/widget-1.0.tar.gz.provenance"`} {
		if !strings.Contains(string(htmlOutput), snippet) {
			t.Errorf("missing %q: %s", snippet, htmlOutput)
		}
	}
	if strings.Contains(string(htmlOutput), `/repository/proxy/files/https/attest.example`) {
		t.Fatalf("provenance incorrectly rewritten: %s", htmlOutput)
	}
}

func TestPyPIJSONVersionMatchesFileSizeSchema(t *testing.T) {
	validJSON := []byte(`{"meta":{"api-version":"1.4"},"name":"widget","project-status":{"status":"archived"},"versions":["1.0","2.0"],"files":[{"filename":"widget-1.0.tar.gz","url":"https://files.example/widget-1.0.tar.gz","hashes":{},"size":17,"provenance":"https://attest.example/widget-1.0.tar.gz.provenance"}]}`)
	page, err := parsePage(validJSON, kindProject)
	if err != nil {
		t.Fatal(err)
	}
	output, _, err := page.render()
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(output), `"api-version":"1.4"`) || !strings.Contains(string(output), `"versions":["1.0","2.0"]`) {
		t.Fatalf("valid JSON 1.4 downgraded: %s", output)
	}

	htmlMember := []byte(`<html><a href="https://files.example/widget-3.0.tar.gz">widget-3.0.tar.gz</a></html>`)
	merged, _, err := (Format{}).MergeGroupContent(format.Repository{}, "simple/widget/", [][]byte{validJSON, htmlMember})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(merged), `"api-version":"1.0"`) || !strings.Contains(string(merged), `"status":"archived"`) || !strings.Contains(string(merged), `"provenance":"https://attest.example/`) {
		t.Fatalf("mixed group lost fields or claimed sizes it lacks: %s", merged)
	}
}

func TestPyPIGroupFirstStatusOwns(t *testing.T) {
	first := []byte(`{"meta":{"api-version":"1.4"},"name":"widget","project-status":{"status":"quarantined","reason":"first"},"files":[]}`)
	second := []byte(`{"meta":{"api-version":"1.4"},"name":"widget","project-status":{"status":"active","reason":"second"},"files":[]}`)
	output, _, err := (Format{}).MergeGroupContent(format.Repository{}, "simple/widget/", [][]byte{first, second})
	if err != nil {
		t.Fatal(err)
	}
	var document struct {
		ProjectStatus map[string]string `json:"project-status"`
	}
	if err := json.Unmarshal(output, &document); err != nil {
		t.Fatal(err)
	}
	if document.ProjectStatus["status"] != "quarantined" || document.ProjectStatus["reason"] != "first" {
		t.Fatalf("status ownership: %s", output)
	}
}

func TestPyPISignatureIdentityAndOwnership(t *testing.T) {
	plugin := Format{}
	repo := format.Repository{Type: "proxy", Upstream: "https://index.example"}
	const publicPath = "files/https/files.example/widget-1.0.tar.gz.asc"
	const filename = "widget-1.0.tar.gz"
	for _, body := range []string{
		`<html><a href="https://files.example/widget-1.0.tar.gz?token=one" data-gpg-sig="true">widget-1.0.tar.gz</a></html>`,
		`{"meta":{"api-version":"1.0"},"name":"widget","files":[{"filename":"widget-1.0.tar.gz","url":"https://files.example/widget-1.0.tar.gz?token=one","hashes":{},"gpg-sig":true}]}`,
	} {
		stored := conformanceStoredIndexes{body: []byte(body)}
		resolved, err := plugin.ResolveProxyRequest(context.Background(), repo, publicPath, "token=one", stored)
		if err != nil || resolved.UpstreamURL != "https://files.example/widget-1.0.tar.gz.asc?token=one" || len(resolved.ExpectedDigests) != 0 {
			t.Fatalf("signature resolution: %+v %v", resolved, err)
		}
		present, matches, err := plugin.GroupSourceArtifact(repo, "simple/widget/", filename, format.GroupArtifactRequest{Path: publicPath, RawQuery: "token=one", StoredPath: resolved.CachePath}, []byte(body))
		if err != nil || !present || !matches {
			t.Fatalf("signature owner: %v %v %v", present, matches, err)
		}
	}
}

func TestPyPICompanionGenerationFollowsParentHashes(t *testing.T) {
	plugin := Format{}
	repo := format.Repository{Type: "proxy", Upstream: "https://index.example"}
	for _, companion := range []string{".asc", ".metadata"} {
		var previous string
		for _, digest := range []string{"aa", "bb"} {
			body := []byte(`{"name":"widget","files":[{"filename":"widget-1.0.whl","url":"https://files.example/widget-1.0.whl?token=one","hashes":{"sha256":"` + digest + `"},"gpg-sig":true,"core-metadata":true}]}`)
			path := "files/https/files.example/widget-1.0.whl" + companion
			resolved, err := plugin.ResolveProxyRequest(context.Background(), repo, path, "token=one", conformanceStoredIndexes{body: body})
			if err != nil || resolved.UpstreamURL == "" {
				t.Fatalf("resolve %s: %+v %v", companion, resolved, err)
			}
			if previous != "" && previous == resolved.CachePath {
				t.Fatalf("%s reused companion generation after parent hash changed", companion)
			}
			previous = resolved.CachePath
			present, matches, err := plugin.GroupSourceArtifact(repo, "simple/widget/", "widget-1.0.whl",
				format.GroupArtifactRequest{Path: path, RawQuery: "token=one", StoredPath: resolved.CachePath}, body)
			if err != nil || !present || !matches {
				t.Fatalf("%s ownership disagrees with proxy generation: %v %v %v", companion, present, matches, err)
			}
		}
	}
}

func boolPtr(value bool) *bool { return &value }

type conformanceStoredIndexes struct{ body []byte }

func (s conformanceStoredIndexes) ReadAsset(_ context.Context, path string) ([]byte, bool, error) {
	if path == "simple/widget/" {
		return s.body, true, nil
	}
	return nil, false, nil
}
func (s conformanceStoredIndexes) VisitAssetPaths(_ context.Context, prefix string, visit func(string) (bool, error)) error {
	if strings.HasPrefix("simple/widget/", prefix) {
		_, err := visit("simple/widget/")
		return err
	}
	return nil
}
