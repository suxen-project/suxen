package server

import (
	"errors"
	"fmt"
	"net/http"
	"path"
	"strings"

	"github.com/suxen-project/suxen/internal/content"
	"github.com/suxen-project/suxen/internal/domain"
	"github.com/suxen-project/suxen/internal/httpx"
	"github.com/suxen-project/suxen/internal/rawpath"
	"github.com/suxen-project/suxen/internal/retention"
	spiformat "github.com/suxen-project/suxen/spi/format"
)

func (s *Server) handleRepository(w http.ResponseWriter, r *http.Request) {
	repositoryPath := strings.TrimPrefix(r.URL.Path, "/repository/")
	repositoryName, assetPath, found := strings.Cut(repositoryPath, "/")
	if !found || repositoryName == "" {
		httpx.WriteProblem(
			w,
			http.StatusNotFound,
			"not_found",
			"repository path requires a repository name and an asset path",
		)
		return
	}

	repository, err := s.repositoryReads().Repository(r.Context(), repositoryName)
	if err != nil {
		httpx.WriteResult(w, nil, err)
		return
	}
	httpx.SetRequestMetricLabels(w, repository.Name, repository.Format)
	// Formats with their own wire protocol (OCI, git) claim their paths
	// first; everything else rides the generic asset pipeline below.
	if s.serveWireProtocol(w, r, repository, assetPath) {
		return
	}
	action, supported := repositoryAction(r.Method)
	if !supported {
		httpx.MethodNotAllowed(
			w,
			http.MethodGet,
			http.MethodHead,
			http.MethodPost,
			http.MethodPatch,
			http.MethodPut,
			http.MethodDelete,
		)
		return
	}
	if !s.identity.RequireRepositoryPrivilege(w, r, repository.Name, action) {
		return
	}
	if action == "write" {
		release, err := s.content.AcquireUpload(r.Context())
		if err != nil {
			httpx.WriteResult(w, nil, err)
			return
		}
		defer release()
	}
	s.handleRaw(w, r, repository, assetPath)
}

// wireResponsePreparer is implemented by core wire formats that must shape
// host-written responses (an OCI error envelope on a 401) before the
// privilege check runs. It is not part of the public SPI.
type wireResponsePreparer interface {
	PrepareWireResponse(w http.ResponseWriter, requestPath string)
}

// wireProtocol finds the wire protocol serving a format: a registered plugin
// first, then the core formats compiled into the server.
func (s *Server) wireProtocol(formatName string) (spiformat.WireProtocol, bool) {
	if registered, found := spiformat.Lookup(formatName); found {
		wire, ok := registered.(spiformat.WireProtocol)
		return wire, ok
	}
	wire, ok := s.coreWire[formatName]
	return wire, ok
}

// serveWireProtocol dispatches a request to the repository format's own wire
// protocol when the format claims the path. The host checks the privilege the
// format declares before handing over the request. It reports whether the
// request was handled.
func (s *Server) serveWireProtocol(
	w http.ResponseWriter,
	r *http.Request,
	repository domain.Repository,
	requestPath string,
) bool {
	wire, ok := s.wireProtocol(repository.Format)
	if !ok {
		return false
	}
	view := repository.FormatView()
	action, claimed := wire.WireAction(view, r.Method, requestPath, r.URL.Query())
	if !claimed {
		return false
	}
	if preparer, ok := wire.(wireResponsePreparer); ok {
		preparer.PrepareWireResponse(w, requestPath)
	}
	switch action {
	case "":
	case "read", "write", "delete":
		if !s.identity.RequireRepositoryPrivilege(w, r, repository.Name, action) {
			return true
		}
	default:
		httpx.WriteServerProblem(
			w,
			http.StatusInternalServerError,
			"wire_action_invalid",
			"format declared an unknown repository action",
			fmt.Errorf("format %q declared action %q", repository.Format, action),
		)
		return true
	}
	tools := s.content.WireTools(repository)
	if action == "write" {
		release, err := s.content.AcquireUpload(r.Context())
		if err != nil {
			httpx.WriteResult(w, nil, err)
			return true
		}
		defer release()
		tools = s.content.WireToolsWithUploadAdmission(repository)
	}
	wire.ServeWire(w, r, view, requestPath, tools)
	return true
}

// validateFormatUpload lets a registered format plugin reject a hosted upload
// by path before the request body is read. Core formats accept every path.
func validateFormatUpload(repository domain.Repository, assetPath string) error {
	registered, found := spiformat.Lookup(repository.Format)
	if !found {
		return nil
	}
	policy, ok := registered.(spiformat.UploadPolicy)
	if !ok {
		return nil
	}
	return policy.ValidateUpload(repository.FormatView(), assetPath)
}

func repositoryAction(method string) (string, bool) {
	switch method {
	case http.MethodGet, http.MethodHead:
		return "read", true
	case http.MethodPost, http.MethodPatch, http.MethodPut:
		return "write", true
	case http.MethodDelete:
		return "delete", true
	default:
		return "", false
	}
}

func (s *Server) handleRaw(
	w http.ResponseWriter,
	r *http.Request,
	repository domain.Repository,
	assetPath string,
) {
	if !domain.ValidAssetPath(assetPath) {
		httpx.WriteProblem(w, http.StatusBadRequest, "invalid_path", domain.ErrInvalidAssetPath.Error())
		return
	}
	if repository.Format == "raw" {
		if err := rawpath.Validate(assetPath); err != nil {
			httpx.WriteProblem(w, http.StatusBadRequest, "invalid_path", err.Error())
			return
		}
	}
	cleanPath := strings.TrimPrefix(path.Clean("/"+assetPath), "/")
	if cleanPath == "" || cleanPath == "." {
		httpx.WriteProblem(w, http.StatusBadRequest, "invalid_path", "asset path is required")
		return
	}

	switch r.Method {
	case http.MethodGet, http.MethodHead:
		s.readRaw(w, r, repository, cleanPath)
	case http.MethodPut:
		s.uploadRaw(w, r, repository, cleanPath)
	case http.MethodDelete:
		s.deleteRaw(w, r, repository, cleanPath)
	default:
		httpx.MethodNotAllowed(
			w,
			http.MethodGet,
			http.MethodHead,
			http.MethodPut,
			http.MethodDelete,
		)
	}
}

func (s *Server) uploadRaw(
	w http.ResponseWriter,
	r *http.Request,
	repository domain.Repository,
	assetPath string,
) {
	if repository.Type != "hosted" {
		httpx.WriteResult(w, nil, domain.ErrReadOnly)
		return
	}
	if err := validateFormatUpload(repository, assetPath); err != nil {
		httpx.WriteResult(w, nil, err)
		return
	}
	// Raw paths were validated in handleRaw. Other formats keep their own path
	// rules while Location still escapes reserved URL characters.
	location := rawpath.EscapeURLPath(repository.Name, assetPath)

	staged, err := s.content.StageUpload(w, r.Body)
	if err != nil {
		switch {
		case content.IsUploadTooLarge(err):
			httpx.WriteProblem(w, http.StatusRequestEntityTooLarge, "upload_failed", "upload exceeds the size limit")
		case errors.Is(err, content.ErrUploadSourceRead):
			httpx.WriteProblem(w, http.StatusBadRequest, "upload_failed", "could not read upload body")
		default:
			httpx.WriteServerProblem(w, http.StatusInternalServerError, "upload_failed", "upload failed", err)
		}
		return
	}
	defer staged.Remove()

	if err := content.VerifyRequestedDigest(r.Header.Get("Digest"), staged.Digest); err != nil {
		httpx.WriteResult(w, nil, err)
		return
	}

	incomingAsset := domain.Asset{
		Repository:   repository.Name,
		RepositoryID: repository.ID,
		Path:         assetPath,
		Digest:       staged.Digest,
		Size:         staged.Size,
		ContentType:  content.ContentType(r.Header.Get("Content-Type"), assetPath),
		Kind:         "raw",
	}
	provenance, err := s.content.VerifyIncomingAsset(r.Context(), incomingAsset, r.Header)
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
		incomingAsset.Attributes = map[string]any{"provenance": value}
	}
	// Pin the write to the identity resolved for this request so a same-name
	// recreate cannot capture the uploaded asset.
	incomingAsset.RepositoryID = repository.ID
	blobInfos, assets, err := s.content.CommitStagedAssets(
		r.Context(), repository.Name, []content.StagedUpload{staged}, []domain.Asset{incomingAsset},
	)
	if err != nil {
		httpx.WriteResult(w, nil, err)
		return
	}
	blobInfo := blobInfos[0]
	asset := assets[0]
	w.Header().Set("Docker-Content-Digest", blobInfo.Digest)
	w.Header().Set("ETag", content.QuoteETag(blobInfo.Digest))
	w.Header().Set("Location", location)
	httpx.WriteJSON(w, http.StatusCreated, content.ProjectAsset(asset, repository))
}

func (s *Server) deleteRaw(
	w http.ResponseWriter,
	r *http.Request,
	repository domain.Repository,
	assetPath string,
) {
	if repository.Type != "hosted" {
		httpx.WriteResult(w, nil, domain.ErrReadOnly)
		return
	}

	// Remove the artifact's declared companion metadata in the same transaction so
	// an interactive delete cannot orphan a companion record and leak its blob the
	// way policy cleanup already avoids. Fail closed on an invalid declaration.
	companionPaths, ok := s.declaredCompanionPaths(repository, retention.FormatGrouping(repository.Format), assetPath)
	if !ok {
		httpx.WriteProblem(w, http.StatusInternalServerError, "invalid_companion_paths",
			"the repository format declared invalid companion paths for this asset")
		return
	}
	deleted, err := s.metadata.ForRepository(repository).DeleteAssetWithCompanions(r.Context(), assetPath, companionPaths)
	if err != nil {
		httpx.WriteResult(w, nil, err)
		return
	}
	s.content.EnqueueAssetEvent(r.Context(), domain.WebhookAssetDeleted, deleted)
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) readRaw(
	w http.ResponseWriter,
	r *http.Request,
	repository domain.Repository,
	assetPath string,
) {
	if repository.Type == "group" &&
		s.content.ServeMergedGroupContent(w, r, repository, assetPath) {
		return
	}
	asset, found, err := s.content.ResolveAsset(w, r, repository, assetPath)
	if err != nil {
		content.WriteAssetResolutionError(w, err)
		return
	}
	if !found {
		if repository.Type == "hosted" &&
			s.content.ServeSynthesizedHostedContent(w, r, repository, assetPath) {
			return
		}
		httpx.WriteProblem(w, http.StatusNotFound, "not_found", "asset not found")
		return
	}
	if s.content.ServeRewrittenProxyIndex(w, r, repository, asset) {
		return
	}
	s.content.ServeAsset(w, r, asset)
}
