package server

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/suxen-project/suxen/internal/domain"
)

func TestAbandonedGenericStagingReclaimedAfterRestart(t *testing.T) {
	dir := t.TempDir()
	initial := openSQLiteRecoveryServer(t, dir)
	staged, err := initial.handler.content.StageUpload(nil, strings.NewReader("abandoned request bytes"))
	if err != nil {
		t.Fatal(err)
	}
	stopRecoveryServer(t, initial)
	old := time.Now().Add(-72 * time.Hour)
	if err := os.Chtimes(staged.Path, old, old); err != nil {
		t.Fatal(err)
	}
	restarted := openSQLiteRecoveryServer(t, dir)
	defer stopRecoveryServer(t, restarted)
	preview, err := restarted.handler.runGarbageCollection(context.Background(), true, 0, "")
	if err != nil {
		t.Fatal(err)
	}
	if preview.StaleStagingFiles != 1 || preview.StaleStagingFilesDeleted != 0 {
		t.Fatalf("staging GC preview = %+v", preview)
	}
	result, err := restarted.handler.runGarbageCollection(context.Background(), false, 0, "")
	if err != nil {
		t.Fatal(err)
	}
	if result.StaleStagingFiles != 1 || result.StaleStagingFilesDeleted != 1 {
		t.Fatalf("staging GC result = %+v", result)
	}
	if _, err := os.Stat(staged.Path); !os.IsNotExist(err) {
		t.Fatalf("abandoned staging file after GC: %v", err)
	}
}

func TestRewrittenProxyIndexRecordsOnlySuccessfulGET(t *testing.T) {
	f := newServerFixture(t)
	createTestRepository(t, f, domain.Repository{Name: "npm-proxy", Format: "npm", Type: "proxy", Upstream: "https://registry.example"})
	f.Handler.cfg.ProxyManifestTTL = 0
	f.Handler.content.SetConfig(f.Handler.cfg)
	f.Handler.setHTTPClient(&http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
		if r.Header.Get("If-None-Match") != "" {
			return testHTTPResponse(r, 304, ""), nil
		}
		return testHTTPResponse(r, 200, `{"versions":{}}`), nil
	})})
	request := func(method string, headers map[string]string, writer http.ResponseWriter) {
		req := httptest.NewRequest(method, "/repository/npm-proxy/pkg", nil)
		req.Header.Set("Authorization", "Bearer "+testToken)
		for key, value := range headers {
			req.Header.Set(key, value)
		}
		f.Handler.ServeHTTP(writer, req)
		f.Handler.backgroundTasks.Wait()
	}
	first := httptest.NewRecorder()
	request(http.MethodGet, nil, first)
	if first.Code != http.StatusOK {
		t.Fatalf("initial GET = %d", first.Code)
	}
	asset, err := f.Metadata.Asset(context.Background(), "npm-proxy", "pkg")
	if err != nil {
		t.Fatal(err)
	}
	last := *asset.LastAccessed
	assertUnchanged := func(name string) {
		t.Helper()
		current, err := f.Metadata.Asset(context.Background(), "npm-proxy", "pkg")
		if err != nil {
			t.Fatal(err)
		}
		if !current.LastAccessed.Equal(last) {
			t.Fatalf("%s changed last access: %v -> %v", name, last, current.LastAccessed)
		}
	}
	request(http.MethodHead, nil, httptest.NewRecorder())
	assertUnchanged("HEAD")
	request(http.MethodGet, map[string]string{"If-None-Match": first.Header().Get("ETag")}, httptest.NewRecorder())
	assertUnchanged("conditional GET")
	request(http.MethodGet, nil, &failedBodyWriter{header: make(http.Header)})
	assertUnchanged("failed response write")
	good := httptest.NewRecorder()
	request(http.MethodGet, nil, good)
	if good.Code != http.StatusOK {
		t.Fatalf("rewritten GET = %d: %s", good.Code, good.Body.String())
	}
	current, err := f.Metadata.Asset(context.Background(), "npm-proxy", "pkg")
	if err != nil {
		t.Fatal(err)
	}
	if !current.LastAccessed.After(last) {
		t.Fatalf("successful GET did not update last access: %v -> %v", last, current.LastAccessed)
	}
}

type failedBodyWriter struct{ header http.Header }

func (w *failedBodyWriter) Header() http.Header { return w.header }
func (w *failedBodyWriter) WriteHeader(int)     {}
func (w *failedBodyWriter) Write([]byte) (int, error) {
	return 0, errors.New("client disconnected")
}

func TestMergedProxyIndexRecordsSourceAccess(t *testing.T) {
	f := newServerFixture(t)
	for _, repository := range []domain.Repository{
		{Name: "npm-member", Format: "npm", Type: "proxy", Upstream: "https://registry.example"},
		{Name: "npm-group", Format: "npm", Type: "group", Members: []string{"npm-member"}},
	} {
		createTestRepository(t, f, repository)
	}
	f.Handler.setHTTPClient(&http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
		return testHTTPResponse(r, 200, `{"versions":{}}`), nil
	})})
	response := f.request(t, http.MethodGet, "/repository/npm-member/pkg", nil, true)
	assertStatus(t, response, http.StatusOK)
	response.Body.Close()
	f.Handler.backgroundTasks.Wait()
	before, err := f.Metadata.Asset(context.Background(), "npm-member", "pkg")
	if err != nil {
		t.Fatal(err)
	}
	response = f.request(t, http.MethodGet, "/repository/npm-group/pkg", nil, true)
	if response.StatusCode != http.StatusOK {
		t.Fatalf("merged group GET = %d: %s", response.StatusCode, fmt.Sprint(response.Body))
	}
	response.Body.Close()
	f.Handler.backgroundTasks.Wait()
	after, err := f.Metadata.Asset(context.Background(), "npm-member", "pkg")
	if err != nil {
		t.Fatal(err)
	}
	if !after.LastAccessed.After(*before.LastAccessed) {
		t.Fatalf("merged GET did not update source last access: %v -> %v", before.LastAccessed, after.LastAccessed)
	}
}
