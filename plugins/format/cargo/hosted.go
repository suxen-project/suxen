package cargo

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"sort"
	"strconv"
	"strings"

	"golang.org/x/mod/semver"

	"github.com/suxen-project/suxen/spi/format"
)

const (
	// publishPath is the registry API endpoint cargo publishes to, relative to
	// the repository root (its config.json `api` entry).
	publishPath = "api/v1/crates/new"
	// crateContentType is the stored content type of an uploaded .crate.
	crateContentType = "application/gzip"
	// metadataPrefix holds the per-version index entries a hosted repository
	// synthesizes its index files from.
	metadataPrefix = "index-meta/"
	// claimPrefix holds immutable spelling claims for SemVer identities.
	// Unlike index metadata, these rows never enter a sparse index.
	claimPrefix = "index-claim/"
	// metadataLimit bounds the incoming JSON frame before allocating it.
	metadataLimit = 1 << 20
	// storedMetadataLimit bounds an encoded sparse-index entry. Encoding can
	// expand publish metadata through escaping and dependency normalization.
	// Leave room for entries accepted by older releases above metadataLimit.
	// Reserve one byte for the newline when a single entry is served as an index.
	storedMetadataLimit = (8 << 20) - 1
	publishMemoryLimit  = 64 << 20
)

// WireAction claims, on hosted repositories only, the publish PUT, the
// synthesized config.json, and the synthesized sparse-index files. Crate
// downloads (dl/…/download) keep riding the generic asset pipeline, so proxy
// and group repositories are untouched.
func (Format) WireAction(
	repository format.Repository,
	method string,
	requestPath string,
	_ url.Values,
) (string, bool) {
	if repository.Type != "hosted" {
		return "", false
	}
	switch method {
	case http.MethodPut:
		if strings.Trim(requestPath, "/") == publishPath {
			return "write", true
		}
	case http.MethodGet, http.MethodHead:
		if info, ok := parsePath(requestPath); ok && (info.kind == kindConfig || info.kind == kindIndex) {
			return "read", true
		}
	}
	return "", false
}

// ServeWire serves a hosted publish or a synthesized config/index response.
func (Format) ServeWire(
	w http.ResponseWriter,
	r *http.Request,
	repository format.Repository,
	requestPath string,
	tools format.WireTools,
) {
	if r.Method == http.MethodPut {
		if strings.Trim(requestPath, "/") == publishPath {
			publish(w, r, tools)
			return
		}
		http.NotFound(w, r)
		return
	}
	info, ok := parsePath(requestPath)
	if !ok {
		http.NotFound(w, r)
		return
	}
	switch info.kind {
	case kindConfig:
		serveConfig(w, r, repository, tools)
	case kindIndex:
		serveIndex(w, r, repository, tools, info.name)
	default:
		http.NotFound(w, r)
	}
}

// publishMeta is the subset of the cargo publish metadata this format consumes.
type publishMeta struct {
	Name        string              `json:"name"`
	Vers        string              `json:"vers"`
	Deps        []publishDep        `json:"deps"`
	Features    map[string][]string `json:"features"`
	Links       *string             `json:"links"`
	RustVersion *string             `json:"rust_version"`
}

type publishDep struct {
	Name               string   `json:"name"`
	VersionReq         string   `json:"version_req"`
	Features           []string `json:"features"`
	Optional           bool     `json:"optional"`
	DefaultFeatures    bool     `json:"default_features"`
	Target             *string  `json:"target"`
	Kind               string   `json:"kind"`
	Registry           *string  `json:"registry"`
	ExplicitNameInToml *string  `json:"explicit_name_in_toml"`
}

// indexEntry is one line of a sparse-index file.
type indexEntry struct {
	Name        string              `json:"name"`
	Vers        string              `json:"vers"`
	Deps        []indexDep          `json:"deps"`
	Cksum       string              `json:"cksum"`
	Features    map[string][]string `json:"features"`
	Yanked      bool                `json:"yanked"`
	Links       *string             `json:"links,omitempty"`
	RustVersion *string             `json:"rust_version,omitempty"`
}

type indexDep struct {
	Name            string   `json:"name"`
	Req             string   `json:"req"`
	Features        []string `json:"features"`
	Optional        bool     `json:"optional"`
	DefaultFeatures bool     `json:"default_features"`
	Target          *string  `json:"target"`
	Kind            string   `json:"kind"`
	Registry        *string  `json:"registry,omitempty"`
	Package         *string  `json:"package,omitempty"`
}

// publish decodes one `cargo publish` body (a length-prefixed metadata JSON
// followed by the length-prefixed .crate), stores the crate and a per-version
// index entry, and confirms with the registry's warnings envelope.
func publish(w http.ResponseWriter, r *http.Request, tools format.WireTools) {
	ctx := r.Context()
	limit := tools.MaxUploadBytes()
	if limit <= 0 {
		limit = 1
	}
	if limit > publishMemoryLimit {
		limit = publishMemoryLimit
	}
	body := http.MaxBytesReader(w, r.Body, limit)
	metaLength, err := readUint32(body)
	if err != nil {
		writeReadError(w, "read metadata length", err)
		return
	}
	if metaLength > metadataLimit || int64(metaLength) > limit {
		writeError(w, http.StatusRequestEntityTooLarge, "publish metadata exceeds the configured limit")
		return
	}
	metaBytes := make([]byte, metaLength)
	if _, err := io.ReadFull(body, metaBytes); err != nil {
		writeReadError(w, "read metadata", err)
		return
	}
	var meta publishMeta
	if err := json.Unmarshal(metaBytes, &meta); err != nil {
		writeError(w, http.StatusBadRequest, "invalid metadata: "+err.Error())
		return
	}
	if !validCrateName(meta.Name) || !validVersion(meta.Vers) {
		writeError(w, http.StatusBadRequest, "publish metadata has no valid name/version")
		return
	}
	if err := checkLegacyVersionClaim(ctx, tools, meta.Name, meta.Vers); err != nil {
		writePublicationError(w, r, tools, err)
		return
	}
	crateLength, err := readUint32(body)
	if err != nil {
		writeReadError(w, "read crate length", err)
		return
	}
	if int64(crateLength) > limit {
		writeError(w, http.StatusRequestEntityTooLarge, "crate exceeds the configured limit")
		return
	}
	crateBytes := make([]byte, crateLength)
	if _, err := io.ReadFull(body, crateBytes); err != nil {
		writeReadError(w, "read crate", err)
		return
	}
	var trailing [1]byte
	if count, err := body.Read(trailing[:]); count != 0 || (err != nil && err != io.EOF) {
		if _, tooLarge := err.(*http.MaxBytesError); tooLarge {
			writeError(w, http.StatusRequestEntityTooLarge, "publish body exceeds the configured limit")
		} else {
			writeError(w, http.StatusBadRequest, "publish body has trailing data")
		}
		return
	}
	cratePath := downloadPrefix + meta.Name + "/" + meta.Vers + downloadSuffix
	digest := sha256.Sum256(crateBytes)
	entry := buildIndexEntry(meta, hex.EncodeToString(digest[:]))
	line, err := json.Marshal(entry)
	if err != nil {
		writeInternalError(w, r, tools, "render index entry", err)
		return
	}
	if len(line) > storedMetadataLimit {
		writeError(w, http.StatusRequestEntityTooLarge, "encoded cargo index entry exceeds the supported limit")
		return
	}
	assets := []format.AssetInput{
		{Path: cratePath, ContentType: crateContentType, Content: bytes.NewReader(crateBytes)},
		{Path: metadataPath(meta.Name, meta.Vers), ContentType: configContentType, Content: bytes.NewReader(line), Metadata: true},
	}
	// The claim's content is the exact spelling, while its path omits build
	// metadata. An identical spelling can replace its crate/index entry when
	// repository policy allows it; a different spelling conflicts atomically.
	assets = append(assets, format.AssetInput{
		Path: claimPath(meta.Name, meta.Vers), ContentType: "text/plain; charset=utf-8",
		Content: strings.NewReader(meta.Name + "\n" + meta.Vers), Metadata: true, ImmutableIdentity: true,
	})
	if _, err := tools.StoreAssets(ctx, assets); err != nil {
		writePublicationError(w, r, tools, err)
		return
	}
	w.Header().Set("Content-Type", configContentType)
	w.WriteHeader(http.StatusOK)
	_ = json.NewEncoder(w).Encode(map[string]any{
		"warnings": map[string]any{"invalid_categories": []string{}, "invalid_badges": []string{}, "other": []string{}},
	})
}

func writePublicationError(w http.ResponseWriter, r *http.Request, tools format.WireTools, err error) {
	switch {
	case errors.Is(err, format.ErrConflict):
		writeError(w, http.StatusConflict, "crate version already exists")
	case errors.Is(err, format.ErrPolicyRejected):
		writeError(w, http.StatusForbidden, "publication rejected by repository policy")
	case errors.Is(err, format.ErrUploadLimit):
		writeError(w, http.StatusRequestEntityTooLarge, "publication exceeds upload limit")
	default:
		writeInternalError(w, r, tools, "publish crate", err)
	}
}

func writeReadError(w http.ResponseWriter, operation string, err error) {
	if _, tooLarge := err.(*http.MaxBytesError); tooLarge {
		writeError(w, http.StatusRequestEntityTooLarge, "publish body exceeds the configured limit")
		return
	}
	writeError(w, http.StatusBadRequest, operation+" failed")
}

// buildIndexEntry maps cargo's publish metadata to a sparse-index entry. The
// checksum is the stored crate's own sha256, never a client-supplied value.
func buildIndexEntry(meta publishMeta, cksum string) indexEntry {
	deps := make([]indexDep, 0, len(meta.Deps))
	for _, dependency := range meta.Deps {
		entry := indexDep{
			Name:            dependency.Name,
			Req:             dependency.VersionReq,
			Features:        nonNilStrings(dependency.Features),
			Optional:        dependency.Optional,
			DefaultFeatures: dependency.DefaultFeatures,
			Target:          dependency.Target,
			Kind:            dependency.Kind,
			Registry:        dependency.Registry,
		}
		if entry.Kind == "" {
			entry.Kind = "normal"
		}
		// A renamed dependency carries its crate name in `explicit_name_in_toml`
		// and the real crate under `package`.
		if dependency.ExplicitNameInToml != nil && *dependency.ExplicitNameInToml != "" && *dependency.ExplicitNameInToml != dependency.Name {
			pkg := dependency.Name
			entry.Name = *dependency.ExplicitNameInToml
			entry.Package = &pkg
		}
		deps = append(deps, entry)
	}
	features := meta.Features
	if features == nil {
		features = map[string][]string{}
	}
	return indexEntry{
		Name:        meta.Name,
		Vers:        meta.Vers,
		Deps:        deps,
		Cksum:       cksum,
		Features:    features,
		Yanked:      false,
		Links:       meta.Links,
		RustVersion: meta.RustVersion,
	}
}

// serveConfig synthesizes the registry config: a download template and an api
// endpoint, both under the hostname the client used, so cargo fetches crates
// and publishes back through this repository.
func serveConfig(w http.ResponseWriter, r *http.Request, repository format.Repository, tools format.WireTools) {
	repositoryURL := repositoryURL(r, repository.Name)
	config, err := json.Marshal(map[string]any{
		"dl":  repositoryURL + downloadTemplate,
		"api": repositoryURL,
		// Cargo sends registry tokens on crate downloads only when this flag is
		// true. suxen repositories are private by default, so advertise one
		// stable client contract instead of varying config by the current caller.
		"auth-required": true,
	})
	if err != nil {
		writeInternalError(w, r, tools, "render config", err)
		return
	}
	w.Header().Set("Content-Type", configContentType)
	w.WriteHeader(http.StatusOK)
	if r.Method != http.MethodHead {
		_, _ = w.Write(config)
	}
}

// serveIndex synthesizes a crate's sparse-index file from its stored per-version
// entries, one JSON line per version in ascending semver order.
func serveIndex(
	w http.ResponseWriter,
	r *http.Request,
	_ format.Repository,
	tools format.WireTools,
	name string,
) {
	ctx := r.Context()
	prefix := metadataPrefix + strings.ToLower(name) + "/"
	type versioned struct {
		version string
		line    []byte
	}
	var entries []versioned
	seen := make(map[string]struct{})
	err := tools.VisitAssetPaths(ctx, prefix, func(assetPath string) (bool, error) {
		version, ok := strings.CutSuffix(strings.TrimPrefix(assetPath, prefix), ".json")
		if !ok || version == "" || strings.Contains(version, "/") {
			return true, nil
		}
		reader, _, found, err := tools.OpenMetadataAsset(ctx, assetPath)
		if err != nil {
			return false, err
		}
		if !found {
			return true, nil
		}
		line, err := readStoredIndexEntry(reader)
		_ = reader.Close()
		if err != nil {
			return false, err
		}
		var entry struct {
			Name string `json:"name"`
			Vers string `json:"vers"`
		}
		if err := json.Unmarshal(line, &entry); err != nil || !validCrateName(entry.Name) || !validVersion(entry.Vers) || entry.Vers != version {
			return true, nil
		}
		_, exists, err := tools.StatAsset(ctx, downloadPrefix+entry.Name+"/"+version+downloadSuffix)
		if err != nil {
			return false, err
		}
		if !exists {
			return true, nil
		}
		identity := versionIdentity(version)
		if _, duplicate := seen[identity]; duplicate {
			return true, nil
		}
		seen[identity] = struct{}{}
		entries = append(entries, versioned{version: version, line: bytes.TrimSpace(line)})
		return true, nil
	})
	if err != nil {
		writeInternalError(w, r, tools, "list versions", err)
		return
	}
	if len(entries) == 0 {
		http.NotFound(w, r)
		return
	}
	sort.Slice(entries, func(i, j int) bool {
		return compareVersions(entries[i].version, entries[j].version) < 0
	})
	var index bytes.Buffer
	for _, entry := range entries {
		index.Write(entry.line)
		index.WriteByte('\n')
	}
	w.Header().Set("Content-Type", indexContentType)
	w.WriteHeader(http.StatusOK)
	if r.Method != http.MethodHead {
		_, _ = w.Write(index.Bytes())
	}
}

func writeInternalError(w http.ResponseWriter, r *http.Request, tools format.WireTools, operation string, err error) {
	format.ReportWireError(tools, r.Context(), operation, err)
	writeError(w, http.StatusInternalServerError, "registry temporarily unavailable")
}

// compareVersions orders two crate versions by semver, sorting any version
// that is not valid semver after the valid ones (by raw string).
func compareVersions(a, b string) int {
	canonicalA, canonicalB := "v"+a, "v"+b
	validA, validB := validVersion(a), validVersion(b)
	switch {
	case validA && validB:
		return semver.Compare(canonicalA, canonicalB)
	case validA:
		return -1
	case validB:
		return 1
	default:
		return strings.Compare(a, b)
	}
}

func metadataPath(name, version string) string {
	return metadataPrefix + strings.ToLower(name) + "/" + version + ".json"
}

func claimPath(name, version string) string {
	return claimPrefix + strings.ToLower(name) + "/" + versionIdentity(version) + ".txt"
}

// Older repositories used the canonical index metadata row as the identity
// claim. Honor an existing live row while the new dedicated claim is created.
func checkLegacyVersionClaim(ctx context.Context, tools format.WireTools, name, version string) error {
	canonical := versionIdentity(version)
	reader, _, found, err := tools.OpenMetadataAsset(ctx, metadataPath(name, canonical))
	if err != nil || !found {
		return err
	}
	defer reader.Close()
	var entry struct {
		Name string `json:"name"`
		Vers string `json:"vers"`
	}
	line, err := readStoredIndexEntry(reader)
	if err != nil {
		return err
	}
	if err := json.Unmarshal(line, &entry); err != nil || !validCrateName(entry.Name) || !validVersion(entry.Vers) {
		return errors.New("stored canonical cargo index metadata is invalid")
	}
	if entry.Name == name && entry.Vers == version {
		return nil
	}
	_, exists, err := tools.StatAsset(ctx, downloadPrefix+entry.Name+"/"+entry.Vers+downloadSuffix)
	if err != nil {
		return err
	}
	if exists {
		return format.ErrConflict
	}
	return nil
}

// readStoredIndexEntry reads one persisted entry and checks the encoded-size
// contract without treating a truncated JSON document as malformed metadata.
func readStoredIndexEntry(reader io.Reader) ([]byte, error) {
	line, err := io.ReadAll(io.LimitReader(reader, storedMetadataLimit+1))
	if err != nil {
		return nil, err
	}
	if len(line) > storedMetadataLimit {
		return nil, fmt.Errorf("stored cargo index metadata exceeds %d bytes", storedMetadataLimit)
	}
	return line, nil
}

// Cargo treats build metadata as part of the published spelling, but not as
// part of a version's identity.
func versionIdentity(version string) string {
	identity, _, _ := strings.Cut(version, "+")
	return identity
}

func validVersion(version string) bool {
	core := versionIdentity(version)
	core, _, _ = strings.Cut(core, "-")
	if !semver.IsValid("v" + version) {
		return false
	}
	parts := strings.Split(core, ".")
	if len(parts) != 3 {
		return false
	}
	// Rust semver stores core components as u64. Its prerelease identifiers
	// remain strings and may contain larger decimal numbers.
	for _, part := range parts {
		if _, err := strconv.ParseUint(part, 10, 64); err != nil {
			return false
		}
	}
	return true
}

func readUint32(r io.Reader) (uint32, error) {
	var buffer [4]byte
	if _, err := io.ReadFull(r, buffer[:]); err != nil {
		return 0, err
	}
	return binary.LittleEndian.Uint32(buffer[:]), nil
}

func nonNilStrings(values []string) []string {
	if values == nil {
		return []string{}
	}
	return values
}

// repositoryURL is the absolute URL of the repository root as the request
// reached it, so synthesized links work under every hostname the instance is
// served on.
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
	http.Error(w, message, status)
}
