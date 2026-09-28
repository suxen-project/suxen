package npm

import (
	"encoding/json"
	"fmt"

	"github.com/suxen-project/suxen/spi/format"
)

var _ format.GroupArtifactSelector = Format{}

func (Format) GroupArtifactSource(_ format.Repository, assetPath string) (string, bool) {
	info, ok := parsePath(assetPath)
	if !ok || info.kind != kindTarball {
		return "", false
	}
	return info.name, true
}

func (Format) GroupSourceContainsArtifact(
	_ format.Repository,
	sourcePath, assetPath string,
	body []byte,
) (bool, error) {
	info, ok := parsePath(assetPath)
	if !ok || info.kind != kindTarball || sourcePath != info.name {
		return false, nil
	}
	var document struct {
		Versions map[string]json.RawMessage `json:"versions"`
	}
	if err := json.Unmarshal(body, &document); err != nil {
		return false, fmt.Errorf("invalid npm packument: %w", err)
	}
	if document.Versions == nil {
		return false, fmt.Errorf("invalid npm packument: missing versions")
	}
	_, found := document.Versions[info.version]
	return found, nil
}
