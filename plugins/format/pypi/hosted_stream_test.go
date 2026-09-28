package pypi

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"io"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/suxen-project/suxen/spi/format"
)

type recordingUploadTools struct {
	format.WireTools
	path    string
	content []byte
	called  int
}

func (*recordingUploadTools) MaxUploadBytes() int64 { return 80 << 20 }

func (tools *recordingUploadTools) StoreAsset(_ context.Context, path, _ string, source io.Reader) (format.Asset, error) {
	tools.called++
	content, err := io.ReadAll(source)
	if err != nil {
		return format.Asset{}, err
	}
	tools.path, tools.content = path, content
	return format.Asset{}, nil
}

type uploadPart struct {
	name, filename, value string
}

func testMultipartUpload(t *testing.T, parts []uploadPart) (*httptest.ResponseRecorder, *recordingUploadTools) {
	t.Helper()
	var body bytes.Buffer
	writer := multipart.NewWriter(&body)
	for _, part := range parts {
		if part.filename == "" {
			if err := writer.WriteField(part.name, part.value); err != nil {
				t.Fatal(err)
			}
			continue
		}
		file, err := writer.CreateFormFile(part.name, part.filename)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := io.WriteString(file, part.value); err != nil {
			t.Fatal(err)
		}
	}
	if err := writer.Close(); err != nil {
		t.Fatal(err)
	}
	request := httptest.NewRequest(http.MethodPost, "/", &body)
	request.Header.Set("Content-Type", writer.FormDataContentType())
	response := httptest.NewRecorder()
	tools := &recordingUploadTools{}
	uploadFile(response, request, tools)
	return response, tools
}

func TestUploadStreamsContentBeforeMetadata(t *testing.T) {
	content := strings.Repeat("distribution bytes", 2<<20) // over the old 32 MiB spill threshold
	sum := sha256.Sum256([]byte(content))
	response, tools := testMultipartUpload(t, []uploadPart{
		{name: "content", filename: "widget-1.0.0-py3-none-any.whl", value: content},
		{name: "name", value: "Widget"},
		{name: "sha256_digest", value: hex.EncodeToString(sum[:])},
	})
	if response.Code != http.StatusOK || tools.called != 1 || tools.path != "packages/widget/widget-1.0.0-py3-none-any.whl" || tools.content == nil || len(tools.content) != len(content) {
		t.Fatalf("streamed upload: status=%d calls=%d path=%q bytes=%d body=%s", response.Code, tools.called, tools.path, len(tools.content), response.Body)
	}
}

func TestUploadRejectsInvalidTrailingMetadataBeforePublication(t *testing.T) {
	file := uploadPart{name: "content", filename: "widget-1.0.0-py3-none-any.whl", value: "content"}
	tests := []struct {
		name   string
		parts  []uploadPart
		status int
	}{
		{"wrong name", []uploadPart{file, {name: "name", value: "other"}}, http.StatusBadRequest},
		{"wrong digest", []uploadPart{file, {name: "sha256_digest", value: strings.Repeat("0", 64)}}, http.StatusBadRequest},
		{"second file", []uploadPart{file, file}, http.StatusRequestEntityTooLarge},
		{"unexpected file", []uploadPart{file, {name: "other", filename: "other.whl", value: "x"}}, http.StatusBadRequest},
		{"large field names", []uploadPart{{name: strings.Repeat("x", 6<<20), value: "x"}, {name: strings.Repeat("y", 6<<20), value: "x"}, file}, http.StatusRequestEntityTooLarge},
		{"large field value", []uploadPart{file, {name: "description", value: strings.Repeat("x", maximumFieldBytes+1)}}, http.StatusRequestEntityTooLarge},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			response, tools := testMultipartUpload(t, tt.parts)
			if response.Code != tt.status || tools.content != nil {
				t.Fatalf("invalid upload: status=%d want=%d published=%t body=%s", response.Code, tt.status, tools.content != nil, response.Body)
			}
		})
	}
}

func TestUploadRejectsTruncatedMultipartContent(t *testing.T) {
	const boundary = "unfinished-upload"
	body := "--" + boundary + "\r\n" +
		"Content-Disposition: form-data; name=\"content\"; filename=\"widget-1.0.0-py3-none-any.whl\"\r\n\r\n" +
		"distribution bytes"
	request := httptest.NewRequest(http.MethodPost, "/", strings.NewReader(body))
	request.Header.Set("Content-Type", "multipart/form-data; boundary="+boundary)
	response := httptest.NewRecorder()
	tools := &recordingUploadTools{}
	uploadFile(response, request, tools)
	if response.Code != http.StatusBadRequest || tools.content != nil {
		t.Fatalf("truncated multipart: status=%d published=%t body=%s", response.Code, tools.content != nil, response.Body)
	}
}
