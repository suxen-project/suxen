package npm

import (
	"bytes"
	"context"
	"crypto/sha1"
	"crypto/sha256"
	"crypto/sha512"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"

	"github.com/suxen-project/suxen/spi/format"
)

const (
	// metadataInfix separates a package's reserved "-" folder from the
	// per-version metadata assets a hosted publish stores.
	metadataInfix = "/-/metadata/"
	// metadataAssetLimit bounds a per-version metadata asset read.
	metadataAssetLimit = 8 << 20
	// npm embeds base64 tarballs in JSON, which necessarily requires bounded
	// buffering. Keep the parser request body bounded independently of the
	// server-wide upload limit.
	publishMemoryLimit  = 64 << 20
	maximumPublishItems = 64
)

// WireAction claims, on hosted repositories only, the publish PUT and the
// packument GET for a package. Tarball downloads and the per-version metadata
// assets keep riding the generic asset pipeline (ok=false), so proxy and group
// npm repositories are untouched.
func (Format) WireAction(
	repository format.Repository,
	method string,
	requestPath string,
	_ url.Values,
) (string, bool) {
	if repository.Type != "hosted" {
		return "", false
	}
	info, ok := parsePath(requestPath)
	if !ok || info.kind != kindPackument {
		return "", false
	}
	switch method {
	case http.MethodPut:
		return "write", true
	case http.MethodGet, http.MethodHead:
		return "read", true
	default:
		return "", false
	}
}

// ServeWire serves a hosted publish or a hosted packument read.
func (Format) ServeWire(
	w http.ResponseWriter,
	r *http.Request,
	repository format.Repository,
	requestPath string,
	tools format.WireTools,
) {
	info, ok := parsePath(requestPath)
	if !ok || info.kind != kindPackument {
		http.NotFound(w, r)
		return
	}
	switch r.Method {
	case http.MethodPut:
		if !validPublishPackageName(info.name) {
			writeError(w, http.StatusBadRequest, "invalid package name for publication")
			return
		}
		publish(w, r, tools, info.name)
	case http.MethodGet, http.MethodHead:
		servePackument(w, r, repository, tools, info.name)
	default:
		writeError(w, http.StatusMethodNotAllowed, "method not allowed")
	}
}

// publishRequest is the subset of an `npm publish` body this format consumes.
type publishRequest struct {
	Name        string                     `json:"name"`
	Versions    map[string]json.RawMessage `json:"versions"`
	Attachments map[string]attachment      `json:"_attachments"`
}

type attachment struct {
	ContentType string `json:"content_type"`
	Data        string `json:"data"`
}

// publish decodes one publish request and stores each version's tarball and
// metadata as separate assets.
func publish(
	w http.ResponseWriter,
	r *http.Request,
	tools format.WireTools,
	name string,
) {
	ctx := r.Context()
	limit := tools.MaxUploadBytes()
	if limit <= 0 {
		limit = 1
	}
	if limit > publishMemoryLimit {
		limit = publishMemoryLimit
	}
	body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, limit))
	if err != nil {
		if _, ok := err.(*http.MaxBytesError); ok {
			writeError(w, http.StatusRequestEntityTooLarge, "publish body exceeds the configured limit")
		} else {
			writeError(w, http.StatusBadRequest, "read publish body failed")
		}
		return
	}
	var request publishRequest
	if err := json.Unmarshal(body, &request); err != nil {
		writeError(w, http.StatusBadRequest, "invalid publish body: "+err.Error())
		return
	}
	if request.Name != "" && request.Name != name {
		writeError(w, http.StatusBadRequest, fmt.Sprintf("publish name %q does not match path %q", request.Name, name))
		return
	}
	if len(request.Versions) == 0 {
		writeError(w, http.StatusBadRequest, "publish body carries no versions")
		return
	}
	if len(request.Versions) > maximumPublishItems || len(request.Attachments) > maximumPublishItems {
		writeError(w, http.StatusRequestEntityTooLarge, "publish body contains too many versions or attachments")
		return
	}
	inputs := make([]format.AssetInput, 0, len(request.Versions)*2)
	for version, metadata := range request.Versions {
		if !validPublicationVersion(name, version) {
			writeError(w, http.StatusBadRequest, fmt.Sprintf("invalid package version %q", version))
			return
		}
		file := tarballFile(name, version)
		tarball, err := decodeAttachment(request.Attachments, file, len(request.Versions) == 1)
		if err != nil {
			writeError(w, http.StatusBadRequest, err.Error())
			return
		}
		if err := validatePublicationMetadata(name, version, metadata, tarball); err != nil {
			writeError(w, http.StatusBadRequest, err.Error())
			return
		}
		inputs = append(inputs,
			format.AssetInput{Path: tarballPath(name, file), ContentType: "application/octet-stream", Content: bytes.NewReader(tarball)},
			format.AssetInput{Path: metadataPath(name, version), ContentType: packumentContentType, Content: bytes.NewReader(metadata), Metadata: true},
		)
	}
	if _, err := tools.StoreAssets(ctx, inputs); err != nil {
		writePublicationError(w, r, tools, err)
		return
	}
	w.Header().Set("Content-Type", packumentContentType)
	w.WriteHeader(http.StatusCreated)
	_ = json.NewEncoder(w).Encode(map[string]any{"success": true, "id": name})
}

// A publication version becomes one path segment in both the tarball and its
// companion metadata. Require a complete SemVer version, preserving build
// metadata, so hosted discovery and npm clients can use every accepted key.
func validPublicationVersion(name, version string) bool {
	if version == "" || len(metadataPath(name, version)) > 512 {
		return false
	}
	return validNpmVersion(version)
}

func validatePublicationMetadata(name, version string, metadata, tarball []byte) error {
	if len(metadata) > metadataAssetLimit {
		return fmt.Errorf("metadata for version %q exceeds the supported limit", version)
	}
	var document struct {
		Name    string          `json:"name"`
		Version string          `json:"version"`
		Dist    json.RawMessage `json:"dist"`
	}
	if err := json.Unmarshal(metadata, &document); err != nil {
		return fmt.Errorf("invalid metadata for version %q: %w", version, err)
	}
	if document.Name != name || document.Version != version {
		return fmt.Errorf("metadata for version %q must match the published name and version", version)
	}
	var dist map[string]json.RawMessage
	if err := json.Unmarshal(document.Dist, &dist); err != nil || dist == nil {
		return fmt.Errorf("metadata for version %q requires a dist object", version)
	}
	if raw, present := dist["integrity"]; present {
		var integrity string
		if string(raw) == "null" || json.Unmarshal(raw, &integrity) != nil || !validIntegrity(integrity, tarball) {
			return fmt.Errorf("metadata for version %q has an invalid dist.integrity for its attachment", version)
		}
	}
	if raw, present := dist["shasum"]; present {
		var shasum string
		if string(raw) == "null" || json.Unmarshal(raw, &shasum) != nil || !validShasum(shasum, tarball) {
			return fmt.Errorf("metadata for version %q has an invalid dist.shasum for its attachment", version)
		}
	}
	return nil
}

func validIntegrity(integrity string, tarball []byte) bool {
	tokens := strings.Fields(integrity)
	if len(tokens) == 0 {
		return false
	}
	for _, token := range tokens {
		algorithm, encoded, ok := strings.Cut(token, "-")
		if !ok {
			return false
		}
		encoded, _, _ = strings.Cut(encoded, "?")
		want, err := base64.StdEncoding.DecodeString(encoded)
		if err != nil {
			return false
		}
		var got []byte
		switch algorithm {
		case "sha1":
			sum := sha1.Sum(tarball)
			got = sum[:]
		case "sha256":
			sum := sha256.Sum256(tarball)
			got = sum[:]
		case "sha384":
			sum := sha512.Sum384(tarball)
			got = sum[:]
		case "sha512":
			sum := sha512.Sum512(tarball)
			got = sum[:]
		default:
			return false
		}
		if !bytes.Equal(got, want) {
			return false
		}
	}
	return true
}

func validShasum(shasum string, tarball []byte) bool {
	want, err := hex.DecodeString(shasum)
	if err != nil {
		return false
	}
	got := sha1.Sum(tarball)
	return bytes.Equal(got[:], want)
}

func writePublicationError(w http.ResponseWriter, r *http.Request, tools format.WireTools, err error) {
	switch {
	case errors.Is(err, format.ErrConflict):
		writeError(w, http.StatusConflict, "package version already exists")
	case errors.Is(err, format.ErrPolicyRejected):
		writeError(w, http.StatusForbidden, "publication rejected by repository policy")
	case errors.Is(err, format.ErrUploadLimit):
		writeError(w, http.StatusRequestEntityTooLarge, "publication exceeds upload limit")
	default:
		writeInternalError(w, r, tools, "publish package", err)
	}
}

// decodeAttachment finds and base64-decodes the tarball for one version. It
// matches the attachment by the canonical file name, then — when the publish
// carries a single version — falls back to the sole attachment, so it does not
// depend on how a client spells the attachment key.
func decodeAttachment(attachments map[string]attachment, file string, single bool) ([]byte, error) {
	entry, ok := attachments[file]
	if !ok {
		if single && len(attachments) == 1 {
			for _, only := range attachments {
				entry = only
				ok = true
			}
		}
	}
	if !ok {
		return nil, fmt.Errorf("publish body has no attachment for %q", file)
	}
	tarball, err := base64.StdEncoding.DecodeString(strings.TrimSpace(entry.Data))
	if err != nil {
		return nil, fmt.Errorf("decode attachment %q: %w", file, err)
	}
	return tarball, nil
}

// servePackument synthesizes the packument from the stored per-version
// metadata assets, pointing every dist.tarball at this repository under the
// hostname the client used.
func servePackument(
	w http.ResponseWriter,
	r *http.Request,
	repository format.Repository,
	tools format.WireTools,
	name string,
) {
	ctx := r.Context()
	repositoryURL := repositoryURL(r, repository.Name)
	versions := map[string]any{}
	var versionBytes int64
	err := tools.VisitAssetPaths(ctx, name+metadataInfix, func(assetPath string) (bool, error) {
		version, ok := versionFromMetadataPath(name, assetPath)
		if !ok {
			return true, nil
		}
		_, exists, err := tools.StatAsset(ctx, tarballPath(name, tarballFile(name, version)))
		if err != nil {
			return false, err
		}
		if !exists {
			return true, nil
		}
		metadata, ok, err := readAsset(ctx, tools, assetPath)
		if err != nil {
			return false, err
		}
		if !ok {
			return true, nil
		}
		var object map[string]any
		if err := json.Unmarshal(metadata, &object); err != nil {
			return true, nil
		}
		if dist, ok := object["dist"].(map[string]any); ok {
			dist["tarball"] = repositoryURL + "/" + tarballPath(name, tarballFile(name, version))
		}
		if err := addHostedVersion(versions, version, object, &versionBytes, hostedPackumentLimit); err != nil {
			return false, err
		}
		return true, nil
	})
	if err != nil {
		writeInternalError(w, r, tools, "list versions", err)
		return
	}
	if len(versions) == 0 {
		writeError(w, http.StatusNotFound, "package not found")
		return
	}
	document := map[string]any{
		"_id":       name,
		"name":      name,
		"versions":  versions,
		"dist-tags": map[string]any{},
	}
	if latest := highestRelease(versions); latest != "" {
		document["dist-tags"].(map[string]any)["latest"] = latest
	}
	if _, err := jsonEncodedSize(document, hostedPackumentLimit); err != nil {
		writeInternalError(w, r, tools, "render packument", err)
		return
	}
	rendered, err := json.Marshal(document)
	if err != nil {
		writeInternalError(w, r, tools, "render packument", err)
		return
	}
	w.Header().Set("Content-Type", packumentContentType)
	w.Header().Set("Content-Length", fmt.Sprintf("%d", len(rendered)))
	w.WriteHeader(http.StatusOK)
	if r.Method != http.MethodHead {
		_, _ = w.Write(rendered)
	}
}

func addHostedVersion(versions map[string]any, version string, object map[string]any, used *int64, limit int64) error {
	objectSize, err := jsonEncodedSize(object, limit)
	if err != nil {
		return err
	}
	keySize, err := jsonEncodedSize(version, limit)
	if err != nil {
		return err
	}
	// Reserve the entry's colon/comma too; the final document budget accounts
	// exactly for enclosing fields and braces.
	entrySize := objectSize + keySize + 2
	if entrySize > limit-*used {
		return errPackumentBudget
	}
	*used += entrySize
	versions[version] = object
	return nil
}

func readAsset(ctx context.Context, tools format.WireTools, assetPath string) ([]byte, bool, error) {
	reader, _, found, err := tools.OpenMetadataAsset(ctx, assetPath)
	if err != nil || !found {
		return nil, false, err
	}
	defer reader.Close()
	content, err := io.ReadAll(io.LimitReader(reader, metadataAssetLimit))
	if err != nil {
		return nil, false, err
	}
	return content, true, nil
}

// tarballFile is the canonical tarball file name for a package version:
// the unscoped base name, a dash, the version, and .tgz.
func tarballFile(name, version string) string {
	base := name[strings.LastIndex(name, "/")+1:]
	return base + "-" + version + ".tgz"
}

func metadataPath(name, version string) string {
	return name + metadataInfix + version + ".json"
}

// versionFromMetadataPath inverts metadataPath.
func versionFromMetadataPath(name, assetPath string) (string, bool) {
	rest, ok := strings.CutPrefix(assetPath, name+metadataInfix)
	if !ok {
		return "", false
	}
	version, ok := strings.CutSuffix(rest, ".json")
	if !ok || version == "" || strings.Contains(version, "/") {
		return "", false
	}
	return version, true
}

// repositoryURL is the absolute URL of the repository root as the request
// reached it, so a synthesized dist.tarball works under every hostname the
// instance is served on.
func repositoryURL(r *http.Request, repositoryName string) string {
	scheme := "http"
	if r.TLS != nil || forwardedHTTPS(r) {
		scheme = "https"
	}
	host := r.Host
	if host == "" {
		host = "suxen"
	}
	return scheme + "://" + host + "/repository/" + url.PathEscape(repositoryName)
}

func forwardedHTTPS(r *http.Request) bool {
	proto := strings.ToLower(strings.TrimSpace(r.Header.Get("X-Forwarded-Proto")))
	if comma := strings.IndexByte(proto, ','); comma >= 0 {
		proto = strings.TrimSpace(proto[:comma])
	}
	return proto == "https"
}

func writeError(w http.ResponseWriter, status int, message string) {
	w.Header().Set("Content-Type", packumentContentType)
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(map[string]any{"error": message})
}

func writeInternalError(w http.ResponseWriter, r *http.Request, tools format.WireTools, operation string, err error) {
	format.ReportWireError(tools, r.Context(), operation, err)
	writeError(w, http.StatusInternalServerError, "registry temporarily unavailable")
}
