package cargo_test

import (
	"bytes"
	"crypto/sha256"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

func TestCargoProxyAndGroupRequireIndexedVersionSpelling(t *testing.T) {
	payload := []byte("same crate bytes")
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/config.json":
			fmt.Fprintf(w, `{"dl":"http://%s/dl/{crate}/{version}/download"}`, r.Host)
		case "/wi/dg/widget":
			fmt.Fprintf(w, `{"name":"widget","vers":"1.0.0+one","deps":[],"cksum":"%x","features":{},"yanked":false}`, sha256.Sum256(payload))
		case "/dl/widget/1.0.0+one/download", "/dl/widget/1.0.0+two/download":
			w.Write(payload)
		default:
			http.NotFound(w, r)
		}
	}))
	defer upstream.Close()
	f := newFixture(t, time.Hour)
	mustCreate(t, f.createRepository(t, map[string]any{"name": "proxy", "format": "cargo", "type": "proxy", "upstream": upstream.URL}))
	mustCreate(t, f.createRepository(t, map[string]any{"name": "group", "format": "cargo", "type": "group", "members": []string{"proxy"}}))
	for _, path := range []string{"config.json", "wi/dg/widget"} {
		response, body := f.do(t, "GET", "/repository/group/"+path, nil, nil)
		if response.StatusCode != 200 {
			t.Fatalf("warm %s: %d %s", path, response.StatusCode, body)
		}
	}
	for _, repository := range []string{"proxy", "group"} {
		response, body := f.do(t, "GET", "/repository/"+repository+"/dl/widget/1.0.0+one/download", nil, nil)
		if response.StatusCode != http.StatusOK || string(body) != string(payload) {
			t.Errorf("advertised build variant via %s: %d %q", repository, response.StatusCode, body)
		}
		response, body = f.do(t, "GET", "/repository/"+repository+"/dl/widget/1.0.0+two/download", nil, nil)
		if response.StatusCode != http.StatusNotFound {
			t.Errorf("unadvertised build variant via %s: %d %q", repository, response.StatusCode, body)
		}
	}
}

func TestCargoClientFetchesIndexedBuildMetadataVersion(t *testing.T) {
	requireCargo(t)
	f := newFixture(t, time.Hour)
	reg := newRegistry(t, map[string][]string{"hello": {"0.1.0+one"}})
	mustCreate(t, f.createRepository(t, map[string]any{"name": "proxy", "format": "cargo", "type": "proxy", "upstream": reg.index.URL}))
	project := newProject(t, f, "proxy")
	runCargo(t, cargoEnvironment(t), project, "fetch")
	if reg.hits.Load() == 0 {
		t.Fatal("native Cargo made no registry requests")
	}
}

func TestCargoGroupKeepsFirstCanonicalBuildVariant(t *testing.T) {
	f := newFixture(t, time.Hour)
	for _, member := range []struct{ name, version, payload string }{
		{"first", "1.0.0+one", "first bytes"},
		{"second", "1.0.0+two", "second bytes"},
	} {
		member := member
		upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			switch r.URL.Path {
			case "/config.json":
				fmt.Fprintf(w, `{"dl":"http://%s/dl/{crate}/{version}/download"}`, r.Host)
			case "/wi/dg/widget":
				fmt.Fprintf(w, `{"name":"widget","vers":%q,"deps":[],"cksum":"%x","features":{},"yanked":false}`, member.version, sha256.Sum256([]byte(member.payload)))
			case "/dl/widget/" + member.version + "/download":
				fmt.Fprint(w, member.payload)
			default:
				http.NotFound(w, r)
			}
		}))
		t.Cleanup(upstream.Close)
		mustCreate(t, f.createRepository(t, map[string]any{"name": member.name, "format": "cargo", "type": "proxy", "upstream": upstream.URL}))
		for _, path := range []string{"config.json", "wi/dg/widget"} {
			response, body := f.do(t, http.MethodGet, "/repository/"+member.name+"/"+path, nil, nil)
			if response.StatusCode != http.StatusOK {
				t.Fatalf("warm %s %s: %d %s", member.name, path, response.StatusCode, body)
			}
		}
	}
	mustCreate(t, f.createRepository(t, map[string]any{"name": "group", "format": "cargo", "type": "group", "members": []string{"first", "second"}}))
	response, body := f.do(t, http.MethodGet, "/repository/group/wi/dg/widget", nil, nil)
	if response.StatusCode != http.StatusOK || !bytes.Contains(body, []byte(`"vers":"1.0.0+one"`)) || bytes.Contains(body, []byte(`"vers":"1.0.0+two"`)) {
		t.Fatalf("group index must advertise first build spelling: %d %s", response.StatusCode, body)
	}
	for _, test := range []struct {
		repository, version, payload string
		status                       int
	}{
		{"first", "1.0.0+one", "first bytes", http.StatusOK},
		{"second", "1.0.0+two", "second bytes", http.StatusOK},
		{"group", "1.0.0+one", "first bytes", http.StatusOK},
		{"group", "1.0.0+two", "", http.StatusNotFound},
	} {
		response, body = f.do(t, http.MethodGet, "/repository/"+test.repository+"/dl/widget/"+test.version+"/download", nil, nil)
		if response.StatusCode != test.status || test.status == http.StatusOK && string(body) != test.payload {
			t.Errorf("%s %s: %d %q, want %d %q", test.repository, test.version, response.StatusCode, body, test.status, test.payload)
		}
	}
}
