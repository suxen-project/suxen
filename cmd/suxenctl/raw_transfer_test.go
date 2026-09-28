package main

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/suxen-project/suxen/internal/rawpath"
)

func TestRawCommandEscapesAssetPath(t *testing.T) {
	asset := "release notes/what? #1/100% café.txt"
	want, err := rawpath.URLPath("raw", asset)
	if err != nil {
		t.Fatal(err)
	}
	var received string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		received = r.URL.EscapedPath()
		w.WriteHeader(http.StatusNoContent)
	}))
	defer server.Close()
	api := &client{baseURL: server.URL, http: newHTTPClient()}
	if err := rawCommand(api, []string{"delete", "raw", asset}); err != nil {
		t.Fatal(err)
	}
	if received != want {
		t.Fatalf("request path = %q, want %q", received, want)
	}
	if err := rawCommand(api, []string{"delete", "raw", "a/../b"}); err == nil {
		t.Fatal("accepted traversal")
	}
}

func TestDownloadReplacesOnlyAfterCompleteCopy(t *testing.T) {
	directory := t.TempDir()
	destination := filepath.Join(directory, "artifact")
	if err := os.WriteFile(destination, []byte("previous"), 0o600); err != nil {
		t.Fatal(err)
	}
	copyFailure := errors.New("broken transfer")
	api := &client{baseURL: "http://suxen.test", http: &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
		return &http.Response{StatusCode: http.StatusOK, Body: io.NopCloser(&brokenReader{err: copyFailure}), Header: make(http.Header), Request: r}, nil
	})}}
	if err := api.download("/repository/raw/artifact", destination); !errors.Is(err, copyFailure) {
		t.Fatalf("download error = %v", err)
	}
	contents, err := os.ReadFile(destination)
	if err != nil || string(contents) != "previous" {
		t.Fatalf("destination = %q, error = %v", contents, err)
	}
	entries, err := os.ReadDir(directory)
	if err != nil || len(entries) != 1 {
		t.Fatalf("staging files remain: %v, error = %v", entries, err)
	}

	api.http.Transport = roundTripFunc(func(r *http.Request) (*http.Response, error) {
		return &http.Response{StatusCode: http.StatusOK, Body: io.NopCloser(strings.NewReader("complete")), Header: make(http.Header), Request: r}, nil
	})
	if err := api.download("/repository/raw/artifact", destination); err != nil {
		t.Fatal(err)
	}
	contents, err = os.ReadFile(destination)
	if err != nil || string(contents) != "complete" {
		t.Fatalf("destination = %q, error = %v", contents, err)
	}
	info, err := os.Stat(destination)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0o600 {
		t.Fatalf("download mode = %o, want 600", info.Mode().Perm())
	}
}

func TestDownloadReplacementFailureRemovesStagingFile(t *testing.T) {
	directory := t.TempDir()
	destination := filepath.Join(directory, "existing-directory")
	if err := os.Mkdir(destination, 0o750); err != nil {
		t.Fatal(err)
	}
	marker := filepath.Join(destination, "keep")
	if err := os.WriteFile(marker, []byte("previous"), 0o600); err != nil {
		t.Fatal(err)
	}
	api := &client{baseURL: "http://suxen.test", http: &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
		return &http.Response{StatusCode: http.StatusOK, Body: io.NopCloser(strings.NewReader("complete")), Header: make(http.Header), Request: r}, nil
	})}}
	if err := api.download("/repository/raw/artifact", destination); err == nil {
		t.Fatal("download unexpectedly replaced a destination directory")
	}
	contents, err := os.ReadFile(marker)
	if err != nil || string(contents) != "previous" {
		t.Fatalf("existing destination changed: %q, error = %v", contents, err)
	}
	entries, err := os.ReadDir(directory)
	if err != nil || len(entries) != 1 || entries[0].Name() != filepath.Base(destination) {
		t.Fatalf("staging files remain: %v, error = %v", entries, err)
	}
}

func TestCanceledDownloadPreservesDestination(t *testing.T) {
	directory := t.TempDir()
	destination := filepath.Join(directory, "artifact")
	if err := os.WriteFile(destination, []byte("previous"), 0o600); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	api := &client{baseURL: "http://suxen.test", ctx: ctx, http: &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
		return &http.Response{StatusCode: http.StatusOK, Body: io.NopCloser(&cancelingReader{ctx: r.Context(), cancel: cancel}), Header: make(http.Header), Request: r}, nil
	})}}
	if err := api.download("/repository/raw/artifact", destination); !errors.Is(err, context.Canceled) {
		t.Fatalf("download error = %v", err)
	}
	contents, err := os.ReadFile(destination)
	if err != nil || string(contents) != "previous" {
		t.Fatalf("destination = %q, error = %v", contents, err)
	}
	entries, err := os.ReadDir(directory)
	if err != nil || len(entries) != 1 {
		t.Fatalf("staging files remain: %v, error = %v", entries, err)
	}
}

type brokenReader struct{ err error }

func (reader *brokenReader) Read(p []byte) (int, error) {
	return copy(p, "partial"), reader.err
}

type cancelingReader struct {
	ctx    context.Context
	cancel context.CancelFunc
	read   bool
}

func (reader *cancelingReader) Read(p []byte) (int, error) {
	if reader.read {
		return 0, reader.ctx.Err()
	}
	reader.read = true
	reader.cancel()
	return copy(p, "partial"), nil
}

func TestRequestsRespectCancellationAndHeaderTimeout(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	api := &client{baseURL: "http://suxen.test", ctx: ctx, http: &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
		<-r.Context().Done()
		return nil, r.Context().Err()
	})}}
	cancel()
	if _, err := api.do(http.MethodGet, "/repository/raw/artifact", nil); !errors.Is(err, context.Canceled) {
		t.Fatalf("canceled request error = %v", err)
	}

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		<-r.Context().Done()
	}))
	defer server.Close()
	bounded := newHTTPClient()
	transport := bounded.Transport.(*http.Transport)
	if bounded.Timeout != 0 || transport.TLSHandshakeTimeout <= 0 || transport.ResponseHeaderTimeout <= 0 {
		t.Fatal("CLI client must bound setup and headers without limiting body transfer")
	}
	transport.ResponseHeaderTimeout = 20 * time.Millisecond
	api = &client{baseURL: server.URL, http: bounded}
	if _, err := api.do(http.MethodGet, "/repository/raw/artifact", nil); err == nil {
		t.Fatal("header timeout did not stop request")
	}
}
