package server

import (
	"bytes"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestGenericUploadStagingFailureIsInternal(t *testing.T) {
	fixture := newServerFixture(t)
	var logs bytes.Buffer
	fixture.Handler.setLogger(slog.New(slog.NewJSONHandler(&logs, nil)))
	if err := os.WriteFile(filepath.Join(fixture.Handler.cfg.DataDir, "uploads"), []byte("blocked"), 0o600); err != nil {
		t.Fatal(err)
	}

	response := fixture.request(t, http.MethodPut, "/repository/raw/sample.bin", []byte("valid bytes"), true)
	defer response.Body.Close()
	body, err := io.ReadAll(response.Body)
	if err != nil {
		t.Fatal(err)
	}
	if response.StatusCode != http.StatusInternalServerError || strings.Contains(string(body), fixture.Handler.cfg.DataDir) {
		t.Fatalf("staging failure: status=%d body=%s", response.StatusCode, body)
	}
	if !strings.Contains(logs.String(), fixture.Handler.cfg.DataDir) {
		t.Fatalf("staging error missing from server log: %s", logs.String())
	}
}

type failedUploadReader struct{}

func (failedUploadReader) Read([]byte) (int, error) {
	return 0, errors.New("broken request stream")
}

func TestGenericUploadBodyReadFailureIsBadRequest(t *testing.T) {
	fixture := newServerFixture(t)
	request := httptest.NewRequest(http.MethodPut, "/repository/raw/sample.bin", failedUploadReader{})
	request.Header.Set("Authorization", "Bearer "+testToken)
	response := httptest.NewRecorder()
	fixture.Handler.ServeHTTP(response, request)

	if response.Code != http.StatusBadRequest || !strings.Contains(response.Body.String(), "could not read upload body") {
		t.Fatalf("read failure: status=%d body=%s", response.Code, response.Body.String())
	}
}
