package npm_test

import (
	"bytes"
	"encoding/binary"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	_ "github.com/suxen-project/suxen/plugins/format/cargo"
	_ "github.com/suxen-project/suxen/plugins/format/pypi"
)

func TestHostedGroupsIncludeWireProtocolIndexes(t *testing.T) {
	for _, typ := range []string{"npm", "cargo", "pypi"} {
		t.Run(typ, func(t *testing.T) {
			f := newFixture(t, time.Hour)
			empty := httptest.NewServer(http.NotFoundHandler())
			t.Cleanup(empty.Close)
			mustCreate(t, f.createRepository(t, map[string]any{"name": "hosted", "format": typ, "type": "hosted"}))
			mustCreate(t, f.createRepository(t, map[string]any{"name": "empty", "format": typ, "type": "proxy", "upstream": empty.URL}))
			mustCreate(t, f.createRepository(t, map[string]any{"name": "all", "format": typ, "type": "group", "members": []string{"hosted"}}))
			mustCreate(t, f.createRepository(t, map[string]any{"name": "mixed", "format": typ, "type": "group", "members": []string{"empty", "hosted"}}))
			var body []byte
			var artifact []byte
			var method, upload, index, download, ct string
			switch typ {
			case "npm":
				method = "PUT"
				upload = "widget"
				index = "widget"
				download = "widget/-/widget-1.0.0.tgz"
				ct = "application/json"
				artifact = []byte("abc")
				body = []byte(`{"name":"widget","versions":{"1.0.0":{"name":"widget","version":"1.0.0","dist":{}}},"_attachments":{"widget-1.0.0.tgz":{"data":"YWJj"}}}`)
			case "cargo":
				method = "PUT"
				upload = "api/v1/crates/new"
				index = "wi/dg/widget"
				download = "dl/widget/1.0.0/download"
				ct = "application/octet-stream"
				meta := []byte(`{"name":"widget","vers":"1.0.0","deps":[],"features":{}}`)
				var b bytes.Buffer
				binary.Write(&b, binary.LittleEndian, uint32(len(meta)))
				b.Write(meta)
				binary.Write(&b, binary.LittleEndian, uint32(3))
				b.WriteString("abc")
				body = b.Bytes()
				artifact = []byte("abc")
			case "pypi":
				method = "POST"
				upload = ""
				index = "simple/widget/"
				download = "packages/widget/widget-1.0.0.tar.gz"
				var b bytes.Buffer
				mw := multipart.NewWriter(&b)
				mw.WriteField("name", "widget")
				file, _ := mw.CreateFormFile("content", "widget-1.0.0.tar.gz")
				artifact = tarball(t, "widget", "1.0.0")
				file.Write(artifact)
				mw.Close()
				ct = mw.FormDataContentType()
				body = b.Bytes()
			}
			resp, b := f.do(t, method, "/repository/hosted/"+upload, body, http.Header{"Content-Type": {ct}})
			if resp.StatusCode != 200 && resp.StatusCode != 201 {
				t.Fatalf("publish %d %s", resp.StatusCode, b)
			}
			resp, b = f.do(t, "GET", "/repository/hosted/"+index, nil, nil)
			if resp.StatusCode != 200 {
				t.Fatalf("hosted index %d %s", resp.StatusCode, b)
			}
			resp, b = f.do(t, "GET", "/repository/hosted/"+download, nil, nil)
			if resp.StatusCode != 200 {
				t.Fatalf("hosted artifact %d %s", resp.StatusCode, b)
			}
			resp, b = f.do(t, "GET", "/repository/all/"+download, nil, nil)
			if resp.StatusCode != 200 || !bytes.Equal(b, artifact) {
				t.Errorf("group download %d (%d bytes)", resp.StatusCode, len(b))
			}
			resp, b = f.do(t, "GET", "/repository/all/"+index, nil, nil)
			if resp.StatusCode != 200 {
				t.Errorf("group index unavailable despite hosted index/artifact=200: %d %s", resp.StatusCode, b)
			}
			if typ != "cargo" && !strings.Contains(string(b), "/repository/all/") {
				t.Errorf("group index does not route downloads through the group: %s", b)
			}
			if typ == "cargo" {
				resp, b = f.do(t, "GET", "/repository/all/config.json", nil, nil)
				if resp.StatusCode != 200 || !strings.Contains(string(b), "/repository/all/dl/") {
					t.Errorf("Cargo group config = %d %s", resp.StatusCode, b)
				}
			}
			resp, b = f.do(t, "GET", "/repository/mixed/"+index, nil, nil)
			if resp.StatusCode != 200 {
				t.Errorf("mixed group index = %d %s", resp.StatusCode, b)
			}
			resp, b = f.do(t, "GET", "/repository/mixed/"+download, nil, nil)
			if resp.StatusCode != 200 || !bytes.Equal(b, artifact) {
				t.Errorf("mixed group download %d (%d bytes)", resp.StatusCode, len(b))
			}
			resp, b = f.do(
				t,
				http.MethodPut,
				"/api/v1/repositories/hosted/download-gate",
				[]byte(`{"criteria":[{"path":"scan.status","op":"=","value":"passed"}]}`),
				http.Header{"Content-Type": {"application/json"}},
			)
			if resp.StatusCode != http.StatusCreated {
				t.Fatalf("configure hosted download gate = %d %s", resp.StatusCode, b)
			}
			resp, b = f.do(t, "GET", "/repository/all/"+download, nil, nil)
			if resp.StatusCode != http.StatusForbidden {
				t.Errorf("group bypassed hosted member download gate: %d %s", resp.StatusCode, b)
			}
		})
	}
}
