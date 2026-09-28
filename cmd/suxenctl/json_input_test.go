package main

import (
	"encoding/json"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

func TestAttributeSetPreservesJSONNumbersAndRejectsTrailingInput(t *testing.T) {
	for _, test := range []struct {
		name, input string
		valid       bool
	}{
		{"precise numbers", `{"serial":9007199254740993,"fraction":0.1234567890123456789}`, true},
		{"whitespace", "{\"serial\":9007199254740993} \n\t", true},
		{"second object", `{"serial":1} {"serial":2}`, false},
		{"trailing null", `{"serial":1} null`, false},
		{"trailing garbage", `{"serial":1} invalid`, false},
	} {
		t.Run(test.name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "attributes.json")
			if err := os.WriteFile(path, []byte(test.input), 0o600); err != nil {
				t.Fatal(err)
			}
			requests := 0
			api := &client{baseURL: "http://suxen.test", http: &http.Client{
				Transport: roundTripFunc(func(request *http.Request) (*http.Response, error) {
					requests++
					body, err := io.ReadAll(request.Body)
					if err != nil {
						t.Fatal(err)
					}
					if test.valid {
						var got, want map[string]json.RawMessage
						if err := json.Unmarshal(body, &got); err != nil {
							t.Fatal(err)
						}
						if err := json.Unmarshal([]byte(test.input), &want); err != nil {
							t.Fatal(err)
						}
						if !reflect.DeepEqual(got, want) {
							t.Errorf("sent %s, want unchanged numbers in %s", body, test.input)
						}
					}
					return &http.Response{StatusCode: http.StatusNoContent,
						Body: io.NopCloser(strings.NewReader("")), Header: make(http.Header)}, nil
				}),
			}}
			err := dispatch(api, []string{"attribute", "set", "--if-match", "sha256:abc", "raw", "1", "scan", path})
			if test.valid {
				if err != nil || requests != 1 {
					t.Fatalf("error = %v, requests = %d", err, requests)
				}
			} else if err == nil || requests != 0 {
				t.Fatalf("invalid input: error = %v, requests = %d; want rejection before sending", err, requests)
			}
		})
	}
}
