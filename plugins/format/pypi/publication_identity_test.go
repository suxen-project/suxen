package pypi_test

import (
	"bytes"
	"net/http"
	"testing"
	"time"
)

func TestPyPIUploadRequiresMatchingDistributionProject(t *testing.T) {
	f := newFixture(t, time.Hour)
	mustCreate(t, f.createRepository(t, map[string]any{"name": "hosted", "format": "pypi", "type": "hosted"}))

	filename := "other-1.0.0-py3-none-any.whl"
	response, body := twineUpload(t, f, "hosted", "widget", "1.0.0", filename, wheel(t, "other", "1.0.0"))
	if response.StatusCode != http.StatusBadRequest {
		t.Fatalf("mismatched project upload = %d %s", response.StatusCode, body)
	}
	response, body = f.do(t, http.MethodGet, "/repository/hosted/simple/widget/", nil, nil)
	if response.StatusCode != http.StatusNotFound {
		t.Fatalf("rejected upload created an index: %d %s", response.StatusCode, body)
	}
	response, body = f.do(t, http.MethodGet, "/repository/hosted/packages/widget/"+filename, nil, nil)
	if response.StatusCode != http.StatusNotFound {
		t.Fatalf("rejected upload stored a wheel: %d %s", response.StatusCode, body)
	}

	alias := "Widget_Package-1.0.0-py3-none-any.whl"
	response, body = twineUpload(t, f, "hosted", "widget.package", "1.0.0", alias, wheel(t, "Widget_Package", "1.0.0"))
	if response.StatusCode != http.StatusOK {
		t.Fatalf("normalized project alias upload = %d %s", response.StatusCode, body)
	}
	response, body = f.do(t, http.MethodGet, "/repository/hosted/simple/widget-package/", nil, nil)
	if response.StatusCode != http.StatusOK || !bytes.Contains(body, []byte(alias)) {
		t.Fatalf("normalized project index = %d %s", response.StatusCode, body)
	}
}
