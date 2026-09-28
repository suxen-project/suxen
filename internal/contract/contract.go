// Package contract is the source of truth for suxen's independently versioned
// public contract surfaces: the Go SPIs, the HTTP control-plane API, and the
// webhook delivery contract. Each surface carries its own SemVer, decoupled
// from the application version, in the embedded versions.yaml manifest.
//
// The manifest is the only hand-edited version record. Runtime reporting reads
// it (the /api discovery matrix and the served OpenAPI info.version). During
// prerelease, the surfaces share the application RC version and may still change.
package contract

import (
	_ "embed"
	"fmt"
	"strings"

	"go.yaml.in/yaml/v3"
	"golang.org/x/mod/semver"
)

// Surface identifiers. Stable once published; never renamed.
const (
	SurfaceBlobstoreSPI = "blobstore-spi"
	SurfaceFormatSPI    = "format-spi"
	SurfacePluginAPISPI = "plugin-api-spi"
	SurfaceHTTPAPI      = "http-api"
	SurfaceWebhookAPI   = "webhook-api"
)

//go:embed versions.yaml
var manifestYAML []byte

// Surface is one independently versioned contract as declared in the manifest.
type Surface struct {
	ID      string `yaml:"id"`
	Version string `yaml:"version"`
	Source  string `yaml:"source,omitempty"`
	Path    string `yaml:"path,omitempty"`
}

// SurfaceVersion is the reduced {id, version} view exposed on the wire; it
// omits the repository-internal source and path bookkeeping.
type SurfaceVersion struct {
	ID      string `json:"id"`
	Version string `json:"version"`
}

type manifest struct {
	Surfaces []Surface `yaml:"surfaces"`
}

var loaded manifest

func init() {
	if err := yaml.Unmarshal(manifestYAML, &loaded); err != nil {
		panic(fmt.Sprintf("contract: invalid versions.yaml: %v", err))
	}
	if err := validate(loaded.Surfaces); err != nil {
		panic("contract: " + err.Error())
	}
}

// validate enforces the manifest invariants: non-empty and unique ids, a valid
// SemVer per surface, a wire surface's path major equal to its version major,
// and the presence of the http-api surface the server reports.
func validate(surfaces []Surface) error {
	if len(surfaces) == 0 {
		return fmt.Errorf("versions.yaml declares no surfaces")
	}
	seen := make(map[string]bool, len(surfaces))
	for _, s := range surfaces {
		if s.ID == "" {
			return fmt.Errorf("surface with empty id")
		}
		if seen[s.ID] {
			return fmt.Errorf("duplicate surface id %q", s.ID)
		}
		seen[s.ID] = true
		if !semver.IsValid("v" + s.Version) {
			return fmt.Errorf("surface %q has invalid version %q", s.ID, s.Version)
		}
		if s.Path != "" {
			pathMajor, ok := majorFromPath(s.Path)
			if !ok {
				return fmt.Errorf("surface %q has non-versioned path %q", s.ID, s.Path)
			}
			if pathMajor != majorFromVersion(s.Version) {
				return fmt.Errorf(
					"surface %q path %q major %q does not match version %q",
					s.ID, s.Path, pathMajor, s.Version,
				)
			}
		}
	}
	if !seen[SurfaceHTTPAPI] {
		return fmt.Errorf("versions.yaml is missing the %q surface", SurfaceHTTPAPI)
	}
	return nil
}

// majorFromVersion returns the numeric major of a bare "1.2.3" version.
func majorFromVersion(version string) string {
	return strings.TrimPrefix(semver.Major("v"+version), "v")
}

// majorFromPath returns the numeric major encoded in a "/api/v1"-style path.
func majorFromPath(path string) (string, bool) {
	index := strings.LastIndex(path, "/v")
	if index < 0 {
		return "", false
	}
	digits := path[index+2:]
	if digits == "" {
		return "", false
	}
	for _, r := range digits {
		if r < '0' || r > '9' {
			return "", false
		}
	}
	return digits, true
}

// Surfaces returns the full declared matrix, including source and path.
func Surfaces() []Surface {
	out := make([]Surface, len(loaded.Surfaces))
	copy(out, loaded.Surfaces)
	return out
}

// Matrix returns the reduced {id, version} view for wire reporting.
func Matrix() []SurfaceVersion {
	out := make([]SurfaceVersion, len(loaded.Surfaces))
	for i, s := range loaded.Surfaces {
		out[i] = SurfaceVersion{ID: s.ID, Version: s.Version}
	}
	return out
}

// Version returns the declared version of a surface and whether it is declared.
func Version(id string) (string, bool) {
	for _, s := range loaded.Surfaces {
		if s.ID == id {
			return s.Version, true
		}
	}
	return "", false
}

// HTTPAPIVersion returns the http-api surface version. The manifest is
// validated to declare it, so the result is always a valid SemVer.
func HTTPAPIVersion() string {
	version, _ := Version(SurfaceHTTPAPI)
	return version
}

// WebhookAPIVersion returns the webhook-api surface version, or the empty
// string if the manifest does not declare it.
func WebhookAPIVersion() string {
	version, _ := Version(SurfaceWebhookAPI)
	return version
}
