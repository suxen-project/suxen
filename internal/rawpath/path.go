// Package rawpath defines the URL and storage path contract for Raw artifacts.
package rawpath

import (
	"errors"
	"net/url"
	"strings"

	"github.com/suxen-project/suxen/internal/domain"
)

var ErrInvalid = errors.New("Raw asset path must be at most 2048 UTF-8 bytes and have nonempty segments without . or .., backslashes, or control characters")

// Validate accepts slash-separated, nonempty path segments. It rejects path
// normalization so a client and server always agree on the stored asset name.
func Validate(assetPath string) error {
	if !domain.ValidAssetPath(assetPath) {
		return ErrInvalid
	}
	for _, segment := range strings.Split(assetPath, "/") {
		if segment == "" || segment == "." || segment == ".." || strings.Contains(segment, "\\") {
			return ErrInvalid
		}
		for _, character := range segment {
			if character < 0x20 || character == 0x7f {
				return ErrInvalid
			}
		}
	}
	return nil
}

// URLPath escapes every asset segment while retaining intended slash separators.
func URLPath(repositoryName, assetPath string) (string, error) {
	if err := Validate(assetPath); err != nil {
		return "", err
	}
	return EscapeURLPath(repositoryName, assetPath), nil
}

// EscapeURLPath escapes a path supplied by another repository format. Those
// formats retain their own path rules while their Location stays URL safe.
func EscapeURLPath(repositoryName, assetPath string) string {
	segments := strings.Split(assetPath, "/")
	for index, segment := range segments {
		segments[index] = url.PathEscape(segment)
	}
	return "/repository/" + url.PathEscape(repositoryName) + "/" + strings.Join(segments, "/")
}
