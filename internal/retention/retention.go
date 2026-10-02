// Package retention computes the keepLast grouping that cleanup uses, so the
// store can persist each asset's group at write time and cleanup can read one
// group at a time instead of a whole repository.
package retention

import (
	"crypto/sha256"
	"encoding/hex"
	"path"
	"strings"
	"unicode/utf8"

	"github.com/suxen-project/suxen/internal/domain"
	"github.com/suxen-project/suxen/internal/ocimodel"
	"github.com/suxen-project/suxen/internal/rawcomponent"
	spiformat "github.com/suxen-project/suxen/spi/format"
)

// rawComponentGroupPrefix and rawDirectoryGroupPrefix mark Raw group keys of
// pattern and directory components; Raw paths never contain control bytes,
// and the prefixes stay storable in PostgreSQL text, which rejects NUL.
const (
	rawComponentGroupPrefix = "\x01raw-component\x01"
	rawDirectoryGroupPrefix = "\x01raw-directory\x01"
)

// maxStoredGroupLength keeps a stored group key well inside index entry limits.
const maxStoredGroupLength = 1024

// Supports reports whether cleanup may select the asset at all: Raw-kind files
// and tagged OCI manifests.
func Supports(asset domain.Asset) bool {
	if asset.Kind == "raw" {
		return true
	}
	return asset.Kind == "oci-manifest" &&
		asset.Reference != "" &&
		!strings.HasPrefix(asset.Reference, "sha256:")
}

// FormatGrouping returns the registered format's optional retention-grouping
// capability, or nil when the format does not own its grouping.
func FormatGrouping(formatName string) spiformat.RetentionGrouping {
	registered, found := spiformat.Lookup(formatName)
	if !found {
		return nil
	}
	grouping, _ := registered.(spiformat.RetentionGrouping)
	return grouping
}

// GroupKey is the keepLast grouping key: the identity whose versions compete
// for retention. Raw assets group by component; a format that owns its
// grouping decides the key; otherwise OCI manifests group by image name and
// everything else by parent directory.
func GroupKey(repository domain.Repository, grouping spiformat.RetentionGrouping, asset domain.Asset) string {
	if repository.Format == "raw" {
		return RawGroupKey(rawcomponent.ForConfig(repository.FormatConfig).Match(asset.Path))
	}
	if grouping != nil {
		if key, ok := grouping.RetentionGroupKey(repository.FormatView(), asset.FormatView()); ok {
			return key
		}
	}
	if asset.Kind == "oci-manifest" {
		if route, ok := ocimodel.ParseAssetPath(asset.Path); ok && route.Kind == ocimodel.ManifestRoute {
			return route.ImageName
		}
	}
	directory := path.Dir(asset.Path)
	if directory == "." {
		return ""
	}
	return directory
}

// RawGroupKey is the group key of a Raw component. Directory components of
// unmatched paths get their own marker, so a stray file in a pattern
// component's directory never competes with that component's versions.
func RawGroupKey(match rawcomponent.Match) string {
	if match.Implicit {
		return rawDirectoryGroupPrefix + match.Name
	}
	return rawComponentGroupPrefix + match.Name
}

// StoredGroup is the value persisted in assets.retention_group: empty for
// assets cleanup never selects, otherwise StoredKey of the group key.
func StoredGroup(repository domain.Repository, asset domain.Asset) string {
	if !Supports(asset) {
		return ""
	}
	return StoredKey(GroupKey(repository, FormatGrouping(repository.Format), asset))
}

// StoredKey encodes a group key for storage. Keys are stored verbatim behind a
// "g" marker, so the root directory's empty key stays distinct from "no
// group"; a key that is too long, invalid UTF-8, or holds NUL is replaced by
// its digest behind an "h" marker, which preserves equality.
func StoredKey(key string) string {
	if len(key) < maxStoredGroupLength && utf8.ValidString(key) && !strings.ContainsRune(key, 0) {
		return "g" + key
	}
	digest := sha256.Sum256([]byte(key))
	return "h" + hex.EncodeToString(digest[:])
}
