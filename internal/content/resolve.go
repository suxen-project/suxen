package content

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/url"

	"github.com/suxen-project/suxen/internal/domain"
	"github.com/suxen-project/suxen/internal/httpx"
	spiformat "github.com/suxen-project/suxen/spi/format"
)

func (rt *Runtime) ResolveAsset(
	w http.ResponseWriter,
	r *http.Request,
	repository domain.Repository,
	assetPath string,
) (domain.Asset, bool, error) {
	switch repository.Type {
	case "group":
		return rt.resolveGroupAsset(w, r, repository, assetPath)
	case "proxy":
		return rt.ResolveProxyAsset(w, r, repository, assetPath)
	default:
		asset, err := rt.metaFor(repository).Asset(r.Context(), assetPath)
		if errors.Is(err, domain.ErrNotFound) {
			return domain.Asset{}, false, nil
		}
		if err != nil {
			return domain.Asset{}, false, fmt.Errorf("resolve hosted asset: %w", err)
		}
		return asset, true, nil
	}
}

// resolveGroupAsset returns the first member that resolves the path, except
// when a format selects artifact ownership from merged index sources. Groups
// contain only leaf repositories, so no recursion or cycle tracking is needed.
func (rt *Runtime) resolveGroupAsset(
	w http.ResponseWriter,
	r *http.Request,
	group domain.Repository,
	assetPath string,
) (domain.Asset, bool, error) {
	selectedMember := ""
	if locator := formatGroupArtifactLocator(group.Format); locator != nil {
		member, matches, handled, err := rt.selectLocatedGroupArtifactMember(w, r, group,
			spiformat.GroupArtifactRequest{Path: assetPath, RawQuery: r.URL.RawQuery}, locator, false)
		if err != nil {
			return domain.Asset{}, false, err
		}
		if handled {
			if !matches {
				return domain.Asset{}, false, nil
			}
			selectedMember = member
		}
	} else if selector := formatGroupArtifactSelector(group.Format); selector != nil {
		if sourcePath, ok := selector.GroupArtifactSource(group.FormatView(), assetPath); ok {
			member, indexed, err := rt.selectGroupArtifactMember(w, r, group, assetPath, sourcePath, selector, false)
			if err != nil {
				return domain.Asset{}, false, err
			}
			if indexed {
				if member == "" {
					return domain.Asset{}, false, nil
				}
				selectedMember = member
			}
		}
	}
	var firstError error
	for _, memberName := range group.Members {
		if selectedMember != "" && memberName != selectedMember {
			continue
		}
		member, err := rt.repositoryResolver().Repository(r.Context(), memberName)
		if errors.Is(err, domain.ErrNotFound) {
			continue
		}
		if err != nil {
			if firstError == nil {
				firstError = fmt.Errorf("load group member %q: %w", memberName, err)
			}
			continue
		}
		if member.Format != group.Format {
			continue
		}
		if member.Type == "group" {
			if firstError == nil {
				firstError = fmt.Errorf("group %q contains nested group %q", group.Name, memberName)
			}
			continue
		}

		asset, found, err := rt.ResolveAsset(w, r, member, assetPath)
		if found {
			return asset, true, nil
		}
		if err != nil && firstError == nil {
			firstError = err
		}
	}
	return domain.Asset{}, false, firstError
}

// selectGroupArtifactMember applies the same member order as index merging.
// A source miss in every member means this is a direct artifact request with
// no index, so the ordinary first-match path remains available.
func (rt *Runtime) selectGroupArtifactMember(
	w http.ResponseWriter,
	r *http.Request,
	group domain.Repository,
	assetPath, sourcePath string,
	selector spiformat.GroupArtifactSelector,
	storedOnly bool,
) (memberName string, indexed bool, err error) {
	selected := ""
	for _, name := range group.Members {
		member, err := rt.repositoryResolver().Repository(r.Context(), name)
		if errors.Is(err, domain.ErrNotFound) {
			continue
		}
		if err != nil {
			return "", false, fmt.Errorf("load group member %q: %w", name, err)
		}
		if member.Format != group.Format {
			continue
		}
		if member.Type == "group" {
			return "", false, fmt.Errorf("group %q contains nested group %q", group.Name, name)
		}
		var body []byte
		var found bool
		if storedOnly {
			body, found, err = rt.storedGroupSourceContent(r, member, sourcePath)
		} else {
			body, _, found, err = rt.memberSourceContent(w, r, member, sourcePath)
		}
		if err != nil {
			return "", false, fmt.Errorf("collect from group member %q: %w", name, err)
		}
		if !found {
			continue
		}
		indexed = true
		contains, err := selector.GroupSourceContainsArtifact(member.FormatView(), sourcePath, assetPath, body)
		if err != nil {
			return "", false, fmt.Errorf("inspect group member %q: %w", name, err)
		}
		if contains && selected == "" {
			selected = name
		}
	}
	return selected, indexed, nil
}

// StoredGroupArtifactOwner evaluates group artifact visibility from stored
// member indexes only. Browse, detail, and ID downloads must never trigger a
// proxy fetch or refresh. indexed=false means the inventory should retain its
// ordinary first-match visibility rule.
func (rt *Runtime) StoredGroupArtifactOwner(
	ctx context.Context, group domain.Repository, assetPath, storedPath string,
) (member string, indexed bool, err error) {
	if locator := formatGroupArtifactLocator(group.Format); locator != nil {
		r := (&http.Request{Method: http.MethodGet, Header: make(http.Header)}).WithContext(ctx)
		member, matches, handled, err := rt.selectLocatedGroupArtifactMember(nil, r, group,
			spiformat.GroupArtifactRequest{Path: assetPath, StoredPath: storedPath}, locator, true)
		if err != nil || !handled {
			return "", handled, err
		}
		if !matches {
			return "", true, nil
		}
		return member, true, nil
	}
	selector := formatGroupArtifactSelector(group.Format)
	if selector == nil {
		return "", false, nil
	}
	sourcePath, ok := selector.GroupArtifactSource(group.FormatView(), assetPath)
	if !ok {
		return "", false, nil
	}
	r := (&http.Request{Method: http.MethodGet, Header: make(http.Header)}).WithContext(ctx)
	member, indexed, err = rt.selectGroupArtifactMember(nil, r, group, assetPath, sourcePath, selector, true)
	if err != nil || !indexed || member == "" {
		return member, indexed, err
	}

	// Selection establishes which member owns the public path. A proxy can
	// retain several immutable cache rows for that path after its advertised
	// URL or digest changes, so ownership alone cannot make every row visible.
	owner, err := rt.repositoryResolver().Repository(ctx, member)
	if err != nil {
		return "", true, fmt.Errorf("load group artifact owner %q: %w", member, err)
	}
	if owner.Type != "proxy" {
		return member, true, nil
	}
	resolver := formatProxyRequestResolver(owner.Format)
	if resolver == nil {
		return member, true, nil
	}
	resolved, err := resolver.ResolveProxyRequest(ctx, owner.FormatView(), assetPath, "",
		storedAssetsView{runtime: rt, repository: owner})
	if err != nil {
		return "", true, fmt.Errorf("resolve stored group artifact %q: %w", assetPath, err)
	}
	// CacheOnly can be a retained-cache recovery when metadata needed to
	// establish the current generation is gone. It is valid for a direct
	// proxy read, but cannot prove this row is advertised by the group.
	if resolved.CacheOnly || resolved.CachePath != storedPath {
		return "", true, nil
	}
	return member, true, nil
}

func (rt *Runtime) storedGroupSourceContent(
	r *http.Request, member domain.Repository, sourcePath string,
) ([]byte, bool, error) {
	asset, err := rt.metaFor(member).Asset(r.Context(), sourcePath)
	if err == nil {
		body, readErr := rt.readAssetContentForFormat(r.Context(), asset, member.Format)
		return body, readErr == nil, readErr
	}
	if !errors.Is(err, domain.ErrNotFound) {
		return nil, false, err
	}
	if member.Type != "hosted" {
		return nil, false, nil
	}
	if synthesizer := formatHostedSynthesizer(member.Format); synthesizer != nil {
		body, _, found, err := synthesizer.SynthesizeHosted(
			r.Context(), member.FormatView(), sourcePath,
			storedAssetsView{runtime: rt, repository: member},
		)
		return body, found && err == nil, err
	}
	request := r.Clone(r.Context())
	request.Method = http.MethodGet
	request.URL = &url.URL{Path: "/repository/" + member.Name + "/" + sourcePath}
	return rt.synthesizeHostedWireRead(request, member, sourcePath)
}

func formatGroupArtifactSelector(formatName string) spiformat.GroupArtifactSelector {
	registered, found := spiformat.Lookup(formatName)
	if !found {
		return nil
	}
	selector, _ := registered.(spiformat.GroupArtifactSelector)
	return selector
}

func formatGroupArtifactLocator(formatName string) spiformat.GroupArtifactLocator {
	registered, found := spiformat.Lookup(formatName)
	if !found {
		return nil
	}
	locator, _ := registered.(spiformat.GroupArtifactLocator)
	return locator
}

func writeAssetResolutionError(w http.ResponseWriter, err error) {
	WriteAssetResolutionError(w, err)
}

func WriteAssetResolutionError(w http.ResponseWriter, err error) {
	var violation *spiformat.PolicyViolation
	if errors.As(err, &violation) {
		httpx.WriteResult(w, nil, err)
		return
	}
	var upstream *UpstreamError
	if errors.As(err, &upstream) {
		httpx.WriteServerProblem(
			w,
			http.StatusBadGateway,
			"upstream_error",
			"upstream request failed",
			err,
		)
		return
	}
	httpx.WriteServerProblem(
		w,
		http.StatusInternalServerError,
		"resolution_error",
		"resolve asset failed",
		err,
	)
}
