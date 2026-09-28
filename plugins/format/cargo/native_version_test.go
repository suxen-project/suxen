package cargo

import (
	"bytes"
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"

	"github.com/suxen-project/suxen/spi/format"
)

func TestCargoNativeVersionValidityOracle(t *testing.T) {
	data, err := os.ReadFile("testdata/cargo-semver-1.0.28-validity.tsv")
	if err != nil {
		t.Fatal(err)
	}
	for _, line := range bytes.Split(bytes.TrimSpace(data), []byte{'\n'}) {
		version, expected, ok := strings.Cut(string(line), "\t")
		if !ok || expected != "true" && expected != "false" {
			t.Fatalf("invalid oracle row %q", line)
		}
		if got := validVersion(version); got != (expected == "true") {
			t.Errorf("validVersion(%q) = %v; Rust semver 1.0.28 = %s", version, got, expected)
		}
	}
}

func TestCargoIndexAndGroupIgnoreNativeInvalidVersion(t *testing.T) {
	invalid := "18446744073709551616.0.0"
	valid := "1.0.0"
	checksum := strings.Repeat("0", 64)
	index := []byte(`{"name":"widget","vers":"` + invalid + `","cksum":"` + checksum + `"}` + "\n" +
		`{"name":"widget","vers":"` + valid + `","cksum":"` + checksum + `"}` + "\n")
	if _, _, found, err := indexVersionChecksum(index, "widget", invalid); found || err != nil {
		t.Fatalf("invalid proxy index version found=%v err=%v", found, err)
	}
	merged, err := mergeIndexEntries("widget", [][]byte{index})
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Contains(merged, []byte(invalid)) || !bytes.Contains(merged, []byte(valid)) {
		t.Fatalf("group index = %s", merged)
	}
}

type legacyVersionIndexTools struct {
	format.WireTools
	rows map[string][]byte
}

func (tools legacyVersionIndexTools) VisitAssetPaths(_ context.Context, prefix string, visit func(string) (bool, error)) error {
	for path := range tools.rows {
		if !strings.HasPrefix(path, prefix) {
			continue
		}
		more, err := visit(path)
		if err != nil || !more {
			return err
		}
	}
	return nil
}

func (tools legacyVersionIndexTools) OpenMetadataAsset(_ context.Context, path string) (io.ReadCloser, format.Asset, bool, error) {
	row, found := tools.rows[path]
	if !found {
		return nil, format.Asset{}, false, nil
	}
	return io.NopCloser(bytes.NewReader(row)), format.Asset{}, true, nil
}

func (tools legacyVersionIndexTools) StatAsset(_ context.Context, path string) (format.Asset, bool, error) {
	return format.Asset{}, path == "dl/widget/1.0.0/download" || path == "dl/widget/18446744073709551616.0.0/download", nil
}

func TestCargoHostedIndexFiltersLegacyNativeInvalidVersion(t *testing.T) {
	const invalid = "18446744073709551616.0.0"
	tools := legacyVersionIndexTools{rows: map[string][]byte{
		"index-meta/widget/" + invalid + ".json": []byte(`{"name":"widget","vers":"` + invalid + `"}`),
		"index-meta/widget/1.0.0.json":           []byte(`{"name":"widget","vers":"1.0.0"}`),
	}}
	request := httptest.NewRequest(http.MethodGet, "/repository/hosted/wi/dg/widget", nil)
	response := httptest.NewRecorder()
	serveIndex(response, request, format.Repository{Name: "hosted"}, tools, "widget")
	if response.Code != http.StatusOK || strings.Contains(response.Body.String(), invalid) ||
		!strings.Contains(response.Body.String(), `"vers":"1.0.0"`) {
		t.Fatalf("hosted index includes unusable legacy row: %d %s", response.Code, response.Body.String())
	}
}
