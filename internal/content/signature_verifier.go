package content

import (
	"crypto"
	"crypto/ecdsa"
	"crypto/ed25519"
	"crypto/rsa"
	"crypto/sha256"
	"crypto/x509"
	"encoding/asn1"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"encoding/pem"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/suxen-project/suxen/internal/domain"
	"github.com/suxen-project/suxen/internal/trustmaterial"
)

var fulcioIssuerOID = asn1.ObjectIdentifier{1, 3, 6, 1, 4, 1, 57264, 1, 1}

// FulcioIssuerOID is the Fulcio issuer OID used by certificate identity tests.
var FulcioIssuerOID = fulcioIssuerOID

type signerCandidate struct {
	publicKey   any
	fingerprint string
	identity    string
	issuer      string
}

// VerifyProvenance checks a signature against a trust policy. HTTP handlers and
// tests call this; the data plane records the result separately.
func VerifyProvenance(
	policy domain.TrustPolicy,
	asset domain.Asset,
	request domain.VerificationRequest,
) domain.ProvenanceResult {
	return verifyProvenance(policy, asset, request)
}

func DSSEPreAuthenticationEncoding(payloadType string, payload []byte) []byte {
	return dssePreAuthenticationEncoding(payloadType, payload)
}

func verifyProvenance(
	policy domain.TrustPolicy,
	asset domain.Asset,
	request domain.VerificationRequest,
) domain.ProvenanceResult {
	result := domain.ProvenanceResult{
		Status:     "failed",
		Digest:     asset.Digest,
		PolicyAt:   policy.UpdatedAt,
		VerifiedAt: time.Now().UTC(),
	}
	payload, signatures, format, err := signedPayload(request, asset.Digest)
	result.Format = format
	if err != nil {
		result.Reason = err.Error()
		return result
	}
	candidates, err := trustedSignerCandidates(policy, request)
	if err != nil {
		result.Reason = err.Error()
		return result
	}
	for _, candidate := range candidates {
		for _, signature := range signatures {
			if verifyDetachedSignature(candidate.publicKey, payload, signature) {
				result.Status = "passed"
				result.Fingerprint = candidate.fingerprint
				result.Identity = candidate.identity
				result.Issuer = candidate.issuer
				return result
			}
		}
	}
	result.Reason = "signature did not match allowed trust material"
	return result
}

func signedPayload(
	request domain.VerificationRequest,
	expectedDigest string,
) ([]byte, [][]byte, string, error) {
	if request.DSSE != nil {
		return signedDSSEPayload(*request.DSSE, expectedDigest)
	}
	signature, err := decodeBase64(request.Signature)
	if err != nil {
		return nil, nil, "detached", fmt.Errorf("decode signature: %w", err)
	}
	if request.Payload == "" {
		return []byte(expectedDigest), [][]byte{signature}, "digest", nil
	}
	payload, err := decodeBase64(request.Payload)
	if err != nil {
		return nil, nil, "detached", fmt.Errorf("decode payload: %w", err)
	}
	format, err := validateSignedPayloadBinding(payload, expectedDigest)
	if err != nil {
		return nil, nil, format, err
	}
	return payload, [][]byte{signature}, format, nil
}

func signedDSSEPayload(
	envelope domain.DSSEEnvelope,
	expectedDigest string,
) ([]byte, [][]byte, string, error) {
	if envelope.PayloadType == "" || len(envelope.Signatures) == 0 {
		return nil, nil, "dsse", errors.New("DSSE envelope requires a payload type and signature")
	}
	payload, err := decodeBase64(envelope.Payload)
	if err != nil {
		return nil, nil, "dsse", fmt.Errorf("decode DSSE payload: %w", err)
	}
	if err := validateInTotoSubject(payload, expectedDigest); err != nil {
		return nil, nil, "dsse", err
	}
	signatures := make([][]byte, 0, len(envelope.Signatures))
	for _, encoded := range envelope.Signatures {
		signature, err := decodeBase64(encoded.Signature)
		if err != nil {
			return nil, nil, "dsse", fmt.Errorf("decode DSSE signature: %w", err)
		}
		signatures = append(signatures, signature)
	}
	return dssePreAuthenticationEncoding(envelope.PayloadType, payload), signatures, "dsse", nil
}

func validateSignedPayloadBinding(payload []byte, expectedDigest string) (string, error) {
	if string(payload) == expectedDigest {
		return "digest", nil
	}
	var simpleSigning struct {
		Critical struct {
			Image struct {
				Digest string `json:"Docker-manifest-digest"`
			} `json:"image"`
			Type string `json:"type"`
		} `json:"critical"`
	}
	if err := json.Unmarshal(payload, &simpleSigning); err == nil &&
		simpleSigning.Critical.Type == "cosign container image signature" {
		if simpleSigning.Critical.Image.Digest != expectedDigest {
			return "cosign-simple-signing", errors.New("cosign payload references a different digest")
		}
		return "cosign-simple-signing", nil
	}
	if err := validateInTotoSubject(payload, expectedDigest); err == nil {
		return "in-toto", nil
	}
	return "detached", errors.New("signed payload does not reference the asset digest")
}

func validateInTotoSubject(payload []byte, expectedDigest string) error {
	var statement struct {
		Subject []struct {
			Digest map[string]string `json:"digest"`
		} `json:"subject"`
	}
	if err := json.Unmarshal(payload, &statement); err != nil {
		return fmt.Errorf("decode in-toto statement: %w", err)
	}
	expectedHash := strings.TrimPrefix(expectedDigest, "sha256:")
	for _, subject := range statement.Subject {
		if strings.EqualFold(subject.Digest["sha256"], expectedHash) {
			return nil
		}
	}
	return errors.New("in-toto statement does not reference the asset digest")
}

func dssePreAuthenticationEncoding(payloadType string, payload []byte) []byte {
	return []byte(fmt.Sprintf(
		"DSSEv1 %d %s %d %s",
		len(payloadType),
		payloadType,
		len(payload),
		payload,
	))
}

func trustedSignerCandidates(
	policy domain.TrustPolicy,
	request domain.VerificationRequest,
) ([]signerCandidate, error) {
	denied := make(map[string]struct{}, len(policy.DeniedFingerprints))
	for _, fingerprint := range policy.DeniedFingerprints {
		denied[trustmaterial.NormalizeFingerprint(fingerprint)] = struct{}{}
	}
	if request.Certificate != "" {
		candidate, err := certificateSigner(policy, request)
		if err != nil {
			return nil, err
		}
		if _, blocked := denied[candidate.fingerprint]; blocked {
			return nil, errors.New("signer fingerprint is denied")
		}
		return []signerCandidate{candidate}, nil
	}

	candidates := make([]signerCandidate, 0, len(policy.PublicKeys))
	for _, encoded := range policy.PublicKeys {
		publicKey, err := trustmaterial.ParsePublicKeyPEM(encoded)
		if err != nil {
			return nil, err
		}
		fingerprint, err := publicKeyFingerprint(publicKey)
		if err != nil {
			return nil, err
		}
		if _, blocked := denied[fingerprint]; blocked {
			continue
		}
		candidates = append(candidates, signerCandidate{
			publicKey:   publicKey,
			fingerprint: fingerprint,
		})
	}
	if len(candidates) == 0 {
		return nil, errors.New("no allowed signing key remains after denylist evaluation")
	}
	return candidates, nil
}

func certificateSigner(
	policy domain.TrustPolicy,
	request domain.VerificationRequest,
) (signerCandidate, error) {
	certificate, err := parseCertificatePEM(request.Certificate)
	if err != nil {
		return signerCandidate{}, err
	}
	roots, err := trustmaterial.CertificatePool(policy.CertificateAuthorities)
	if err != nil {
		return signerCandidate{}, err
	}
	intermediates, err := trustmaterial.CertificatePool(request.CertificateChain)
	if err != nil {
		return signerCandidate{}, err
	}
	_, err = certificate.Verify(x509.VerifyOptions{
		Roots:         roots,
		Intermediates: intermediates,
		KeyUsages:     []x509.ExtKeyUsage{x509.ExtKeyUsageCodeSigning},
		CurrentTime:   time.Now().UTC(),
	})
	if err != nil {
		return signerCandidate{}, fmt.Errorf("verify signing certificate: %w", err)
	}
	identity, issuer, err := trustedCertificateIdentity(policy, certificate)
	if err != nil {
		return signerCandidate{}, err
	}
	fingerprint, err := publicKeyFingerprint(certificate.PublicKey)
	if err != nil {
		return signerCandidate{}, err
	}
	return signerCandidate{
		publicKey:   certificate.PublicKey,
		fingerprint: fingerprint,
		identity:    identity,
		issuer:      issuer,
	}, nil
}

func trustedCertificateIdentity(
	policy domain.TrustPolicy,
	certificate *x509.Certificate,
) (string, string, error) {
	issuer := fulcioIssuer(certificate)
	identities := make([]string, 0, len(certificate.EmailAddresses)+len(certificate.URIs))
	identities = append(identities, certificate.EmailAddresses...)
	for _, uri := range certificate.URIs {
		identities = append(identities, uri.String())
	}
	if len(policy.AllowedIdentities) == 0 {
		identity := ""
		if len(identities) > 0 {
			identity = identities[0]
		}
		return identity, issuer, nil
	}
	for _, allowed := range policy.AllowedIdentities {
		if allowed.Issuer != issuer {
			continue
		}
		for _, identity := range identities {
			if identity == allowed.Subject {
				return identity, issuer, nil
			}
		}
	}
	return "", issuer, errors.New("signing certificate identity is not allowed")
}

func fulcioIssuer(certificate *x509.Certificate) string {
	for _, extension := range certificate.Extensions {
		if !extension.Id.Equal(fulcioIssuerOID) {
			continue
		}
		var value string
		if _, err := asn1.Unmarshal(extension.Value, &value); err == nil {
			return value
		}
		return string(extension.Value)
	}
	return ""
}

func parseCertificatePEM(encoded string) (*x509.Certificate, error) {
	block, _ := pem.Decode([]byte(encoded))
	if block == nil {
		return nil, errors.New("certificate is not PEM encoded")
	}
	certificate, err := x509.ParseCertificate(block.Bytes)
	if err != nil {
		return nil, fmt.Errorf("parse certificate: %w", err)
	}
	return certificate, nil
}

func publicKeyFingerprint(publicKey any) (string, error) {
	encoded, err := x509.MarshalPKIXPublicKey(publicKey)
	if err != nil {
		return "", fmt.Errorf("encode public key: %w", err)
	}
	digest := sha256.Sum256(encoded)
	return hex.EncodeToString(digest[:]), nil
}

func verifyDetachedSignature(publicKey any, payload, signature []byte) bool {
	digest := sha256.Sum256(payload)
	switch key := publicKey.(type) {
	case *ecdsa.PublicKey:
		return ecdsa.VerifyASN1(key, digest[:], signature)
	case *rsa.PublicKey:
		if rsa.VerifyPKCS1v15(key, crypto.SHA256, digest[:], signature) == nil {
			return true
		}
		return rsa.VerifyPSS(key, crypto.SHA256, digest[:], signature, nil) == nil
	case ed25519.PublicKey:
		return ed25519.Verify(key, payload, signature)
	default:
		return false
	}
}

func decodeBase64(value string) ([]byte, error) {
	decoded, err := base64.StdEncoding.DecodeString(value)
	if err == nil {
		return decoded, nil
	}
	return base64.RawStdEncoding.DecodeString(value)
}
