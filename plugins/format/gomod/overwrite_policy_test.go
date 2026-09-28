package gomod_test

import (
	"net/http"
	"testing"
	"time"
)

func TestGoHostedNoOverwriteProtectsVersionFile(t *testing.T) {
	f := newFixture(t, time.Hour)
	f.createRepository(t, map[string]any{"name": "locked", "format": "go", "type": "hosted", "allowOverwrite": false})
	path := modulePath + "/@v/v1.0.0.info"
	first, body := f.upload(t, "locked", path, []byte(`{"Version":"v1.0.0"}`))
	mustStatus(t, first, body, http.StatusCreated)
	same, body := f.upload(t, "locked", path, []byte(`{"Version":"v1.0.0"}`))
	mustStatus(t, same, body, http.StatusCreated)
	changed, body := f.upload(t, "locked", path, []byte(`{"Version":"v1.0.0","Time":"2026-01-01T00:00:00Z"}`))
	mustStatus(t, changed, body, http.StatusConflict)
	stored, body := f.get(t, "locked", path)
	mustStatus(t, stored, body, http.StatusOK)
	if string(body) != `{"Version":"v1.0.0"}` {
		t.Fatalf("version file changed: %s", body)
	}
}
