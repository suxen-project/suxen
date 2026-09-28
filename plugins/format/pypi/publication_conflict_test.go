package pypi_test

import (
	"bytes"
	"net/http"
	"testing"
	"time"
)

func TestPyPIRepublishConflictPreservesDistributionAndIndex(t *testing.T) {
	f := newFixture(t, time.Hour)
	mustCreate(t, f.createRepository(t, map[string]any{"name": "hosted", "format": "pypi", "type": "hosted"}))
	filename := "widget-1.0.0-py3-none-any.whl"
	first, body := twineUpload(t, f, "hosted", "widget", "1.0.0", filename, []byte("original wheel"))
	if first.StatusCode != http.StatusOK {
		t.Fatalf("initial upload: %d %s", first.StatusCode, body)
	}
	index, originalIndex := f.do(t, http.MethodGet, "/repository/hosted/simple/widget/", nil, nil)
	if index.StatusCode != http.StatusOK {
		t.Fatalf("original index: %d %s", index.StatusCode, originalIndex)
	}
	second, body := twineUpload(t, f, "hosted", "widget", "1.0.0", filename, []byte("changed wheel"))
	if second.StatusCode != http.StatusConflict {
		t.Fatalf("immutable conflict: %d %s", second.StatusCode, body)
	}
	idempotent, body := twineUpload(t, f, "hosted", "widget", "1.0.0", filename, []byte("original wheel"))
	if idempotent.StatusCode != http.StatusOK {
		t.Fatalf("byte-identical republish: %d %s", idempotent.StatusCode, body)
	}
	artifact, original := f.do(t, http.MethodGet, "/repository/hosted/packages/widget/"+filename, nil, nil)
	if artifact.StatusCode != http.StatusOK || string(original) != "original wheel" {
		t.Fatalf("rejected publication changed distribution: %d %s", artifact.StatusCode, original)
	}
	index, after := f.do(t, http.MethodGet, "/repository/hosted/simple/widget/", nil, nil)
	if index.StatusCode != http.StatusOK || !bytes.Equal(after, originalIndex) {
		t.Fatalf("rejected publication changed index: %d %s", index.StatusCode, after)
	}
}
