package blob

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	spiblob "github.com/suxen-project/suxen/spi/blob"
)

// The built-in drivers register through the same public SPI as third-party
// plugins, so linking this package is what makes fs:// and s3:// available.
func init() {
	spiblob.Register(spiblob.Driver{
		Name:      "fs",
		URLScheme: "fs",
		Open: func(configuration string) (spiblob.Store, error) {
			if !strings.HasPrefix(configuration, "fs://") {
				return nil, errors.New("filesystem blob store configuration must use fs://")
			}
			return NewFS(strings.TrimPrefix(configuration, "fs://"))
		},
		CanonicalConfiguration: canonicalFSConfiguration,
		// Local disk is not visible to other replicas.
		SharedStorage: false,
	})
	spiblob.Register(spiblob.Driver{
		Name:      "s3",
		URLScheme: "s3",
		Open: func(configuration string) (spiblob.Store, error) {
			if !strings.HasPrefix(configuration, "s3://") {
				return nil, errors.New("S3 blob store configuration must use s3://")
			}
			return NewS3(configuration)
		},
		CanonicalConfiguration: func(configuration string) (string, error) {
			canonical, err := CanonicalS3Configuration(configuration)
			if err != nil {
				return "", errors.New("S3 blob store configuration is invalid")
			}
			return canonical, nil
		},
		SharedStorage: true,
	})
}

// canonicalFSConfiguration resolves the configured root to a symlink-free
// absolute path so two configurations addressing the same directory hash to
// the same physical identity.
func canonicalFSConfiguration(configuration string) (string, error) {
	if !strings.HasPrefix(configuration, "fs://") {
		return "", errors.New("filesystem blob store configuration must use fs://")
	}
	root, err := filepath.Abs(strings.TrimPrefix(configuration, "fs://"))
	if err != nil {
		return "", fmt.Errorf("resolve filesystem blob store root: %w", err)
	}
	return canonicalFilesystemRoot(filepath.Clean(root))
}

// canonicalFilesystemRoot resolves symlinks in the deepest existing ancestor
// so not-yet-created roots still canonicalize deterministically.
func canonicalFilesystemRoot(root string) (string, error) {
	current := root
	missing := make([]string, 0)
	for {
		resolved, err := filepath.EvalSymlinks(current)
		if err == nil {
			for index := len(missing) - 1; index >= 0; index-- {
				resolved = filepath.Join(resolved, missing[index])
			}
			return filepath.Clean(resolved), nil
		}
		if !errors.Is(err, os.ErrNotExist) {
			return "", fmt.Errorf("resolve filesystem blob store root: %w", err)
		}
		parent := filepath.Dir(current)
		if parent == current {
			return "", fmt.Errorf("resolve filesystem blob store root: %w", err)
		}
		missing = append(missing, filepath.Base(current))
		current = parent
	}
}
