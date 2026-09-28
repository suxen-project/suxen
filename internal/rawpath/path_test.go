package rawpath

import (
	"net/url"
	"strings"
	"testing"

	"github.com/suxen-project/suxen/internal/domain"
)

func TestURLPathRoundTrip(t *testing.T) {
	asset := "release notes/what? #1/100% café.txt"
	encoded, err := URLPath("raw", asset)
	if err != nil {
		t.Fatal(err)
	}
	if encoded != "/repository/raw/release%20notes/what%3F%20%231/100%25%20caf%C3%A9.txt" {
		t.Fatalf("encoded path = %q", encoded)
	}
	parsed, err := url.Parse(encoded)
	if err != nil {
		t.Fatal(err)
	}
	if got := strings.TrimPrefix(parsed.Path, "/repository/raw/"); got != asset {
		t.Fatalf("decoded asset = %q, want %q", got, asset)
	}
}

func TestInvalidPaths(t *testing.T) {
	for _, asset := range []string{"", "/a", "a/", "a//b", ".", "a/./b", "a/../b", "../a", "a\\b", "a\x00b", "a\x1fb", "a\x7fb", "bad\xff", "bad\xc3"} {
		if _, err := URLPath("raw", asset); err == nil {
			t.Errorf("URLPath accepted %q", asset)
		}
	}
}

func TestPathByteLimit(t *testing.T) {
	valid := strings.Repeat("x", domain.MaxAssetPathBytes)
	if err := Validate(valid); err != nil {
		t.Fatalf("%d-byte path rejected: %v", len(valid), err)
	}
	for _, invalid := range []string{
		valid + "x",
		strings.Repeat("é", domain.MaxAssetPathBytes/2+1),
	} {
		if err := Validate(invalid); err == nil {
			t.Fatalf("%d-byte path accepted", len(invalid))
		}
	}
}

func TestOtherFormatsCanEscapeWithoutRawValidation(t *testing.T) {
	if got := EscapeURLPath("maven", "legacy//name?.jar"); got != "/repository/maven/legacy//name%3F.jar" {
		t.Fatalf("escaped path = %q", got)
	}
}
