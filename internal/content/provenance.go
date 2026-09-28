package content

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"time"

	"github.com/suxen-project/suxen/internal/assetattrs"
	"github.com/suxen-project/suxen/internal/domain"
	"github.com/suxen-project/suxen/internal/trustmaterial"
)

func ValidateTrustPolicyMaterial(policy domain.TrustPolicy) error {
	if err := policy.Validate(); err != nil {
		return err
	}
	return trustmaterial.Validate(policy)
}

func (rt *Runtime) effectiveTrustPolicyForAsset(ctx context.Context, asset domain.Asset) (domain.TrustPolicy, error) {
	if asset.RepositoryID != "" {
		return rt.metaFor(domain.Repository{Name: asset.Repository, ID: asset.RepositoryID}).EffectiveTrustPolicy(ctx)
	}
	return rt.policyReader().EffectiveTrustPolicy(ctx, asset.Repository)
}

// ValidateTrustPolicyDefaultsMaterial validates the instance-wide trust-policy
// default, which carries no repository, then its PEM trust material.
func ValidateTrustPolicyDefaultsMaterial(policy domain.TrustPolicy) error {
	if err := policy.ValidateDefaults(); err != nil {
		return err
	}
	return trustmaterial.Validate(policy)
}

func (rt *Runtime) VerifyIncomingAsset(
	ctx context.Context,
	asset domain.Asset,
	header http.Header,
) (*domain.ProvenanceResult, error) {
	if !provenanceBearingAsset(asset) {
		return nil, nil
	}
	policy, err := rt.effectiveTrustPolicyForAsset(ctx, asset)
	if errors.Is(err, domain.ErrNotFound) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	request := domain.VerificationRequest{
		Signature: header.Get("X-Suxen-Signature"),
	}
	if encodedCertificate := header.Get("X-Suxen-Certificate"); encodedCertificate != "" {
		certificate, err := decodeBase64(encodedCertificate)
		if err != nil {
			return nil, fmt.Errorf(
				"%w: decode X-Suxen-Certificate: %v",
				domain.ErrProvenanceRejected,
				err,
			)
		}
		request.Certificate = string(certificate)
	}
	result := verifyProvenance(policy, asset, request)
	rt.RecordProvenanceMetric(result)
	if policy.Mode == "verify-on-push" && result.Status != "passed" {
		return &result, fmt.Errorf("%w: %s", domain.ErrProvenanceRejected, result.Reason)
	}
	return &result, nil
}

func (rt *Runtime) RecordProvenanceMetric(result domain.ProvenanceResult) {
	rt.observeProvenance(result.Status == "passed")
}

func (rt *Runtime) RecordProvenance(
	ctx context.Context,
	asset domain.Asset,
	result domain.ProvenanceResult,
) error {
	if result.Digest != asset.Digest {
		return domain.ErrConflict
	}
	policy, err := rt.effectiveTrustPolicyForAsset(ctx, asset)
	if err != nil {
		return err
	}
	if !policy.UpdatedAt.Equal(result.PolicyAt) {
		return domain.ErrConflict
	}
	value, err := resultMap(result)
	if err != nil {
		return err
	}
	return rt.metaFor(domain.Repository{Name: asset.Repository, ID: asset.RepositoryID}).SetAttributes(
		ctx,
		asset.ID,
		assetattrs.ProvenanceNamespace,
		value,
	)
}

func (rt *Runtime) FinalizeIncomingProvenance(
	ctx context.Context,
	asset domain.Asset,
	result *domain.ProvenanceResult,
) (domain.Asset, error) {
	if result == nil {
		return asset, nil
	}
	if err := rt.RecordProvenance(ctx, asset, *result); err != nil {
		return asset, err
	}
	return rt.metaFor(domain.Repository{Name: asset.Repository, ID: asset.RepositoryID}).AssetByID(ctx, asset.ID)
}

func provenanceBearingAsset(asset domain.Asset) bool {
	if asset.Kind == "raw" {
		return true
	}
	if asset.Kind != "oci-manifest" {
		return false
	}
	// A subject descriptor and artifactType are both publisher-controlled. The
	// OCI handler grants the exemption only after validating a recognized
	// signature/attestation referrer shape and records that decision in the
	// protected OCI namespace.
	return asset.SubjectDigest == "" || !assetattrs.OCIProvenanceArtifact(asset.Attributes)
}

func (rt *Runtime) provenanceDownloadAllowed(
	ctx context.Context,
	asset domain.Asset,
) (bool, error) {
	if !provenanceBearingAsset(asset) {
		return true, nil
	}
	policy, err := rt.effectiveTrustPolicyForAsset(ctx, asset)
	if errors.Is(err, domain.ErrNotFound) {
		return true, nil
	}
	if err != nil {
		return false, err
	}
	if policy.Mode != "verify-on-pull" {
		return true, nil
	}
	repository, err := rt.repositoryResolver().Repository(ctx, asset.Repository)
	if err != nil {
		return false, err
	}
	attributes := assetattrs.Project(asset, repository)
	status, found := assetattrs.Lookup(
		attributes,
		assetattrs.ProvenanceNamespace+".status",
	)
	if !found || status != "passed" {
		return false, nil
	}
	verifiedDigest, found := assetattrs.Lookup(
		attributes,
		assetattrs.ProvenanceNamespace+".digest",
	)
	if !found || verifiedDigest != asset.Digest {
		return false, nil
	}
	policyAtValue, found := assetattrs.Lookup(
		attributes,
		assetattrs.ProvenanceNamespace+".policyUpdatedAt",
	)
	if !found {
		return false, nil
	}
	policyAt, err := provenanceVerificationTime(policyAtValue)
	if err != nil || !policyAt.Equal(policy.UpdatedAt) {
		return false, nil
	}
	verifiedAtValue, found := assetattrs.Lookup(
		attributes,
		assetattrs.ProvenanceNamespace+".verifiedAt",
	)
	if !found {
		return false, nil
	}
	verifiedAt, err := provenanceVerificationTime(verifiedAtValue)
	if err != nil {
		return false, nil
	}
	return !verifiedAt.Before(policy.UpdatedAt), nil
}

func provenanceVerificationTime(value any) (time.Time, error) {
	switch typed := value.(type) {
	case time.Time:
		return typed, nil
	case string:
		return time.Parse(time.RFC3339Nano, typed)
	default:
		return time.Time{}, errors.New("provenance verification time has an invalid type")
	}
}

func resultMap(result any) (map[string]any, error) {
	encoded, err := json.Marshal(result)
	if err != nil {
		return nil, err
	}
	value := make(map[string]any)
	if err := json.Unmarshal(encoded, &value); err != nil {
		return nil, err
	}
	return value, nil
}

// ProvenanceAttributes converts a verified result to its persisted namespace.
func ProvenanceAttributes(result domain.ProvenanceResult) (map[string]any, error) {
	return resultMap(result)
}
