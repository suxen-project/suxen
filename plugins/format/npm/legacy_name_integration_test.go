package npm_test

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"
	"time"
)

func TestHistoricalNpmNameProxyAndGroupStayOnRepository(t *testing.T) {
	f := newFixture(t, time.Nanosecond)
	registry := newRegistry(t, map[string][]string{
		"JSONStream":             {"1.3.5"},
		"@Legacy/Widget":         {"1.0.0"},
		"old!name":               {"1.0.0"},
		".legacy":                {"1.0.0"},
		"node_modules":           {"1.0.0"},
		strings.Repeat("x", 215): {"1.0.0"},
	})
	mustCreate(t, f.createRepository(t, map[string]any{"name": "proxy", "format": "npm", "type": "proxy", "upstream": registry.metadata.URL}))
	mustCreate(t, f.createRepository(t, map[string]any{"name": "group", "format": "npm", "type": "group", "members": []string{"proxy"}}))
	for name, version := range map[string]string{
		"JSONStream":             "1.3.5",
		"@Legacy/Widget":         "1.0.0",
		"old!name":               "1.0.0",
		".legacy":                "1.0.0",
		"node_modules":           "1.0.0",
		strings.Repeat("x", 215): "1.0.0",
	} {
		for _, repository := range []string{"group", "proxy"} {
			response, body := f.do(t, http.MethodGet, "/repository/"+repository+"/"+name, nil, nil)
			if response.StatusCode != http.StatusOK {
				t.Fatalf("%s/%s packument: %d %s", repository, name, response.StatusCode, body)
			}
			var doc struct {
				Versions map[string]struct {
					Dist struct {
						Tarball string `json:"tarball"`
					} `json:"dist"`
				} `json:"versions"`
			}
			if err := json.Unmarshal(body, &doc); err != nil {
				t.Fatal(err)
			}
			link := doc.Versions[version].Dist.Tarball
			if !strings.HasPrefix(link, f.suxen.URL+"/repository/"+repository+"/"+name+"/-/") {
				t.Fatalf("%s/%s tarball link escaped repository: %s", repository, name, link)
			}
			response, body = f.do(t, http.MethodGet, strings.TrimPrefix(link, f.suxen.URL), nil, nil)
			if response.StatusCode != http.StatusOK || len(body) == 0 {
				t.Fatalf("%s/%s tarball: %d %d bytes", repository, name, response.StatusCode, len(body))
			}
		}
	}
}

func TestNativeNpmInstallsHistoricalNameThroughProxyAndGroup(t *testing.T) {
	requireNpm(t)
	f := newFixture(t, time.Hour)
	registry := newRegistry(t, map[string][]string{"JSONStream": {"1.3.5"}})
	mustCreate(t, f.createRepository(t, map[string]any{"name": "proxy", "format": "npm", "type": "proxy", "upstream": registry.metadata.URL}))
	mustCreate(t, f.createRepository(t, map[string]any{"name": "group", "format": "npm", "type": "group", "members": []string{"proxy"}}))
	for _, repository := range []string{"proxy", "group"} {
		project := newProject(t)
		runNpm(t, npmEnvironment(t, f, repository), project, "install", "JSONStream@1.3.5")
		if got := installedVersion(t, project, "JSONStream"); got != "1.3.5" {
			t.Fatalf("%s installed %s", repository, got)
		}
	}
}

func TestHistoricalNpmNamePackumentRevalidates(t *testing.T) {
	f := newFixture(t, time.Nanosecond)
	registry := newRegistry(t, map[string][]string{"JSONStream": {"1.3.5"}})
	mustCreate(t, f.createRepository(t, map[string]any{"name": "proxy", "format": "npm", "type": "proxy", "upstream": registry.metadata.URL}))
	mustCreate(t, f.createRepository(t, map[string]any{"name": "group", "format": "npm", "type": "group", "members": []string{"proxy"}}))
	response, body := f.do(t, http.MethodGet, "/repository/group/JSONStream", nil, nil)
	if response.StatusCode != http.StatusOK || strings.Contains(string(body), `"1.3.6"`) {
		t.Fatalf("initial group packument: %d %s", response.StatusCode, body)
	}
	registry.packages["JSONStream"]["1.3.6"] = tarball(t, "JSONStream", "1.3.6")
	for _, repository := range []string{"proxy", "group"} {
		response, body = f.do(t, http.MethodGet, "/repository/"+repository+"/JSONStream", nil, nil)
		if response.StatusCode != http.StatusOK || !strings.Contains(string(body), `"1.3.6"`) {
			t.Fatalf("updated %s packument: %d %s", repository, response.StatusCode, body)
		}
	}
}

func TestHostedNpmRejectsHistoricalOnlyPublicationName(t *testing.T) {
	f := newFixture(t, time.Hour)
	mustCreate(t, f.createRepository(t, map[string]any{"name": "hosted", "format": "npm", "type": "hosted"}))
	for _, name := range []string{"JSONStream", "@Legacy/Widget", "old!name", ".legacy", "_legacy", "-legacy", "node_modules", "favicon.ico", strings.Repeat("x", 215)} {
		response, body := f.do(t, http.MethodPut, "/repository/hosted/"+name, []byte(`{}`), nil)
		if response.StatusCode != http.StatusBadRequest || !strings.Contains(string(body), "invalid package name") {
			t.Fatalf("hosted publish %q: %d %s", name, response.StatusCode, body)
		}
	}
}
