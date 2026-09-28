package content

import (
	"fmt"
	"net/http"
	"net/url"
	"strings"

	"github.com/suxen-project/suxen/internal/domain"
	"github.com/suxen-project/suxen/internal/httpx"
	spiformat "github.com/suxen-project/suxen/spi/format"
)

// ServeRewrittenProxyIndex serves a cached proxy index through the format's
// IndexRewriter when the path is mutable, so embedded upstream URLs point back
// at this repository as the client reached it. Stored bytes stay verbatim;
// the rewrite happens per request. It reports whether it served the request;
// false means the path does not require rewriting. Once rewriting applies,
// any read or rewrite failure is returned to the client; serving the cached
// upstream document verbatim would leak external download URLs.
func (rt *Runtime) ServeRewrittenProxyIndex(
	w http.ResponseWriter,
	r *http.Request,
	repository domain.Repository,
	asset domain.Asset,
) bool {
	source := repository
	if repository.Type == "group" && asset.Repository != repository.Name {
		var err error
		source, err = rt.repositoryResolver().Repository(r.Context(), asset.Repository)
		if err != nil {
			httpx.WriteServerProblem(w, http.StatusInternalServerError,
				"index_rewrite_error", "load proxy source for rewriting failed", err)
			return true
		}
	}
	if source.Type != "proxy" {
		return false
	}
	formatPath := asset.Path
	if asset.FormatPath != "" {
		formatPath = asset.FormatPath
	}
	rewriter := formatIndexRewriter(repository.Format)
	if rewriter == nil || !mutableProxyPath(source, asset, formatPath) {
		return false
	}
	if !rt.downloadPermitted(w, r, asset) {
		return true
	}
	release, err := rt.acquireLargeIndex(r.Context(), repository.Format, formatPath)
	if err != nil {
		writeAssetResolutionError(w, err)
		return true
	}
	defer release()
	content, err := rt.readAssetContentForFormat(r.Context(), asset, repository.Format)
	if err != nil {
		httpx.WriteServerProblem(
			w,
			http.StatusInternalServerError,
			"index_rewrite_error",
			"read proxy index for rewriting failed",
			err,
		)
		return true
	}
	rewritten, contentType, negotiated, err := rewriteIndexContent(
		rewriter,
		repository.FormatView(),
		formatPath,
		content,
		asset.ContentType,
		repositoryURL(r, repository.Name),
		strings.Join(r.Header.Values("Accept"), ", "),
	)
	if err != nil {
		httpx.WriteServerProblem(
			w,
			http.StatusInternalServerError,
			"index_rewrite_error",
			"rewrite proxy index failed",
			err,
		)
		return true
	}
	if npmRenderedContentTooLarge(repository.Format, formatPath, len(rewritten)) {
		httpx.WriteServerProblem(w, http.StatusInternalServerError,
			"index_rewrite_error", "rewritten npm packument exceeds rendered content limit",
			fmt.Errorf("rewritten npm packument exceeds %d bytes", npmRenderedContentLimit))
		return true
	}
	if contentType == "" {
		contentType = asset.ContentType
	}
	if negotiated {
		w.Header().Add("Vary", "Accept")
	}
	served := rt.serveSynthesizedContent(w, r, rewritten, contentType)
	// The derived index is still a read of the backing cached asset. Keep
	// cleanup's last-access projection and download events consistent with an
	// ordinary successful GET, including group reads of a member asset.
	if served && r.Method == http.MethodGet {
		rt.recordAssetDownload(r, asset)
	}
	return true
}

// rewriteMergedIndex applies the format's IndexRewriter to merged group
// content on a mutable path, with the group's own URL, so members' stored
// upstream URLs never leak through a group.
func rewriteMergedIndex(
	r *http.Request,
	group domain.Repository,
	assetPath string,
	content []byte,
	contentType string,
) ([]byte, string, bool, error) {
	rewriter := formatIndexRewriter(group.Format)
	if rewriter == nil {
		return content, contentType, false, nil
	}
	policy, ok := spiformat.Lookup(group.Format)
	if !ok {
		return content, contentType, false, nil
	}
	mutable, ok := policy.(spiformat.ProxyPolicy)
	if !ok || !mutable.MutableUpstreamPath(group.FormatView(), assetPath) {
		return content, contentType, false, nil
	}
	rewritten, newContentType, negotiated, err := rewriteIndexContent(
		rewriter,
		group.FormatView(),
		assetPath,
		content,
		contentType,
		repositoryURL(r, group.Name),
		strings.Join(r.Header.Values("Accept"), ", "),
	)
	if err != nil {
		return nil, "", false, err
	}
	if newContentType == "" {
		newContentType = contentType
	}
	return rewritten, newContentType, negotiated, nil
}

func rewriteIndexContent(
	rewriter spiformat.IndexRewriter,
	repository spiformat.Repository,
	assetPath string,
	content []byte,
	contentType string,
	repositoryURL string,
	accept string,
) ([]byte, string, bool, error) {
	if negotiated, ok := rewriter.(spiformat.NegotiatedIndexRewriter); ok {
		body, mediaType, err := negotiated.RewriteIndexForAccept(
			repository, assetPath, content, contentType, repositoryURL, accept,
		)
		return body, mediaType, true, err
	}
	body, mediaType, err := rewriter.RewriteIndex(
		repository, assetPath, content, contentType, repositoryURL,
	)
	return body, mediaType, false, err
}

// repositoryURL is the absolute URL of a repository root as the current
// request reached it.
func repositoryURL(r *http.Request, repositoryName string) string {
	return httpx.RequestOrigin(r) + "/repository/" + url.PathEscape(repositoryName)
}

// upstreamRequestURL uses the proxy read's resolved target, or the repository
// upstream joined with the original path for direct protocol requests.
// Credentials are sent only to the configured upstream origin.
func upstreamRequestURL(
	repository domain.Repository,
	assetPath string,
	preresolved resolvedUpstream,
) (string, upstreamCredentials, error) {
	requestURL, credentials, err := proxyURL(repository.Upstream, assetPath)
	if err != nil {
		return "", upstreamCredentials{}, err
	}
	if preresolved.used {
		if preresolved.url == "" {
			return requestURL, credentials, nil
		}
		return validatedResolvedUpstream(repository, preresolved.url, credentials)
	}
	return requestURL, credentials, nil
}

func validatedResolvedUpstream(
	repository domain.Repository,
	resolved string,
	credentials upstreamCredentials,
) (string, upstreamCredentials, error) {
	target, err := url.Parse(resolved)
	if err != nil {
		return "", upstreamCredentials{}, fmt.Errorf("invalid resolved upstream URL")
	}
	if (target.Scheme != "http" && target.Scheme != "https") || target.Host == "" {
		return "", upstreamCredentials{}, fmt.Errorf("resolved upstream URL must be absolute http or https")
	}
	if target.User != nil {
		return "", upstreamCredentials{}, fmt.Errorf("resolved upstream URL must not carry credentials")
	}
	upstream, err := url.Parse(strings.TrimSpace(repository.Upstream))
	if err != nil || !sameOrigin(upstream, target) {
		credentials = upstreamCredentials{}
	}
	return target.String(), credentials, nil
}

func formatProxyRequestResolver(formatName string) spiformat.ProxyRequestResolver {
	registered, found := spiformat.Lookup(formatName)
	if !found {
		return nil
	}
	resolver, ok := registered.(spiformat.ProxyRequestResolver)
	if !ok {
		return nil
	}
	return resolver
}

func formatIndexRewriter(formatName string) spiformat.IndexRewriter {
	registered, found := spiformat.Lookup(formatName)
	if !found {
		return nil
	}
	rewriter, ok := registered.(spiformat.IndexRewriter)
	if !ok {
		return nil
	}
	return rewriter
}
