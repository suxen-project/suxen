package pypi

import (
	"encoding/json"
	"testing"

	"github.com/suxen-project/suxen/spi/format"
)

func TestEmptyJSONIndexesKeepArrayShapeAndMetadata(t *testing.T) {
	plugin := Format{}
	for _, test := range []struct {
		name string
		path string
		key  string
		body string
	}{
		{"project", "simple/empty/", "files", `{"name":"empty","files":[],"meta":{"api-version":"1.0"},"extra":{"keep":true}}`},
		{"root", "simple/", "projects", `{"projects":[],"meta":{"api-version":"1.0"},"extra":{"keep":true}}`},
	} {
		t.Run(test.name, func(t *testing.T) {
			rewritten, contentType, err := plugin.RewriteIndex(
				format.Repository{Upstream: "https://pypi.example"}, test.path, []byte(test.body),
				jsonContentType, "https://registry.example/repository/pypi",
			)
			if err != nil || contentType != jsonContentType {
				t.Fatalf("rewrite = %s, %q, %v", rewritten, contentType, err)
			}
			assertEmptyIndexArray(t, rewritten, test.key)
			merged, contentType, err := plugin.MergeGroupContent(
				format.Repository{Type: "group"}, test.path,
				[][]byte{[]byte(test.body), []byte(test.body)},
			)
			if err != nil || contentType != jsonContentType {
				t.Fatalf("merge = %s, %q, %v", merged, contentType, err)
			}
			assertEmptyIndexArray(t, merged, test.key)
		})
	}
}

func assertEmptyIndexArray(t *testing.T, body []byte, key string) {
	t.Helper()
	var document map[string]any
	if err := json.Unmarshal(body, &document); err != nil {
		t.Fatal(err)
	}
	entries, ok := document[key].([]any)
	if !ok || len(entries) != 0 {
		t.Fatalf("%s must be an empty array: %s", key, body)
	}
	extra, ok := document["extra"].(map[string]any)
	if !ok || extra["keep"] != true {
		t.Fatalf("unrelated metadata changed: %s", body)
	}
}
