package npm_test

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"net/http"
	"testing"
	"time"
)

func TestNpmPublicationReportsExpectedFailuresWithoutChangingVersion(t *testing.T) {
	f := newFixture(t, time.Hour)
	mustCreate(t, f.createRepository(t, map[string]any{"name": "hosted", "format": "npm", "type": "hosted"}))
	publish := func(payload []byte, token string) (*http.Response, []byte) {
		t.Helper()
		body, err := json.Marshal(map[string]any{
			"name":         "widget",
			"versions":     map[string]any{"1.0.0": map[string]any{"name": "widget", "version": "1.0.0", "dist": map[string]any{}}},
			"_attachments": map[string]any{"widget-1.0.0.tgz": map[string]any{"data": base64.StdEncoding.EncodeToString(payload)}},
		})
		if err != nil {
			t.Fatal(err)
		}
		header := http.Header{"Content-Type": {"application/json"}}
		if token != "" {
			header.Set("Authorization", "Bearer "+token)
		}
		return f.do(t, http.MethodPut, "/repository/hosted/widget", body, header)
	}
	first, body := publish([]byte("original archive"), "")
	if first.StatusCode != http.StatusCreated {
		t.Fatalf("initial publish: %d %s", first.StatusCode, body)
	}
	packument, originalIndex := f.do(t, http.MethodGet, "/repository/hosted/widget", nil, nil)
	if packument.StatusCode != http.StatusOK {
		t.Fatalf("initial packument: %d %s", packument.StatusCode, originalIndex)
	}
	second, body := publish([]byte("different archive"), "")
	if second.StatusCode != http.StatusConflict || bytes.Contains(body, []byte("database")) {
		t.Fatalf("immutable conflict: %d %s", second.StatusCode, body)
	}
	idempotent, body := publish([]byte("original archive"), "")
	if idempotent.StatusCode != http.StatusCreated {
		t.Fatalf("byte-identical republish: %d %s", idempotent.StatusCode, body)
	}

	created, response := f.do(t, http.MethodPost, "/api/v1/users/admin/tokens",
		[]byte(`{"name":"read-only-publisher","scopes":["repository:hosted:read"]}`),
		http.Header{"Content-Type": {"application/json"}})
	if created.StatusCode != http.StatusCreated {
		t.Fatalf("create reader token: %d %s", created.StatusCode, response)
	}
	var token struct {
		Token string `json:"token"`
	}
	if err := json.Unmarshal(response, &token); err != nil || token.Token == "" {
		t.Fatalf("decode reader token: %v", err)
	}
	denied, body := publish([]byte("policy rejected archive"), token.Token)
	if denied.StatusCode != http.StatusForbidden {
		t.Fatalf("read-only publisher: %d %s", denied.StatusCode, body)
	}
	// The native parser's item budget is also a publication limit. It must
	// reject before committing any of the offered versions.
	versions := make(map[string]any, 65)
	for i := 0; i < 65; i++ {
		versions[string(rune(0x100+i))] = map[string]any{"name": "widget"}
	}
	oversized, err := json.Marshal(map[string]any{"name": "widget", "versions": versions})
	if err != nil {
		t.Fatal(err)
	}
	limited, body := f.do(t, http.MethodPut, "/repository/hosted/widget", oversized,
		http.Header{"Content-Type": {"application/json"}})
	if limited.StatusCode != http.StatusRequestEntityTooLarge {
		t.Fatalf("item limit: %d %s", limited.StatusCode, body)
	}
	artifact, bytesAfter := f.do(t, http.MethodGet, "/repository/hosted/widget/-/widget-1.0.0.tgz", nil, nil)
	if artifact.StatusCode != http.StatusOK || string(bytesAfter) != "original archive" {
		t.Fatalf("rejected publication changed archive: %d %s", artifact.StatusCode, bytesAfter)
	}
	index, indexAfter := f.do(t, http.MethodGet, "/repository/hosted/widget", nil, nil)
	if index.StatusCode != http.StatusOK || !bytes.Equal(indexAfter, originalIndex) {
		t.Fatalf("rejected publication changed metadata: %d %s", index.StatusCode, indexAfter)
	}
}

func TestNpmPublicationRejectsUnservableVersionsAndMetadata(t *testing.T) {
	f := newFixture(t, time.Hour)
	mustCreate(t, f.createRepository(t, map[string]any{"name": "hosted", "format": "npm", "type": "hosted"}))
	for _, test := range []struct {
		name     string
		version  string
		metadata any
	}{
		{"path segment", "../x", map[string]any{"name": "widget", "version": "../x"}},
		{"URL delimiter", "1.0.0?build", map[string]any{"name": "widget", "version": "1.0.0?build"}},
		{"non-semver", "banana", map[string]any{"name": "widget", "version": "banana"}},
		{"short semver", "1.0", map[string]any{"name": "widget", "version": "1.0"}},
		{"null", "1.0.0", nil},
		{"array", "1.0.0", []string{"widget"}},
		{"name mismatch", "1.0.0", map[string]any{"name": "other", "version": "1.0.0", "dist": map[string]any{}}},
		{"version mismatch", "1.0.0", map[string]any{"name": "widget", "version": "2.0.0", "dist": map[string]any{}}},
		{"missing dist", "1.0.0", map[string]any{"name": "widget", "version": "1.0.0"}},
		{"null dist", "1.0.0", map[string]any{"name": "widget", "version": "1.0.0", "dist": nil}},
	} {
		t.Run(test.name, func(t *testing.T) {
			body, err := json.Marshal(map[string]any{
				"name": "widget",
				"versions": map[string]any{
					"1.0.0+valid": map[string]any{"name": "widget", "version": "1.0.0+valid", "dist": map[string]any{}},
					test.version:  test.metadata,
				},
				"_attachments": map[string]any{
					"widget-1.0.0+valid.tgz":          map[string]any{"data": "YWJj"},
					"widget-" + test.version + ".tgz": map[string]any{"data": "YWJj"},
				},
			})
			if err != nil {
				t.Fatal(err)
			}
			response, data := f.do(t, http.MethodPut, "/repository/hosted/widget", body, http.Header{"Content-Type": {"application/json"}})
			if response.StatusCode != http.StatusBadRequest {
				t.Fatalf("invalid publication = %d %s", response.StatusCode, data)
			}
			response, data = f.do(t, http.MethodGet, "/repository/hosted/widget", nil, nil)
			if response.StatusCode != http.StatusNotFound {
				t.Fatalf("rejected publication left a version: %d %s", response.StatusCode, data)
			}
		})
	}
	body, err := json.Marshal(map[string]any{
		"name": "widget",
		"versions": map[string]any{
			"1.0.0+build.1": map[string]any{"name": "widget", "version": "1.0.0+build.1", "dist": map[string]any{}},
		},
		"_attachments": map[string]any{"one.tgz": map[string]any{"data": "YWJj"}},
	})
	if err != nil {
		t.Fatal(err)
	}
	response, data := f.do(t, http.MethodPut, "/repository/hosted/widget", body, http.Header{"Content-Type": {"application/json"}})
	if response.StatusCode != http.StatusCreated {
		t.Fatalf("safe build version publication = %d %s", response.StatusCode, data)
	}
	response, data = f.do(t, http.MethodGet, "/repository/hosted/widget", nil, nil)
	if response.StatusCode != http.StatusOK || !bytes.Contains(data, []byte(`"1.0.0+build.1"`)) {
		t.Fatalf("safe build version packument = %d %s", response.StatusCode, data)
	}
}
