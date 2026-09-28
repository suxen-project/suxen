package pypi_test

import (
	"bytes"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func TestPyPILargeRootIndexThroughProxyAndGroup(t *testing.T) {
	var page bytes.Buffer
	page.WriteString(`{"meta":{"api-version":"1.0"},"projects":[`)
	count := 0
	for page.Len() < 44_176_586 {
		if count != 0 {
			page.WriteByte(',')
		}
		fmt.Fprintf(&page, `{"name":"package-with-a-realistically-long-project-name-%06d"}`, count)
		count++
	}
	page.WriteString(`]}`)
	t.Logf("root source: %d projects, %d bytes", count, page.Len())
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/vnd.pypi.simple.v1+json")
		_, _ = w.Write(page.Bytes())
	}))
	defer upstream.Close()
	f := newFixture(t, time.Hour, 128<<20)
	mustCreate(t, f.createRepository(t, map[string]any{"name": "proxy", "format": "pypi", "type": "proxy", "upstream": upstream.URL}))
	mustCreate(t, f.createRepository(t, map[string]any{"name": "group", "format": "pypi", "type": "group", "members": []string{"proxy"}}))
	for _, repository := range []string{"proxy", "group"} {
		response, body := f.do(t, http.MethodGet, "/repository/"+repository+"/simple/", nil, http.Header{"Accept": {"text/html"}})
		if response.StatusCode != http.StatusOK {
			t.Fatalf("%s root = %d %s", repository, response.StatusCode, body)
		}
		if len(body) <= 64<<20 {
			t.Fatalf("%s rewritten root size = %d, want more than 64 MiB", repository, len(body))
		}
		if !bytes.Contains(body, []byte(f.suxen.URL+"/repository/"+repository+"/simple/package-with-a-realistically-long-project-name-000000/")) ||
			!bytes.Contains(body, []byte(fmt.Sprintf("package-with-a-realistically-long-project-name-%06d", count-1))) {
			t.Fatalf("%s root lost first or last project", repository)
		}
	}
}

func TestPyPIRootGroupThroughSameServerProxy(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html")
		_, _ = w.Write([]byte(`<html><a href="/simple/widget/">widget</a></html>`))
	}))
	defer upstream.Close()
	f := newFixture(t, time.Hour)
	mustCreate(t, f.createRepository(t, map[string]any{"name": "base", "format": "pypi", "type": "proxy", "upstream": upstream.URL}))
	mustCreate(t, f.createRepository(t, map[string]any{"name": "inner", "format": "pypi", "type": "group", "members": []string{"base"}}))
	mustCreate(t, f.createRepository(t, map[string]any{"name": "nested", "format": "pypi", "type": "proxy", "upstream": f.suxen.URL + "/repository/inner"}))
	mustCreate(t, f.createRepository(t, map[string]any{"name": "outer", "format": "pypi", "type": "group", "members": []string{"nested"}}))
	client := &http.Client{Timeout: 5 * time.Second}
	request, err := http.NewRequest(http.MethodGet, f.suxen.URL+"/repository/outer/simple/", nil)
	if err != nil {
		t.Fatal(err)
	}
	request.Header.Set("Authorization", "Bearer "+adminToken)
	response, err := client.Do(request)
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		t.Fatalf("nested root = %d", response.StatusCode)
	}
	if got := response.Header.Get("Content-Type"); !strings.Contains(got, "html") {
		t.Fatalf("nested root content type = %s", got)
	}
}
