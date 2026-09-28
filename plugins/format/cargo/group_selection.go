package cargo

import (
	"github.com/suxen-project/suxen/spi/format"
)

var _ format.GroupArtifactSelector = Format{}

func (Format) GroupArtifactSource(_ format.Repository, assetPath string) (string, bool) {
	info, ok := parsePath(assetPath)
	if !ok || info.kind != kindCrate {
		return "", false
	}
	return indexPath(info.name), true
}

func (Format) GroupSourceContainsArtifact(_ format.Repository, sourcePath, assetPath string, body []byte) (bool, error) {
	info, ok := parsePath(assetPath)
	if !ok || info.kind != kindCrate || sourcePath != indexPath(info.name) {
		return false, nil
	}
	_, _, found, err := indexVersionChecksum(body, info.name, info.version)
	return found, err
}
