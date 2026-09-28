package content

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"time"

	"github.com/suxen-project/suxen/internal/domain"
	"github.com/suxen-project/suxen/internal/httpx"
	spiformat "github.com/suxen-project/suxen/spi/format"
)

// WireTools returns the capability handle a WireProtocol format uses for one
// repository.
func (rt *Runtime) WireTools(repository domain.Repository) spiformat.WireTools {
	return wireTools{runtime: rt, repository: repository}
}

// WireToolsWithUploadAdmission is used by the HTTP wire dispatcher after it
// reserves a slot for a write action. Read actions receive the ordinary tools
// and reserve a slot only if they publish a cache fill.
func (rt *Runtime) WireToolsWithUploadAdmission(repository domain.Repository) spiformat.WireTools {
	return wireTools{runtime: rt, repository: repository, uploadAdmitted: true}
}

type wireTools struct {
	runtime        *Runtime
	repository     domain.Repository
	uploadAdmitted bool
}

// ReportWireError keeps internal causes in request-correlated server logs.
func (tools wireTools) ReportWireError(ctx context.Context, operation string, err error) {
	log := tools.runtime.Log
	if log == nil {
		log = slog.Default()
	}
	if request := httpx.RequestLogFrom(ctx); request != nil {
		log = log.With("request_id", request.RequestID, "subject", request.Subject)
	}
	log.ErrorContext(ctx, "wire protocol failure",
		"repository", tools.repository.Name,
		"format", tools.repository.Format,
		"operation", operation,
		"error", err,
	)
}

// WireHost is the in-tree extension of the WireTools handle. Core wire
// formats compiled into the server (OCI) reach the full runtime through it;
// plugins only see the public spi/format.WireTools surface.
type WireHost interface {
	Runtime() *Runtime
	Repository() domain.Repository
}

func (tools wireTools) MaxUploadBytes() int64 { return tools.runtime.Config.MaxUploadBytes }

func (tools wireTools) Runtime() *Runtime { return tools.runtime }

func (tools wireTools) Repository() domain.Repository { return tools.repository }

func (tools wireTools) VisitAssetPaths(
	ctx context.Context,
	prefix string,
	visit func(string) (bool, error),
) error {
	return visitAssetPaths(ctx, tools.runtime.metaFor(tools.repository), prefix, visit)
}

func (tools wireTools) StatAsset(
	ctx context.Context,
	assetPath string,
) (spiformat.Asset, bool, error) {
	asset, err := tools.runtime.metaFor(tools.repository).Asset(ctx, assetPath)
	if errors.Is(err, domain.ErrNotFound) {
		return spiformat.Asset{}, false, nil
	}
	if err != nil {
		return spiformat.Asset{}, false, err
	}
	return asset.FormatView(), true, nil
}

func (tools wireTools) OpenAsset(
	ctx context.Context,
	assetPath string,
) (io.ReadCloser, spiformat.Asset, bool, error) {
	asset, err := tools.runtime.metaFor(tools.repository).Asset(ctx, assetPath)
	if errors.Is(err, domain.ErrNotFound) {
		return nil, spiformat.Asset{}, false, nil
	}
	if err != nil {
		return nil, spiformat.Asset{}, false, err
	}
	denial, err := tools.runtime.DownloadAllowed(ctx, asset)
	if err != nil {
		return nil, spiformat.Asset{}, false, err
	}
	if denial != "" {
		return nil, spiformat.Asset{}, false, wireToolError(fmt.Errorf("%w: %s", domain.ErrDownloadDenied, denial))
	}
	return tools.openStoredAsset(ctx, asset)
}

func (tools wireTools) OpenMetadataAsset(
	ctx context.Context,
	assetPath string,
) (io.ReadCloser, spiformat.Asset, bool, error) {
	asset, err := tools.runtime.metaFor(tools.repository).Asset(ctx, assetPath)
	if errors.Is(err, domain.ErrNotFound) {
		return nil, spiformat.Asset{}, false, nil
	}
	if err != nil {
		return nil, spiformat.Asset{}, false, err
	}
	if asset.Kind != "metadata" {
		return nil, spiformat.Asset{}, false, fmt.Errorf("asset %q is not format metadata", assetPath)
	}
	return tools.openStoredAsset(ctx, asset)
}

func (tools wireTools) openStoredAsset(
	ctx context.Context,
	asset domain.Asset,
) (io.ReadCloser, spiformat.Asset, bool, error) {
	reader, _, err := tools.runtime.OpenStoredAsset(ctx, asset)
	if err != nil {
		return nil, spiformat.Asset{}, false, fmt.Errorf("open blob: %w", err)
	}
	return reader, asset.FormatView(), true, nil
}

func (tools wireTools) StoreAsset(
	ctx context.Context,
	assetPath string,
	contentType string,
	content io.Reader,
) (spiformat.Asset, error) {
	return tools.storeAsset(ctx, assetPath, contentType, "raw", content)
}

func (tools wireTools) StoreMetadataAsset(
	ctx context.Context,
	assetPath string,
	contentType string,
	content io.Reader,
) (spiformat.Asset, error) {
	stored, err := tools.StoreAssets(ctx, []spiformat.AssetInput{{
		Path: assetPath, ContentType: contentType, Content: content,
		Metadata: true, Replace: true,
	}})
	if err != nil {
		return spiformat.Asset{}, err
	}
	return stored[0], nil
}

func (tools wireTools) storeAsset(
	ctx context.Context,
	assetPath string,
	contentType string,
	kind string,
	content io.Reader,
) (spiformat.Asset, error) {
	stored, err := tools.StoreAssets(ctx, []spiformat.AssetInput{{
		Path:        assetPath,
		ContentType: contentType,
		Content:     content,
		Metadata:    kind == "metadata",
	}})
	if err != nil {
		return spiformat.Asset{}, err
	}
	return stored[0], nil
}

func (tools wireTools) StoreAssets(
	ctx context.Context,
	inputs []spiformat.AssetInput,
) ([]spiformat.Asset, error) {
	if !tools.uploadAdmitted {
		release, err := tools.runtime.AcquireUpload(ctx)
		if err != nil {
			return nil, wireToolError(err)
		}
		defer release()
	}
	stagedUploads := make([]StagedUpload, 0, len(inputs))
	defer func() {
		for _, staged := range stagedUploads {
			staged.remove()
		}
	}()
	incomingAssets := make([]domain.Asset, 0, len(inputs))
	for _, input := range inputs {
		staged, err := tools.runtime.stageUpload(nil, input.Content)
		if err != nil {
			return nil, wireToolError(err)
		}
		stagedUploads = append(stagedUploads, staged)
		kind := "raw"
		if input.Metadata {
			kind = "metadata"
		}
		incoming := domain.Asset{
			Repository: tools.repository.Name, Path: input.Path, Digest: staged.Digest,
			RepositoryID: tools.repository.ID,
			Size:         staged.Size, ContentType: input.ContentType, Kind: kind,
			Immutable:         !input.Replace,
			ImmutableIdentity: input.ImmutableIdentity,
		}
		if !input.Metadata {
			provenance, err := tools.runtime.VerifyIncomingAsset(ctx, incoming, http.Header{})
			if err != nil {
				return nil, wireToolError(err)
			}
			if provenance != nil {
				value, err := resultMap(*provenance)
				if err != nil {
					return nil, err
				}
				incoming.Attributes = map[string]any{"provenance": value}
			}
		}
		incomingAssets = append(incomingAssets, incoming)
	}
	// Pin the writes to the identity resolved for this request so a same-name
	// recreate cannot capture the published assets.
	for index := range incomingAssets {
		incomingAssets[index].RepositoryID = tools.repository.ID
	}
	_, assets, err := tools.runtime.CommitStagedAssets(
		ctx, tools.repository.Name, stagedUploads, incomingAssets,
	)
	if err != nil {
		return nil, wireToolError(err)
	}
	result := make([]spiformat.Asset, 0, len(assets))
	for _, asset := range assets {
		result = append(result, asset.FormatView())
	}
	return result, nil
}

func wireToolError(err error) error {
	if err == nil {
		return nil
	}
	var policy *spiformat.PolicyViolation
	var size *http.MaxBytesError
	switch {
	case errors.Is(err, domain.ErrConflict):
		return fmt.Errorf("%w: %w", spiformat.ErrConflict, err)
	case errors.Is(err, domain.ErrProvenanceRejected),
		errors.Is(err, domain.ErrDownloadDenied), errors.As(err, &policy):
		return fmt.Errorf("%w: %w", spiformat.ErrPolicyRejected, err)
	case errors.Is(err, domain.ErrUploadSessionSizeExceeded), errors.As(err, &size):
		return fmt.Errorf("%w: %w", spiformat.ErrUploadLimit, err)
	default:
		return err
	}
}

func (tools wireTools) ProxyTTL() time.Duration {
	return tools.runtime.Config.ProxyManifestTTL
}

func (tools wireTools) Upstream(
	ctx context.Context,
	method string,
	upstreamPath string,
	header http.Header,
	body io.Reader,
) (*http.Response, error) {
	if tools.repository.Type != "proxy" {
		return nil, fmt.Errorf("repository %q is not a proxy", tools.repository.Name)
	}
	requestURL, credentials, err := proxyURLWithQuery(tools.repository.Upstream, upstreamPath)
	if err != nil {
		return nil, err
	}
	request, err := http.NewRequestWithContext(ctx, method, requestURL, body)
	if err != nil {
		return nil, err
	}
	for name, values := range header {
		for _, value := range values {
			request.Header.Add(name, value)
		}
	}
	request.Header.Set("User-Agent", tools.runtime.UserAgent)
	if credentials.Username != "" {
		request.SetBasicAuth(credentials.Username, credentials.Password)
	}
	response, err := tools.runtime.client().Do(request)
	if err != nil {
		return nil, redactTransportError(err, request.URL.String())
	}
	return response, nil
}
