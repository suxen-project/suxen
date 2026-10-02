package main

import (
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"testing"
)

func TestRepositoryCreateOverwriteFlag(t *testing.T) {
	for _, option := range []string{"", "--allow-overwrite=false", "--allow-overwrite=true"} {
		t.Run(option, func(t *testing.T) {
			var sent map[string]any
			api := &client{baseURL: "http://suxen.test", http: &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
				if err := json.NewDecoder(r.Body).Decode(&sent); err != nil {
					t.Fatal(err)
				}
				return &http.Response{StatusCode: 201, Header: make(http.Header), Body: io.NopCloser(strings.NewReader(`{}`)), Request: r}, nil
			})}}
			arguments := []string{"create"}
			if option != "" {
				arguments = append(arguments, option)
			}
			arguments = append(arguments, "files")
			if err := repositoryCommand(api, arguments); err != nil {
				t.Fatal(err)
			}
			got, present := sent["allowOverwrite"]
			if option == "" {
				if present {
					t.Fatal("omitted flag must preserve server default")
				}
			} else if !present || got != (option == "--allow-overwrite=true") {
				t.Fatalf("allowOverwrite=%v present=%v", got, present)
			}
		})
	}
}

func capturingClient(t *testing.T, sent *map[string]any, paths *[]string) *client {
	t.Helper()
	return &client{baseURL: "http://suxen.test", http: &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
		*paths = append(*paths, r.URL.RequestURI())
		if r.Body != nil && r.Method != http.MethodGet {
			if err := json.NewDecoder(r.Body).Decode(sent); err != nil {
				t.Fatal(err)
			}
		}
		return &http.Response{StatusCode: 200, Header: make(http.Header), Body: io.NopCloser(strings.NewReader(`{"items":[]}`)), Request: r}, nil
	})}}
}

func TestRepositoryCreateComponentFlags(t *testing.T) {
	var sent map[string]any
	var paths []string
	api := capturingClient(t, &sent, &paths)
	err := repositoryCommand(api, []string{"create",
		"--component", `^(?P<name>models/.+)/(?P<version>[^/]+)/[^/]+$`, "--component-anchor", `\.glb$`,
		"--component", `^(?P<name>client/alpha)/[^/]+/trackmaniac-(?P<version>[^/-]+)-[^/]+$`,
		"models"})
	if err != nil {
		t.Fatal(err)
	}
	encoded, _ := json.Marshal(sent["formatConfig"])
	want := `{"components":[{"anchor":"\\.glb$","pattern":"^(?P\u003cname\u003emodels/.+)/(?P\u003cversion\u003e[^/]+)/[^/]+$"},` +
		`{"pattern":"^(?P\u003cname\u003eclient/alpha)/[^/]+/trackmaniac-(?P\u003cversion\u003e[^/-]+)-[^/]+$"}]}`
	if string(encoded) != want {
		t.Fatalf("formatConfig = %s, want %s", encoded, want)
	}

	for _, arguments := range [][]string{
		{"create", "--component-anchor", `\.glb$`, "models"},
		{"create", "--component", "a", "--component-anchor", "b", "--component-anchor", "c", "models"},
		{"create", "--format-config", `{"components":[]}`, "--component", "a", "models"},
	} {
		if err := repositoryCommand(api, arguments); err == nil {
			t.Errorf("repositoryCommand(%v) accepted invalid component flags", arguments)
		}
	}
}

func TestRepositoryComponentsCommand(t *testing.T) {
	var sent map[string]any
	var paths []string
	api := capturingClient(t, &sent, &paths)
	if err := repositoryCommand(api, []string{"components", "models"}); err != nil {
		t.Fatal(err)
	}
	if len(paths) == 0 || !strings.HasPrefix(paths[0], "/api/v1/repositories/models/components") {
		t.Fatalf("requested %v", paths)
	}
	if err := repositoryCommand(api, []string{"components"}); err == nil {
		t.Fatal("components without a repository name was accepted")
	}
}
