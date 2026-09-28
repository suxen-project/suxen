package content

import (
	"bytes"
	"crypto/sha1"
	"crypto/sha256"
	"crypto/sha512"
	"encoding/hex"
	"fmt"
	"hash"
	"io"
	"os"
	"strings"

	"github.com/suxen-project/suxen/internal/domain"
)

// verifyProxyExpectedDigests checks the resolver's strongest advertised hash
// before publication. Multiple digests of that algorithm are alternatives,
// as with multiple integrity tokens for one package version.
func verifyProxyExpectedDigests(staged StagedUpload, expected []string) error {
	if len(expected) == 0 {
		return nil
	}
	var algorithm string
	var hasher hash.Hash
	wants := make([][]byte, 0, len(expected))
	for _, entry := range expected {
		name, encoded, ok := strings.Cut(entry, ":")
		if !ok || name == "" || encoded == "" || algorithm != "" && algorithm != name {
			return fmt.Errorf("invalid proxy expected digest %q", entry)
		}
		if algorithm == "" {
			algorithm = name
			switch algorithm {
			case "sha1":
				hasher = sha1.New()
			case "sha256":
				hasher = sha256.New()
			case "sha384":
				hasher = sha512.New384()
			case "sha512":
				hasher = sha512.New()
			default:
				return fmt.Errorf("unsupported proxy expected digest algorithm %q", name)
			}
		}
		want, err := hex.DecodeString(encoded)
		if err != nil || len(want) != hasher.Size() || hex.EncodeToString(want) != encoded {
			return fmt.Errorf("invalid proxy expected digest %q", entry)
		}
		wants = append(wants, want)
	}
	file, err := os.Open(staged.Path)
	if err != nil {
		return fmt.Errorf("open staged proxy content for integrity verification: %w", err)
	}
	defer file.Close()
	if _, err := io.Copy(hasher, file); err != nil {
		return fmt.Errorf("hash staged proxy content: %w", err)
	}
	actual := hasher.Sum(nil)
	for _, want := range wants {
		if bytes.Equal(actual, want) {
			return nil
		}
	}
	return fmt.Errorf("%w: upstream content does not match advertised %s integrity", domain.ErrDigestMismatch, algorithm)
}
