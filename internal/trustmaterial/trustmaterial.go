// Package trustmaterial validates and parses the PEM public keys, certificate
// authorities, and fingerprints carried by a trust policy. It is transport
// independent and imports only the domain types and the standard library, so
// the HTTP API and provisioning preflight share one definition of valid trust
// material and cannot disagree on whether a policy is acceptable.
package trustmaterial

import (
	"crypto/x509"
	"encoding/hex"
	"encoding/pem"
	"errors"
	"fmt"
	"strings"

	"github.com/suxen-project/suxen/internal/domain"
)

// Validate checks a trust policy's material: every public key and certificate
// authority must be PEM decodable, allowed identities require a certificate
// authority, and denied fingerprints must be SHA-256 hexadecimal values. It
// validates only material; structural checks stay on domain.TrustPolicy.
func Validate(policy domain.TrustPolicy) error {
	for _, encoded := range policy.PublicKeys {
		if _, err := ParsePublicKeyPEM(encoded); err != nil {
			return fmt.Errorf("%w: public key: %v", domain.ErrInvalidTrustMaterial, err)
		}
	}
	if _, err := CertificatePool(policy.CertificateAuthorities); err != nil {
		return fmt.Errorf(
			"%w: certificate authority: %v",
			domain.ErrInvalidTrustMaterial,
			err,
		)
	}
	if len(policy.AllowedIdentities) > 0 && len(policy.CertificateAuthorities) == 0 {
		return fmt.Errorf(
			"%w: allowed identities require a certificate authority",
			domain.ErrInvalidTrustMaterial,
		)
	}
	for _, fingerprint := range policy.DeniedFingerprints {
		normalized := NormalizeFingerprint(fingerprint)
		if len(normalized) != 64 {
			return fmt.Errorf(
				"%w: denied fingerprints must be SHA-256 hexadecimal values",
				domain.ErrInvalidTrustMaterial,
			)
		}
		if _, err := hex.DecodeString(normalized); err != nil {
			return fmt.Errorf(
				"%w: denied fingerprints must be SHA-256 hexadecimal values",
				domain.ErrInvalidTrustMaterial,
			)
		}
	}
	return nil
}

// ParsePublicKeyPEM decodes a PEM-encoded public key, accepting PKIX, PKCS#1,
// and certificate-wrapped keys.
func ParsePublicKeyPEM(encoded string) (any, error) {
	block, _ := pem.Decode([]byte(encoded))
	if block == nil {
		return nil, errors.New("public key is not PEM encoded")
	}
	publicKey, err := x509.ParsePKIXPublicKey(block.Bytes)
	if err == nil {
		return publicKey, nil
	}
	if rsaKey, rsaErr := x509.ParsePKCS1PublicKey(block.Bytes); rsaErr == nil {
		return rsaKey, nil
	}
	if certificate, certificateErr := x509.ParseCertificate(block.Bytes); certificateErr == nil {
		return certificate.PublicKey, nil
	}
	return nil, fmt.Errorf("parse public key: %w", err)
}

// CertificatePool builds a certificate pool from PEM-encoded authorities. Each
// entry must contain at least one certificate.
func CertificatePool(encodedCertificates []string) (*x509.CertPool, error) {
	pool := x509.NewCertPool()
	for _, encoded := range encodedCertificates {
		remaining := []byte(encoded)
		found := false
		for len(remaining) > 0 {
			block, rest := pem.Decode(remaining)
			if block == nil {
				break
			}
			remaining = rest
			certificate, err := x509.ParseCertificate(block.Bytes)
			if err != nil {
				return nil, fmt.Errorf("parse certificate: %w", err)
			}
			pool.AddCert(certificate)
			found = true
		}
		if !found {
			return nil, errors.New("certificate is not PEM encoded")
		}
	}
	return pool, nil
}

// NormalizeFingerprint lowercases a fingerprint and removes colon separators
// and surrounding whitespace.
func NormalizeFingerprint(value string) string {
	value = strings.ReplaceAll(value, ":", "")
	return strings.ToLower(strings.TrimSpace(value))
}
