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
