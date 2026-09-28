package npm_test

import (
	"bytes"
	"encoding/binary"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"testing"
	"time"
)

func publishCleanupFixture(t *testing.T, f *fixture, typ, version string) {
	t.Helper()
	var body []byte
	upload := "widget"
	if typ == "npm" {
		body = []byte(fmt.Sprintf(`{"name":"widget","versions":{%q:{"name":"widget","version":%q,"dist":{}}},"_attachments":{"one.tgz":{"data":"YWJj"}}}`, version, version))
	} else {
		upload = "api/v1/crates/new"
		meta := []byte(fmt.Sprintf(`{"name":"widget","vers":%q,"deps":[],"features":{}}`, version))
		var b bytes.Buffer
		binary.Write(&b, binary.LittleEndian, uint32(len(meta)))
		b.Write(meta)
		binary.Write(&b, binary.LittleEndian, uint32(3))
		b.WriteString("abc")
		body = b.Bytes()
	}
	resp, b := f.do(t, "PUT", "/repository/hosted/"+upload, body, http.Header{"Content-Type": {"application/json"}})
	if resp.StatusCode != 200 && resp.StatusCode != 201 {
		t.Fatalf("publish %d %s", resp.StatusCode, b)
	}
}
func TestCleanupHostedIndex(t *testing.T) {
	for _, typ := range []string{"npm", "cargo"} {
		t.Run(typ, func(t *testing.T) {
			f := newFixture(t, time.Hour)
			mustCreate(t, f.createRepository(t, map[string]any{"name": "hosted", "format": typ, "type": "hosted"}))
			publishCleanupFixture(t, f, typ, "1.0.0")
			resp, b := f.do(t, "POST", "/api/v1/cleanup-policies", []byte(`{"name":"sweep","repositories":["hosted"],"criteria":[{"path":"sys.path","op":"exists"}],"keepLast":0,"action":"delete","enabled":false}`), http.Header{"Content-Type": {"application/json"}})
			if resp.StatusCode != 201 {
				t.Fatalf("policy %d %s", resp.StatusCode, b)
			}
			path := "widget/-/widget-1.0.0.tgz"
			index := "widget"
			metadataPath := "widget/-/metadata/1.0.0.json"
			if typ == "cargo" {
				path = "dl/widget/1.0.0/download"
				index = "wi/dg/widget"
				metadataPath = "index-meta/widget/1.0.0.json"
			}
			wantDeleted := []string{path, metadataPath}
			if typ == "cargo" {
				wantDeleted = append(wantDeleted, "index-claim/widget/1.0.0.txt")
			}
			resp, b = f.do(t, "POST", "/api/v1/cleanup-policies/sweep/run?dryRun=true", nil, nil)
			var preview struct {
				Tasks []struct {
					Result struct {
						WouldDelete []string `json:"wouldDelete"`
					} `json:"result"`
				} `json:"tasks"`
			}
			if resp.StatusCode != 200 || json.Unmarshal(b, &preview) != nil || len(preview.Tasks) != 1 ||
				!slices.Equal(preview.Tasks[0].Result.WouldDelete, wantDeleted) {
				t.Fatalf("dry run did not describe artifact and metadata: %d %s", resp.StatusCode, b)
			}
			resp, b = f.do(t, "POST", "/api/v1/cleanup-policies/sweep/run?dryRun=false", nil, nil)
			t.Logf("cleanup %d %s", resp.StatusCode, b)
			resp, b = f.do(t, "GET", "/repository/hosted/"+path, nil, nil)
			if resp.StatusCode != 404 {
				t.Fatalf("deleted artifact %d %s", resp.StatusCode, b)
			}
			resp, b = f.do(t, "GET", "/repository/hosted/"+index, nil, nil)
			if resp.StatusCode == 200 && strings.Contains(string(b), "1.0.0") {
				t.Errorf("index still advertises deleted artifact: %d %s", resp.StatusCode, b)
			}
		})
	}
}
func TestCargoKeepLast(t *testing.T) {
	f := newFixture(t, time.Hour)
	mustCreate(t, f.createRepository(t, map[string]any{"name": "hosted", "format": "cargo", "type": "hosted"}))
	publishCleanupFixture(t, f, "cargo", "1.0.0")
	publishCleanupFixture(t, f, "cargo", "2.0.0")
	resp, b := f.do(t, "POST", "/api/v1/cleanup-policies", []byte(`{"name":"sweep","repositories":["hosted"],"criteria":[{"path":"sys.path","op":"exists"}],"keepLast":1,"action":"delete","enabled":false}`), http.Header{"Content-Type": {"application/json"}})
	if resp.StatusCode != 201 {
		t.Fatalf("policy %d %s", resp.StatusCode, b)
	}
	resp, b = f.do(t, "POST", "/api/v1/cleanup-policies/sweep/run?dryRun=false", nil, nil)
	t.Logf("cleanup %d %s", resp.StatusCode, b)
	resp, b = f.do(t, "GET", "/repository/hosted/dl/widget/1.0.0/download", nil, nil)
	if resp.StatusCode != 404 {
		t.Fatalf("old version not removed: %d %s", resp.StatusCode, b)
	}
}
func TestNpmTarballURL(t *testing.T) {
	for _, suffix := range []string{"/downloads/widget-1.0.0.tgz", "/widget/-/widget-1.0.0.tgz?sig=one"} {
		t.Run(suffix, func(t *testing.T) {
			f := newFixture(t, time.Hour)
			cdn := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { fmt.Fprint(w, "artifact") }))
			defer cdn.Close()
			up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Path == "/widget" {
					fmt.Fprintf(w, `{"name":"widget","versions":{"1.0.0":{"dist":{"tarball":%q}}}}`, cdn.URL+suffix)
				} else {
					http.NotFound(w, r)
				}
			}))
			defer up.Close()
			mustCreate(t, f.createRepository(t, map[string]any{"name": "proxy", "format": "npm", "type": "proxy", "upstream": up.URL}))
			resp, b := f.do(t, "GET", "/repository/proxy/widget", nil, nil)
			if resp.StatusCode != 200 {
				t.Fatal(resp.StatusCode)
			}
			var p struct {
				Versions map[string]struct {
					Dist struct {
						Tarball string `json:"tarball"`
					} `json:"dist"`
				} `json:"versions"`
			}
			if err := json.Unmarshal(b, &p); err != nil {
				t.Fatal(err)
			}
			link := p.Versions["1.0.0"].Dist.Tarball
			resp, b = f.do(t, "GET", strings.TrimPrefix(link, f.suxen.URL), nil, nil)
			if resp.StatusCode != 200 || string(b) != "artifact" {
				t.Errorf("advertised tarball %s = %d %s", link, resp.StatusCode, b)
			}
		})
	}
}

func TestNpmGroupUsesExactSignedCDNURL(t *testing.T) {
	f := newFixture(t, time.Hour)
	cdn := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/downloads/object" || r.URL.RawQuery != "sig=one" || r.Header.Get("Authorization") != "" {
			http.Error(w, "wrong CDN request", http.StatusForbidden)
			return
		}
		fmt.Fprint(w, "artifact")
	}))
	defer cdn.Close()
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/widget" {
			http.NotFound(w, r)
			return
		}
		fmt.Fprintf(w, `{"name":"widget","versions":{"1.0.0":{"dist":{"tarball":%q}}}}`, cdn.URL+"/downloads/object?sig=one")
	}))
	defer up.Close()
	mustCreate(t, f.createRepository(t, map[string]any{"name": "proxy", "format": "npm", "type": "proxy", "upstream": strings.Replace(up.URL, "://", "://user:password@", 1)}))
	mustCreate(t, f.createRepository(t, map[string]any{"name": "group", "format": "npm", "type": "group", "members": []string{"proxy"}}))
	response, body := f.do(t, "GET", "/repository/group/widget", nil, nil)
	if response.StatusCode != http.StatusOK {
		t.Fatalf("packument = %d %s", response.StatusCode, body)
	}
	var document struct {
		Versions map[string]struct {
			Dist struct {
				Tarball string `json:"tarball"`
			} `json:"dist"`
		} `json:"versions"`
	}
	if err := json.Unmarshal(body, &document); err != nil {
		t.Fatal(err)
	}
	link := document.Versions["1.0.0"].Dist.Tarball
	if !strings.HasPrefix(link, f.suxen.URL+"/repository/group/widget/-/") {
		t.Fatalf("group tarball link = %s", link)
	}
	response, body = f.do(t, "GET", strings.TrimPrefix(link, f.suxen.URL), nil, nil)
	if response.StatusCode != http.StatusOK || string(body) != "artifact" {
		t.Fatalf("signed group tarball = %d %s", response.StatusCode, body)
	}
}
