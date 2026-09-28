package server

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"testing"

	"github.com/getkin/kin-openapi/openapi3"
)

func TestAssetIDDownloadConditionalContract(t *testing.T) {
	loader := openapi3.NewLoader()
	document, err := loader.LoadFromData(openAPITemplate)
	if err != nil {
		t.Fatal(err)
	}
	operation := document.Paths.Value("/api/v1/repositories/{name}/assets/{id}/download").Get
	parameters := make(map[string]bool)
	for _, parameter := range operation.Parameters {
		if parameter.Value.In == "header" {
			parameters[parameter.Value.Name] = true
		}
	}
	for _, name := range []string{"Range", "If-Range", "If-Match", "If-Unmodified-Since", "If-None-Match", "If-Modified-Since"} {
		if !parameters[name] {
			t.Errorf("missing request header %s", name)
		}
	}
	fixture := newServerFixture(t)
	uploaded := fixture.request(t, http.MethodPut, "/repository/raw/contract.bin", []byte("abcdef"), true)
	assertStatus(t, uploaded, http.StatusCreated)
	uploaded.Body.Close()
	asset, err := fixture.Metadata.Asset(context.Background(), "raw", "contract.bin")
	if err != nil {
		t.Fatal(err)
	}
	endpoint := fmt.Sprintf("/api/v1/repositories/raw/assets/%d/download", asset.ID)
	etag := `"` + asset.Digest + `"`
	for _, tc := range []struct {
		name    string
		headers map[string]string
		status  int
		body    string
	}{
		{"full", nil, 200, "abcdef"},
		{"range", map[string]string{"Range": "bytes=2-3", "If-Range": etag}, 206, "cd"},
		{"unchanged", map[string]string{"If-None-Match": etag}, 304, ""},
		{"precondition", map[string]string{"If-Match": `"other"`}, 412, ""},
		{"unsatisfiable", map[string]string{"Range": "bytes=100-"}, 416, ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			response := conditionalRead(t, fixture, http.MethodGet, endpoint, tc.headers)
			defer response.Body.Close()
			assertStatus(t, response, tc.status)
			definition := operation.Responses.Value(strconv.Itoa(response.StatusCode))
			if definition == nil {
				t.Fatalf("undocumented status %d", response.StatusCode)
			}
			body, err := io.ReadAll(response.Body)
			if err != nil {
				t.Fatal(err)
			}
			if tc.status == 416 {
				if definition.Value.Content["application/problem+json"] == nil {
					t.Fatal("missing problem response schema")
				}
				if response.Header.Get("Content-Range") != "bytes */6" {
					t.Fatal("missing unsatisfiable range length")
				}
			} else if string(body) != tc.body {
				t.Fatalf("body=%q want %q", body, tc.body)
			}
			if tc.status == 304 || tc.status == 412 {
				if len(definition.Value.Content) != 0 {
					t.Fatal("bodyless response declares content")
				}
			}
		})
	}
}
