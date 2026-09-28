package pypi

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"html"
	"io"
	"net"
	"net/url"
	"sort"
	"strconv"
	"strings"

	"github.com/suxen-project/suxen/spi/format"
	htmltoken "golang.org/x/net/html"
)

const (
	jsonContentType     = "application/vnd.pypi.simple.v1+json"
	htmlContentType     = "text/html; charset=utf-8"
	cachePathMarker     = ".suxen-query-sha256-"
	cacheIdentityMarker = ".suxen-source-sha256-"
)

// proxyCachePath partitions every proxied distribution by its exact query,
// including an empty query. This also bypasses legacy path-only entries that
// could contain bytes fetched for a different advertised target.
func proxyCachePath(assetPath string, rawQuery string) string {
	info, ok := parsePath(assetPath)
	if !ok || (info.kind != kindFile && info.kind != kindHostedFile) {
		return assetPath
	}
	digest := sha256.Sum256([]byte(rawQuery))
	return assetPath + cachePathMarker + hex.EncodeToString(digest[:])
}

// RewriteIndex points every file link of a project page, and every project
// link of the root index, back at this repository. Links are made absolute
// against the upstream page first, so relative links (devpi, mirrors) work;
// hash fragments and data attributes are preserved.
func (Format) RewriteIndex(
	repository format.Repository,
	assetPath string,
	body []byte,
	contentType string,
	repositoryURL string,
) ([]byte, string, error) {
	return rewriteIndex(repository, assetPath, body, contentType, repositoryURL, nil)
}

// RewriteIndexForAccept renders an index from either cached representation in
// the format selected by this request. Cached bytes remain upstream bytes, so
// later HTML and JSON clients can share one proxy cache entry.
func (Format) RewriteIndexForAccept(
	repository format.Repository,
	assetPath string,
	body []byte,
	contentType string,
	repositoryURL string,
	accept string,
) ([]byte, string, error) {
	selected := simpleMediaTypeForAccept(accept)
	asJSON := selected == jsonContentType
	rewritten, mediaType, err := rewriteIndex(repository, assetPath, body, contentType, repositoryURL, &asJSON)
	if err == nil && mediaType == htmlContentType && selected == vendorHTMLContentType {
		mediaType = vendorHTMLContentType
	}
	return rewritten, mediaType, err
}

func rewriteIndex(
	repository format.Repository,
	assetPath string,
	body []byte,
	contentType string,
	repositoryURL string,
	asJSON *bool,
) ([]byte, string, error) {
	info, ok := parsePath(assetPath)
	if !ok || info.kind == kindFile {
		return body, contentType, nil
	}
	if info.kind == kindRoot {
		return rewriteRootIndex(repository, body, repositoryURL, asJSON)
	}
	page, err := parsePage(body, info.kind)
	if err != nil {
		return nil, "", fmt.Errorf("upstream simple index: %w", err)
	}
	base := pageDocumentURL(repository.Upstream, info, page.baseHref)
	if info.kind == kindProject && page.name == "" {
		page.name = normalizeProjectName(info.project)
	}
	if asJSON != nil {
		page.asJSON = *asJSON
	}
	budget := newOutputBudget()
	for _, entry := range page.entries {
		if link, ok := entry["url"].(string); ok && link != "" {
			rewritten, err := rewriteLinkWithBudget(link, base, repositoryURL, info.kind, &budget)
			if err != nil {
				return nil, "", err
			}
			entry["url"] = rewritten
		}
	}
	// Re-render parsed entries only. Executable upstream markup and attributes
	// never reach the control-plane origin.
	return page.render()
}

// pageDocumentURL applies the first HTML base href to the upstream page URL.
func pageDocumentURL(upstream string, info pathInfo, baseHref string) *url.URL {
	base := pageURL(upstream, info)
	if baseHref == "" {
		return base
	}
	resolved, err := base.Parse(baseHref)
	if err != nil || (resolved.Scheme != "http" && resolved.Scheme != "https") || resolved.Host == "" {
		return base
	}
	resolved.User = nil
	return resolved
}

// pageURL is the upstream URL a page's relative links resolve against.
func pageURL(upstream string, info pathInfo) *url.URL {
	base, err := url.Parse(strings.TrimSuffix(strings.TrimSpace(upstream), "/") + "/" + simplePrefix + "/")
	if err != nil {
		return &url.URL{}
	}
	if info.kind == kindProject {
		base = base.JoinPath(info.project + "/")
	}
	return base
}

// rewriteLink maps one upstream link to this repository. File links become
// files/<scheme>/<host>/<path>; project links (root index) become
// simple/<project>/; anything unparseable is left alone.
func rewriteLink(link string, base *url.URL, upstream string, repositoryURL string, kind pathKind) string {
	rewritten, _ := rewriteLinkWithBudget(link, base, repositoryURL, kind, nil)
	return rewritten
}

func rewriteLinkWithBudget(link string, base *url.URL, repositoryURL string, kind pathKind, budget *outputBudget) (string, error) {
	charge := func(size int64) error {
		if budget != nil {
			return budget.charge(size)
		}
		return nil
	}
	target, err := base.Parse(link)
	if err != nil || (target.Scheme != "http" && target.Scheme != "https") || target.Host == "" {
		return "", nil
	}
	fragment := ""
	if target.Fragment != "" {
		fragment = "#" + target.EscapedFragment()
	}
	if kind == kindRoot {
		// Root index links name projects; keep them under this repository's
		// simple/ tree whatever the upstream layout.
		trimmed := strings.Trim(target.Path, "/")
		project := trimmed[strings.LastIndex(trimmed, "/")+1:]
		if !validProjectName(project) {
			return link, charge(int64(len(link)))
		}
		if err := charge(int64(len(repositoryURL) + len(simplePrefix) + len(project) + 3 + len(fragment))); err != nil {
			return "", err
		}
		return repositoryURL + "/" + simplePrefix + "/" + project + "/" + fragment, nil
	}
	if hostedPath, ok := sameOriginHostedPath(target, repositoryURL); ok {
		if err := charge(int64(len(repositoryURL) + 1 + len(hostedPath) + len(fragment))); err != nil {
			return "", err
		}
		return repositoryURL + "/" + hostedPath + fragment, nil
	}
	query := ""
	if target.RawQuery != "" {
		query = "?" + target.RawQuery
	}
	path := strings.TrimPrefix(target.EscapedPath(), "/")
	length := int64(len(repositoryURL)+1+len(filesPrefix)+len(target.Scheme)+1+len(target.Host)+1+len(query)+len(fragment)) + escapedPathSize(path)
	if err := charge(length); err != nil {
		return "", err
	}
	return repositoryURL + "/" + filePath(target.Scheme, target.Host, target.EscapedPath()) + query + fragment, nil
}

func sameOriginHostedPath(target *url.URL, repositoryURL string) (string, bool) {
	repository, err := url.Parse(repositoryURL)
	if err != nil || !strings.EqualFold(target.Scheme, repository.Scheme) ||
		!strings.EqualFold(target.Host, repository.Host) {
		return "", false
	}
	trimmed := strings.TrimPrefix(target.Path, "/repository/")
	_, assetPath, ok := strings.Cut(trimmed, "/")
	if !ok {
		return "", false
	}
	info, ok := parsePath(assetPath)
	return assetPath, ok && info.kind == kindHostedFile
}

func isJSON(contentType string, body []byte) bool {
	if strings.Contains(strings.ToLower(contentType), "json") {
		return true
	}
	trimmed := bytes.TrimSpace(body)
	return len(trimmed) > 0 && trimmed[0] == '{'
}

// ResolveProxyRequest locates a distribution in the cached project indexes.
// A cold file request requires an index advertising its exact URL. An
// immutable distribution already cached under the exact request identity can
// still be served after its index disappears, without fetching upstream.
// Non-file index paths use the default upstream.
func (f Format) ResolveProxyRequest(
	ctx context.Context,
	repository format.Repository,
	assetPath string,
	rawQuery string,
	stored format.StoredAssets,
) (format.ResolvedProxyRequest, error) {
	cachePath := proxyCachePath(assetPath, rawQuery)
	if _, ok := requestedFileURL(repository, assetPath, rawQuery); !ok {
		return format.ResolvedProxyRequest{CachePath: cachePath}, nil
	}
	advertised, advertisedHashes, expected, ok, quarantined, err := advertisedFileURL(ctx, repository, assetPath, rawQuery, stored)
	if err != nil {
		return format.ResolvedProxyRequest{}, err
	}
	if quarantined {
		return format.ResolvedProxyRequest{}, &format.PolicyViolation{Code: "pypi_quarantined_project", Message: "PyPI project is quarantined"}
	}
	if !ok {
		// The index may have been deleted after this immutable file was
		// fetched. Check only the precise cache identity, including the raw
		// query digest; a prefix match could serve another query or path.
		var retained string
		ambiguous := false
		prefix := cachePath + cacheIdentityMarker
		err := stored.VisitAssetPaths(ctx, cachePath, func(path string) (bool, error) {
			if path != cachePath && !validCacheIdentity(path, prefix) {
				return true, nil
			}
			if retained != "" {
				ambiguous = true
				return false, nil
			}
			retained = path
			return true, nil
		})
		if err != nil {
			return format.ResolvedProxyRequest{}, err
		}
		if retained != "" && !ambiguous {
			// A concurrent deletion must not turn this into an upstream fetch.
			return format.ResolvedProxyRequest{CachePath: retained, CacheOnly: true}, nil
		}
		return format.ResolvedProxyRequest{}, &format.PolicyViolation{
			Code:    "pypi_unadvertised_file",
			Message: "PyPI distribution URL was not advertised by a cached project index",
		}
	}
	return format.ResolvedProxyRequest{
		CachePath:   cachePath + cacheIdentityMarker + proxyAdvertisedIdentity(advertised, advertisedHashes, expected),
		UpstreamURL: advertised, ExpectedDigests: expected,
	}, nil
}

func proxyAdvertisedIdentity(advertised, advertisedHashes string, expected []string) string {
	identity := sha256.Sum256([]byte(advertised + "\n" + advertisedHashes + "\n" + strings.Join(expected, "\n")))
	return hex.EncodeToString(identity[:])
}

const (
	distributionEntry = "distribution"
	metadataEntry     = "metadata"
	signatureEntry    = "signature"
)

// Keep companion generations tied to the parent advertisement. A signature
// and unhashed metadata can change when a file is replaced at the same URL.
func entryIdentityMaterial(entry map[string]any, fragment, kind string) (string, []string) {
	parentHashes, _ := json.Marshal(entry["hashes"])
	parent := fragment + "\n" + string(parentHashes)
	switch kind {
	case metadataEntry:
		value := metadataHashes(entry)
		companionHashes, _ := json.Marshal(value)
		expected := strongestAdvertisedDigest(value)
		if len(expected) != 0 {
			// Keep the existing cache identity when the companion itself has a
			// verified hash; changing the parent cannot change those bytes.
			return string(companionHashes), expected
		}
		return "metadata\n" + parent + "\n" + string(companionHashes), nil
	case signatureEntry:
		return "signature\n" + parent, nil
	default:
		expected := strongestAdvertisedDigest(entry["hashes"])
		if len(expected) == 0 {
			if algorithm, digest, ok := strings.Cut(fragment, "="); ok {
				expected = strongestAdvertisedDigest(map[string]any{algorithm: digest})
			}
		}
		return parent, expected
	}
}

func companionURL(distribution *url.URL, suffix string) *url.URL {
	companion := *distribution
	companion.Path += suffix
	if companion.RawPath != "" {
		companion.RawPath += suffix
	}
	return &companion
}

func validCacheIdentity(path, prefix string) bool {
	if !strings.HasPrefix(path, prefix) || len(path) != len(prefix)+sha256.Size*2 {
		return false
	}
	_, err := hex.DecodeString(path[len(prefix):])
	return err == nil
}

func advertisedFileURL(
	ctx context.Context,
	repository format.Repository,
	assetPath string,
	rawQuery string,
	stored format.StoredAssets,
) (string, string, []string, bool, bool, error) {
	target, ok := requestedFileURL(repository, assetPath, rawQuery)
	if !ok {
		return "", "", nil, false, false, nil
	}
	var advertisedURL string
	var advertisedHashes string
	var expected []string
	var quarantined bool
	err := stored.VisitAssetPaths(ctx, simplePrefix+"/", func(indexPath string) (bool, error) {
		info, ok := parsePath(indexPath)
		if !ok || info.kind != kindProject {
			return true, nil
		}
		body, found, err := stored.ReadAsset(ctx, indexPath)
		if err != nil {
			return false, err
		}
		if !found {
			return true, nil
		}
		page, err := parsePage(body, kindProject)
		if err != nil {
			return true, nil
		}
		base := pageDocumentURL(repository.Upstream, info, page.baseHref)
		for _, entry := range page.entries {
			advertised, _ := entry["url"].(string)
			candidate, err := base.Parse(advertised)
			if err != nil || candidate.Scheme == "" || candidate.Host == "" {
				continue
			}
			candidate.User = nil
			fragment := candidate.Fragment
			candidate.Fragment = ""
			if sameAdvertisedTarget(candidate, target) {
				if page.isQuarantined() {
					quarantined = true
					return false, nil
				}
				advertisedURL = candidate.String()
				advertisedHashes, expected = entryIdentityMaterial(entry, fragment, distributionEntry)
				return false, nil
			}
			if advertisesMetadata(entry) {
				metadataCandidate := companionURL(candidate, ".metadata")
				if sameAdvertisedTarget(metadataCandidate, target) {
					if page.isQuarantined() {
						quarantined = true
						return false, nil
					}
					advertisedURL = metadataCandidate.String()
					advertisedHashes, expected = entryIdentityMaterial(entry, fragment, metadataEntry)
					return false, nil
				}
			}
			if entry["gpg-sig"] == true {
				signature := companionURL(candidate, ".asc")
				if sameAdvertisedTarget(signature, target) {
					if page.isQuarantined() {
						quarantined = true
						return false, nil
					}
					advertisedURL = signature.String()
					advertisedHashes, expected = entryIdentityMaterial(entry, fragment, signatureEntry)
					return false, nil
				}
			}
		}
		return true, nil
	})
	return advertisedURL, advertisedHashes, expected, advertisedURL != "", quarantined, err
}

func metadataHashes(entry map[string]any) any {
	_, value, advertised := metadataAdvertisement(entry)
	if !advertised {
		return nil
	}
	return value
}

// PyPI permits several hash algorithms, while the host verifies the strongest
// supported one. Unsupported algorithms do not suppress a supported hash.
func strongestAdvertisedDigest(value any) []string {
	hashes, ok := value.(map[string]any)
	if !ok {
		return nil
	}
	for _, algorithm := range []string{"sha512", "sha384", "sha256", "sha1"} {
		if value, present := hashes[algorithm]; present {
			digest, ok := value.(string)
			if !ok {
				// Let the shared integrity verifier reject a malformed hash
				// before publishing the upstream response.
				return []string{algorithm + ":"}
			}
			return []string{algorithm + ":" + strings.ToLower(digest)}
		}
	}
	return nil
}

func sameAdvertisedTarget(candidate, requested *url.URL) bool {
	return candidate.Scheme == requested.Scheme && candidate.Host == requested.Host &&
		candidate.EscapedPath() == requested.EscapedPath() && candidate.RawQuery == requested.RawQuery
}

func advertisesMetadata(entry map[string]any) bool {
	_, _, advertised := metadataAdvertisement(entry)
	return advertised
}

// The modern key takes precedence even when it explicitly says that no
// companion exists. All consumers must make the same choice.
func metadataAdvertisement(entry map[string]any) (string, any, bool) {
	for _, key := range []string{"core-metadata", "dist-info-metadata"} {
		value, present := entry[key]
		if !present {
			continue
		}
		switch typed := value.(type) {
		case bool:
			return key, value, typed
		case map[string]any:
			return key, value, metadataValue(typed) != ""
		default:
			return key, value, false
		}
	}
	return "", nil, false
}

func requestedFileURL(repository format.Repository, assetPath string, rawQuery string) (*url.URL, bool) {
	info, ok := parsePath(assetPath)
	if !ok {
		return nil, false
	}
	if info.kind == kindHostedFile {
		base, err := url.Parse(strings.TrimSuffix(repository.Upstream, "/") + "/")
		if err != nil || (base.Scheme != "http" && base.Scheme != "https") || base.Host == "" {
			return nil, false
		}
		target := base.ResolveReference(&url.URL{Path: assetPath, RawQuery: rawQuery})
		target.User = nil
		return target, true
	}
	if info.kind != kindFile {
		return nil, false
	}
	escapedPath := "/" + info.path
	decodedPath, err := url.PathUnescape(escapedPath)
	if err != nil {
		return nil, false
	}
	return &url.URL{
		Scheme:   info.scheme,
		Host:     info.host,
		Path:     decodedPath,
		RawPath:  escapedPath,
		RawQuery: rawQuery,
	}, true
}

// NormalizeGroupSource resolves relative URLs against the proxy member's
// upstream page before the group loses that member context.
func (Format) NormalizeGroupSource(repository format.Repository, assetPath string, body []byte) ([]byte, error) {
	info, ok := parsePath(assetPath)
	if !ok || info.kind == kindFile || info.kind == kindHostedFile {
		return body, nil
	}
	if info.kind == kindRoot {
		return normalizeRootSource(repository, body)
	}
	page, err := parsePage(body, info.kind)
	if err != nil {
		return nil, err
	}
	base := pageDocumentURL(repository.Upstream, info, page.baseHref)
	baseLength := int64(len(base.String()))
	budget := newOutputBudget()
	for _, entry := range page.entries {
		link, _ := entry["url"].(string)
		if link == "" {
			continue
		}
		resolved, valid, err := resolveSourceLinkWithBudget(base, baseLength, link, &budget)
		if err != nil {
			return nil, err
		}
		if !valid {
			continue
		}
		entry["url"] = resolved
	}
	content, _, err := page.render()
	return content, err
}

// GroupMergeSource merges the root index and project pages across members.
// Distribution ownership follows the first member advertising a filename.
func (Format) GroupMergeSource(_ format.Repository, assetPath string) (string, bool) {
	info, ok := parsePath(assetPath)
	if !ok || info.kind == kindFile || info.kind == kindHostedFile {
		return "", false
	}
	return assetPath, true
}

// MergeGroupContent unions member pages. Project pages keep one entry per
// file name (first member wins); the root index one entry per project. The
// intermediate output is JSON so file metadata from later members survives
// until the IndexRewriter applies the client's requested representation.
func (Format) MergeGroupContent(
	_ format.Repository,
	assetPath string,
	sources [][]byte,
) ([]byte, string, error) {
	info, ok := parsePath(assetPath)
	if !ok || info.kind == kindFile || info.kind == kindHostedFile {
		return nil, "", fmt.Errorf("%q is not a mergeable pypi path", assetPath)
	}
	if info.kind == kindRoot {
		return mergeRootIndexes(sources)
	}
	var merged *simplePage
	for _, source := range sources {
		page, err := parsePage(source, info.kind)
		if err != nil {
			continue
		}
		if merged == nil {
			merged = page
			continue
		}
		merged.merge(page)
	}
	if merged == nil {
		return nil, "", errors.New("no member of the group serves a valid simple index page")
	}
	merged.asJSON = true
	return merged.render()
}

// simplePage is the format-independent model of a simple index page: a list
// of entries (files or projects) keyed by their name, each carrying its raw
// PEP 691 object so JSON metadata survives the round trip.
type simplePage struct {
	baseHref       string
	kind           pathKind
	asJSON         bool
	name           string
	document       map[string]any
	entries        []map[string]any
	seen           map[string]struct{}
	listedVersions []string
	hasVersionList bool
}

func (page *simplePage) add(entry map[string]any) {
	if page.kind == kindProject {
		if hashes, ok := entry["hashes"].(map[string]any); !ok || hashes == nil {
			entry["hashes"] = map[string]any{}
		}
		// PEP 691 carries digests in hashes, while HTML uses a URL fragment.
		// Canonicalize either representation before rewrite and cache identity
		// selection. When both are present, the JSON hashes object takes
		// precedence so one file never advertises two conflicting digests.
		if link, ok := entry["url"].(string); ok {
			if withoutFragment, fragment, found := strings.Cut(link, "#"); found {
				if algorithm, digest, valid := strings.Cut(fragment, "="); valid && algorithm != "" && digest != "" {
					hashes := entry["hashes"].(map[string]any)
					if _, exists := hashes[algorithm]; !exists {
						hashes[algorithm] = digest
					}
					entry["url"] = withoutFragment
				}
			}
		}
	}
	key, _ := entry["filename"].(string)
	if page.kind == kindRoot {
		key, _ = entry["name"].(string)
		key = normalizeProjectName(key)
	}
	if key == "" {
		return
	}
	if _, duplicate := page.seen[key]; duplicate {
		return
	}
	page.seen[key] = struct{}{}
	page.entries = append(page.entries, entry)
}

func (page *simplePage) merge(other *simplePage) {
	if other.isQuarantined() && !page.isQuarantined() {
		if page.document == nil {
			page.document = make(map[string]any)
		}
		page.document["project-status"] = other.projectStatus()
	}
	for _, entry := range other.entries {
		page.add(entry)
	}
	page.listedVersions = append(page.listedVersions, other.listedVersions...)
	page.hasVersionList = page.hasVersionList || other.hasVersionList
}

func parsePage(body []byte, kind pathKind) (*simplePage, error) {
	page := &simplePage{kind: kind, entries: make([]map[string]any, 0), seen: map[string]struct{}{}}
	if isJSON("", body) {
		var document map[string]any
		decoder := json.NewDecoder(bytes.NewReader(body))
		decoder.UseNumber()
		if err := decoder.Decode(&document); err != nil {
			return nil, err
		}
		var trailing any
		if err := decoder.Decode(&trailing); err != io.EOF {
			if err != nil {
				return nil, err
			}
			return nil, errors.New("unexpected data after JSON simple index")
		}
		page.asJSON = true
		page.document = document
		page.name, _ = document["name"].(string)
		if versions, ok := document["versions"].([]any); ok {
			page.hasVersionList = true
			for _, version := range versions {
				if text, ok := version.(string); ok {
					page.listedVersions = append(page.listedVersions, text)
				}
			}
		}
		key := "files"
		if kind == kindRoot {
			key = "projects"
		}
		entries, _ := document[key].([]any)
		for _, entry := range entries {
			if object, ok := entry.(map[string]any); ok {
				page.add(object)
			}
		}
		return page, nil
	}
	tokenizer := htmltoken.NewTokenizer(bytes.NewReader(body))
	var anchor *htmltoken.Token
	var baseSeen bool
	var label strings.Builder
	for {
		switch tokenizer.Next() {
		case htmltoken.ErrorToken:
			if tokenizer.Err() != nil && tokenizer.Err() != io.EOF {
				return nil, tokenizer.Err()
			}
			goto parsed
		case htmltoken.StartTagToken, htmltoken.SelfClosingTagToken:
			token := tokenizer.Token()
			if token.Data == "meta" && kind == kindProject {
				page.addProjectStatusMeta(token)
			}
			if token.Data == "base" && !baseSeen {
				for _, attr := range token.Attr {
					if strings.EqualFold(attr.Key, "href") {
						page.baseHref = attr.Val
						baseSeen = true
						break
					}
				}
			}
			if token.Data == "a" {
				anchor = &token
				label.Reset()
			}
		case htmltoken.TextToken:
			if anchor != nil {
				label.WriteString(tokenizer.Token().Data)
			}
		case htmltoken.EndTagToken:
			if tokenizer.Token().Data == "a" && anchor != nil {
				page.add(anchorEntry(*anchor, label.String(), kind))
				anchor = nil
			}
		}
	}

parsed:
	if len(page.entries) == 0 && !bytes.Contains(bytes.ToLower(body), []byte("<html")) && !bytes.Contains(body, []byte("<a")) {
		return nil, errors.New("not a simple index page")
	}
	return page, nil
}

// anchorEntry converts a PEP 503 anchor into the PEP 691 object shape.
func anchorEntry(anchor htmltoken.Token, label string, kind pathKind) map[string]any {
	text := strings.TrimSpace(label)
	entry := map[string]any{}
	if kind == kindRoot {
		entry["name"] = text
	} else {
		entry["filename"] = text
	}
	for _, attribute := range anchor.Attr {
		name := strings.ToLower(attribute.Key)
		value := attribute.Val
		switch name {
		case "href":
			link, fragment, _ := strings.Cut(value, "#")
			entry["url"] = link
			if algorithm, digest, ok := strings.Cut(fragment, "="); ok && algorithm != "" {
				entry["hashes"] = map[string]any{algorithm: digest}
			}
		case "data-requires-python":
			entry["requires-python"] = value
		case "data-yanked":
			if value == "" {
				entry["yanked"] = true
			} else {
				entry["yanked"] = value
			}
		case "data-core-metadata", "data-dist-info-metadata":
			entry[strings.TrimPrefix(name, "data-")] = metadataAttribute(value)
		case "data-gpg-sig":
			entry["gpg-sig"] = value == "true"
		case "data-provenance":
			entry["provenance"] = value
		}
	}
	return entry
}

// addProjectStatusMeta maps the HTML 1.4 head tags to the JSON project-status
// object. The first value for each field wins, as it does for file entries.
func (page *simplePage) addProjectStatusMeta(token htmltoken.Token) {
	var name, value string
	for _, attr := range token.Attr {
		switch strings.ToLower(attr.Key) {
		case "name":
			name = strings.ToLower(attr.Val)
		case "content":
			value = attr.Val
		}
	}
	key := ""
	switch name {
	case "pypi:project-status":
		key = "status"
	case "pypi:project-status-reason":
		key = "reason"
	default:
		return
	}
	if page.document == nil {
		page.document = make(map[string]any)
	}
	status, _ := page.document["project-status"].(map[string]any)
	if status == nil {
		status = make(map[string]any)
		page.document["project-status"] = status
	}
	if _, exists := status[key]; !exists {
		status[key] = value
	}
}

func (page *simplePage) projectStatus() map[string]any {
	status, _ := page.document["project-status"].(map[string]any)
	return status
}

func (page *simplePage) isQuarantined() bool {
	return page.projectStatus()["status"] == "quarantined"
}

// renderedVersion reports only features that this serialization actually
// emits. JSON 1.1 versions and size have no HTML counterpart.
func (page *simplePage) renderedVersion(asJSON bool) int {
	version := 0
	if asJSON && (len(page.versions()) != 0 || page.hasVersionList) {
		version = 1
	}
	if len(page.projectStatus()) != 0 {
		version = 4
	}
	for _, entry := range page.entries {
		if asJSON && (entry["size"] != nil || entry["upload-time"] != nil) && version < 1 {
			version = 1
		}
		if provenance, ok := entry["provenance"].(string); ok && validProvenanceURL(provenance) && version < 3 {
			version = 3
		}
	}
	return version
}

func (page *simplePage) allFilesSized() bool {
	if page.kind != kindProject {
		return true
	}
	for _, entry := range page.entries {
		if !validFileSize(entry["size"]) {
			return false
		}
	}
	return true
}

func validFileSize(value any) bool {
	switch size := value.(type) {
	case json.Number:
		parsed, err := size.Int64()
		return err == nil && parsed >= 0
	case int64:
		return size >= 0
	case int:
		return size >= 0
	default:
		return false
	}
}

// Provenance is an external absolute URL. Keep it external: the authorized
// /files route covers distributions and their .metadata/.asc companions only.
func validProvenanceURL(raw string) bool {
	target, err := url.Parse(raw)
	if err != nil || !target.IsAbs() || target.Host == "" || target.User != nil {
		return false
	}
	if target.Scheme == "https" {
		return true
	}
	if target.Scheme != "http" {
		return false
	}
	host := strings.ToLower(target.Hostname())
	if host == "localhost" || strings.HasSuffix(host, ".localhost") {
		return true
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}

// metadataAttribute decodes data-core-metadata: "true" or "sha256=<hex>".
func metadataAttribute(value string) any {
	if algorithm, digest, ok := strings.Cut(value, "="); ok {
		return map[string]any{algorithm: digest}
	}
	return value == "true" || value == ""
}

func (page *simplePage) render() ([]byte, string, error) {
	if page.kind == kindProject && page.isQuarantined() {
		// Quarantine is an index-level policy: an upstream page that still
		// lists files must not make them visible in either representation.
		page.entries = make([]map[string]any, 0)
	}
	if page.asJSON {
		document := page.document
		if document == nil {
			document = make(map[string]any)
		}
		meta, ok := document["meta"].(map[string]any)
		if !ok || meta == nil {
			meta = make(map[string]any)
		}
		requiredVersion := page.renderedVersion(true)
		currentVersion, _ := meta["api-version"].(string)
		minor, err := strconv.Atoi(strings.TrimPrefix(currentVersion, "1."))
		if !page.allFilesSized() && (currentVersion == "" || strings.HasPrefix(currentVersion, "1.")) {
			// A JSON 1.1+ file requires size. HTML sources have no size field,
			// so keep newer fields as discoverable extensions under 1.0.
			meta["api-version"] = "1.0"
		} else if currentVersion == "" || (strings.HasPrefix(currentVersion, "1.") && err == nil && minor < requiredVersion) {
			meta["api-version"] = fmt.Sprintf("1.%d", requiredVersion)
		}
		document["meta"] = meta
		if page.entries == nil {
			page.entries = make([]map[string]any, 0)
		}
		if page.kind == kindRoot {
			document["projects"] = page.entries
		} else {
			document["name"] = page.name
			document["files"] = page.entries
			versions := page.versions()
			apiVersion, _ := meta["api-version"].(string)
			apiMinor, apiErr := strconv.Atoi(strings.TrimPrefix(apiVersion, "1."))
			if len(versions) > 0 || page.hasVersionList || (strings.HasPrefix(apiVersion, "1.") && apiErr == nil && apiMinor >= 1) {
				if versions == nil {
					versions = make([]string, 0)
				}
				document["versions"] = versions
			}
		}
		budget := newOutputBudget()
		if err := jsonSize(document, &budget); err != nil {
			return nil, "", err
		}
		content, err := json.Marshal(document)
		return content, jsonContentType, err
	}
	budget := newOutputBudget()
	if err := budget.charge(int64(len(`<!DOCTYPE html><html><head><meta name="pypi:repository-version" content="1.`) + len(strconv.Itoa(page.renderedVersion(false))) + len(`"></head><body>`) + 1 + len("</body></html>\n"))); err != nil {
		return nil, "", err
	}
	if page.kind == kindProject {
		status := page.projectStatus()
		for _, field := range []string{"status", "reason"} {
			if value, ok := status[field].(string); ok {
				name := "pypi:project-status"
				if field == "reason" {
					name += "-reason"
				}
				if err := budget.charge(int64(len(`<meta name="`) + len(name) + len(`" content="">`))); err != nil {
					return nil, "", err
				}
				if err := htmlStringSize(value, &budget); err != nil {
					return nil, "", err
				}
			}
		}
	}
	for _, entry := range page.entries {
		if err := htmlAnchorSize(entry, page.kind, &budget); err != nil {
			return nil, "", err
		}
	}
	var buffer bytes.Buffer
	buffer.WriteString("<!DOCTYPE html><html><head><meta name=\"pypi:repository-version\" content=\"1.")
	buffer.WriteString(strconv.Itoa(page.renderedVersion(false)))
	buffer.WriteString("\">")
	if page.kind == kindProject {
		status := page.projectStatus()
		if value, ok := status["status"].(string); ok {
			buffer.WriteString(`<meta name="pypi:project-status" content="` + html.EscapeString(value) + `">`)
		}
		if value, ok := status["reason"].(string); ok {
			buffer.WriteString(`<meta name="pypi:project-status-reason" content="` + html.EscapeString(value) + `">`)
		}
	}
	buffer.WriteString("</head><body>\n")
	for _, entry := range page.entries {
		buffer.WriteString(renderAnchor(entry, page.kind))
	}
	buffer.WriteString("</body></html>\n")
	return buffer.Bytes(), htmlContentType, nil
}

func htmlAnchorSize(entry map[string]any, kind pathKind, budget *outputBudget) error {
	if err := budget.charge(int64(len(`<a href=""></a>`) + 1)); err != nil {
		return err
	}
	if kind == kindRoot {
		name, _ := entry["name"].(string)
		link, _ := entry["url"].(string)
		if link == "" {
			link = normalizeProjectName(name) + "/"
		}
		if err := htmlStringSize(link, budget); err != nil {
			return err
		}
		return htmlStringSize(name, budget)
	}
	link, _ := entry["url"].(string)
	if algorithm, digest := preferredAdvertisedHash(entry["hashes"]); algorithm != "" {
		link, _, _ = strings.Cut(link, "#")
		// Account for the escaped hash suffix without concatenating it.
		if err := budget.charge(2); err != nil {
			return err
		}
		if err := htmlStringSize(algorithm, budget); err != nil {
			return err
		}
		if err := htmlStringSize(digest, budget); err != nil {
			return err
		}
	}
	if err := htmlStringSize(link, budget); err != nil {
		return err
	}
	if requires, ok := entry["requires-python"].(string); ok && requires != "" {
		if err := budget.charge(int64(len(` data-requires-python=""`))); err != nil {
			return err
		}
		if err := htmlStringSize(requires, budget); err != nil {
			return err
		}
	}
	switch yanked := entry["yanked"].(type) {
	case bool:
		if yanked {
			if err := budget.charge(int64(len(` data-yanked=""`))); err != nil {
				return err
			}
		}
	case string:
		if err := budget.charge(int64(len(` data-yanked=""`))); err != nil {
			return err
		}
		if err := htmlStringSize(yanked, budget); err != nil {
			return err
		}
	}
	if key, metadata, advertised := metadataAdvertisement(entry); advertised {
		if err := budget.charge(int64(len(` data-`) + len(key) + len(`=""`))); err != nil {
			return err
		}
		if err := htmlStringSize(metadataValue(metadata), budget); err != nil {
			return err
		}
	}
	if signature, present := entry["gpg-sig"].(bool); present {
		length := len(` data-gpg-sig="true"`)
		if !signature {
			length++
		}
		if err := budget.charge(int64(length)); err != nil {
			return err
		}
	}
	if provenance, ok := entry["provenance"].(string); ok && validProvenanceURL(provenance) {
		if err := budget.charge(int64(len(` data-provenance=""`))); err != nil {
			return err
		}
		if err := htmlStringSize(provenance, budget); err != nil {
			return err
		}
	}
	filename, _ := entry["filename"].(string)
	return htmlStringSize(filename, budget)
}

// versions lists the distinct versions of the page's files, for the PEP 700
// `versions` key.
func (page *simplePage) versions() []string {
	seen := map[string]struct{}{}
	var versions []string
	for _, version := range page.listedVersions {
		if _, duplicate := seen[version]; !duplicate {
			seen[version] = struct{}{}
			versions = append(versions, version)
		}
	}
	for _, entry := range page.entries {
		filename, _ := entry["filename"].(string)
		if _, version, ok := parseDistributionFilename(filename); ok {
			if _, duplicate := seen[version]; !duplicate {
				seen[version] = struct{}{}
				versions = append(versions, version)
			}
		}
	}
	return versions
}

func renderAnchor(entry map[string]any, kind pathKind) string {
	if kind == kindRoot {
		name, _ := entry["name"].(string)
		link, _ := entry["url"].(string)
		if link == "" {
			link = normalizeProjectName(name) + "/"
		}
		return `<a href="` + html.EscapeString(link) + `">` + html.EscapeString(name) + "</a>\n"
	}
	link, _ := entry["url"].(string)
	if algorithm, digest := preferredAdvertisedHash(entry["hashes"]); algorithm != "" {
		link, _, _ = strings.Cut(link, "#")
		link += "#" + algorithm + "=" + digest
	}
	attributes := `href="` + html.EscapeString(link) + `"`
	if requires, ok := entry["requires-python"].(string); ok && requires != "" {
		attributes += ` data-requires-python="` + html.EscapeString(requires) + `"`
	}
	switch yanked := entry["yanked"].(type) {
	case bool:
		if yanked {
			attributes += ` data-yanked=""`
		}
	case string:
		attributes += ` data-yanked="` + html.EscapeString(yanked) + `"`
	}
	if key, metadata, advertised := metadataAdvertisement(entry); advertised {
		attributes += ` data-` + key + `="` + html.EscapeString(metadataValue(metadata)) + `"`
	}
	if signature, present := entry["gpg-sig"].(bool); present {
		attributes += ` data-gpg-sig="` + strconv.FormatBool(signature) + `"`
	}
	if provenance, ok := entry["provenance"].(string); ok && validProvenanceURL(provenance) {
		attributes += ` data-provenance="` + html.EscapeString(provenance) + `"`
	}
	filename, _ := entry["filename"].(string)
	return "<a " + attributes + ">" + html.EscapeString(filename) + "</a>\n"
}

func metadataValue(metadata any) string {
	switch value := metadata.(type) {
	case map[string]any:
		if algorithm, digest := preferredAdvertisedHash(value); algorithm != "" {
			return algorithm + "=" + digest
		}
	case bool:
		if value {
			return "true"
		}
	}
	return ""
}

// preferredAdvertisedHash picks the same HTML-visible digest from distribution
// and metadata hash objects. JSON may retain more than one algorithm.
func preferredAdvertisedHash(value any) (string, string) {
	hashes, ok := value.(map[string]any)
	if !ok {
		return "", ""
	}
	for _, algorithm := range []string{"sha512", "sha384", "sha256", "sha224", "sha1", "md5"} {
		if digest, ok := hashes[algorithm].(string); ok && digest != "" {
			return algorithm, digest
		}
	}
	algorithms := make([]string, 0, len(hashes))
	for algorithm := range hashes {
		algorithms = append(algorithms, algorithm)
	}
	sort.Strings(algorithms)
	for _, algorithm := range algorithms {
		if digest, ok := hashes[algorithm].(string); ok && digest != "" {
			return algorithm, digest
		}
	}
	return "", ""
}
