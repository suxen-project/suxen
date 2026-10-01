// Package assetattrs defines the merged attribute view exposed for assets.
//
// Stored attributes contain system-owned and writer-owned metadata. Projected
// attributes are derived from authoritative asset and repository fields at read
// time. Projection always wins when a stored value collides with a reserved
// namespace, so callers cannot forge system or coordinate values.
package assetattrs

import (
	"sort"
	"strings"

	"github.com/suxen-project/suxen/internal/domain"
	"github.com/suxen-project/suxen/internal/rawcomponent"
	spiformat "github.com/suxen-project/suxen/spi/format"
)

const (
	ClassificationNamespace = "classification"
	ProvenanceNamespace     = "provenance"
)

var reservedNamespaceRoots = map[string]struct{}{
	"classification": {},
	"oci":            {},
	"provenance":     {},
	"raw":            {},
	"sys":            {},
}

// isReservedRoot reports whether a top-level namespace is system-owned. Every
// registered format name is reserved as that format's coordinate namespace,
// whether or not the format projects attributes yet.
func isReservedRoot(root string) bool {
	if _, reserved := reservedNamespaceRoots[root]; reserved {
		return true
	}
	return spiformat.Registered(root)
}

// IsReservedNamespace reports whether a generic attribute writer must reject
// the namespace. Dotted children are included because writers may use dotted
// namespace names such as "vuln.trivy".
func IsReservedNamespace(namespace string) bool {
	root, _, _ := strings.Cut(namespace, ".")
	return isReservedRoot(root)
}

// knownAttributePaths lists the stable, system-projected attribute paths that
// policy predicates can reference. Roots outside the reserved set address
// writer-owned namespaces, which contribute additional paths at runtime and are
// therefore not enumerated here. Kept in sync with the projection by
// TestKnownAttributePathsCoverProjection.
var knownAttributePaths = []string{
	"sys.repository", "sys.format", "sys.type", "sys.blobStore", "sys.path",
	"sys.digest", "sys.size", "sys.contentType", "sys.kind", "sys.createdAt",
	"sys.updatedAt", "sys.lastAccessed", "sys.validatedAt",
	"raw.path", "raw.component", "raw.version",
	"oci.image", "oci.tag", "oci.digest", "oci.mediaType", "oci.artifactType",
	"provenance.status", "provenance.format", "provenance.fingerprint",
	"provenance.identity", "provenance.issuer", "provenance.reason",
	"provenance.digest", "provenance.policyUpdatedAt", "provenance.verifiedAt",
}

// KnownAttributePaths returns a copy of the stable predicate attribute paths.
func KnownAttributePaths() []string {
	return append([]string(nil), knownAttributePaths...)
}

// ReservedRoots returns the reserved top-level namespaces, sorted. A predicate
// path under any other root addresses a writer-owned attribute namespace.
func ReservedRoots() []string {
	formatNames := spiformat.Names()
	roots := make([]string, 0, len(reservedNamespaceRoots)+len(formatNames))
	for root := range reservedNamespaceRoots {
		roots = append(roots, root)
	}
	for _, name := range formatNames {
		if _, known := reservedNamespaceRoots[name]; !known {
			roots = append(roots, name)
		}
	}
	sort.Strings(roots)
	return roots
}

// SetClassificationLabels returns stored attributes with the system-owned
// classification namespace replaced by the given labels (classification.<key> =
// value). An empty label set removes the namespace entirely, so an asset matched
// by no rule carries no classification.* attributes.
func SetClassificationLabels(attributes map[string]any, labels map[string]string) map[string]any {
	result := cloneMap(attributes)
	if len(labels) == 0 {
		delete(result, ClassificationNamespace)
		return result
	}
	classification := make(map[string]any, len(labels))
	for key, value := range labels {
		classification[key] = value
	}
	result[ClassificationNamespace] = classification
	return result
}

// SetOCIArtifactType records format metadata that is present in an OCI manifest
// but has no authoritative asset row field. Empty artifact types are omitted.
func SetOCIArtifactType(attributes map[string]any, artifactType string) map[string]any {
	result := cloneMap(attributes)
	if artifactType == "" {
		return result
	}
	oci, _ := result["oci"].(map[string]any)
	oci = cloneMap(oci)
	oci["artifactType"] = artifactType
	result["oci"] = oci
	return result
}

// SetOCIProvenanceArtifact records that the OCI handler validated a manifest
// as a recognized signature or attestation referrer. Callers must derive this
// value from the parsed manifest shape; artifactType alone is uploader input
// and is not sufficient to grant the trust-policy exemption.
func SetOCIProvenanceArtifact(attributes map[string]any) map[string]any {
	result := cloneMap(attributes)
	oci, _ := result["oci"].(map[string]any)
	oci = cloneMap(oci)
	oci["provenanceArtifact"] = true
	result["oci"] = oci
	return result
}

// OCIProvenanceArtifact reports whether the protected ingestion marker is set.
func OCIProvenanceArtifact(attributes map[string]any) bool {
	oci, _ := attributes["oci"].(map[string]any)
	validated, _ := oci["provenanceArtifact"].(bool)
	return validated
}

// ClassificationHasValue reports whether any classification label value equals
// the given value. Classification keys are rule-defined, so search matches on the
// value across every key rather than a single fixed label.
func ClassificationHasValue(attributes map[string]any, value string) bool {
	classification, ok := attributes[ClassificationNamespace].(map[string]any)
	if !ok {
		return false
	}
	for _, stored := range classification {
		if label, ok := stored.(string); ok && label == value {
			return true
		}
	}
	return false
}

// Project returns the public/evaluation view of an asset's attributes. The
// returned map is independent from Asset.Attributes and may be safely modified.
func Project(asset domain.Asset, repository domain.Repository) map[string]any {
	attributes := cloneMap(asset.Attributes)
	removeReservedAliases(attributes)
	// The cache identity is an internal lookup key, never a format coordinate.
	// Keep the persisted key on Asset.Path for storage operations and project
	// through the original request path when the two differ.
	formatAsset := asset
	if asset.FormatPath != "" {
		formatAsset.Path = asset.FormatPath
	}
	attributes["sys"] = systemAttributes(formatAsset, repository)

	if projector, found := formatProjectors[repository.Format]; found {
		attributes[repository.Format] = projector(formatAsset, repository)
	} else if projected, ok := pluginFormatAttributes(formatAsset, repository); ok {
		attributes[repository.Format] = projected
	}
	return attributes
}

// pluginFormatAttributes asks a registered format plugin for the asset's
// coordinate namespace.
func pluginFormatAttributes(
	asset domain.Asset,
	repository domain.Repository,
) (map[string]any, bool) {
	registered, found := spiformat.Lookup(repository.Format)
	if !found {
		return nil, false
	}
	projector, ok := registered.(spiformat.AttributeProjector)
	if !ok {
		return nil, false
	}
	projected := projector.ProjectAttributes(spiformat.Asset{
		Repository:  asset.Repository,
		Path:        asset.Path,
		Digest:      asset.Digest,
		Size:        asset.Size,
		ContentType: asset.ContentType,
		Kind:        asset.Kind,
		Reference:   asset.Reference,
	})
	return projected, projected != nil
}

// removeReservedAliases removes representations that can collide with the
// authoritative projected shape. Classification and provenance remain stored,
// system-owned maps; sys and format-coordinate roots are rebuilt below.
func removeReservedAliases(attributes map[string]any) {
	delete(attributes, "sys")
	for format := range formatProjectors {
		delete(attributes, format)
	}
	for key := range attributes {
		if !strings.Contains(key, ".") && spiformat.Registered(key) {
			delete(attributes, key)
		}
		if strings.Contains(key, ".") && IsReservedNamespace(key) {
			delete(attributes, key)
		}
	}
}

// Lookup returns a projected attribute addressed by a dotted path. Writer-owned
// namespaces may themselves contain dots, so traversal selects the longest key
// prefix at each level. Reserved roots are always traversed from their protected
// projected object, preventing flat stored keys such as "sys.blobStore" from
// shadowing authoritative values.
func Lookup(attributes map[string]any, attributePath string) (any, bool) {
	segments := strings.Split(attributePath, ".")
	if len(segments) == 0 || segments[0] == "" {
		return nil, false
	}
	var current any = attributes
	for len(segments) > 0 {
		object, ok := current.(map[string]any)
		if !ok {
			return nil, false
		}
		prefixLength := longestAttributePrefix(object, segments)
		if prefixLength == 0 {
			return nil, false
		}
		key := strings.Join(segments[:prefixLength], ".")
		current = object[key]
		segments = segments[prefixLength:]
	}
	return current, true
}

func longestAttributePrefix(object map[string]any, segments []string) int {
	if isReservedRoot(segments[0]) {
		if _, found := object[segments[0]]; found {
			return 1
		}
		return 0
	}
	for length := len(segments); length > 0; length-- {
		if _, found := object[strings.Join(segments[:length], ".")]; found {
			return length
		}
	}
	return 0
}

// formatProjectors is the coordinate hook for repository formats. A new format
// adds its authoritative virtual coordinate namespace here rather than copying
// coordinates into every stored asset.
var formatProjectors = map[string]func(domain.Asset, domain.Repository) map[string]any{
	"oci": func(asset domain.Asset, _ domain.Repository) map[string]any {
		return ociAttributes(asset)
	},
	"raw": rawAttributes,
}

// rawAttributes adds the component and version of the repository's first
// matching component rule, so rules can address a payload and its side files
// alike.
func rawAttributes(asset domain.Asset, repository domain.Repository) map[string]any {
	attributes := map[string]any{"path": asset.Path}
	component, version := asset.Component, asset.ComponentVersion
	if !asset.ComponentStored {
		component, version = Component(asset, repository)
	}
	if component != "" {
		attributes["component"] = component
		attributes["version"] = version
	}
	return attributes
}

// Component derives the identity stored in an asset's component columns: the
// first matching Raw component pattern, or an OCI tagged manifest's image and
// tag. Other assets have none. asset.Path must be the format path.
func Component(asset domain.Asset, repository domain.Repository) (component, version string) {
	switch repository.Format {
	case "raw":
		if match, ok := rawcomponent.ForConfig(repository.FormatConfig).Match(asset.Path); ok {
			return match.Name, match.Version
		}
	case "oci":
		if asset.Kind != "oci-manifest" || asset.Reference == "" || isDigestReference(asset.Reference) {
			return "", ""
		}
		if image, found := ociImage(asset.Path); found {
			return image, asset.Reference
		}
	}
	return "", ""
}

func systemAttributes(asset domain.Asset, repository domain.Repository) map[string]any {
	attributes := map[string]any{
		"repository":  repository.Name,
		"format":      repository.Format,
		"type":        repository.Type,
		"blobStore":   repository.BlobStore,
		"path":        asset.Path,
		"digest":      asset.Digest,
		"size":        asset.Size,
		"contentType": asset.ContentType,
		"kind":        asset.Kind,
		"createdAt":   asset.CreatedAt,
		"updatedAt":   asset.UpdatedAt,
		"validatedAt": asset.ValidatedAt,
	}
	if asset.LastAccessed != nil {
		attributes["lastAccessed"] = *asset.LastAccessed
	}
	return attributes
}

func ociAttributes(asset domain.Asset) map[string]any {
	attributes := map[string]any{
		"digest":    asset.Digest,
		"mediaType": asset.ContentType,
	}
	if image, found := ociImage(asset.Path); found {
		attributes["image"] = image
	}
	if asset.Reference != "" {
		if asset.Reference == asset.Digest || isDigestReference(asset.Reference) {
			attributes["digest"] = asset.Reference
		} else {
			attributes["tag"] = asset.Reference
		}
	}
	// artifactType is format metadata rather than a duplicated asset row field.
	// OCI ingestion may persist it in the protected namespace when the manifest
	// supplies it; projection carries it into the coordinate view when present.
	if stored, ok := asset.Attributes["oci"].(map[string]any); ok {
		if artifactType, ok := stored["artifactType"].(string); ok && artifactType != "" {
			attributes["artifactType"] = artifactType
		}
	}
	return attributes
}

func ociImage(assetPath string) (string, bool) {
	selectedIndex := -1
	selectedMarker := ""
	for _, marker := range []string{"/manifests/", "/blobs/"} {
		index := strings.LastIndex(assetPath, marker)
		if index <= 0 || index <= selectedIndex {
			continue
		}
		reference := assetPath[index+len(marker):]
		if reference == "" || strings.Contains(reference, "/") {
			continue
		}
		selectedIndex = index
		selectedMarker = marker
	}
	if selectedIndex > 0 && selectedMarker != "" {
		return strings.TrimPrefix(assetPath[:selectedIndex], "v2/"), true
	}
	return "", false
}

func isDigestReference(reference string) bool {
	algorithm, encoded, found := strings.Cut(reference, ":")
	if !found || algorithm == "" || encoded == "" {
		return false
	}
	for _, character := range encoded {
		if (character < '0' || character > '9') &&
			(character < 'a' || character > 'f') &&
			(character < 'A' || character > 'F') {
			return false
		}
	}
	return true
}

func cloneMap(source map[string]any) map[string]any {
	result := make(map[string]any, len(source))
	for key, value := range source {
		result[key] = cloneValue(value)
	}
	return result
}

func cloneValue(value any) any {
	switch typed := value.(type) {
	case map[string]any:
		return cloneMap(typed)
	case []any:
		result := make([]any, len(typed))
		for index, element := range typed {
			result[index] = cloneValue(element)
		}
		return result
	default:
		return value
	}
}
