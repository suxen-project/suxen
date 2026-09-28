package oci

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"sort"
	"strconv"
	"strings"

	"github.com/suxen-project/suxen/internal/domain"
	"github.com/suxen-project/suxen/internal/httpx"
	"github.com/suxen-project/suxen/internal/ocimodel"
)

const maxOCIUpstreamPages = 32
const defaultOCIPageSize = 100
const maxOCIPageSize = 1000

func (h *Handler) handleOCICatalog(
	w http.ResponseWriter,
	r *http.Request,
	repository domain.Repository,
) {
	if r.Method != http.MethodGet {
		httpx.MethodNotAllowed(w, http.MethodGet)
		return
	}
	page, err := ParsePageRequest(r.URL.Query())
	if err != nil {
		httpx.WriteOCIError(w, http.StatusBadRequest, "UNSUPPORTED", "invalid n query parameter")
		return
	}
	names, err := h.collectOCICatalog(r, repository)
	if err != nil {
		httpx.WriteServerProblem(
			w,
			http.StatusBadGateway,
			"upstream_error",
			"upstream request failed",
			err,
		)
		return
	}
	names, next, more := paginateOCINames(names, page)
	if more {
		writeOCINextLink(w, r, page, next)
	}
	httpx.WriteJSON(w, http.StatusOK, map[string]any{"repositories": names})
}

func (h *Handler) collectOCICatalog(
	r *http.Request,
	repository domain.Repository,
) ([]string, error) {
	var names []string
	var err error
	switch repository.Type {
	case "group":
		names, err = h.collectGroupOCICatalog(r, repository)
	case "proxy":
		names, err = h.fetchProxyOCICatalog(r, repository)
	default:
		names, err = h.localOCICatalog(r, repository)
	}
	if err != nil {
		return nil, err
	}
	return uniqueSortedStrings(names), nil
}

// collectGroupOCICatalog iterates the group's leaf members once. Groups contain
// only leaves, so a member that is itself a group is invalid legacy state and is
// rejected rather than walked.
func (h *Handler) collectGroupOCICatalog(
	r *http.Request,
	group domain.Repository,
) ([]string, error) {
	var names []string
	for _, memberName := range group.Members {
		member, err := h.meta().Repository(r.Context(), memberName)
		if err != nil {
			return nil, fmt.Errorf("load group member %q: %w", memberName, err)
		}
		if member.Format != "oci" {
			continue
		}
		if member.Type == "group" {
			return nil, fmt.Errorf("group %q contains nested group %q", group.Name, memberName)
		}
		memberNames, err := h.collectOCICatalog(r, member)
		if err != nil {
			return nil, err
		}
		names = append(names, memberNames...)
	}
	return names, nil
}

func (h *Handler) localOCICatalog(
	r *http.Request,
	repository domain.Repository,
) ([]string, error) {
	assets, err := h.metaFor(repository).Assets(r.Context(), "v2/")
	if err != nil {
		return nil, err
	}
	names := make([]string, 0)
	for _, asset := range assets {
		route, ok := ocimodel.ParseAssetPath(asset.Path)
		if !ok || route.ImageName == "" {
			continue
		}
		names = append(names, route.ImageName)
	}
	return names, nil
}

func (h *Handler) fetchProxyOCICatalog(
	r *http.Request,
	repository domain.Repository,
) ([]string, error) {
	assetPath := "v2/_catalog"
	names := make([]string, 0)
	for page := 0; page < maxOCIUpstreamPages; page++ {
		response, err := h.Runtime.DoUpstreamRequest(r, repository, http.MethodGet, assetPath, nil)
		if err != nil {
			return nil, err
		}
		if response.StatusCode == http.StatusNotFound {
			_ = response.Body.Close()
			if page > 0 {
				return nil, errors.New("upstream catalog continuation was not found")
			}
			return names, nil
		}
		if response.StatusCode < 200 || response.StatusCode >= 300 {
			_ = response.Body.Close()
			return nil, fmt.Errorf("upstream returned %s", response.Status)
		}

		var payload struct {
			Repositories []string `json:"repositories"`
		}
		decoder := json.NewDecoder(io.LimitReader(response.Body, 2<<20))
		decodeErr := decoder.Decode(&payload)
		links := response.Header.Values("Link")
		_ = response.Body.Close()
		if decodeErr != nil {
			return nil, fmt.Errorf("decode upstream catalog: %w", decodeErr)
		}
		names = append(names, payload.Repositories...)

		next, ok := parseOCINextLinkHeaders(links)
		if !ok {
			return names, nil
		}
		if page == maxOCIUpstreamPages-1 {
			return nil, errors.New("upstream catalog exceeds the page limit")
		}
		assetPath, err = ociAssetPathFromLink(repository.Upstream, assetPath, next)
		if err != nil {
			return nil, fmt.Errorf("upstream catalog continuation: %w", err)
		}
	}
	return nil, errors.New("upstream catalog exceeds the page limit")
}

func (h *Handler) collectOCITags(
	r *http.Request,
	repository domain.Repository,
	imageName string,
) ([]string, error) {
	var tags []string
	var err error
	switch repository.Type {
	case "group":
		tags, err = h.collectGroupOCITags(r, repository, imageName)
	case "proxy":
		tags, err = h.fetchProxyOCITags(r, repository, imageName)
	default:
		tags, err = h.localOCITags(r, repository, imageName)
	}
	if err != nil {
		return nil, err
	}
	return uniqueSortedStrings(tags), nil
}

func (h *Handler) collectGroupOCITags(
	r *http.Request,
	group domain.Repository,
	imageName string,
) ([]string, error) {
	var tags []string
	for _, memberName := range group.Members {
		member, err := h.meta().Repository(r.Context(), memberName)
		if err != nil {
			return nil, fmt.Errorf("load group member %q: %w", memberName, err)
		}
		if member.Format != "oci" {
			continue
		}
		if member.Type == "group" {
			return nil, fmt.Errorf("group %q contains nested group %q", group.Name, memberName)
		}
		memberTags, err := h.collectOCITags(r, member, imageName)
		if err != nil {
			return nil, err
		}
		tags = append(tags, memberTags...)
	}
	return tags, nil
}

func (h *Handler) localOCITags(
	r *http.Request,
	repository domain.Repository,
	imageName string,
) ([]string, error) {
	assets, err := h.metaFor(repository).Assets(
		r.Context(),
		ocimodel.ManifestPath(imageName, ""),
	)
	if err != nil {
		return nil, err
	}

	tags := make([]string, 0)
	for _, asset := range assets {
		if asset.Kind != "oci-manifest" || strings.HasPrefix(asset.Reference, "sha256:") {
			continue
		}
		tags = append(tags, asset.Reference)
	}
	return tags, nil
}

func (h *Handler) fetchProxyOCITags(
	r *http.Request,
	repository domain.Repository,
	imageName string,
) ([]string, error) {
	assetPath := "v2/" + imageName + "/tags/list"
	tags := make([]string, 0)
	for page := 0; page < maxOCIUpstreamPages; page++ {
		response, err := h.Runtime.DoUpstreamRequest(r, repository, http.MethodGet, assetPath, nil)
		if err != nil {
			return nil, err
		}
		if response.StatusCode == http.StatusNotFound {
			_ = response.Body.Close()
			if page > 0 {
				return nil, errors.New("upstream tag continuation was not found")
			}
			return tags, nil
		}
		if response.StatusCode < 200 || response.StatusCode >= 300 {
			_ = response.Body.Close()
			return nil, fmt.Errorf("upstream returned %s", response.Status)
		}
		var payload struct {
			Tags []string `json:"tags"`
		}
		decoder := json.NewDecoder(io.LimitReader(response.Body, 1<<20))
		decodeErr := decoder.Decode(&payload)
		links := response.Header.Values("Link")
		_ = response.Body.Close()
		if decodeErr != nil {
			return nil, fmt.Errorf("decode upstream tags: %w", decodeErr)
		}
		tags = append(tags, payload.Tags...)
		next, ok := parseOCINextLinkHeaders(links)
		if !ok {
			return tags, nil
		}
		if page == maxOCIUpstreamPages-1 {
			return nil, errors.New("upstream tags exceed the page limit")
		}
		assetPath, err = ociAssetPathFromLink(repository.Upstream, assetPath, next)
		if err != nil {
			return nil, fmt.Errorf("upstream tag continuation: %w", err)
		}
	}
	return nil, errors.New("upstream tags exceed the page limit")
}

func (h *Handler) collectOCIReferrers(
	r *http.Request,
	repository domain.Repository,
	imageName string,
	subjectDigest string,
) ([]ocimodel.Descriptor, error) {
	var descriptors []ocimodel.Descriptor
	var err error
	switch repository.Type {
	case "group":
		descriptors, err = h.collectGroupOCIReferrers(
			r,
			repository,
			imageName,
			subjectDigest,
		)
	case "proxy":
		descriptors, err = h.fetchProxyOCIReferrers(
			r,
			repository,
			imageName,
			subjectDigest,
		)
	default:
		descriptors, err = h.localOCIReferrers(r, repository, imageName, subjectDigest)
	}
	if err != nil {
		return nil, err
	}
	return uniqueDescriptors(descriptors), nil
}

func (h *Handler) collectGroupOCIReferrers(
	r *http.Request,
	group domain.Repository,
	imageName string,
	subjectDigest string,
) ([]ocimodel.Descriptor, error) {
	var descriptors []ocimodel.Descriptor
	for _, memberName := range group.Members {
		member, err := h.meta().Repository(r.Context(), memberName)
		if err != nil {
			return nil, fmt.Errorf("load group member %q: %w", memberName, err)
		}
		if member.Format != "oci" {
			continue
		}
		if member.Type == "group" {
			return nil, fmt.Errorf("group %q contains nested group %q", group.Name, memberName)
		}
		memberDescriptors, err := h.collectOCIReferrers(
			r,
			member,
			imageName,
			subjectDigest,
		)
		if err != nil {
			return nil, err
		}
		descriptors = append(descriptors, memberDescriptors...)
	}
	return descriptors, nil
}

func (h *Handler) localOCIReferrers(
	r *http.Request,
	repository domain.Repository,
	imageName string,
	subjectDigest string,
) ([]ocimodel.Descriptor, error) {
	assets, err := h.metaFor(repository).Assets(
		r.Context(),
		ocimodel.ManifestPath(imageName, ""),
	)
	if err != nil {
		return nil, err
	}

	descriptors := make([]ocimodel.Descriptor, 0)
	for _, asset := range assets {
		if asset.SubjectDigest != subjectDigest {
			continue
		}
		descriptors = append(descriptors, ocimodel.Descriptor{
			MediaType:    asset.ContentType,
			ArtifactType: ociStoredArtifactType(asset),
			Digest:       asset.Digest,
			Size:         asset.Size,
			Annotations:  ociStoredAnnotations(asset),
		})
	}
	return descriptors, nil
}

func (h *Handler) fetchProxyOCIReferrers(
	r *http.Request,
	repository domain.Repository,
	imageName string,
	subjectDigest string,
) ([]ocimodel.Descriptor, error) {
	assetPath := "v2/" + imageName + "/referrers/" + subjectDigest
	if artifactType := r.URL.Query().Get("artifactType"); artifactType != "" {
		assetPath += "?" + url.Values{"artifactType": {artifactType}}.Encode()
	}
	descriptors := make([]ocimodel.Descriptor, 0)
	for page := 0; page < maxOCIUpstreamPages; page++ {
		response, err := h.Runtime.DoUpstreamRequest(r, repository, http.MethodGet, assetPath, nil)
		if err != nil {
			return nil, err
		}
		if response.StatusCode == http.StatusNotFound {
			_ = response.Body.Close()
			if page > 0 {
				return nil, errors.New("upstream referrer continuation was not found")
			}
			return descriptors, nil
		}
		if response.StatusCode < 200 || response.StatusCode >= 300 {
			_ = response.Body.Close()
			return nil, fmt.Errorf("upstream returned %s", response.Status)
		}
		var index Index
		decoder := json.NewDecoder(io.LimitReader(response.Body, 2<<20))
		decodeErr := decoder.Decode(&index)
		links := response.Header.Values("Link")
		_ = response.Body.Close()
		if decodeErr != nil {
			return nil, fmt.Errorf("decode upstream referrers: %w", decodeErr)
		}
		descriptors = append(descriptors, index.Manifests...)
		next, ok := parseOCINextLinkHeaders(links)
		if !ok {
			return descriptors, nil
		}
		if page == maxOCIUpstreamPages-1 {
			return nil, errors.New("upstream referrers exceed the page limit")
		}
		assetPath, err = ociAssetPathFromLink(repository.Upstream, assetPath, next)
		if err != nil {
			return nil, fmt.Errorf("upstream referrer continuation: %w", err)
		}
	}
	return nil, errors.New("upstream referrers exceed the page limit")
}

func filterOCIReferrers(descriptors []ocimodel.Descriptor, artifactType string) []ocimodel.Descriptor {
	if artifactType == "" {
		return descriptors
	}
	filtered := make([]ocimodel.Descriptor, 0)
	for _, descriptor := range descriptors {
		if descriptor.ArtifactType == artifactType {
			filtered = append(filtered, descriptor)
		}
	}
	return filtered
}

func ociStoredArtifactType(asset domain.Asset) string {
	stored, ok := asset.Attributes["oci"].(map[string]any)
	if !ok {
		return ""
	}
	artifactType, _ := stored["artifactType"].(string)
	return artifactType
}

func ociStoredAnnotations(asset domain.Asset) map[string]string {
	stored, ok := asset.Attributes["oci"].(map[string]any)
	if !ok {
		return nil
	}
	values, ok := stored["annotations"].(map[string]any)
	if !ok || len(values) == 0 {
		return nil
	}
	annotations := make(map[string]string, len(values))
	for name, value := range values {
		if text, ok := value.(string); ok {
			annotations[name] = text
		}
	}
	return annotations
}

type PageRequest struct {
	N    int
	NSet bool
	Last string
}

func ParsePageRequest(query url.Values) (PageRequest, error) {
	page := PageRequest{Last: query.Get("last"), N: defaultOCIPageSize, NSet: true}
	raw := query.Get("n")
	if raw == "" {
		return page, nil
	}
	n, err := strconv.Atoi(raw)
	if err != nil || n < 0 || n > maxOCIPageSize {
		return PageRequest{}, fmt.Errorf("invalid n")
	}
	page.N = n
	return page, nil
}

func paginateOCINames(names []string, page PageRequest) ([]string, string, bool) {
	start := 0
	if page.Last != "" {
		start = len(names)
		for index, name := range names {
			if name > page.Last {
				start = index
				break
			}
		}
	}
	remaining := names[start:]
	if remaining == nil {
		remaining = []string{}
	}
	if !page.NSet {
		return remaining, "", false
	}
	if page.N == 0 {
		return []string{}, "", false
	}
	if len(remaining) > page.N {
		items := remaining[:page.N]
		return items, items[len(items)-1], true
	}
	return remaining, "", false
}

func writeOCINextLink(w http.ResponseWriter, r *http.Request, page PageRequest, last string) {
	query := url.Values{}
	if page.NSet {
		query.Set("n", strconv.Itoa(page.N))
	}
	query.Set("last", last)
	w.Header().Set("Link", fmt.Sprintf("<%s?%s>; rel=\"next\"", r.URL.Path, query.Encode()))
}

func ociAssetPathFromLink(upstream, currentAssetPath, link string) (string, error) {
	base, err := url.Parse(upstream)
	if err != nil {
		return "", fmt.Errorf("parse upstream URL: %w", err)
	}
	path, query, _ := strings.Cut(currentAssetPath, "?")
	current := *base
	current.User = nil
	current.Path = strings.TrimRight(base.Path, "/") + "/" + strings.TrimLeft(path, "/")
	current.RawQuery = query
	next, err := current.Parse(link)
	if err != nil || next.User != nil || next.Fragment != "" ||
		!strings.EqualFold(next.Scheme, current.Scheme) ||
		!strings.EqualFold(next.Host, current.Host) ||
		next.Path != current.Path {
		return "", errors.New("next link leaves the configured upstream endpoint")
	}
	if next.RawQuery == "" {
		return path, nil
	}
	return path + "?" + next.RawQuery, nil
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

func uniqueDescriptors(values []ocimodel.Descriptor) []ocimodel.Descriptor {
	seen := make(map[string]bool, len(values))
	result := make([]ocimodel.Descriptor, 0, len(values))
	for _, value := range values {
		if seen[value.Digest] {
			continue
		}
		seen[value.Digest] = true
		result = append(result, value)
	}
	sort.Slice(result, func(left, right int) bool {
		return result[left].Digest < result[right].Digest
	})
	return result
}
