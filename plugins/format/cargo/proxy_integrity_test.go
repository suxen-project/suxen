package cargo_test

import (
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func TestCargoGroupOutageFailsClosed(t *testing.T) {
	f := newFixture(t, time.Nanosecond)
	var outage atomic.Bool
	upstream := func(payload string, first bool) *httptest.Server {
		return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if first && outage.Load() {
				http.Error(w, "offline", 503)
				return
			}
			switch r.URL.Path {
			case "/config.json":
				fmt.Fprintf(w, `{"dl":"http://%s/dl/{crate}/{version}/download"}`, r.Host)
			case "/wi/dg/widget":
				fmt.Fprintf(w, `{"name":"widget","vers":"1.0.0","deps":[],"cksum":"%x","features":{},"yanked":false}`, sha256.Sum256([]byte(payload)))
			case "/dl/widget/1.0.0/download":
				fmt.Fprint(w, payload)
			default:
				http.NotFound(w, r)
			}
		}))
	}
	a, b := upstream("first", true), upstream("second", false)
	defer a.Close()
	defer b.Close()
	for name, url := range map[string]string{"first": a.URL, "second": b.URL} {
		mustCreate(t, f.createRepository(t, map[string]any{"name": name, "format": "cargo", "type": "proxy", "upstream": url}))
	}
	mustCreate(t, f.createRepository(t, map[string]any{"name": "group", "format": "cargo", "type": "group", "members": []string{"first", "second"}}))
	for _, member := range []string{"first", "second"} {
		for _, p := range []string{"config.json", "wi/dg/widget", "dl/widget/1.0.0/download"} {
			r, body := f.do(t, "GET", "/repository/"+member+"/"+p, nil, nil)
			if r.StatusCode != 200 {
				t.Fatalf("warm %s %s: %d %s", member, p, r.StatusCode, body)
			}
		}
	}
	r, body := f.do(t, "GET", "/api/v1/repositories/group/assets?prefix=dl/widget/", nil, nil)
	if r.StatusCode != http.StatusOK {
		t.Fatalf("group inventory: %d %s", r.StatusCode, body)
	}
	var inventory struct {
		Items []struct {
			FormatPath string `json:"formatPath"`
		} `json:"items"`
	}
	if err := json.Unmarshal(body, &inventory); err != nil {
		t.Fatal(err)
	}
	if len(inventory.Items) != 1 || inventory.Items[0].FormatPath != "dl/widget/1.0.0/download" {
		t.Fatalf("group crate inventory = %+v", inventory.Items)
	}
	outage.Store(true)
	r, body = f.do(t, "GET", "/repository/group/wi/dg/widget", nil, nil)
	if r.StatusCode != http.StatusBadGateway {
		t.Fatalf("outage index: %d %s", r.StatusCode, body)
	}
	r, body = f.do(t, "GET", "/repository/group/dl/widget/1.0.0/download", nil, nil)
	if r.StatusCode != http.StatusBadGateway {
		t.Fatalf("outage crate: %d %s", r.StatusCode, body)
	}
	outage.Store(false)
	r, body = f.do(t, "GET", "/repository/group/wi/dg/widget", nil, nil)
	if r.StatusCode != http.StatusOK {
		t.Fatalf("restored index: %d %s", r.StatusCode, body)
	}
	var entry struct {
		Checksum string `json:"cksum"`
	}
	if err := json.Unmarshal(body, &entry); err != nil {
		t.Fatal(err)
	}
	r, body = f.do(t, "GET", "/repository/group/dl/widget/1.0.0/download", nil, nil)
	actual := fmt.Sprintf("%x", sha256.Sum256(body))
	if r.StatusCode != http.StatusOK || entry.Checksum != actual {
		t.Fatalf("restored index checksum=%s; crate status=%d hash=%s", entry.Checksum, r.StatusCode, actual)
	}
}

func TestCargoProxyRejectsMismatchedCrateAndRetries(t *testing.T) {
	f := newFixture(t, time.Hour)
	var repaired atomic.Bool
	var downloads atomic.Int32
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/config.json":
			fmt.Fprintf(w, `{"dl":"http://%s/dl/{crate}/{version}/download"}`, r.Host)
		case "/wi/dg/widget":
			fmt.Fprintf(w, `{"name":"widget","vers":"1.0.0","deps":[],"cksum":"%x","features":{},"yanked":false}`, sha256.Sum256([]byte("correct")))
		case "/dl/widget/1.0.0/download":
			downloads.Add(1)
			if repaired.Load() {
				fmt.Fprint(w, "correct")
			} else {
				fmt.Fprint(w, "corrupt")
			}
		default:
			http.NotFound(w, r)
		}
	}))
	defer upstream.Close()
	mustCreate(t, f.createRepository(t, map[string]any{"name": "proxy", "format": "cargo", "type": "proxy", "upstream": upstream.URL}))
	for _, p := range []string{"config.json", "wi/dg/widget"} {
		r, body := f.do(t, "GET", "/repository/proxy/"+p, nil, nil)
		if r.StatusCode != 200 {
			t.Fatalf("warm %s: %d %s", p, r.StatusCode, body)
		}
	}
	r, body := f.do(t, "GET", "/repository/proxy/dl/widget/1.0.0/download", nil, nil)
	if r.StatusCode != http.StatusBadGateway || downloads.Load() != 1 {
		t.Fatalf("initial mismatched download: %d %s; fetches=%d", r.StatusCode, body, downloads.Load())
	}
	repaired.Store(true)
	r, body = f.do(t, "GET", "/repository/proxy/dl/widget/1.0.0/download", nil, nil)
	if r.StatusCode != http.StatusOK || string(body) != "correct" || downloads.Load() != 2 {
		t.Fatalf("after upstream repair: %d %s; fetches=%d", r.StatusCode, body, downloads.Load())
	}
}

func TestCargoGroupMissingSelectedCrateDoesNotFallBack(t *testing.T) {
	f := newFixture(t, time.Hour)
	makeUpstream := func(payload string, missingCrate bool) *httptest.Server {
		return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			switch r.URL.Path {
			case "/config.json":
				fmt.Fprintf(w, `{"dl":"http://%s/dl/{crate}/{version}/download"}`, r.Host)
			case "/wi/dg/widget":
				fmt.Fprintf(w, `{"name":"widget","vers":"1.0.0","cksum":"%x"}`+"\n", sha256.Sum256([]byte(payload)))
			case "/dl/widget/1.0.0/download":
				if missingCrate {
					http.NotFound(w, r)
					return
				}
				fmt.Fprint(w, payload)
			default:
				http.NotFound(w, r)
			}
		}))
	}
	first := makeUpstream("selected", true)
	second := makeUpstream("fallback", false)
	defer first.Close()
	defer second.Close()
	for _, member := range []struct{ name, upstream string }{{"first", first.URL}, {"second", second.URL}} {
		mustCreate(t, f.createRepository(t, map[string]any{"name": member.name, "format": "cargo", "type": "proxy", "upstream": member.upstream}))
		for _, path := range []string{"config.json", "wi/dg/widget"} {
			response, body := f.do(t, http.MethodGet, "/repository/"+member.name+"/"+path, nil, nil)
			if response.StatusCode != http.StatusOK {
				t.Fatalf("warm %s %s: %d %s", member.name, path, response.StatusCode, body)
			}
		}
	}
	mustCreate(t, f.createRepository(t, map[string]any{"name": "group", "format": "cargo", "type": "group", "members": []string{"first", "second"}}))
	response, body := f.do(t, http.MethodGet, "/repository/group/wi/dg/widget", nil, nil)
	if response.StatusCode != http.StatusOK || !strings.Contains(string(body), fmt.Sprintf(`"cksum":"%x"`, sha256.Sum256([]byte("selected")))) {
		t.Fatalf("group index: %d %s", response.StatusCode, body)
	}
	response, body = f.do(t, http.MethodGet, "/repository/group/dl/widget/1.0.0/download", nil, nil)
	if response.StatusCode != http.StatusNotFound {
		t.Fatalf("selected member missing crate: %d %s", response.StatusCode, body)
	}
}
