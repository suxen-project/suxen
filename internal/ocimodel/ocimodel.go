// Package ocimodel holds the OCI distribution route model, manifest envelope,
// and pure parsing helpers shared by the proxy-cache ingestion path and the
// hosted OCI handler. It imports neither the server, the content runtime, nor
// the OCI handler package, so both paths resolve routes and manifests through
// one implementation without a dependency cycle.
package ocimodel

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"mime"
	"os"
	"regexp"
	"sort"
	"strings"
)

// MaxManifestBytes bounds the size of an OCI manifest the host will read.
const MaxManifestBytes int64 = 8 << 20

// ErrManifestTooLarge reports a manifest exceeding MaxManifestBytes.
var ErrManifestTooLarge = errors.New("OCI manifest exceeds the 8 MiB limit")

var (
	imageNamePattern = regexp.MustCompile(`^[a-z0-9]+((\.|_|__|-+)[a-z0-9]+)*(\/[a-z0-9]+((\.|_|__|-+)[a-z0-9]+)*)*$`)
	tagPattern       = regexp.MustCompile(`^[a-zA-Z0-9_][a-zA-Z0-9._-]{0,127}$`)
	digestPattern    = regexp.MustCompile(`^sha256:[a-f0-9]{64}$`)
)

var (
	ErrInvalidImageName         = errors.New("invalid OCI image name")
	ErrInvalidManifestReference = errors.New("invalid OCI manifest reference")
)

// RouteKind classifies an OCI distribution route.
type RouteKind uint8

const (
	BlobRoute RouteKind = iota + 1
	ManifestRoute
	TagsRoute
	ReferrersRoute
	UploadRoute
)

// Route is a parsed OCI distribution operation and its image name.
type Route struct {
	Kind      RouteKind
	ImageName string
	Value     string
}

// Validate applies the OCI distribution grammar to route names and manifest
// references. Other digest-bearing routes validate their digests in the handler.
func (route Route) Validate() error {
	if len(route.ImageName) > 255 || !imageNamePattern.MatchString(route.ImageName) {
		return ErrInvalidImageName
	}
	if route.Kind == ManifestRoute && !tagPattern.MatchString(route.Value) && !digestPattern.MatchString(route.Value) {
		return ErrInvalidManifestReference
	}
	return nil
}

// ParseRoute recognizes an operation only at the end of the path. OCI image
// names can contain components such as "blobs" and "manifests", so looking for
// the first matching substring would split valid names early.
func ParseRoute(requestPath string) (Route, bool) {
	path := strings.TrimPrefix(requestPath, "/")
	if path == "" {
		return Route{}, false
	}

	parts := strings.Split(path, "/")
	if len(parts) >= 3 && parts[len(parts)-1] == "" {
		if parts[len(parts)-2] != "uploads" || parts[len(parts)-3] != "blobs" {
			return Route{}, false
		}
		parts = parts[:len(parts)-1]
	}
	for _, part := range parts {
		if part == "" {
			return Route{}, false
		}
	}

	partCount := len(parts)
	if partCount >= 3 && parts[partCount-2] == "blobs" && parts[partCount-1] == "uploads" {
		return newRoute(UploadRoute, parts[:partCount-2], "")
	}
	if partCount >= 4 && parts[partCount-3] == "blobs" && parts[partCount-2] == "uploads" {
		return newRoute(UploadRoute, parts[:partCount-3], parts[partCount-1])
	}
	if partCount >= 3 && parts[partCount-2] == "blobs" {
		return newRoute(BlobRoute, parts[:partCount-2], parts[partCount-1])
	}
	if partCount >= 3 && parts[partCount-2] == "manifests" {
		return newRoute(ManifestRoute, parts[:partCount-2], parts[partCount-1])
	}
	if partCount >= 3 && parts[partCount-2] == "tags" && parts[partCount-1] == "list" {
		return newRoute(TagsRoute, parts[:partCount-2], "")
	}
	if partCount >= 3 && parts[partCount-2] == "referrers" {
		return newRoute(ReferrersRoute, parts[:partCount-2], parts[partCount-1])
	}
	return Route{}, false
}

func newRoute(kind RouteKind, imageParts []string, value string) (Route, bool) {
	if len(imageParts) == 0 {
		return Route{}, false
	}
	return Route{
		Kind:      kind,
		ImageName: strings.Join(imageParts, "/"),
		Value:     value,
	}, true
}

// ParseAssetPath parses a stored asset path of the form "v2/<name>/<op>/<ref>".
func ParseAssetPath(assetPath string) (Route, bool) {
	requestPath, found := strings.CutPrefix(assetPath, "v2/")
	if !found {
		return Route{}, false
	}
	return ParseRoute(requestPath)
}

// ManifestEnvelope is the subset of an OCI manifest the host inspects to derive
// dependencies and provenance.
type ManifestEnvelope struct {
	SchemaVersion int               `json:"schemaVersion"`
	MediaType     string            `json:"mediaType"`
	ArtifactType  string            `json:"artifactType"`
	Annotations   map[string]string `json:"annotations"`
	Config        *Descriptor       `json:"config"`
	Layers        []Descriptor      `json:"layers"`
	Manifests     []Descriptor      `json:"manifests"`
	Subject       *Descriptor       `json:"subject"`
}

// ValidateHostedManifest checks the required structure of the OCI and Docker
// manifest families supported by hosted repositories. Unknown manifest types
// remain extensible, but any descriptor they include must still be complete.
func ValidateHostedManifest(manifest ManifestEnvelope) error {
	const (
		ociImage    = "application/vnd.oci.image.manifest.v1+json"
		ociIndex    = "application/vnd.oci.image.index.v1+json"
		dockerImage = "application/vnd.docker.distribution.manifest.v2+json"
		dockerIndex = "application/vnd.docker.distribution.manifest.list.v2+json"
	)
	switch manifest.MediaType {
	case ociImage, dockerImage:
		if manifest.Config == nil || manifest.Layers == nil || manifest.Manifests != nil {
			return errors.New("image manifest requires config and layers, without manifests")
		}
	case ociIndex, dockerIndex:
		if manifest.Manifests == nil || manifest.Config != nil || manifest.Layers != nil {
			return errors.New("image index requires manifests, without config or layers")
		}
	}
	if manifest.Config != nil {
		if err := validateDescriptor("config", *manifest.Config); err != nil {
			return err
		}
	}
	for index, descriptor := range manifest.Layers {
		if err := validateDescriptor(fmt.Sprintf("layers[%d]", index), descriptor); err != nil {
			return err
		}
	}
	for index, descriptor := range manifest.Manifests {
		if err := validateDescriptor(fmt.Sprintf("manifests[%d]", index), descriptor); err != nil {
			return err
		}
	}
	if manifest.Subject != nil {
		if err := validateDescriptor("subject", *manifest.Subject); err != nil {
			return err
		}
	}
	return nil
}

func validateDescriptor(name string, descriptor Descriptor) error {
	if descriptor.MediaType == "" || !digestPattern.MatchString(descriptor.Digest) || !descriptor.sizePresent || descriptor.Size < 0 {
		return fmt.Errorf("%s descriptor requires a mediaType, canonical sha256 digest, and nonnegative size", name)
	}
	if parsed, parameters, err := mime.ParseMediaType(descriptor.MediaType); err != nil || parsed != descriptor.MediaType || len(parameters) != 0 {
		return fmt.Errorf("%s descriptor has an invalid mediaType", name)
	}
	return nil
}

// Descriptor is an OCI content descriptor.
type Descriptor struct {
	MediaType    string            `json:"mediaType"`
	ArtifactType string            `json:"artifactType,omitempty"`
	Digest       string            `json:"digest"`
	Size         int64             `json:"size"`
	Annotations  map[string]string `json:"annotations,omitempty"`
	sizePresent  bool
}

func (descriptor *Descriptor) UnmarshalJSON(data []byte) error {
	type fields Descriptor
	var decoded fields
	if err := json.Unmarshal(data, &decoded); err != nil {
		return err
	}
	var object map[string]json.RawMessage
	if err := json.Unmarshal(data, &object); err != nil {
		return err
	}
	decoded.sizePresent = object["size"] != nil && string(object["size"]) != "null"
	*descriptor = Descriptor(decoded)
	return nil
}

// ReadManifestEnvelope decodes a staged manifest file, enforcing the size limit.
func ReadManifestEnvelope(filePath string) (ManifestEnvelope, error) {
	file, err := os.Open(filePath)
	if err != nil {
		return ManifestEnvelope{}, fmt.Errorf("open manifest: %w", err)
	}
	defer file.Close()
	info, err := file.Stat()
	if err != nil {
		return ManifestEnvelope{}, fmt.Errorf("stat manifest: %w", err)
	}
	if info.Size() > MaxManifestBytes {
		return ManifestEnvelope{}, ErrManifestTooLarge
	}

	var manifest ManifestEnvelope
	decoder := json.NewDecoder(io.LimitReader(file, MaxManifestBytes))
	if err := decoder.Decode(&manifest); err != nil {
		return ManifestEnvelope{}, fmt.Errorf("decode manifest: %w", err)
	}
	if manifest.SchemaVersion != 2 {
		return ManifestEnvelope{}, errors.New("OCI manifest schemaVersion must be 2")
	}
	if err := decoder.Decode(&struct{}{}); !errors.Is(err, io.EOF) {
		return ManifestEnvelope{}, errors.New("OCI manifest contains trailing data")
	}
	return manifest, nil
}

// ManifestDependencies returns the unique, sorted digests a manifest references.
func ManifestDependencies(manifest ManifestEnvelope) []string {
	digests := make([]string, 0, len(manifest.Layers)+len(manifest.Manifests)+2)
	if manifest.Config != nil && manifest.Config.Digest != "" {
		digests = append(digests, manifest.Config.Digest)
	}
	for _, descriptor := range manifest.Layers {
		if descriptor.Digest != "" {
			digests = append(digests, descriptor.Digest)
		}
	}
	for _, descriptor := range manifest.Manifests {
		if descriptor.Digest != "" {
			digests = append(digests, descriptor.Digest)
		}
	}
	if manifest.Subject != nil && manifest.Subject.Digest != "" {
		digests = append(digests, manifest.Subject.Digest)
	}
	return uniqueSortedStrings(digests)
}

// BlobPath builds the stored asset path for a blob.
func BlobPath(imageName, digest string) string {
	return "v2/" + imageName + "/blobs/" + digest
}

// ManifestPath builds the stored asset path for a manifest reference.
func ManifestPath(imageName, reference string) string {
	return "v2/" + imageName + "/manifests/" + reference
}

func uniqueSortedStrings(values []string) []string {
	set := make(map[string]struct{}, len(values))
	for _, value := range values {
		set[value] = struct{}{}
	}
	result := make([]string, 0, len(set))
	for value := range set {
		result = append(result, value)
	}
	sort.Strings(result)
	return result
}
