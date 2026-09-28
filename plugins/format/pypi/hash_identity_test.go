package pypi_test

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"testing"

	pypi "github.com/suxen-project/suxen/plugins/format/pypi"
	"github.com/suxen-project/suxen/spi/format"
)

func TestPyPIEquivalentHashRepresentationsShareIdentityAndGroupOwnership(t *testing.T) {
	plugin := pypi.Format{}
	repository := format.Repository{Type: "proxy", Upstream: "https://index.example"}
	const fileURL = "https://files.example/widget-1.0.tar.gz"
	const publicPath = "files/https/files.example/widget-1.0.tar.gz"
	const filename = "widget-1.0.tar.gz"
	digest := fmt.Sprintf("%x", sha256.Sum256([]byte("wheel")))
	pages := []struct {
		name string
		body string
	}{
		{"HTML fragment", fmt.Sprintf(`<html><a href="%s#sha256=%s">%s</a></html>`, fileURL, digest, filename)},
		{"JSON hashes", fmt.Sprintf(`{"meta":{"api-version":"1.0"},"name":"widget","files":[{"filename":%q,"url":%q,"hashes":{"sha256":%q}}]}`, filename, fileURL, digest)},
		{"JSON fragment and hashes", fmt.Sprintf(`{"meta":{"api-version":"1.0"},"name":"widget","files":[{"filename":%q,"url":%q,"hashes":{"sha256":%q}}]}`, filename, fileURL+"#sha256="+digest, digest)},
	}
	var wantPath string
	for _, test := range pages {
		t.Run(test.name, func(t *testing.T) {
			stored := storedIndexes{paths: []string{"simple/widget/"}, bodies: map[string][]byte{"simple/widget/": []byte(test.body)}}
			resolved, err := plugin.ResolveProxyRequest(context.Background(), repository, publicPath, "", stored)
			if err != nil {
				t.Fatal(err)
			}
			if wantPath == "" {
				wantPath = resolved.CachePath
			}
			if resolved.CachePath != wantPath || len(resolved.ExpectedDigests) != 1 || resolved.ExpectedDigests[0] != "sha256:"+digest {
				t.Fatalf("resolution differs by representation: %+v", resolved)
			}
			request := format.GroupArtifactRequest{Path: publicPath, StoredPath: resolved.CachePath}
			present, matches, err := plugin.GroupSourceArtifact(repository, "simple/widget/", filename, request, []byte(test.body))
			if err != nil || !present || !matches {
				t.Fatalf("group ownership differs by representation: present=%v matches=%v err=%v", present, matches, err)
			}
		})
	}
	// A secondary advertised hash is part of the cache generation even when
	// SHA-256 remains the host-verified digest.
	var document map[string]any
	if err := json.Unmarshal([]byte(pages[1].body), &document); err != nil {
		t.Fatal(err)
	}
	files := document["files"].([]any)
	files[0].(map[string]any)["hashes"].(map[string]any)["sha1"] = "00"
	changed, _ := json.Marshal(document)
	resolved, err := plugin.ResolveProxyRequest(context.Background(), repository, publicPath, "", storedIndexes{paths: []string{"simple/widget/"}, bodies: map[string][]byte{"simple/widget/": changed}})
	if err != nil || resolved.CachePath == wantPath {
		t.Fatalf("secondary hash change reused generation: %+v, %v", resolved, err)
	}
}

func TestPyPIMetadataCompanionHashRepresentationIdentity(t *testing.T) {
	plugin := pypi.Format{}
	repository := format.Repository{Type: "proxy", Upstream: "https://index.example"}
	const filename = "widget-1.0.whl"
	const publicPath = "files/https/files.example/widget-1.0.whl.metadata"
	wheelDigest := fmt.Sprintf("%x", sha256.Sum256([]byte("wheel")))
	metadataDigest := fmt.Sprintf("%x", sha256.Sum256([]byte("metadata")))
	pages := []string{
		fmt.Sprintf(`<html><a href="https://files.example/%s#sha256=%s" data-core-metadata="sha256=%s">%s</a></html>`, filename, wheelDigest, metadataDigest, filename),
		fmt.Sprintf(`{"name":"widget","files":[{"filename":%q,"url":%q,"hashes":{"sha256":%q},"core-metadata":{"sha256":%q}}]}`, filename, "https://files.example/"+filename+"#sha256="+wheelDigest, wheelDigest, metadataDigest),
	}
	var wantPath string
	for _, body := range pages {
		stored := storedIndexes{paths: []string{"simple/widget/"}, bodies: map[string][]byte{"simple/widget/": []byte(body)}}
		resolved, err := plugin.ResolveProxyRequest(context.Background(), repository, publicPath, "", stored)
		if err != nil {
			t.Fatal(err)
		}
		if wantPath == "" {
			wantPath = resolved.CachePath
		}
		if resolved.CachePath != wantPath || len(resolved.ExpectedDigests) != 1 || resolved.ExpectedDigests[0] != "sha256:"+metadataDigest {
			t.Fatalf("companion identity differs: %+v", resolved)
		}
		present, matches, err := plugin.GroupSourceArtifact(repository, "simple/widget/", filename,
			format.GroupArtifactRequest{Path: publicPath, StoredPath: resolved.CachePath}, []byte(body))
		if err != nil || !present || !matches {
			t.Fatalf("companion group ownership: present=%v matches=%v err=%v", present, matches, err)
		}
	}
}
