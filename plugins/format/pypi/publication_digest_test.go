package pypi_test

import (
	"bytes"
	"mime/multipart"
	"net/http"
	"testing"
	"time"
)

func TestPyPIUploadRejectsMismatchedSHA256(t *testing.T) {
	f := newFixture(t, time.Hour)
	mustCreate(t, f.createRepository(t, map[string]any{"name": "hosted", "format": "pypi", "type": "hosted"}))
	filename := "widget-1.0.0-py3-none-any.whl"
	content := wheel(t, "widget", "1.0.0")

	var body bytes.Buffer
	writer := multipart.NewWriter(&body)
	for name, value := range map[string]string{
		":action": "file_upload", "protocol_version": "1",
		"name": "widget", "version": "1.0.0",
		"sha256_digest": "0000000000000000000000000000000000000000000000000000000000000000",
	} {
		if err := writer.WriteField(name, value); err != nil {
			t.Fatal(err)
		}
	}
	part, err := writer.CreateFormFile("content", filename)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := part.Write(content); err != nil {
		t.Fatal(err)
	}
	if err := writer.Close(); err != nil {
		t.Fatal(err)
	}
	response, data := f.do(t, http.MethodPost, "/repository/hosted/", body.Bytes(),
		http.Header{"Content-Type": {writer.FormDataContentType()}})
	if response.StatusCode != http.StatusBadRequest {
		t.Fatalf("mismatched digest upload = %d %s", response.StatusCode, data)
	}
	response, data = f.do(t, http.MethodGet, "/repository/hosted/packages/widget/"+filename, nil, nil)
	if response.StatusCode != http.StatusNotFound {
		t.Fatalf("rejected upload stored a distribution: %d %s", response.StatusCode, data)
	}
	response, data = twineUpload(t, f, "hosted", "widget", "1.0.0", filename, content)
	if response.StatusCode != http.StatusOK {
		t.Fatalf("valid digest upload = %d %s", response.StatusCode, data)
	}
}
