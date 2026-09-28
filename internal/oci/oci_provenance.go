package oci

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strings"

	"github.com/suxen-project/suxen/internal/assetattrs"
	"github.com/suxen-project/suxen/internal/content"
	"github.com/suxen-project/suxen/internal/domain"
	"github.com/suxen-project/suxen/internal/ocimodel"
)

const (
	cosignSignatureArtifactType   = "application/vnd.dev.cosign.artifact.sig.v1+json"
	cosignAttestationArtifactType = "application/vnd.dev.cosign.artifact.attestation.v1+json"
	cosignSimpleSigningMediaType  = "application/vnd.dev.cosign.simplesigning.v1+json"
	cosignSignatureAnnotation     = "dev.cosignproject.cosign/signature"
	cosignCertificateAnnotation   = "dev.sigstore.cosign/certificate"
	cosignChainAnnotation         = "dev.sigstore.cosign/chain"
	dsseEnvelopeMediaType         = "application/vnd.dsse.envelope.v1+json"
	maximumProvenancePayload      = 4 << 20
)

// provenanceArtifactManifest recognizes only the narrow OCI referrer shapes
// Suxen knows how to verify. A subject descriptor or artifactType declaration
// by itself must never bypass repository provenance policy.
func provenanceArtifactManifest(manifest ocimodel.ManifestEnvelope) bool {
	if manifest.Subject == nil || manifest.Subject.Digest == "" || len(manifest.Layers) != 1 {
		return false
	}
	artifactType := manifest.ArtifactType
	// Cosign's OCI 1.1 encoding places its artifact type on the config
	// descriptor rather than the top-level artifactType field. Accept either
	// standards-compatible representation, but still require the matching
	// signature layer shape below.
	if artifactType == "" && manifest.Config != nil {
		artifactType = manifest.Config.MediaType
	}
	layer := manifest.Layers[0]
	switch artifactType {
	case cosignSignatureArtifactType:
		return layer.MediaType == cosignSimpleSigningMediaType &&
			strings.TrimSpace(layer.Annotations[cosignSignatureAnnotation]) != ""
	case cosignAttestationArtifactType:
		return layer.MediaType == dsseEnvelopeMediaType
	default:
		return false
	}
}

func (h *Handler) verifyOCIReferrer(
	ctx context.Context,
	repository domain.Repository,
	imageName string,
	manifest ocimodel.ManifestEnvelope,
) error {
	if !provenanceArtifactManifest(manifest) {
		return nil
	}

	request, recognized, err := h.verificationRequestFromOCIReferrer(
		ctx,
		repository,
		imageName,
		manifest,
	)
	if err != nil {
		return err
	}
	if !recognized {
		return nil
	}

	policy, err := h.metaFor(repository).EffectiveTrustPolicy(ctx)
	if errors.Is(err, domain.ErrNotFound) {
		return nil
	}
	if err != nil {
		return err
	}
	targetPath := ocimodel.ManifestPath(imageName, manifest.Subject.Digest)
	target, err := h.metaFor(repository).Asset(ctx, targetPath)
	if errors.Is(err, domain.ErrNotFound) {
		return nil
	}
	if err != nil {
		return err
	}

	result := content.VerifyProvenance(policy, target, request)
	h.Runtime.RecordProvenanceMetric(result)
	if result.Status != "passed" {
		return nil
	}
	aliases, err := h.metaFor(repository).Assets(
		ctx,
		"v2/"+imageName+"/manifests/",
	)
	if err != nil {
		return err
	}
	for _, alias := range aliases {
		if alias.Digest != target.Digest ||
			assetattrs.OCIProvenanceArtifact(alias.Attributes) {
			continue
		}
		if err := h.Runtime.RecordProvenance(ctx, alias, result); err != nil {
			return err
		}
	}
	return nil
}

func (h *Handler) verificationRequestFromOCIReferrer(
	ctx context.Context,
	repository domain.Repository,
	imageName string,
	manifest ocimodel.ManifestEnvelope,
) (domain.VerificationRequest, bool, error) {
	if !provenanceArtifactManifest(manifest) {
		return domain.VerificationRequest{}, false, nil
	}
	for _, layer := range manifest.Layers {
		signature := layer.Annotations[cosignSignatureAnnotation]
		if layer.MediaType != dsseEnvelopeMediaType && signature == "" {
			continue
		}
		payload, err := h.readProvenanceBlob(ctx, repository, imageName, layer.Digest)
		if err != nil {
			return domain.VerificationRequest{}, false, err
		}
		if layer.MediaType == dsseEnvelopeMediaType {
			var envelope domain.DSSEEnvelope
			if err := json.Unmarshal(payload, &envelope); err != nil {
				return domain.VerificationRequest{}, false, fmt.Errorf(
					"decode OCI DSSE envelope: %w",
					err,
				)
			}
			return domain.VerificationRequest{DSSE: &envelope}, true, nil
		}

		return domain.VerificationRequest{
			Signature:   signature,
			Payload:     base64.StdEncoding.EncodeToString(payload),
			Certificate: layer.Annotations[cosignCertificateAnnotation],
			CertificateChain: certificateChainFromAnnotation(
				layer.Annotations[cosignChainAnnotation],
			),
		}, true, nil
	}
	return domain.VerificationRequest{}, false, nil
}

func (h *Handler) readProvenanceBlob(
	ctx context.Context,
	repository domain.Repository,
	imageName string,
	digest string,
) ([]byte, error) {
	asset, err := h.metaFor(repository).Asset(ctx, ocimodel.BlobPath(imageName, digest))
	if err != nil {
		return nil, err
	}
	reader, _, err := h.Runtime.OpenStoredAsset(ctx, asset)
	if err != nil {
		return nil, err
	}
	defer reader.Close()

	payload, err := io.ReadAll(io.LimitReader(reader, maximumProvenancePayload+1))
	if err != nil {
		return nil, fmt.Errorf("read provenance payload %s: %w", digest, err)
	}
	if len(payload) > maximumProvenancePayload {
		return nil, errors.New("provenance payload exceeds 4 MiB")
	}
	return payload, nil
}

func certificateChainFromAnnotation(encoded string) []string {
	if strings.TrimSpace(encoded) == "" {
		return nil
	}
	return []string{encoded}
}
