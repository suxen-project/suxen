package pypi_test

import (
	"bytes"
	"io"
	"mime/multipart"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// A multipart body paused inside its content part must already have a host-
// owned staging file. This catches a return to ParseMultipartForm, which waits
// for the entire body and may spill into the process temp directory.
func TestPyPIPartialUploadUsesManagedStagingAndSurvivesGC(t *testing.T) {
	f := newFixture(t, time.Hour)
	mustCreate(t, f.createRepository(t, map[string]any{"name": "hosted", "format": "pypi", "type": "hosted"}))
	reader, writer := io.Pipe()
	multipartWriter := multipart.NewWriter(writer)
	request, err := http.NewRequest(http.MethodPost, f.suxen.URL+"/repository/hosted/", reader)
	if err != nil {
		t.Fatal(err)
	}
	request.Header.Set("Authorization", "Bearer "+adminToken)
	request.Header.Set("Content-Type", multipartWriter.FormDataContentType())
	type result struct {
		status int
		err    error
	}
	responseDone := make(chan result, 1)
	go func() {
		response, err := http.DefaultClient.Do(request)
		if err != nil {
			responseDone <- result{err: err}
			return
		}
		_, _ = io.Copy(io.Discard, response.Body)
		_ = response.Body.Close()
		responseDone <- result{status: response.StatusCode}
	}()
	ready := make(chan error, 1)
	release := make(chan struct{})
	defer func() {
		select {
		case <-release:
		default:
			close(release)
		}
		_ = writer.Close()
	}()
	go func() {
		part, err := multipartWriter.CreateFormFile("content", "widget-1.0.0-py3-none-any.whl")
		if err == nil {
			_, err = io.Copy(part, bytes.NewReader(bytes.Repeat([]byte("x"), 1<<20)))
		}
		ready <- err
		if err != nil {
			_ = writer.CloseWithError(err)
			return
		}
		<-release
		if err = multipartWriter.WriteField("name", "widget"); err == nil {
			err = multipartWriter.Close()
		}
		_ = writer.CloseWithError(err)
	}()
	select {
	case err := <-ready:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("multipart writer did not reach paused content")
	}
	stagingDirectory := filepath.Join(f.dataDirectory, "uploads")
	var stagingPath string
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		entries, err := os.ReadDir(stagingDirectory)
		if err == nil {
			for _, entry := range entries {
				if strings.HasPrefix(entry.Name(), "upload-") {
					path := filepath.Join(stagingDirectory, entry.Name())
					if info, err := os.Stat(path); err == nil && info.Size() == 1<<20 {
						stagingPath = path
						break
					}
				}
			}
		}
		if stagingPath != "" {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	if stagingPath == "" {
		t.Fatal("partial PyPI upload has no managed staging file")
	}
	info, err := os.Stat(stagingPath)
	if err != nil || info.Size() != 1<<20 {
		t.Fatalf("managed staging bytes: info=%v err=%v", info, err)
	}
	old := time.Now().Add(-2 * time.Hour)
	if err := os.Chtimes(stagingPath, old, old); err != nil {
		t.Fatal(err)
	}
	gc, body := f.do(t, http.MethodPost, "/api/v1/gc?dryRun=false&grace=0", nil, nil)
	if gc.StatusCode != http.StatusOK {
		t.Fatalf("GC during upload: %d %s", gc.StatusCode, body)
	}
	if _, err := os.Stat(stagingPath); err != nil {
		t.Fatalf("GC removed active PyPI staging: %v", err)
	}
	close(release)
	select {
	case result := <-responseDone:
		if result.err != nil || result.status != http.StatusOK {
			t.Fatalf("completed upload: status=%d err=%v", result.status, result.err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("completed upload did not return")
	}
	if _, err := os.Stat(stagingPath); !os.IsNotExist(err) {
		t.Fatalf("completed upload left staging file: %v", err)
	}
}
