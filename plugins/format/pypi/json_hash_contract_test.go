package pypi

import (
	"crypto/md5"
	"crypto/sha1"
	"crypto/sha256"
	"crypto/sha512"
	"encoding/json"
	"fmt"
	"strings"
	"testing"

	"github.com/suxen-project/suxen/spi/format"
)

func TestSimpleJSONRequiredShapesAndExtensions(t *testing.T) {
	plugin := Format{}
	repository := format.Repository{Type: "proxy", Upstream: "https://index.example"}
	root, _, err := plugin.RewriteIndexForAccept(repository, "simple/", []byte(`<html></html>`), "text/html", "https://mirror.example/repository/proxy", jsonContentType)
	if err != nil {
		t.Fatal(err)
	}
	var rootDocument map[string]json.RawMessage
	if err := json.Unmarshal(root, &rootDocument); err != nil {
		t.Fatal(err)
	}
	if string(rootDocument["projects"]) != `[]` || string(rootDocument["meta"]) != `{"api-version":"1.0"}` {
		t.Fatalf("empty root shape = %s", root)
	}

	project := []byte(`<html><a href="https://files.example/widget-1.0.tar.gz">widget-1.0.tar.gz</a></html>`)
	converted, _, err := plugin.RewriteIndexForAccept(repository, "simple/widget/", project, "text/html", "https://mirror.example/repository/proxy", jsonContentType)
	if err != nil {
		t.Fatal(err)
	}
	var document struct {
		Meta  map[string]any   `json:"meta"`
		Files []map[string]any `json:"files"`
	}
	if err := json.Unmarshal(converted, &document); err != nil {
		t.Fatal(err)
	}
	if document.Meta["api-version"] != "1.0" || len(document.Files) != 1 {
		t.Fatalf("project shape = %s", converted)
	}
	if hashes, ok := document.Files[0]["hashes"].(map[string]any); !ok || len(hashes) != 0 {
		t.Fatalf("hashless file needs empty object: %s", converted)
	}

	input := []byte(`{"meta":{"api-version":"1.0","serial":9007199254740993},"name":"widget","extra":{"counter":9007199254740993},"files":[{"filename":"widget-1.0.tar.gz","url":"https://files.example/widget-1.0.tar.gz","hashes":{},"extension":{"counter":9007199254740993}}]}`)
	preserved, _, err := plugin.RewriteIndexForAccept(repository, "simple/widget/", input, jsonContentType, "https://mirror.example/repository/proxy", jsonContentType)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Count(string(preserved), "9007199254740993") != 3 {
		t.Fatalf("lost extension numeric values: %s", preserved)
	}
}

func TestSimpleHashRoundTrips(t *testing.T) {
	payload := []byte("wheel bytes")
	algorithms := map[string]string{
		"md5":    fmt.Sprintf("%x", md5.Sum(payload)),
		"sha1":   fmt.Sprintf("%x", sha1.Sum(payload)),
		"sha224": fmt.Sprintf("%x", sha256.Sum224(payload)),
		"sha256": fmt.Sprintf("%x", sha256.Sum256(payload)),
		"sha384": fmt.Sprintf("%x", sha512.Sum384(payload)),
		"sha512": fmt.Sprintf("%x", sha512.Sum512(payload)),
	}
	plugin := Format{}
	repository := format.Repository{Type: "proxy", Upstream: "https://index.example"}
	for algorithm, digest := range algorithms {
		t.Run(algorithm, func(t *testing.T) {
			fragment := "#" + algorithm + "=" + digest
			htmlInput := `<html><a href="https://files.example/widget-1.0.tar.gz` + fragment + `">widget-1.0.tar.gz</a></html>`
			htmlOutput, _, err := plugin.RewriteIndexForAccept(repository, "simple/widget/", []byte(htmlInput), "text/html", "https://mirror.example/repository/proxy", "text/html")
			if err != nil || strings.Count(string(htmlOutput), fragment) != 1 {
				t.Fatalf("HTML fragment = %s, %v", htmlOutput, err)
			}
			jsonOutput, _, err := plugin.RewriteIndexForAccept(repository, "simple/widget/", []byte(htmlInput), "text/html", "https://mirror.example/repository/proxy", jsonContentType)
			if err != nil {
				t.Fatal(err)
			}
			var document struct {
				Files []struct {
					URL    string            `json:"url"`
					Hashes map[string]string `json:"hashes"`
				} `json:"files"`
			}
			if err := json.Unmarshal(jsonOutput, &document); err != nil || len(document.Files) != 1 {
				t.Fatalf("JSON = %s, %v", jsonOutput, err)
			}
			if document.Files[0].Hashes[algorithm] != digest || strings.Contains(document.Files[0].URL, "#") {
				t.Fatalf("HTML to JSON hash = %s", jsonOutput)
			}
			jsonInput := fmt.Sprintf(`{"meta":{"api-version":"1.0"},"name":"widget","files":[{"filename":"widget-1.0.tar.gz","url":"https://files.example/widget-1.0.tar.gz%s","hashes":{"%s":"%s"}}]}`, fragment, algorithm, digest)
			htmlOutput, _, err = plugin.RewriteIndexForAccept(repository, "simple/widget/", []byte(jsonInput), jsonContentType, "https://mirror.example/repository/proxy", "text/html")
			if err != nil || strings.Count(string(htmlOutput), fragment) != 1 || strings.Contains(string(htmlOutput), fragment+"#") {
				t.Fatalf("JSON to HTML fragment = %s, %v", htmlOutput, err)
			}
		})
	}
}

func TestGroupIntermediatePreservesLaterJSONFields(t *testing.T) {
	plugin := Format{}
	firstHTML := []byte(`<html><a href="https://files.example/widget-1.0.tar.gz">widget-1.0.tar.gz</a></html>`)
	laterJSON := []byte(`{"meta":{"api-version":"1.0"},"name":"widget","files":[{"filename":"widget-2.0.tar.gz","url":"https://files.example/widget-2.0.tar.gz","hashes":{"sha256":"aa","sha512":"bb"},"extension":{"serial":9007199254740993}}]}`)
	merged, contentType, err := plugin.MergeGroupContent(format.Repository{Type: "group"}, "simple/widget/", [][]byte{firstHTML, laterJSON})
	if err != nil || contentType != jsonContentType {
		t.Fatalf("intermediate type = %q, %v", contentType, err)
	}
	if !strings.Contains(string(merged), `"sha256":"aa"`) || !strings.Contains(string(merged), `"sha512":"bb"`) || !strings.Contains(string(merged), `"serial":9007199254740993`) {
		t.Fatalf("later metadata lost: %s", merged)
	}
	rootHTML := []byte(`<html><a href="/simple/widget/">widget</a></html>`)
	rootJSON := []byte(`{"meta":{"api-version":"1.0"},"projects":[{"name":"other","extension":{"serial":9007199254740993}}]}`)
	mergedRoot, contentType, err := plugin.MergeGroupContent(format.Repository{Type: "group"}, "simple/", [][]byte{rootHTML, rootJSON})
	if err != nil || contentType != jsonContentType || !strings.Contains(string(mergedRoot), `"serial":9007199254740993`) {
		t.Fatalf("root member extension lost: %s %q %v", mergedRoot, contentType, err)
	}
}
