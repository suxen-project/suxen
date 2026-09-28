package server

import (
	"bytes"
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/sha256"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/base64"
	"encoding/json"
	"encoding/pem"
	"fmt"
	"io"
	"math/big"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/suxen-project/suxen/internal/assetattrs"
	"github.com/suxen-project/suxen/internal/content"
	"github.com/suxen-project/suxen/internal/domain"
)

func TestVerifyOnPushAndCosignPayloadVerification(t *testing.T) {
	t.Parallel()
	fixture := newServerFixture(t)
	privateKey, publicKeyPEM := provenanceTestKey(t)
	ctx := context.Background()
	policy := domain.TrustPolicy{
		Repository: "raw",
		Mode:       "verify-on-push",
		PublicKeys: []string{publicKeyPEM},
	}
	if err := fixture.Metadata.SetTrustPolicy(ctx, policy); err != nil {
		t.Fatal(err)
	}

	content := []byte("signed release content")
	unsigned := fixture.request(
		t,
		http.MethodPut,
		"/repository/raw/signed.bin",
		content,
		true,
	)
	assertStatus(t, unsigned, http.StatusForbidden)
	unsigned.Body.Close()

	digest := testDigest(content)
	signature := signProvenancePayload(t, privateKey, []byte(digest))
	signedUpload := provenanceUploadRequest(
		t,
		fixture.Handler,
		"/repository/raw/signed.bin",
		content,
		signature,
	)
	assertStatus(t, signedUpload, http.StatusCreated)
	signedUpload.Body.Close()
	asset, err := fixture.Metadata.Asset(ctx, "raw", "signed.bin")
	if err != nil {
		t.Fatal(err)
	}
	status, found := assetattrs.Lookup(asset.Attributes, "provenance.status")
	if !found || status != "passed" {
		t.Fatalf("signed upload provenance: %+v", asset.Attributes)
	}

	policy.Mode = "verify-on-pull"
	if err := fixture.Metadata.SetTrustPolicy(ctx, policy); err != nil {
		t.Fatal(err)
	}
	replacement := []byte("replacement awaiting cosign verification")
	accepted := fixture.request(
		t,
		http.MethodPut,
		"/repository/raw/signed.bin",
		replacement,
		true,
	)
	assertStatus(t, accepted, http.StatusCreated)
	accepted.Body.Close()
	blocked := fixture.request(
		t,
		http.MethodGet,
		"/repository/raw/signed.bin",
		nil,
		true,
	)
	assertStatus(t, blocked, http.StatusForbidden)
	blocked.Body.Close()

	asset, err = fixture.Metadata.Asset(ctx, "raw", "signed.bin")
	if err != nil {
		t.Fatal(err)
	}
	cosignPayload := []byte(fmt.Sprintf(`{
		"critical":{
			"identity":{"docker-reference":"example/signed"},
			"image":{"Docker-manifest-digest":%q},
			"type":"cosign container image signature"
		}
	}`, asset.Digest))
	verification := domain.VerificationRequest{
		Signature: base64.StdEncoding.EncodeToString(
			signProvenancePayload(t, privateKey, cosignPayload),
		),
		Payload: base64.StdEncoding.EncodeToString(cosignPayload),
	}
	verificationBody, err := json.Marshal(verification)
	if err != nil {
		t.Fatal(err)
	}
	verified := fixture.requestWithBearer(
		t,
		http.MethodPost,
		fmt.Sprintf(
			"/api/v1/repositories/raw/assets/%d/verification",
			asset.ID,
		),
		verificationBody,
		"application/json",
		testToken,
	)
	assertStatus(t, verified, http.StatusOK)
	verified.Body.Close()

	released := fixture.request(
		t,
		http.MethodGet,
		"/repository/raw/signed.bin",
		nil,
		true,
	)
	assertStatus(t, released, http.StatusOK)
	assertBody(t, released, replacement)
}

func TestRawVerificationUsesInheritedTrustPolicy(t *testing.T) {
	t.Parallel()
	fixture := newServerFixture(t)
	privateKey, publicKeyPEM := provenanceTestKey(t)
	ctx := context.Background()
	if err := fixture.Metadata.SetTrustPolicyDefaults(ctx, domain.TrustPolicy{
		Mode: "verify-on-pull", PublicKeys: []string{publicKeyPEM},
	}); err != nil {
		t.Fatal(err)
	}

	payload := []byte("inherited policy payload")
	upload := fixture.request(
		t, http.MethodPut, "/repository/raw/inherited.bin", payload, true,
	)
	assertStatus(t, upload, http.StatusCreated)
	upload.Body.Close()
	asset, err := fixture.Metadata.Asset(ctx, "raw", "inherited.bin")
	if err != nil {
		t.Fatal(err)
	}
	requestBody, err := json.Marshal(domain.VerificationRequest{
		Signature: base64.StdEncoding.EncodeToString(
			signProvenancePayload(t, privateKey, []byte(asset.Digest)),
		),
	})
	if err != nil {
		t.Fatal(err)
	}
	verified := fixture.requestWithBearer(
		t, http.MethodPost,
		fmt.Sprintf("/api/v1/repositories/raw/assets/%d/verification", asset.ID),
		requestBody, "application/json", testToken,
	)
	assertStatus(t, verified, http.StatusOK)
	verified.Body.Close()

	download := fixture.request(
		t, http.MethodGet, "/repository/raw/inherited.bin", nil, true,
	)
	assertStatus(t, download, http.StatusOK)
	assertBody(t, download, payload)
}

func TestGroupDownloadsEnforceMemberTrustPolicy(t *testing.T) {
	t.Parallel()
	for _, repositoryType := range []string{"hosted", "proxy"} {
		for _, inherited := range []bool{false, true} {
			t.Run(fmt.Sprintf("%s/inherited=%t", repositoryType, inherited), func(t *testing.T) {
				fixture := newServerFixture(t)
				ctx := context.Background()
				privateKey, publicKey := provenanceTestKey(t)
				payload := []byte("artifact served through a group")
				member := domain.Repository{Name: "member", Format: "raw", Type: repositoryType}
				if repositoryType == "proxy" {
					fixture.Handler.setHTTPClient(&http.Client{
						Transport: roundTripFunc(func(request *http.Request) (*http.Response, error) {
							return &http.Response{
								StatusCode: http.StatusOK, Header: make(http.Header),
								Body: io.NopCloser(bytes.NewReader(payload)), Request: request,
							}, nil
						}),
					})
					member.Upstream = "https://upstream.example/artifacts"
				}
				if err := fixture.Metadata.CreateRepository(ctx, member); err != nil {
					t.Fatal(err)
				}
				if err := fixture.Metadata.CreateRepository(ctx, domain.Repository{
					Name: "group", Format: "raw", Type: "group", Members: []string{"member"},
				}); err != nil {
					t.Fatal(err)
				}
				if repositoryType == "hosted" {
					upload := fixture.request(t, http.MethodPut, "/repository/member/artifact.bin", payload, true)
					assertStatus(t, upload, http.StatusCreated)
					upload.Body.Close()
				}
				initial := fixture.request(t, http.MethodGet, "/repository/group/artifact.bin", nil, true)
				assertStatus(t, initial, http.StatusOK)
				assertBody(t, initial, payload)
				policy := domain.TrustPolicy{Mode: "verify-on-pull", PublicKeys: []string{publicKey}}
				if inherited {
					if err := fixture.Metadata.SetTrustPolicyDefaults(ctx, policy); err != nil {
						t.Fatal(err)
					}
				} else {
					policy.Repository = "member"
					if err := fixture.Metadata.SetTrustPolicy(ctx, policy); err != nil {
						t.Fatal(err)
					}
				}
				for _, repository := range []string{"member", "group"} {
					blocked := fixture.request(t, http.MethodGet, "/repository/"+repository+"/artifact.bin", nil, true)
					assertStatus(t, blocked, http.StatusForbidden)
					blocked.Body.Close()
				}
				asset, err := fixture.Metadata.Asset(ctx, "member", "artifact.bin")
				if err != nil {
					t.Fatal(err)
				}
				body, err := json.Marshal(domain.VerificationRequest{
					Signature: base64.StdEncoding.EncodeToString(signProvenancePayload(t, privateKey, []byte(asset.Digest))),
				})
				if err != nil {
					t.Fatal(err)
				}
				verified := fixture.requestWithBearer(t, http.MethodPost,
					fmt.Sprintf("/api/v1/repositories/member/assets/%d/verification", asset.ID),
					body, "application/json", testToken)
				assertStatus(t, verified, http.StatusOK)
				verified.Body.Close()
				for _, repository := range []string{"member", "group"} {
					released := fixture.request(t, http.MethodGet, "/repository/"+repository+"/artifact.bin", nil, true)
					assertStatus(t, released, http.StatusOK)
					assertBody(t, released, payload)
				}
			})
		}
	}
}

func TestOCIReferrerVerifiesSubjectAndPolicyChangesInvalidateVerdict(t *testing.T) {
	t.Parallel()
	fixture := newServerFixture(t)
	privateKey, publicKeyPEM := provenanceTestKey(t)
	ctx := context.Background()
	policy := domain.TrustPolicy{
		Repository: "oci",
		Mode:       "verify-on-pull",
		PublicKeys: []string{publicKeyPEM},
	}
	defaults := policy
	defaults.Repository = ""
	if err := fixture.Metadata.SetTrustPolicyDefaults(ctx, defaults); err != nil {
		t.Fatal(err)
	}

	configDigest := uploadOCIEmptyConfig(t, fixture, "acme/signed")
	seedManifest := []byte(fmt.Sprintf(`{
		"schemaVersion":2,
		"mediaType":"application/vnd.oci.image.manifest.v1+json",
		"config":{"mediaType":"application/vnd.oci.image.config.v1+json","digest":%q,"size":2},
		"layers":[]
	}`, configDigest))
	seedUpload := fixture.requestWithContentType(
		t,
		http.MethodPut,
		"/repository/oci/v2/acme/signed/manifests/seed",
		seedManifest,
		ociManifestMediaTypeForTest,
		true,
	)
	assertStatus(t, seedUpload, http.StatusCreated)
	seedDigest := seedUpload.Header.Get("Docker-Content-Digest")
	seedUpload.Body.Close()

	targetManifest := []byte(fmt.Sprintf(`{
		"schemaVersion":2,
		"mediaType":"application/vnd.oci.image.manifest.v1+json",
		"config":{"mediaType":"application/vnd.oci.image.config.v1+json","digest":%q,"size":2},
		"subject":{"mediaType":"application/vnd.oci.image.manifest.v1+json","digest":%q,"size":%d},
		"layers":[]
	}`, configDigest, seedDigest, len(seedManifest)))
	targetPath := "/repository/oci/v2/acme/signed/manifests/v1"
	targetUpload := fixture.requestWithContentType(
		t,
		http.MethodPut,
		targetPath,
		targetManifest,
		ociManifestMediaTypeForTest,
		true,
	)
	assertStatus(t, targetUpload, http.StatusCreated)
	targetDigest := targetUpload.Header.Get("Docker-Content-Digest")
	targetUpload.Body.Close()
	targetAsset, err := fixture.Metadata.Asset(
		ctx, "oci", "v2/acme/signed/manifests/v1",
	)
	if err != nil {
		t.Fatal(err)
	}
	if targetAsset.SubjectDigest != seedDigest {
		t.Fatalf("target subject digest = %q, want %q", targetAsset.SubjectDigest, seedDigest)
	}
	if assetattrs.OCIProvenanceArtifact(targetAsset.Attributes) {
		t.Fatal("ordinary subject-bearing manifest classified as provenance artifact")
	}

	blocked := fixture.request(t, http.MethodGet, targetPath, nil, true)
	assertStatus(t, blocked, http.StatusForbidden)
	blocked.Body.Close()

	cosignPayload := []byte(fmt.Sprintf(`{
		"critical":{
			"identity":{"docker-reference":"acme/signed"},
			"image":{"Docker-manifest-digest":%q},
			"type":"cosign container image signature"
		}
	}`, targetDigest))
	payloadDigest := testDigest(cosignPayload)
	payloadUpload := fixture.request(
		t,
		http.MethodPost,
		"/repository/oci/v2/acme/signed/blobs/uploads/?digest="+payloadDigest,
		cosignPayload,
		true,
	)
	assertStatus(t, payloadUpload, http.StatusCreated)
	payloadUpload.Body.Close()

	signature := base64.StdEncoding.EncodeToString(
		signProvenancePayload(t, privateKey, cosignPayload),
	)
	signatureManifest := []byte(fmt.Sprintf(`{
		"schemaVersion":2,
		"mediaType":"application/vnd.oci.image.manifest.v1+json",
		"artifactType":"application/vnd.dev.cosign.artifact.sig.v1+json",
		"config":{"mediaType":"application/vnd.oci.image.config.v1+json","digest":%q,"size":2},
		"subject":{"mediaType":"application/vnd.oci.image.manifest.v1+json","digest":%q,"size":%d},
		"layers":[{
			"mediaType":"application/vnd.dev.cosign.simplesigning.v1+json",
			"digest":%q,
			"size":%d,
			"annotations":{"dev.cosignproject.cosign/signature":%q}
		}]
	}`, configDigest, targetDigest, len(targetManifest), payloadDigest, len(cosignPayload), signature))
	signaturePath := "/repository/oci/v2/acme/signed/manifests/signature"
	signatureUpload := fixture.requestWithContentType(
		t,
		http.MethodPut,
		signaturePath,
		signatureManifest,
		ociManifestMediaTypeForTest,
		true,
	)
	assertStatus(t, signatureUpload, http.StatusCreated)
	signatureUpload.Body.Close()

	released := fixture.request(t, http.MethodGet, targetPath, nil, true)
	assertStatus(t, released, http.StatusOK)
	assertBody(t, released, targetManifest)
	referrer := fixture.request(t, http.MethodGet, signaturePath, nil, true)
	assertStatus(t, referrer, http.StatusOK)
	referrer.Body.Close()

	if err := fixture.Metadata.SetTrustPolicy(ctx, policy); err != nil {
		t.Fatal(err)
	}
	stale := fixture.request(t, http.MethodGet, targetPath, nil, true)
	assertStatus(t, stale, http.StatusForbidden)
	stale.Body.Close()
}

func TestOCISubjectDescriptorDoesNotBypassTrustPolicy(t *testing.T) {
	t.Parallel()
	for _, mode := range []string{"verify-on-push", "verify-on-pull"} {
		t.Run(mode, func(t *testing.T) {
			fixture := newServerFixture(t)
			_, publicKey := provenanceTestKey(t)
			ctx := context.Background()

			configDigest := uploadOCIEmptyConfig(t, fixture, "acme/review")
			seed := []byte(fmt.Sprintf(`{
				"schemaVersion":2,
				"mediaType":"application/vnd.oci.image.manifest.v1+json",
				"config":{"mediaType":"application/vnd.oci.image.config.v1+json","digest":%q,"size":2},
				"layers":[]
			}`, configDigest))
			seedResponse := fixture.requestWithContentType(
				t, http.MethodPut,
				"/repository/oci/v2/acme/review/manifests/seed",
				seed, ociManifestMediaTypeForTest, true,
			)
			assertStatus(t, seedResponse, http.StatusCreated)
			seedDigest := seedResponse.Header.Get("Docker-Content-Digest")
			seedResponse.Body.Close()
			config := []byte(`{"architecture":"amd64","os":"linux","rootfs":{"type":"layers","diff_ids":[]}}`)
			layer := []byte("ordinary runnable image layer")
			for _, blob := range [][]byte{config, layer} {
				digest := testDigest(blob)
				upload := fixture.request(
					t, http.MethodPost,
					"/repository/oci/v2/acme/review/blobs/uploads/?digest="+digest,
					blob, true,
				)
				assertStatus(t, upload, http.StatusCreated)
				upload.Body.Close()
			}

			if err := fixture.Metadata.SetTrustPolicy(ctx, domain.TrustPolicy{
				Repository: "oci", Mode: mode, PublicKeys: []string{publicKey},
			}); err != nil {
				t.Fatal(err)
			}

			manifests := map[string][]byte{
				"subject-only": []byte(fmt.Sprintf(`{
					"schemaVersion":2,
					"mediaType":"application/vnd.oci.image.manifest.v1+json",
					"subject":{"mediaType":"application/vnd.oci.image.manifest.v1+json","digest":%q,"size":%d},
					"config":{"mediaType":"application/vnd.oci.image.config.v1+json","digest":%q,"size":2},
					"layers":[]
				}`, seedDigest, len(seed), configDigest)),
				"forged-cosign-type": []byte(fmt.Sprintf(`{
					"schemaVersion":2,
					"mediaType":"application/vnd.oci.image.manifest.v1+json",
					"artifactType":"application/vnd.dev.cosign.artifact.sig.v1+json",
					"subject":{"mediaType":"application/vnd.oci.image.manifest.v1+json","digest":%q,"size":%d},
					"config":{"mediaType":"application/vnd.oci.image.config.v1+json","digest":%q,"size":%d},
					"layers":[{"mediaType":"application/vnd.oci.image.layer.v1.tar","digest":%q,"size":%d}]
				}`, seedDigest, len(seed), testDigest(config), len(config), testDigest(layer), len(layer))),
			}
			for name, manifest := range manifests {
				t.Run(name, func(t *testing.T) {
					path := "/repository/oci/v2/acme/review/manifests/unsigned-" + name
					response := fixture.requestWithContentType(
						t, http.MethodPut, path, manifest, ociManifestMediaTypeForTest, true,
					)
					if mode == "verify-on-push" {
						assertStatus(t, response, http.StatusForbidden)
						response.Body.Close()
						return
					}
					assertStatus(t, response, http.StatusCreated)
					response.Body.Close()

					download := fixture.request(t, http.MethodGet, path, nil, true)
					assertStatus(t, download, http.StatusForbidden)
					download.Body.Close()
				})
			}
		})
	}
}

func uploadOCIEmptyConfig(t *testing.T, fixture *serverFixture, imageName string) string {
	t.Helper()
	content := []byte(`{}`)
	digest := testDigest(content)
	response := fixture.request(t, http.MethodPost,
		"/repository/oci/v2/"+imageName+"/blobs/uploads/?digest="+digest, content, true)
	assertStatus(t, response, http.StatusCreated)
	response.Body.Close()
	return digest
}

func TestSLSADSSEVerificationAndFingerprintDenylist(t *testing.T) {
	privateKey, publicKeyPEM := provenanceTestKey(t)
	artifactDigest := "sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	statement := []byte(`{
		"_type":"https://in-toto.io/Statement/v1",
		"subject":[{"name":"artifact","digest":{"sha256":"aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"}}],
		"predicateType":"https://slsa.dev/provenance/v1",
		"predicate":{}
	}`)
	payloadType := "application/vnd.in-toto+json"
	signature := signProvenancePayload(
		t,
		privateKey,
		content.DSSEPreAuthenticationEncoding(payloadType, statement),
	)
	request := domain.VerificationRequest{
		DSSE: &domain.DSSEEnvelope{
			PayloadType: payloadType,
			Payload:     base64.StdEncoding.EncodeToString(statement),
			Signatures: []domain.DSSESignature{
				{Signature: base64.StdEncoding.EncodeToString(signature)},
			},
		},
	}
	policy := domain.TrustPolicy{
		Repository: "raw",
		Mode:       "audit",
		PublicKeys: []string{publicKeyPEM},
	}
	asset := domain.Asset{Repository: "raw", Digest: artifactDigest, Kind: "raw"}
	result := content.VerifyProvenance(policy, asset, request)
	if result.Status != "passed" || result.Format != "dsse" {
		t.Fatalf("DSSE verification failed: %+v", result)
	}

	policy.DeniedFingerprints = []string{result.Fingerprint}
	denied := content.VerifyProvenance(policy, asset, request)
	if denied.Status != "failed" {
		t.Fatalf("denylisted signer passed verification: %+v", denied)
	}
}

func TestCertificateIdentityVerification(t *testing.T) {
	rootKey, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC()
	rootTemplate := &x509.Certificate{
		SerialNumber:          big.NewInt(1),
		Subject:               pkixName("Suxen Test Root"),
		NotBefore:             now.Add(-time.Hour),
		NotAfter:              now.Add(time.Hour),
		KeyUsage:              x509.KeyUsageCertSign | x509.KeyUsageDigitalSignature,
		BasicConstraintsValid: true,
		IsCA:                  true,
	}
	rootDER, err := x509.CreateCertificate(
		rand.Reader,
		rootTemplate,
		rootTemplate,
		&rootKey.PublicKey,
		rootKey,
	)
	if err != nil {
		t.Fatal(err)
	}
	rootCertificate, err := x509.ParseCertificate(rootDER)
	if err != nil {
		t.Fatal(err)
	}

	leafKey, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	const identity = "builder@example.com"
	const issuer = "https://identity.example"
	leafTemplate := &x509.Certificate{
		SerialNumber:   big.NewInt(2),
		Subject:        pkixName("Ephemeral Builder"),
		NotBefore:      now.Add(-time.Minute),
		NotAfter:       now.Add(time.Hour),
		KeyUsage:       x509.KeyUsageDigitalSignature,
		ExtKeyUsage:    []x509.ExtKeyUsage{x509.ExtKeyUsageCodeSigning},
		EmailAddresses: []string{identity},
		ExtraExtensions: []pkix.Extension{
			{Id: content.FulcioIssuerOID, Value: []byte(issuer)},
		},
	}
	leafDER, err := x509.CreateCertificate(
		rand.Reader,
		leafTemplate,
		rootCertificate,
		&leafKey.PublicKey,
		rootKey,
	)
	if err != nil {
		t.Fatal(err)
	}
	rootPEM := string(pem.EncodeToMemory(&pem.Block{
		Type:  "CERTIFICATE",
		Bytes: rootDER,
	}))
	leafPEM := string(pem.EncodeToMemory(&pem.Block{
		Type:  "CERTIFICATE",
		Bytes: leafDER,
	}))

	asset := domain.Asset{
		Repository: "raw",
		Digest:     "sha256:bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb",
		Kind:       "raw",
	}
	result := content.VerifyProvenance(domain.TrustPolicy{
		Repository:             "raw",
		Mode:                   "audit",
		CertificateAuthorities: []string{rootPEM},
		AllowedIdentities: []domain.TrustIdentity{
			{Issuer: issuer, Subject: identity},
		},
	}, asset, domain.VerificationRequest{
		Signature: base64.StdEncoding.EncodeToString(
			signProvenancePayload(t, leafKey, []byte(asset.Digest)),
		),
		Certificate: leafPEM,
	})
	if result.Status != "passed" || result.Identity != identity || result.Issuer != issuer {
		t.Fatalf("certificate identity verification failed: %+v", result)
	}
}

func provenanceTestKey(t *testing.T) (*ecdsa.PrivateKey, string) {
	t.Helper()
	privateKey, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	encoded, err := x509.MarshalPKIXPublicKey(&privateKey.PublicKey)
	if err != nil {
		t.Fatal(err)
	}
	return privateKey, string(pem.EncodeToMemory(&pem.Block{
		Type:  "PUBLIC KEY",
		Bytes: encoded,
	}))
}

func pkixName(commonName string) pkix.Name {
	return pkix.Name{CommonName: commonName}
}

func signProvenancePayload(
	t *testing.T,
	privateKey *ecdsa.PrivateKey,
	payload []byte,
) []byte {
	t.Helper()
	digest := sha256.Sum256(payload)
	signature, err := ecdsa.SignASN1(rand.Reader, privateKey, digest[:])
	if err != nil {
		t.Fatal(err)
	}
	return signature
}

func provenanceUploadRequest(
	t *testing.T,
	handler http.Handler,
	requestPath string,
	body []byte,
	signature []byte,
) *http.Response {
	t.Helper()
	request := httptest.NewRequest(http.MethodPut, requestPath, bytes.NewReader(body))
	request.Header.Set("Authorization", "Bearer "+testToken)
	request.Header.Set("Content-Type", "application/octet-stream")
	request.Header.Set(
		"X-Suxen-Signature",
		base64.StdEncoding.EncodeToString(signature),
	)
	recorder := httptest.NewRecorder()
	handler.ServeHTTP(recorder, request)
	return recorder.Result()
}
