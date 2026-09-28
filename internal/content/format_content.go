package content

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/suxen-project/suxen/internal/domain"
	"github.com/suxen-project/suxen/internal/httpx"
	spiformat "github.com/suxen-project/suxen/spi/format"
)

// Format hooks buffer source documents. PyPI's root index and high-churn npm
// packuments are much larger than ordinary package indexes. Keep these bounds
// finite: npm's JSON rewrite and merge hooks hold several copies in memory.
const (
	formatContentSourceLimit = 8 << 20
	pypiRootSourceLimit      = 128 << 20
	pypiGroupSourcesLimit    = 256 << 20
	npmPackumentSourceLimit  = 128 << 20
	npmGroupSourcesLimit     = 256 << 20
	npmRenderedContentLimit  = 256 << 20
)

var errPyPIGroupSourcesLimit = errors.New("PyPI group source limit exceeded")
var errNpmGroupSourcesLimit = errors.New("npm group source limit exceeded")

func formatGroupSourceBudget(formatName, sourcePath string) (int64, error) {
	if isPyPIIndex(formatName, sourcePath) {
		return pypiGroupSourcesLimit, errPyPIGroupSourcesLimit
	}
	if isNpmPackument(formatName, sourcePath) {
		return npmGroupSourcesLimit, errNpmGroupSourcesLimit
	}
	return 0, nil
}

func npmRenderedContentTooLarge(formatName, assetPath string, size int) bool {
	return isNpmPackument(formatName, assetPath) && int64(size) > npmRenderedContentLimit
}

func formatContentLimit(formatName, assetPath string) int64 {
	if isPyPIRoot(formatName, assetPath) {
		return pypiRootSourceLimit
	}
	if isNpmPackument(formatName, assetPath) {
		return npmPackumentSourceLimit
	}
	return formatContentSourceLimit
}

func isPyPIRoot(formatName, assetPath string) bool {
	return formatName == "pypi" && strings.Trim(assetPath, "/") == "simple"
}

// Both root and project indexes can grow substantially when relative URLs
// are expanded. Distribution paths do not participate in index budgets.
func isPyPIIndex(formatName, assetPath string) bool {
	path := strings.Trim(assetPath, "/")
	if formatName != "pypi" {
		return false
	}
	project, found := strings.CutPrefix(path, "simple/")
	return path == "simple" || (found && project != "" && !strings.Contains(project, "/"))
}

// npm packuments occupy the package name path. A tarball has the additional
// /-/filename suffix and retains the ordinary buffered-content bound.
func isNpmPackument(formatName, assetPath string) bool {
	return formatName == "npm" && npmPackumentPath(domain.Repository{Format: formatName}, assetPath)
}

func (rt *Runtime) acquireLargeIndex(ctx context.Context, formatName, assetPath string) (func(), error) {
	if !isPyPIIndex(formatName, assetPath) && !isNpmPackument(formatName, assetPath) {
		return func() {}, nil
	}
	select {
	case rt.largeIndexSlots <- struct{}{}:
		return func() { <-rt.largeIndexSlots }, nil
	case <-ctx.Done():
		return nil, ctx.Err()
	}
}

// ServeMergedGroupContent renders a group response through the format's
// GroupMerger hook. It reports whether it handled the request; unmergeable
// paths and paths no member resolves fall back to first-match resolution.
// Merged content is synthesized per request from member content and served
// without an asset row, so per-asset download gates do not apply to it —
// gates target artifacts, merged responses only exist for index paths.
func (rt *Runtime) ServeMergedGroupContent(
	w http.ResponseWriter,
	r *http.Request,
	group domain.Repository,
	assetPath string,
) bool {
	merger := formatGroupMerger(group.Format)
	if merger == nil {
		return false
	}
	sourcePath, mergeable := merger.GroupMergeSource(group.FormatView(), assetPath)
	if !mergeable {
		return false
	}
	sources, sourceAssets, firstError := rt.collectGroupMergeSources(w, r, group, sourcePath)
	// Formats that select artifact members from merged indexes cannot publish
	// a partial index: an unavailable member may still hold cached bytes for
	// the same public artifact path. Other formats retain tolerant merging.
	if firstError != nil && (errors.Is(firstError, errPyPIGroupSourcesLimit) || errors.Is(firstError, errNpmGroupSourcesLimit) || formatGroupArtifactSelector(group.Format) != nil || formatGroupArtifactLocator(group.Format) != nil) {
		writeAssetResolutionError(w, firstError)
		return true
	}
	if len(sources) == 0 {
		if firstError != nil {
			writeAssetResolutionError(w, firstError)
			return true
		}
		return false
	}
	// Source acquisition may make outbound HTTP requests, including a proxy to
	// another repository in this process. Reserve the render slot only after
	// acquisition so nested requests cannot wait on their caller's slot.
	release, err := rt.acquireLargeIndex(r.Context(), group.Format, sourcePath)
	if err != nil {
		writeAssetResolutionError(w, err)
		return true
	}
	defer release()
	content, contentType, err := merger.MergeGroupContent(
		group.FormatView(),
		assetPath,
		sources,
	)
	if err != nil {
		httpx.WriteServerProblem(
			w,
			http.StatusInternalServerError,
			"group_merge_error",
			"merge group content failed",
			err,
		)
		return true
	}
	if npmRenderedContentTooLarge(group.Format, assetPath, len(content)) {
		writeAssetResolutionError(w, fmt.Errorf("npm merged packument exceeds the %d byte rendered content limit", npmRenderedContentLimit))
		return true
	}
	var negotiated bool
	content, contentType, negotiated, err = rewriteMergedIndex(r, group, assetPath, content, contentType)
	if err != nil {
		httpx.WriteServerProblem(
			w,
			http.StatusInternalServerError,
			"index_rewrite_error",
			"rewrite group index failed",
			err,
		)
		return true
	}
	if npmRenderedContentTooLarge(group.Format, assetPath, len(content)) {
		writeAssetResolutionError(w, fmt.Errorf("npm rewritten group packument exceeds the %d byte rendered content limit", npmRenderedContentLimit))
		return true
	}
	if negotiated {
		w.Header().Add("Vary", "Accept")
	}
	if rt.serveSynthesizedContent(w, r, content, contentType) && r.Method == http.MethodGet {
		for _, asset := range sourceAssets {
			rt.recordAssetDownload(r, asset)
		}
	}
	return true
}

// collectGroupMergeSources gathers the source-path content of every member
// that resolves it, in member order. Groups contain only leaf repositories, so
// this iterates them once with no recursion or cycle tracking. Hosted members
// without a stored asset contribute synthesized content when the format supports
// it, so groups over hosted repositories compose with hosted synthesis.
// Failures are recorded for callers to either fail closed for formats whose
// artifacts depend on the merged index, or preserve tolerant merging.
func (rt *Runtime) collectGroupMergeSources(
	w http.ResponseWriter,
	r *http.Request,
	group domain.Repository,
	sourcePath string,
) ([][]byte, []domain.Asset, error) {
	sources := make([][]byte, 0, len(group.Members))
	assets := make([]domain.Asset, 0, len(group.Members))
	var firstError error
	var totalSourceBytes int64
	sourceBudget, budgetError := formatGroupSourceBudget(group.Format, sourcePath)
	recordError := func(err error) {
		if err != nil && firstError == nil {
			firstError = err
		}
	}
	for _, memberName := range group.Members {
		member, err := rt.repositoryResolver().Repository(r.Context(), memberName)
		if errors.Is(err, domain.ErrNotFound) {
			continue
		}
		if err != nil {
			recordError(fmt.Errorf("load group member %q: %w", memberName, err))
			continue
		}
		if member.Format != group.Format {
			continue
		}
		if member.Type == "group" {
			recordError(fmt.Errorf("group %q contains nested group %q", group.Name, memberName))
			continue
		}

		content, asset, found, err := rt.memberSourceContent(w, r, member, sourcePath)
		if err != nil {
			recordError(fmt.Errorf("collect from group member %q: %w", memberName, err))
			continue
		}
		if found {
			if member.Type == "proxy" {
				if normalizer, ok := formatGroupSourceNormalizer(member.Format); ok {
					content, err = normalizer.NormalizeGroupSource(member.FormatView(), sourcePath, content)
					if err != nil {
						recordError(fmt.Errorf("normalize group member %q: %w", memberName, err))
						continue
					}
				}
			}
			if sourceBudget > 0 {
				totalSourceBytes += int64(len(content))
				if totalSourceBytes > sourceBudget {
					firstError = fmt.Errorf("%w: %d bytes", budgetError, sourceBudget)
					break
				}
			}
			sources = append(sources, content)
			if asset != nil {
				assets = append(assets, *asset)
			}
		}
	}
	return sources, assets, firstError
}

func formatGroupSourceNormalizer(formatName string) (spiformat.GroupSourceNormalizer, bool) {
	registered, found := spiformat.Lookup(formatName)
	if !found {
		return nil, false
	}
	normalizer, ok := registered.(spiformat.GroupSourceNormalizer)
	return normalizer, ok
}

func (rt *Runtime) memberSourceContent(
	w http.ResponseWriter,
	r *http.Request,
	member domain.Repository,
	sourcePath string,
) ([]byte, *domain.Asset, bool, error) {
	if member.Type == "proxy" {
		asset, found, err := rt.ResolveProxyAsset(w, r, member, sourcePath)
		if err != nil || !found {
			return nil, nil, false, err
		}
		content, err := rt.readAssetContentForFormat(r.Context(), asset, member.Format)
		return content, &asset, err == nil, err
	}

	asset, err := rt.metaFor(member).Asset(r.Context(), sourcePath)
	if err == nil {
		content, readErr := rt.readAssetContentForFormat(r.Context(), asset, member.Format)
		return content, &asset, readErr == nil, readErr
	}
	if !errors.Is(err, domain.ErrNotFound) {
		return nil, nil, false, err
	}
	synthesizer := formatHostedSynthesizer(member.Format)
	if member.Type != "hosted" {
		return nil, nil, false, nil
	}
	if synthesizer != nil {
		content, _, ok, err := synthesizer.SynthesizeHosted(
			r.Context(),
			member.FormatView(),
			sourcePath,
			storedAssetsView{runtime: rt, repository: member},
		)
		return content, nil, ok && err == nil, err
	}
	content, found, err := rt.synthesizeHostedWireRead(r, member, sourcePath)
	return content, nil, found, err
}

// synthesizeHostedWireRead reuses a hosted format's normal read handler when
// a group needs an index that is generated by WireProtocol rather than stored
// as an asset. The bounded writer keeps the same memory limit as the explicit
// HostedSynthesizer path.
func (rt *Runtime) synthesizeHostedWireRead(
	r *http.Request,
	repository domain.Repository,
	assetPath string,
) ([]byte, bool, error) {
	registered, found := spiformat.Lookup(repository.Format)
	if !found {
		return nil, false, nil
	}
	wire, ok := registered.(spiformat.WireProtocol)
	if !ok {
		return nil, false, nil
	}
	request := r.Clone(r.Context())
	request.Method = http.MethodGet
	requestURL := *r.URL
	request.URL = &requestURL
	request.URL.Path = "/repository/" + repository.Name + "/" + assetPath
	// Group merging needs the complete source representation, so a client's
	// conditional/range preconditions must not reach internal acquisition: a 304
	// or 206 here would be unmergeable. They are honored at the outer boundary.
	stripConditionalRangeHeaders(request.Header)
	action, claimed := wire.WireAction(
		repository.FormatView(), request.Method, assetPath, request.URL.Query(),
	)
	if !claimed || action != "read" {
		return nil, false, nil
	}
	writer := &formatContentWriter{header: make(http.Header), limit: formatContentLimit(repository.Format, assetPath)}
	wire.ServeWire(
		writer,
		request,
		repository.FormatView(),
		assetPath,
		rt.WireTools(repository),
	)
	if writer.tooLarge {
		return nil, false, fmt.Errorf(
			"hosted %s content exceeds the %d byte format content limit",
			repository.Name,
			writer.limit,
		)
	}
	if writer.status == 0 {
		writer.status = http.StatusOK
	}
	if writer.status == http.StatusNotFound {
		return nil, false, nil
	}
	if writer.status < 200 || writer.status >= 300 {
		return nil, false, fmt.Errorf(
			"hosted %s synthesis returned HTTP %d",
			repository.Name,
			writer.status,
		)
	}
	return writer.body.Bytes(), true, nil
}

// stripConditionalRangeHeaders removes the request preconditions that make a
// hosted handler return a partial or not-modified response, which internal
// source acquisition cannot merge.
func stripConditionalRangeHeaders(header http.Header) {
	for _, name := range []string{
		"If-None-Match",
		"If-Modified-Since",
		"If-Match",
		"If-Unmodified-Since",
		"If-Range",
		"Range",
	} {
		header.Del(name)
	}
}

type formatContentWriter struct {
	header   http.Header
	body     bytes.Buffer
	status   int
	tooLarge bool
	limit    int64
}

func (writer *formatContentWriter) Header() http.Header { return writer.header }

func (writer *formatContentWriter) WriteHeader(status int) {
	if writer.status == 0 {
		writer.status = status
	}
}

func (writer *formatContentWriter) Write(content []byte) (int, error) {
	if writer.status == 0 {
		writer.status = http.StatusOK
	}
	if int64(writer.body.Len())+int64(len(content)) > writer.limit {
		writer.tooLarge = true
		return 0, fmt.Errorf("format content limit exceeded")
	}
	return writer.body.Write(content)
}

// ServeSynthesizedHostedContent renders a hosted read miss through the
// format's HostedSynthesizer hook. It reports whether it handled the request.
func (rt *Runtime) ServeSynthesizedHostedContent(
	w http.ResponseWriter,
	r *http.Request,
	repository domain.Repository,
	assetPath string,
) bool {
	synthesizer := formatHostedSynthesizer(repository.Format)
	if synthesizer == nil {
		return false
	}
	content, contentType, ok, err := synthesizer.SynthesizeHosted(
		r.Context(),
		repository.FormatView(),
		assetPath,
		storedAssetsView{runtime: rt, repository: repository},
	)
	if err != nil {
		httpx.WriteServerProblem(
			w,
			http.StatusInternalServerError,
			"content_synthesis_error",
			"synthesize hosted content failed",
			err,
		)
		return true
	}
	if !ok {
		return false
	}
	rt.serveSynthesizedContent(w, r, content, contentType)
	return true
}

// serveSynthesizedContent writes derived (never stored) content. The ETag is
// the content digest, so unchanged synthesized responses stay cacheable.
func (rt *Runtime) serveSynthesizedContent(
	w http.ResponseWriter,
	r *http.Request,
	content []byte,
	contentType string,
) bool {
	if contentType == "" {
		contentType = "application/octet-stream"
	}
	digest := sha256.Sum256(content)
	w.Header().Set("Content-Type", contentType)
	w.Header().Set("ETag", quoteETag("sha256:"+hex.EncodeToString(digest[:])))
	if !checkReadPreconditions(w, r, time.Time{}) {
		return false
	}
	w.Header().Set("Content-Length", fmt.Sprint(len(content)))
	if r.Method == http.MethodHead {
		w.WriteHeader(http.StatusOK)
		return false
	}
	w.WriteHeader(http.StatusOK)
	if _, err := w.Write(content); err != nil {
		rt.requestLogger(r).Warn("synthesized content stream interrupted", "error", err)
		return false
	}
	return true
}

// readAssetContent loads one asset's blob content, bounded by the format
// content limit.
func (rt *Runtime) readAssetContent(
	ctx context.Context,
	asset domain.Asset,
) ([]byte, error) {
	return rt.readAssetContentForFormat(ctx, asset, "")
}

func (rt *Runtime) readAssetContentForFormat(
	ctx context.Context,
	asset domain.Asset,
	formatName string,
) ([]byte, error) {
	assetPath := asset.Path
	if asset.FormatPath != "" {
		assetPath = asset.FormatPath
	}
	limit := formatContentLimit(formatName, assetPath)
	if asset.Size > limit {
		return nil, fmt.Errorf("asset %s exceeds the %d byte format content limit", assetPath, limit)
	}
	reader, _, err := rt.OpenStoredAsset(ctx, asset)
	if err != nil {
		return nil, err
	}
	defer reader.Close()
	content, err := io.ReadAll(io.LimitReader(reader, limit+1))
	if err != nil {
		return nil, err
	}
	if int64(len(content)) > limit {
		return nil, fmt.Errorf(
			"asset %s exceeds the %d byte format content limit",
			assetPath,
			limit,
		)
	}
	return content, nil
}

// storedAssetsView is the StoredAssets capability handle scoped to one
// hosted repository.
type storedAssetsView struct {
	runtime    *Runtime
	repository domain.Repository
}

func (view storedAssetsView) VisitAssetPaths(
	ctx context.Context,
	prefix string,
	visit func(string) (bool, error),
) error {
	return visitAssetPaths(ctx, view.runtime.metaFor(view.repository), prefix, visit)
}

func (view storedAssetsView) ReadAsset(
	ctx context.Context,
	assetPath string,
) ([]byte, bool, error) {
	asset, err := view.runtime.metaFor(view.repository).Asset(ctx, assetPath)
	if errors.Is(err, domain.ErrNotFound) {
		return nil, false, nil
	}
	if err != nil {
		return nil, false, err
	}
	content, err := view.runtime.readAssetContentForFormat(ctx, asset, view.repository.Format)
	if err != nil {
		return nil, false, err
	}
	return content, true, nil
}

func formatGroupMerger(formatName string) spiformat.GroupMerger {
	registered, found := spiformat.Lookup(formatName)
	if !found {
		return nil
	}
	merger, ok := registered.(spiformat.GroupMerger)
	if !ok {
		return nil
	}
	return merger
}

func formatHostedSynthesizer(formatName string) spiformat.HostedSynthesizer {
	registered, found := spiformat.Lookup(formatName)
	if !found {
		return nil
	}
	synthesizer, ok := registered.(spiformat.HostedSynthesizer)
	if !ok {
		return nil
	}
	return synthesizer
}

// validateFormatProxyPath lets a registered format refuse a proxy path before
// the cache or upstream is consulted.
func validateFormatProxyPath(repository domain.Repository, assetPath string) error {
	registered, found := spiformat.Lookup(repository.Format)
	if !found {
		return nil
	}
	policy, ok := registered.(spiformat.ProxyPathPolicy)
	if !ok {
		return nil
	}
	return policy.ValidateProxyPath(repository.FormatView(), assetPath)
}

var _ spiformat.StoredAssets = storedAssetsView{}
