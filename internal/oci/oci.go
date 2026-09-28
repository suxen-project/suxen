package oci

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"strings"

	"github.com/suxen-project/suxen/internal/assetattrs"
	"github.com/suxen-project/suxen/internal/blob"
	"github.com/suxen-project/suxen/internal/content"
	"github.com/suxen-project/suxen/internal/domain"
	"github.com/suxen-project/suxen/internal/httpx"
	"github.com/suxen-project/suxen/internal/identity"
	"github.com/suxen-project/suxen/internal/ocimodel"
)

const IndexMediaType = "application/vnd.oci.image.index.v1+json"

var errManifestDependencyMissing = errors.New("manifest dependency is missing")
var errManifestDescriptorMismatch = errors.New("manifest descriptor does not match referenced content")

type Index struct {
	SchemaVersion int                   `json:"schemaVersion"`
	MediaType     string                `json:"mediaType"`
	Manifests     []ocimodel.Descriptor `json:"manifests"`
}

func (h *Handler) Handle(
	w http.ResponseWriter,
	r *http.Request,
	repository domain.Repository,
	requestPath string,
) {
	httpx.SelectErrorResponseFormat(w, httpx.ErrorResponseFormatOCI)
	w.Header().Set("Docker-Distribution-API-Version", "registry/2.0")
	requestPath = strings.TrimPrefix(requestPath, "/")

	if requestPath == "" {
		h.handleOCIPing(w, r, repository)
		return
	}
	if identity.DistributionAuthPath(requestPath) {
		h.Runtime.Auth.HandleOCIToken(w, r, repository)
		return
	}
	if requestPath == "_catalog" || requestPath == "_catalog/" {
		h.handleOCICatalog(w, r, repository)
		return
	}

	route, ok := ocimodel.ParseRoute(requestPath)
	if !ok {
		httpx.WriteOCIError(w, http.StatusNotFound, "NAME_UNKNOWN", "OCI route not found")
		return
	}
	if err := route.Validate(); err != nil {
		code := "NAME_INVALID"
		if errors.Is(err, ocimodel.ErrInvalidManifestReference) {
			code = "TAG_INVALID"
		}
		httpx.WriteOCIError(w, http.StatusBadRequest, code, err.Error())
		return
	}

	switch route.Kind {
	case ocimodel.UploadRoute:
		h.handleOCIUploadRoute(w, r, repository, route.ImageName, route.Value)
	case ocimodel.BlobRoute:
		h.handleOCIBlobRoute(w, r, repository, route.ImageName, route.Value)
	case ocimodel.ManifestRoute:
		h.handleOCIManifestRoute(w, r, repository, route.ImageName, route.Value)
	case ocimodel.TagsRoute:
		h.handleOCITags(w, r, repository, route.ImageName)
	case ocimodel.ReferrersRoute:
		h.handleOCIReferrers(w, r, repository, route.ImageName, route.Value)
	default:
		httpx.WriteOCIError(w, http.StatusNotFound, "NAME_UNKNOWN", "OCI route not found")
	}
}

func (h *Handler) handleOCIPing(
	w http.ResponseWriter,
	r *http.Request,
	repository domain.Repository,
) {
	if r.Method != http.MethodGet {
		httpx.MethodNotAllowed(w, http.MethodGet)
		return
	}
	// Docker omits stored credentials after an anonymous 200 ping, so pushes
	// then fail with unauthorized. Challenge unauthenticated clients here.
	// Artifact GET/HEAD still follow repository read, including anonymous.
	if _, authenticated := h.Runtime.Auth.Authenticate(r); !authenticated {
		h.Runtime.Auth.ChallengeOCIAuthentication(w, r, repository)
		return
	}
	if !h.Runtime.Auth.RequireRepositoryPrivilege(w, r, repository.Name, "read") {
		return
	}
	w.WriteHeader(http.StatusOK)
}

func (h *Handler) handleOCIBlobRoute(
	w http.ResponseWriter,
	r *http.Request,
	repository domain.Repository,
	imageName string,
	digest string,
) {
	if _, err := blob.NormalizeDigest(digest); err != nil {
		httpx.WriteOCIError(w, http.StatusBadRequest, "DIGEST_INVALID", err.Error())
		return
	}

	assetPath := ocimodel.BlobPath(imageName, digest)
	switch r.Method {
	case http.MethodGet, http.MethodHead:
		asset, found, err := h.Runtime.ResolveAsset(w, r, repository, assetPath)
		if err != nil {
			content.WriteAssetResolutionError(w, err)
			return
		}
		if !found {
			httpx.WriteOCIError(w, http.StatusNotFound, "BLOB_UNKNOWN", "blob not found")
			return
		}
		h.Runtime.ServeAsset(w, r, asset)
	case http.MethodDelete:
		if repository.Type != "hosted" {
			httpx.WriteResult(w, nil, domain.ErrReadOnly)
			return
		}
		deleted, err := h.metaFor(repository).DeleteAsset(
			r.Context(),
			assetPath,
		)
		if err != nil {
			httpx.WriteResult(w, nil, err)
			return
		}
		h.Runtime.EnqueueAssetEvent(r.Context(), domain.WebhookAssetDeleted, deleted)
		w.WriteHeader(http.StatusAccepted)
	default:
		httpx.MethodNotAllowed(w, http.MethodGet, http.MethodHead, http.MethodDelete)
	}
}

func (h *Handler) handleOCIManifestRoute(
	w http.ResponseWriter,
	r *http.Request,
	repository domain.Repository,
	imageName string,
	reference string,
) {
	switch r.Method {
	case http.MethodGet, http.MethodHead:
		assetPath := ocimodel.ManifestPath(imageName, reference)
		asset, found, err := h.Runtime.ResolveAsset(w, r, repository, assetPath)
		if err != nil {
			content.WriteAssetResolutionError(w, err)
			return
		}
		if !found {
			httpx.WriteOCIError(w, http.StatusNotFound, "MANIFEST_UNKNOWN", "manifest not found")
			return
		}
		h.Runtime.ServeAsset(w, r, asset)
	case http.MethodPut:
		h.putOCIManifest(w, r, repository, imageName, reference)
	case http.MethodDelete:
		h.deleteOCIManifest(w, r, repository, imageName, reference)
	default:
		httpx.MethodNotAllowed(w, http.MethodGet, http.MethodHead, http.MethodPut, http.MethodDelete)
	}
}

func (h *Handler) putOCIManifest(
	w http.ResponseWriter,
	r *http.Request,
	repository domain.Repository,
	imageName string,
	reference string,
) {
	if repository.Type != "hosted" {
		httpx.WriteResult(w, nil, domain.ErrReadOnly)
		return
	}

	if r.ContentLength > ocimodel.MaxManifestBytes {
		httpx.WriteOCIError(
			w,
			http.StatusRequestEntityTooLarge,
			"MANIFEST_INVALID",
			ocimodel.ErrManifestTooLarge.Error(),
		)
		return
	}

	staged, err := h.Runtime.StageOCIManifest(w, r.Body)
	if err != nil {
		if errors.Is(err, ocimodel.ErrManifestTooLarge) {
			httpx.WriteOCIError(
				w,
				http.StatusRequestEntityTooLarge,
				"MANIFEST_INVALID",
				err.Error(),
			)
			return
		}
		httpx.WriteOCIError(w, http.StatusBadRequest, "MANIFEST_INVALID", err.Error())
		return
	}
	defer staged.Remove()

	manifest, err := ocimodel.ReadManifestEnvelope(staged.Path)
	if err != nil {
		httpx.WriteOCIError(w, http.StatusBadRequest, "MANIFEST_INVALID", err.Error())
		return
	}
	if err := ocimodel.ValidateHostedManifest(manifest); err != nil {
		httpx.WriteOCIError(w, http.StatusBadRequest, "MANIFEST_INVALID", err.Error())
		return
	}
	if manifest.Subject != nil {
		canonicalDigest, digestErr := blob.NormalizeDigest(manifest.Subject.Digest)
		if digestErr != nil || canonicalDigest != manifest.Subject.Digest {
			httpx.WriteOCIError(w, http.StatusBadRequest, "MANIFEST_INVALID", "subject must have a canonical sha256 digest")
			return
		}
	}
	if strings.HasPrefix(reference, "sha256:") && reference != staged.Digest {
		message := fmt.Sprintf("expected %s, got %s", reference, staged.Digest)
		httpx.WriteOCIError(w, http.StatusBadRequest, "DIGEST_INVALID", message)
		return
	}
	if _, err := h.manifestDependencyStores(
		r.Context(), repository, imageName, manifest, nil,
	); err != nil {
		code := "MANIFEST_BLOB_UNKNOWN"
		if errors.Is(err, errManifestDescriptorMismatch) {
			code = "MANIFEST_INVALID"
		}
		httpx.WriteOCIError(w, http.StatusBadRequest, code, err.Error())
		return
	}

	mediaType := content.ContentType(r.Header.Get("Content-Type"), "manifest.json")
	if manifest.MediaType != "" {
		mediaType = manifest.MediaType
	}
	subjectDigest := ""
	if manifest.Subject != nil {
		subjectDigest = manifest.Subject.Digest
	}

	asset := domain.Asset{
		Repository:    repository.Name,
		Path:          ocimodel.ManifestPath(imageName, reference),
		Digest:        staged.Digest,
		Size:          staged.Size,
		ContentType:   mediaType,
		Kind:          "oci-manifest",
		Reference:     reference,
		SubjectDigest: subjectDigest,
		Dependencies:  ocimodel.ManifestDependencies(manifest),
	}
	artifactType := manifest.ArtifactType
	if artifactType == "" && manifest.Config != nil {
		artifactType = manifest.Config.MediaType
	}
	asset.Attributes = assetattrs.SetOCIArtifactType(asset.Attributes, artifactType)
	if len(manifest.Annotations) > 0 {
		if asset.Attributes == nil {
			asset.Attributes = make(map[string]any)
		}
		oci, _ := asset.Attributes["oci"].(map[string]any)
		if oci == nil {
			oci = make(map[string]any)
			asset.Attributes["oci"] = oci
		}
		oci["annotations"] = manifest.Annotations
	}
	if provenanceArtifactManifest(manifest) {
		asset.Attributes = assetattrs.SetOCIProvenanceArtifact(asset.Attributes)
	}
	asset.RepositoryID = repository.ID
	provenance, err := h.Runtime.VerifyIncomingAsset(r.Context(), asset, r.Header)
	if err != nil {
		httpx.WriteResult(w, nil, err)
		return
	}
	if provenance != nil {
		value, err := content.ProvenanceAttributes(*provenance)
		if err != nil {
			httpx.WriteResult(w, nil, err)
			return
		}
		asset.Attributes["provenance"] = value
	}
	manifestAssets := []domain.Asset{asset}
	if reference != staged.Digest {
		asset.Path = ocimodel.ManifestPath(imageName, staged.Digest)
		asset.Reference = staged.Digest
		manifestAssets = append(manifestAssets, asset)
	}
	// Pin the write to the identity resolved for this request so a same-name
	// recreate cannot capture the manifest.
	for index := range manifestAssets {
		manifestAssets[index].RepositoryID = repository.ID
	}
	dependencyStores := func(ctx context.Context) ([]string, error) {
		return h.manifestDependencyStores(ctx, repository, imageName, manifest, nil)
	}
	validateDependencies := func(
		ctx context.Context,
		locked map[string]struct{},
	) error {
		_, validationErr := h.manifestDependencyStores(
			ctx, repository, imageName, manifest, locked,
		)
		return validationErr
	}
	_, _, err = h.Runtime.CommitStagedAssetsValidated(
		r.Context(),
		repository.Name,
		[]content.StagedUpload{staged},
		manifestAssets,
		dependencyStores,
		validateDependencies,
	)
	if err != nil {
		if errors.Is(err, errManifestDescriptorMismatch) {
			httpx.WriteOCIError(w, http.StatusBadRequest, "MANIFEST_INVALID", err.Error())
			return
		}
		if errors.Is(err, errManifestDependencyMissing) {
			httpx.WriteOCIError(w, http.StatusBadRequest, "MANIFEST_BLOB_UNKNOWN", err.Error())
			return
		}
		httpx.WriteResult(w, nil, err)
		return
	}
	if err := h.verifyOCIReferrer(
		r.Context(),
		repository,
		imageName,
		manifest,
	); err != nil {
		h.requestLogger(r).Warn(
			"verify OCI referrer",
			"reference",
			reference,
			"error",
			err,
		)
	}
	w.Header().Set("Docker-Content-Digest", staged.Digest)
	if subjectDigest != "" {
		w.Header().Set("OCI-Subject", subjectDigest)
	}
	w.Header().Set(
		"Location",
		ociLocationForRequest(
			r,
			repository.Name,
			ocimodel.ManifestPath(imageName, staged.Digest),
		),
	)
	w.WriteHeader(http.StatusCreated)
}

func (h *Handler) manifestDependencyStores(
	ctx context.Context,
	repository domain.Repository,
	imageName string,
	manifest ocimodel.ManifestEnvelope,
	locked map[string]struct{},
) ([]string, error) {
	stores := make([]string, 0, len(manifest.Layers)+len(manifest.Manifests)+1)
	descriptors := make([]ocimodel.Descriptor, 0, len(manifest.Layers)+1)
	if manifest.Config != nil {
		descriptors = append(descriptors, *manifest.Config)
	}
	descriptors = append(descriptors, manifest.Layers...)

	for _, descriptor := range descriptors {
		if descriptor.Digest == "" {
			continue
		}
		assetPath := ocimodel.BlobPath(imageName, descriptor.Digest)
		asset, err := h.metaFor(repository).Asset(ctx, assetPath)
		if err != nil {
			return nil, fmt.Errorf(
				"%w: referenced blob %s is not present",
				errManifestDependencyMissing,
				descriptor.Digest,
			)
		}
		if asset.Size != descriptor.Size {
			return nil, fmt.Errorf("%w: referenced blob %s has size %d, descriptor declares %d", errManifestDescriptorMismatch, descriptor.Digest, asset.Size, descriptor.Size)
		}
		storeName := asset.BlobStore
		if storeName == "" {
			storeName = repository.BlobStore
			if storeName == "" {
				storeName = "default"
			}
		}
		if locked != nil {
			if _, found := locked[storeName]; !found {
				return nil, content.ErrBlobStoreLeaseSetChanged
			}
		}
		stores = append(stores, storeName)
	}
	for _, descriptor := range manifest.Manifests {
		assetPath := ocimodel.ManifestPath(imageName, descriptor.Digest)
		asset, err := h.metaFor(repository).Asset(ctx, assetPath)
		if err != nil {
			return nil, fmt.Errorf(
				"%w: referenced manifest %s is not present",
				errManifestDependencyMissing,
				descriptor.Digest,
			)
		}
		if asset.Size != descriptor.Size {
			return nil, fmt.Errorf("%w: referenced manifest %s has size %d, descriptor declares %d", errManifestDescriptorMismatch, descriptor.Digest, asset.Size, descriptor.Size)
		}
		storeName := asset.BlobStore
		if storeName == "" {
			storeName = repository.BlobStore
			if storeName == "" {
				storeName = "default"
			}
		}
		if locked != nil {
			if _, found := locked[storeName]; !found {
				return nil, content.ErrBlobStoreLeaseSetChanged
			}
		}
		stores = append(stores, storeName)
	}
	return uniqueSortedStrings(stores), nil
}

func (h *Handler) deleteOCIManifest(
	w http.ResponseWriter,
	r *http.Request,
	repository domain.Repository,
	imageName string,
	reference string,
) {
	if repository.Type != "hosted" {
		httpx.WriteResult(w, nil, domain.ErrReadOnly)
		return
	}

	assetPath := ocimodel.ManifestPath(imageName, reference)
	var deleted []domain.Asset
	if strings.HasPrefix(reference, "sha256:") {
		var err error
		deleted, err = h.metaFor(repository).DeleteOCIManifestByDigest(
			r.Context(),
			assetPath,
		)
		if err != nil {
			httpx.WriteResult(w, nil, err)
			return
		}
	} else {
		removed, err := h.metaFor(repository).DeleteAsset(r.Context(), assetPath)
		if err != nil {
			httpx.WriteResult(w, nil, err)
			return
		}
		deleted = []domain.Asset{removed}
		if _, err := h.meta().DeleteDanglingManifestAliases(
			r.Context(),
			repository.Name,
			deleted,
		); err != nil {
			httpx.WriteResult(w, nil, err)
			return
		}
	}
	for _, asset := range deleted {
		h.Runtime.EnqueueAssetEvent(r.Context(), domain.WebhookAssetDeleted, asset)
	}
	w.WriteHeader(http.StatusAccepted)
}

func (h *Handler) handleOCITags(
	w http.ResponseWriter,
	r *http.Request,
	repository domain.Repository,
	imageName string,
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
	tags, err := h.collectOCITags(r, repository, imageName)
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
	tags, next, more := paginateOCINames(tags, page)
	if more {
		writeOCINextLink(w, r, page, next)
	}

	httpx.WriteJSON(w, http.StatusOK, map[string]any{"name": imageName, "tags": tags})
}

func (h *Handler) handleOCIReferrers(
	w http.ResponseWriter,
	r *http.Request,
	repository domain.Repository,
	imageName string,
	subjectDigest string,
) {
	if r.Method != http.MethodGet {
		httpx.MethodNotAllowed(w, http.MethodGet)
		return
	}
	canonicalDigest, err := blob.NormalizeDigest(subjectDigest)
	if err != nil || canonicalDigest != subjectDigest {
		httpx.WriteOCIError(w, http.StatusBadRequest, "DIGEST_INVALID", "subject must be a canonical sha256 digest")
		return
	}
	artifactType := r.URL.Query().Get("artifactType")
	descriptors, err := h.collectOCIReferrers(
		r,
		repository,
		imageName,
		subjectDigest,
	)
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
	if artifactType != "" {
		descriptors = filterOCIReferrers(descriptors, artifactType)
		w.Header().Set("OCI-Filters-Applied", "artifactType")
	}

	w.Header().Set("Content-Type", IndexMediaType)
	httpx.WriteJSON(w, http.StatusOK, Index{
		SchemaVersion: 2,
		MediaType:     IndexMediaType,
		Manifests:     descriptors,
	})
}

func ociRepositoryLocation(repositoryName, assetPath string) string {
	return "/repository/" + repositoryName + "/" + assetPath
}

func ociLocationForRequest(
	r *http.Request,
	repositoryName string,
	assetPath string,
) string {
	if r.URL.Path == "/v2" || strings.HasPrefix(r.URL.Path, "/v2/") {
		return "/" + assetPath
	}
	return ociRepositoryLocation(repositoryName, assetPath)
}

func ociErrorCode(status int, problemCode string) string {
	return httpx.OCIErrorCode(status, problemCode)
}
