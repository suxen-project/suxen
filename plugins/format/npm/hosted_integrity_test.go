package npm_test

import (
	"crypto/sha1"
	"crypto/sha256"
	"crypto/sha512"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"strings"
	"testing"
	"time"
)

func TestNpmHostedPublishVerifiesAttachmentDigests(t *testing.T) {
	f := newFixture(t, time.Hour)
	mustCreate(t, f.createRepository(t, map[string]any{"name": "hosted", "format": "npm", "type": "hosted"}))
	for _, test := range []struct {
		name      string
		integrity func([]byte) any
		shasum    func([]byte) any
		status    int
	}{
		{name: "matching digests", integrity: sha512Integrity, shasum: sha1Shasum, status: http.StatusCreated},
		{name: "matching multiple integrity algorithms", integrity: multipleIntegrity, status: http.StatusCreated},
		{name: "matching legacy sha1 integrity", integrity: sha1Integrity, status: http.StatusCreated},
		{name: "matching integrity with options", integrity: func(content []byte) any { return sha512Integrity(content).(string) + "?source=npm" }, status: http.StatusCreated},
		{name: "wrong integrity", integrity: func([]byte) any { return "sha512-" + base64.StdEncoding.EncodeToString(make([]byte, sha512.Size)) }, status: http.StatusBadRequest},
		{name: "wrong shasum", shasum: func([]byte) any { return strings.Repeat("0", sha1.Size*2) }, status: http.StatusBadRequest},
		{name: "malformed integrity", integrity: func([]byte) any { return "sha512-???" }, status: http.StatusBadRequest},
		{name: "invalid shasum type", shasum: func([]byte) any { return 123 }, status: http.StatusBadRequest},
	} {
		t.Run(test.name, func(t *testing.T) {
			name := "widget-" + strings.ReplaceAll(test.name, " ", "-")
			payload := tarball(t, name, "1.0.0")
			dist := map[string]any{}
			if test.integrity != nil {
				dist["integrity"] = test.integrity(payload)
			}
			if test.shasum != nil {
				dist["shasum"] = test.shasum(payload)
			}
			body, err := json.Marshal(map[string]any{
				"name": name,
				"versions": map[string]any{"1.0.0": map[string]any{
					"name": name, "version": "1.0.0", "dist": dist,
				}},
				"_attachments": map[string]any{name + "-1.0.0.tgz": map[string]any{
					"data": base64.StdEncoding.EncodeToString(payload),
				}},
			})
			if err != nil {
				t.Fatal(err)
			}
			response, content := f.do(t, http.MethodPut, "/repository/hosted/"+name, body,
				http.Header{"Content-Type": {"application/json"}})
			if response.StatusCode != test.status {
				t.Fatalf("publish status = %d, want %d: %s", response.StatusCode, test.status, content)
			}
			if test.status == http.StatusBadRequest {
				response, _ = f.do(t, http.MethodGet, "/repository/hosted/"+name, nil, nil)
				if response.StatusCode != http.StatusNotFound {
					t.Fatalf("rejected package is visible: %d", response.StatusCode)
				}
			}
		})
	}
}

func sha512Integrity(content []byte) any {
	sum := sha512.Sum512(content)
	return "sha512-" + base64.StdEncoding.EncodeToString(sum[:])
}

func multipleIntegrity(content []byte) any {
	sum256 := sha256.Sum256(content)
	return "sha256-" + base64.StdEncoding.EncodeToString(sum256[:]) + " " + sha512Integrity(content).(string)
}

func sha1Shasum(content []byte) any {
	sum := sha1.Sum(content)
	return hex.EncodeToString(sum[:])
}

func sha1Integrity(content []byte) any {
	sum := sha1.Sum(content)
	return "sha1-" + base64.StdEncoding.EncodeToString(sum[:])
}
