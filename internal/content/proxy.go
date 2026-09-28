package content

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"mime"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/suxen-project/suxen/internal/blob"
	"github.com/suxen-project/suxen/internal/domain"
	"github.com/suxen-project/suxen/internal/ocimodel"
	"github.com/suxen-project/suxen/internal/store"
	spiformat "github.com/suxen-project/suxen/spi/format"
)

const negativeCacheTTL = time.Minute

type BearerChallenge struct {
	Realm   string
	Service string
	Scope   string
}

type bearerChallenge = BearerChallenge

type UpstreamCredentials struct {
	Username string
	Password string
}

type upstreamCredentials = UpstreamCredentials

// UpstreamError wraps a failed proxy fetch so HTTP handlers can map it to 502.
type UpstreamError struct {
	cause error
}

type upstreamError = UpstreamError

type redactedTransportError struct {
	cause   error
	message string
}

func (err *redactedTransportError) Error() string { return err.message }
func (err *redactedTransportError) Unwrap() error { return err.cause }

func redactTransportError(err error, requestURL string) error {
	if err == nil {
		return nil
	}
	message := redactedTransportErrorMessage(err)
	redacted := domain.RedactURLSecrets(requestURL)
	if requestURL != "" && redacted != "" {
		message = strings.ReplaceAll(message, requestURL, redacted)
	}
	if parsed, parseErr := url.Parse(requestURL); parseErr == nil && parsed.RawQuery != "" {
		message = strings.ReplaceAll(message, parsed.RawQuery, "[redacted]")
		for _, values := range parsed.Query() {
			for _, value := range values {
				if value != "" {
					message = strings.ReplaceAll(message, value, "[redacted]")
					message = strings.ReplaceAll(message, url.QueryEscape(value), "[redacted]")
				}
			}
		}
	}
	return &redactedTransportError{cause: err, message: message}
}

func redactedTransportErrorMessage(err error) string {
	if err == nil {
		return "transport error"
	}
	if urlError, ok := err.(*url.Error); ok {
		redactedURL := domain.RedactURLSecrets(urlError.URL)
		cause := errors.New(redactedTransportErrorMessage(urlError.Err))
		return (&url.Error{Op: urlError.Op, URL: redactedURL, Err: cause}).Error()
	}
	return err.Error()
}

func (err *UpstreamError) Error() string {
	return err.cause.Error()
}

func (err *UpstreamError) Unwrap() error {
	return err.cause
}

// resolvedUpstream carries a unified ProxyRequestResolver outcome through the
// fetch path so the granular upstream resolvers are not consulted a second
// time. used is true when the format's ProxyRequestResolver produced it; url
// is "" when the resolver wants the host's default upstream construction.
type resolvedUpstream struct {
	url             string
	used            bool
	expectedDigests []string
}

func (rt *Runtime) ResolveProxyAsset(
	w http.ResponseWriter,
	r *http.Request,
	repository domain.Repository,
	assetPath string,
) (domain.Asset, bool, error) {
	if err := validateFormatProxyPath(repository, assetPath); err != nil {
		return domain.Asset{}, false, err
	}

	// Resolve once before either cache lookup so policy, identity, and target
	// cannot disagree between a hit, miss, and revalidation.
	var upstream resolvedUpstream
	cacheOnly := false
	cachePath := assetPath
	if resolver := formatProxyRequestResolver(repository.Format); resolver != nil {
		resolved, err := resolver.ResolveProxyRequest(
			r.Context(),
			repository.FormatView(),
			assetPath,
			r.URL.RawQuery,
			storedAssetsView{runtime: rt, repository: repository},
		)
		if err != nil {
			return domain.Asset{}, false, err
		}
		if resolved.CachePath == "" {
			return domain.Asset{}, false, fmt.Errorf(
				"format %q resolved proxy request with an empty cache path", repository.Format)
		}
		if resolved.UpstreamURL != "" {
			validated, _, err := validatedResolvedUpstream(repository, resolved.UpstreamURL, upstreamCredentials{})
			if err != nil {
				return domain.Asset{}, false, fmt.Errorf("format %q resolved invalid upstream target: %w", repository.Format, err)
			}
			resolved.UpstreamURL = validated
		}
		cachePath = resolved.CachePath
		cacheOnly = resolved.CacheOnly
		upstream = resolvedUpstream{
			url: resolved.UpstreamURL, used: true, expectedDigests: resolved.ExpectedDigests,
		}
	}

	cached, err := rt.metaFor(repository).Asset(r.Context(), cachePath)
	legacyNpmPackument := false
	if err == nil {
		// One cache identity may be reached through multiple public URLs.
		// The current request path governs policy and rewriting on every hit.
		cached.FormatPath = assetPath
		legacyNpmPackument = npmPackumentPath(repository, assetPath) && abbreviatedNpmPackumentMediaType(cached.ContentType)
		if !legacyNpmPackument && !rt.proxyAssetRequiresRevalidation(repository, cached, assetPath, time.Now()) {
			rt.observeProxyCache(repository.Name, repository.Format, "hit")
			return cached, true, nil
		}
	} else if !errors.Is(err, domain.ErrNotFound) {
		return domain.Asset{}, false, fmt.Errorf("read proxy cache: %w", err)
	}
	if cacheOnly {
		rt.observeProxyCache(repository.Name, repository.Format, "miss")
		return domain.Asset{}, false, nil
	}

	negativeHit, err := rt.metaFor(repository).NegativeCacheHit(
		r.Context(),
		cachePath,
		time.Now(),
	)
	if err != nil {
		return domain.Asset{}, false, fmt.Errorf("read negative proxy cache: %w", err)
	}
	if negativeHit {
		rt.observeProxyCache(repository.Name, repository.Format, "hit")
		return domain.Asset{}, false, nil
	}
	var stale *domain.Asset
	if cached.ID != 0 && !legacyNpmPackument {
		stale = &cached
	}
	rt.observeProxyCache(repository.Name, repository.Format, "miss")
	// The store issues a per-path generation before upstream I/O. Results may
	// finish in the opposite order, including across replicas.
	token, err := rt.metaFor(repository).BeginProxyFetch(r.Context(), cachePath, time.Now().Add(24*time.Hour))
	if err != nil {
		return domain.Asset{}, false, fmt.Errorf("begin proxy fetch: %w", err)
	}
	return rt.fetchProxyAsset(w, r, repository, assetPath, cachePath, stale, upstream, token)
}

func (rt *Runtime) fetchProxyAsset(
	w http.ResponseWriter,
	r *http.Request,
	repository domain.Repository,
	assetPath string,
	cachePath string,
	cached *domain.Asset,
	upstream resolvedUpstream,
	token domain.ProxyFetchToken,
) (domain.Asset, bool, error) {
	response, err := rt.doUpstreamRequest(
		r,
		repository,
		http.MethodGet,
		assetPath,
		cached,
		upstream,
	)
	if err != nil {
		return domain.Asset{}, false, &upstreamError{cause: err}
	}
	defer response.Body.Close()

	if response.StatusCode == http.StatusNotModified {
		if cached == nil {
			message := "upstream returned 304 without a cached asset"
			return domain.Asset{}, false, &upstreamError{cause: errors.New(message)}
		}
		validatedAt := time.Now().UTC()
		published, err := rt.metaFor(repository).PublishProxyNotModified(r.Context(), cachePath, cached.ID, token, validatedAt)
		if err != nil {
			return domain.Asset{}, false, fmt.Errorf("refresh proxy cache: %w", err)
		}
		if published {
			cached.ValidatedAt = validatedAt
		}
		return *cached, true, nil
	}
	if response.StatusCode == http.StatusNotFound {
		expiresAt := time.Now().Add(negativeCacheTTL)
		_, err := rt.metaFor(repository).PublishProxyNotFound(
			r.Context(),
			cachePath,
			token,
			expiresAt,
		)
		if err != nil {
			rt.requestLogger(r).Warn("record negative cache entry", "error", err)
		}
		return domain.Asset{}, false, nil
	}
	// A cache fill needs a complete representation. In particular, a 206 body
	// is only one range even when the upstream sent it to our ordinary GET;
	// storing it under the full asset path would poison subsequent downloads.
	// 203 can contain a complete representation transformed by an intermediary.
	if response.StatusCode != http.StatusOK && response.StatusCode != http.StatusNonAuthoritativeInfo {
		message := fmt.Sprintf("upstream returned %s", response.Status)
		return domain.Asset{}, false, &upstreamError{cause: errors.New(message)}
	}
	if npmPackumentPath(repository, assetPath) && abbreviatedNpmPackumentMediaType(response.Header.Get("Content-Type")) {
		return domain.Asset{}, false, &upstreamError{cause: fmt.Errorf(
			"npm upstream returned abbreviated packument despite full metadata request: %q",
			response.Header.Get("Content-Type"),
		)}
	}
	// A cache fill stages the complete upstream body. Reserve a slot before
	// reading it, and hold the slot through publication just like a hosted upload.
	release, err := rt.AcquireUpload(r.Context())
	if err != nil {
		return domain.Asset{}, false, err
	}
	defer release()

	var staged StagedUpload
	if route, ok := ocimodel.ParseAssetPath(assetPath); repository.Format == "oci" &&
		ok && route.Kind == ocimodel.ManifestRoute {
		staged, err = rt.StageOCIManifest(w, response.Body)
	} else {
		staged, err = rt.StageUpload(w, response.Body)
	}
	if err != nil {
		return domain.Asset{}, false, &upstreamError{cause: err}
	}
	defer staged.remove()
	if err := verifyProxyExpectedDigests(staged, upstream.expectedDigests); err != nil {
		return domain.Asset{}, false, &upstreamError{cause: err}
	}

	if repository.Format == "oci" {
		if err := verifyProxyDigestReference(assetPath, staged.Digest); err != nil {
			return domain.Asset{}, false, &upstreamError{cause: err}
		}
	}
	asset, err := proxyAsset(
		repository,
		assetPath,
		response.Header.Get("Content-Type"),
		staged,
	)
	if err != nil {
		return domain.Asset{}, false, &upstreamError{cause: err}
	}
	asset.RepositoryID = repository.ID
	provenance, err := rt.VerifyIncomingAsset(r.Context(), asset, http.Header{})
	if err != nil {
		return domain.Asset{}, false, err
	}
	if provenance != nil {
		value, err := resultMap(*provenance)
		if err != nil {
			return domain.Asset{}, false, err
		}
		asset.Attributes = map[string]any{"provenance": value}
	}
	asset.Path = cachePath
	asset.FormatPath = assetPath
	asset.ProxyFetch = token
	assets := []domain.Asset{asset}
	if alias, ok := proxyManifestAlias(asset, staged.Digest); ok {
		assets = append(assets, alias)
	}
	// Pin the write to the identity resolved when this fetch began. If the
	// repository was deleted and recreated under the same name while the fetch
	// was in flight, the stale id is gone and the publication fails instead of
	// polluting the replacement.
	for index := range assets {
		assets[index].RepositoryID = repository.ID
	}
	_, stored, err := rt.CommitStagedAssets(
		r.Context(), repository.Name, []StagedUpload{staged}, assets,
	)
	if errors.Is(err, store.ErrProxyResultSuperseded) {
		// Another request already made a newer result authoritative. Read that
		// result instead of serving this stale staged generation.
		return rt.authoritativeProxyResult(r, repository, assetPath, cachePath)
	}
	if err != nil {
		return domain.Asset{}, false, err
	}
	asset = stored[0]

	return asset, true, nil
}

func (rt *Runtime) authoritativeProxyResult(r *http.Request, repository domain.Repository, assetPath, cachePath string) (domain.Asset, bool, error) {
	view := rt.metaFor(repository)
	if hit, err := view.NegativeCacheHit(r.Context(), cachePath, time.Now()); err != nil {
		return domain.Asset{}, false, err
	} else if hit {
		return domain.Asset{}, false, nil
	}
	current, err := view.Asset(r.Context(), cachePath)
	if errors.Is(err, domain.ErrNotFound) {
		return domain.Asset{}, false, fmt.Errorf("proxy fetch completed after its cache publication lease without an authoritative result: %w", store.ErrProxyResultSuperseded)
	}
	if err != nil {
		return domain.Asset{}, false, err
	}
	current.FormatPath = assetPath
	return current, true, nil
}

// proxyAssetRequiresRevalidation reports whether a cached proxy asset sits on
// a mutable upstream path and has outlived the proxy TTL. Mutable paths are
// OCI manifests addressed by tag, plus whatever a registered format plugin
// declares mutable (package indexes, snapshot metadata).
func (rt *Runtime) proxyAssetRequiresRevalidation(
	repository domain.Repository,
	asset domain.Asset,
	assetPath string,
	now time.Time,
) bool {
	if !mutableProxyPath(repository, asset, assetPath) {
		return false
	}
	return rt.Config.ProxyManifestTTL == 0 ||
		now.Sub(asset.ValidatedAt) >= rt.Config.ProxyManifestTTL
}

func mutableProxyPath(
	repository domain.Repository,
	asset domain.Asset,
	assetPath string,
) bool {
	if asset.Kind == "oci-manifest" && proxyDigestReference(assetPath) == "" {
		return true
	}
	registered, found := spiformat.Lookup(repository.Format)
	if !found {
		return false
	}
	policy, ok := registered.(spiformat.ProxyPolicy)
	return ok && policy.MutableUpstreamPath(repository.FormatView(), assetPath)
}

func verifyProxyDigestReference(assetPath string, actualDigest string) error {
	expectedDigest := proxyDigestReference(assetPath)
	if expectedDigest == "" || expectedDigest == actualDigest {
		return nil
	}
	return fmt.Errorf(
		"%w: expected %s, got %s",
		domain.ErrDigestMismatch,
		expectedDigest,
		actualDigest,
	)
}

func proxyDigestReference(assetPath string) string {
	route, ok := ocimodel.ParseAssetPath(assetPath)
	if !ok || (route.Kind != ocimodel.BlobRoute && route.Kind != ocimodel.ManifestRoute) {
		return ""
	}
	digest, err := blob.NormalizeDigest(route.Value)
	if err != nil {
		return ""
	}
	return digest
}

func proxyAsset(
	repository domain.Repository,
	assetPath string,
	mediaType string,
	upload StagedUpload,
) (domain.Asset, error) {
	asset := domain.Asset{
		Repository:  repository.Name,
		Path:        assetPath,
		Digest:      upload.Digest,
		Size:        upload.Size,
		ContentType: contentType(mediaType, assetPath),
		Kind:        "raw",
	}
	if npmPackumentPath(repository, assetPath) {
		// The upstream response may have a nonstandard media type; the
		// representation was requested as full JSON and is shared by all clients.
		asset.ContentType = "application/json"
	}
	if repository.Format != "oci" {
		return asset, nil
	}

	route, ok := ocimodel.ParseAssetPath(assetPath)
	if !ok {
		return domain.Asset{}, fmt.Errorf("invalid OCI asset path %q", assetPath)
	}

	switch route.Kind {
	case ocimodel.BlobRoute:
		asset.Kind = "oci-blob"
	case ocimodel.ManifestRoute:
		manifest, err := ocimodel.ReadManifestEnvelope(upload.Path)
		if err != nil {
			return domain.Asset{}, err
		}
		asset.Kind = "oci-manifest"
		asset.Reference = route.Value
		if mediaType == "" && manifest.MediaType != "" {
			asset.ContentType = manifest.MediaType
		}
		if manifest.Subject != nil {
			asset.SubjectDigest = manifest.Subject.Digest
		}
		asset.Dependencies = ocimodel.ManifestDependencies(manifest)
	}
	return asset, nil
}

func proxyManifestAlias(
	asset domain.Asset,
	digest string,
) (domain.Asset, bool) {
	if asset.Kind != "oci-manifest" || asset.Reference == digest {
		return domain.Asset{}, false
	}
	route, ok := ocimodel.ParseAssetPath(asset.Path)
	if !ok || route.Kind != ocimodel.ManifestRoute {
		return domain.Asset{}, false
	}
	asset.Path = ocimodel.ManifestPath(route.ImageName, digest)
	asset.Reference = digest
	return asset, true
}

// DoUpstreamRequest fetches a protocol-specific request from the configured
// upstream. Generic proxy reads resolve their format target separately.
func (rt *Runtime) DoUpstreamRequest(
	r *http.Request,
	repository domain.Repository,
	method string,
	assetPath string,
	cached *domain.Asset,
) (*http.Response, error) {
	requestURL, _, err := proxyURLWithQuery(repository.Upstream, assetPath)
	if err != nil {
		return nil, err
	}
	return rt.doUpstreamRequest(r, repository, method, assetPath, cached, resolvedUpstream{url: requestURL, used: true})
}

func (rt *Runtime) doUpstreamRequest(
	r *http.Request,
	repository domain.Repository,
	method string,
	assetPath string,
	cached *domain.Asset,
	upstream resolvedUpstream,
) (*http.Response, error) {
	requestURL, credentials, err := upstreamRequestURL(repository, assetPath, upstream)
	if err != nil {
		return nil, err
	}

	request, err := newUpstreamRequest(r, method, requestURL, credentials, rt.UserAgent)
	if err != nil {
		return nil, err
	}
	setProxyUpstreamAccept(request, repository, assetPath)
	setUpstreamValidators(request, cached)
	response, err := rt.client().Do(request)
	if err != nil {
		return nil, redactTransportError(err, request.URL.String())
	}
	if response.StatusCode != http.StatusUnauthorized {
		return response, nil
	}

	challenge, ok := parseBearerChallenges(response.Header.Values("WWW-Authenticate"))
	if !ok {
		return response, nil
	}
	_ = response.Body.Close()

	token, err := rt.FetchBearerToken(r, requestURL, challenge, credentials)
	if err != nil {
		return nil, err
	}
	retry, err := newUpstreamRequest(r, method, requestURL, upstreamCredentials{}, rt.UserAgent)
	if err != nil {
		return nil, err
	}
	setProxyUpstreamAccept(retry, repository, assetPath)
	setUpstreamValidators(retry, cached)
	retry.Header.Set("Authorization", "Bearer "+token)
	response, err = rt.client().Do(retry)
	if err != nil {
		return nil, redactTransportError(err, retry.URL.String())
	}
	return response, nil
}

func newUpstreamRequest(
	r *http.Request,
	method string,
	requestURL string,
	credentials upstreamCredentials,
	userAgent string,
) (*http.Request, error) {
	request, err := http.NewRequestWithContext(r.Context(), method, requestURL, nil)
	if err != nil {
		return nil, err
	}
	request.Header.Set("User-Agent", userAgent)
	if accept := strings.Join(r.Header.Values("Accept"), ","); accept != "" {
		request.Header.Set("Accept", accept)
	}
	if credentials.Username != "" {
		request.SetBasicAuth(credentials.Username, credentials.Password)
	}
	return request, nil
}

func npmPackumentPath(repository domain.Repository, assetPath string) bool {
	return repository.Format == "npm" && mutableProxyPath(repository, domain.Asset{}, assetPath)
}

func abbreviatedNpmPackumentMediaType(value string) bool {
	mediaType, _, err := mime.ParseMediaType(value)
	if err == nil {
		return strings.EqualFold(mediaType, "application/vnd.npm.install-v1+json")
	}
	return strings.HasPrefix(strings.ToLower(strings.TrimSpace(value)), "application/vnd.npm.install-v1+json")
}

func setProxyUpstreamAccept(request *http.Request, repository domain.Repository, assetPath string) {
	if npmPackumentPath(repository, assetPath) {
		// Full npm metadata contains fields omitted from the install-v1 variant.
		// Both clients share one cache path, so fetch a single representation.
		request.Header.Set("Accept", "application/json")
	}
}

func setUpstreamValidators(request *http.Request, cached *domain.Asset) {
	if cached == nil {
		return
	}
	// The digest can validate an upstream ETag that uses the same value. The
	// local validation time is not the upstream's Last-Modified timestamp and
	// must not be sent as If-Modified-Since.
	request.Header.Set("If-None-Match", quoteETag(cached.Digest))
}

func (rt *Runtime) FetchBearerToken(
	r *http.Request,
	upstreamURL string,
	challenge bearerChallenge,
	credentials upstreamCredentials,
) (string, error) {
	tokenURL, err := url.Parse(challenge.Realm)
	if err != nil {
		return "", fmt.Errorf("parse registry token realm: %w", err)
	}
	if tokenURL.Scheme != "http" && tokenURL.Scheme != "https" {
		return "", fmt.Errorf("registry token realm must use http or https")
	}
	if tokenURL.Hostname() == "" {
		return "", fmt.Errorf("registry token realm host is missing")
	}
	if tokenURL.User != nil {
		return "", fmt.Errorf("registry token realm must not contain user information")
	}
	upstream, err := url.Parse(upstreamURL)
	if err != nil {
		return "", fmt.Errorf("parse registry upstream URL: %w", err)
	}
	if credentialSecurityDowngrade(upstream, tokenURL) {
		return "", fmt.Errorf("registry token realm must not downgrade HTTPS to HTTP")
	}
	tokenCredentials := upstreamCredentials{}
	sameRegistryOrigin := sameOrigin(upstream, tokenURL)
	allowedRealmHost := hostListed(tokenURL.Hostname(), rt.Config.ProxyRealmHosts)
	if sameRegistryOrigin || allowedRealmHost {
		tokenCredentials = credentials
	}
	query := tokenURL.Query()
	if challenge.Service != "" {
		query.Set("service", challenge.Service)
	}
	if challenge.Scope != "" {
		query.Set("scope", challenge.Scope)
	}
	tokenURL.RawQuery = query.Encode()

	request, err := newUpstreamRequest(
		r,
		http.MethodGet,
		tokenURL.String(),
		tokenCredentials,
		rt.UserAgent,
	)
	if err != nil {
		return "", err
	}
	response, err := rt.client().Do(request)
	if err != nil {
		return "", fmt.Errorf(
			"request registry token: %w",
			redactTransportError(err, request.URL.String()),
		)
	}
	defer response.Body.Close()
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		return "", fmt.Errorf("registry token service returned %s", response.Status)
	}

	var payload struct {
		Token       string `json:"token"`
		AccessToken string `json:"access_token"`
	}
	decoder := json.NewDecoder(io.LimitReader(response.Body, 1<<20))
	if err := decoder.Decode(&payload); err != nil {
		return "", fmt.Errorf("decode registry token: %w", err)
	}
	if payload.Token != "" {
		return payload.Token, nil
	}
	if payload.AccessToken != "" {
		return payload.AccessToken, nil
	}
	return "", fmt.Errorf("registry token response did not contain a token")
}

func sameOrigin(first, second *url.URL) bool {
	return strings.EqualFold(first.Scheme, second.Scheme) &&
		strings.EqualFold(
			strings.TrimSuffix(first.Hostname(), "."),
			strings.TrimSuffix(second.Hostname(), "."),
		) &&
		effectivePort(first) == effectivePort(second)
}

func credentialSecurityDowngrade(upstream, tokenRealm *url.URL) bool {
	return strings.EqualFold(upstream.Scheme, "https") &&
		!strings.EqualFold(tokenRealm.Scheme, "https")
}

func effectivePort(target *url.URL) string {
	if port := target.Port(); port != "" {
		return port
	}
	if strings.EqualFold(target.Scheme, "https") {
		return "443"
	}
	if strings.EqualFold(target.Scheme, "http") {
		return "80"
	}
	return ""
}

func hostListed(host string, allowed []string) bool {
	host = strings.ToLower(strings.TrimSuffix(host, "."))
	for _, candidate := range allowed {
		if host == strings.ToLower(strings.TrimSuffix(candidate, ".")) {
			return true
		}
	}
	return false
}

func proxyURL(
	upstream string,
	assetPath string,
) (string, upstreamCredentials, error) {
	parsed, err := url.Parse(upstream)
	if err != nil {
		return "", upstreamCredentials{}, fmt.Errorf("parse upstream URL: %w", err)
	}
	if parsed.Scheme != "http" && parsed.Scheme != "https" {
		return "", upstreamCredentials{}, fmt.Errorf("upstream URL must use http or https")
	}

	credentials := upstreamCredentials{}
	if parsed.User != nil {
		credentials.Username = parsed.User.Username()
		credentials.Password, _ = parsed.User.Password()
		parsed.User = nil
	}
	parsed.Path = strings.TrimRight(parsed.Path, "/") + "/" + strings.TrimLeft(assetPath, "/")
	return parsed.String(), credentials, nil
}

// proxyURLWithQuery accepts a protocol request target whose query string is
// supplied deliberately by the protocol handler, rather than by a decoded
// asset path.
func proxyURLWithQuery(upstream, requestTarget string) (string, upstreamCredentials, error) {
	assetPath, query, hasQuery := strings.Cut(requestTarget, "?")
	requestURL, credentials, err := proxyURL(upstream, assetPath)
	if err != nil || !hasQuery {
		return requestURL, credentials, err
	}
	parsed, err := url.Parse(requestURL)
	if err != nil {
		return "", upstreamCredentials{}, err
	}
	parsed.RawQuery = query
	return parsed.String(), credentials, nil
}
