package trustmaterial

import (
	"crypto/ed25519"
	"crypto/rand"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"errors"
	"math/big"
	"strings"
	"testing"
	"time"

	"github.com/suxen-project/suxen/internal/domain"
)

func validPublicKeyPEM(t *testing.T) string {
	t.Helper()
	public, _, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	der, err := x509.MarshalPKIXPublicKey(public)
	if err != nil {
		t.Fatal(err)
	}
	return string(pem.EncodeToMemory(&pem.Block{Type: "PUBLIC KEY", Bytes: der}))
}

func validCertificatePEM(t *testing.T) string {
	t.Helper()
	public, private, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	template := &x509.Certificate{
		SerialNumber:          big.NewInt(1),
		Subject:               pkix.Name{CommonName: "trustmaterial-test"},
		NotBefore:             time.Now().Add(-time.Hour),
		NotAfter:              time.Now().Add(time.Hour),
		IsCA:                  true,
		BasicConstraintsValid: true,
	}
	der, err := x509.CreateCertificate(rand.Reader, template, template, public, private)
	if err != nil {
		t.Fatal(err)
	}
	return string(pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}))
}

func TestValidateAcceptsWellFormedMaterial(t *testing.T) {
	key := validPublicKeyPEM(t)
	certificate := validCertificatePEM(t)
	policies := map[string]domain.TrustPolicy{
		"empty":                 {},
		"public key":            {PublicKeys: []string{key}},
		"certificate authority": {CertificateAuthorities: []string{certificate}},
		"identities with a CA":  {CertificateAuthorities: []string{certificate}, AllowedIdentities: []domain.TrustIdentity{{Issuer: "https://issuer.example", Subject: "ci@example.com"}}},
		"denied fingerprint":    {DeniedFingerprints: []string{strings.Repeat("ab", 32)}},
		"colon-separated fpr":   {DeniedFingerprints: []string{strings.TrimSuffix(strings.Repeat("AB:", 32), ":")}},
	}
	for name, policy := range policies {
		t.Run(name, func(t *testing.T) {
			if err := Validate(policy); err != nil {
				t.Fatalf("Validate(%s) = %v, want nil", name, err)
			}
		})
	}
}

func TestValidateRejectsInvalidMaterial(t *testing.T) {
	certificate := validCertificatePEM(t)
	cases := map[string]domain.TrustPolicy{
		"public key not PEM":      {PublicKeys: []string{"not a PEM key"}},
		"certificate not PEM":     {CertificateAuthorities: []string{"not a certificate"}},
		"identities without a CA": {AllowedIdentities: []domain.TrustIdentity{{Issuer: "https://issuer.example", Subject: "ci@example.com"}}},
		"fingerprint too short":   {CertificateAuthorities: []string{certificate}, DeniedFingerprints: []string{"abcd"}},
		"fingerprint not hex":     {DeniedFingerprints: []string{strings.Repeat("zz", 32)}},
	}
	for name, policy := range cases {
		t.Run(name, func(t *testing.T) {
			err := Validate(policy)
			if !errors.Is(err, domain.ErrInvalidTrustMaterial) {
				t.Fatalf("Validate(%s) = %v, want ErrInvalidTrustMaterial", name, err)
			}
		})
	}
}

func TestParsePublicKeyPEM(t *testing.T) {
	if _, err := ParsePublicKeyPEM(validPublicKeyPEM(t)); err != nil {
		t.Fatalf("ParsePublicKeyPEM(valid) = %v", err)
	}
	if _, err := ParsePublicKeyPEM("garbage"); err == nil {
		t.Fatal("ParsePublicKeyPEM(garbage) returned no error")
	}
}

func TestCertificatePoolRejectsNonPEM(t *testing.T) {
	if _, err := CertificatePool([]string{validCertificatePEM(t)}); err != nil {
		t.Fatalf("CertificatePool(valid) = %v", err)
	}
	if _, err := CertificatePool([]string{"nope"}); err == nil {
		t.Fatal("CertificatePool(non-PEM) returned no error")
	}
}

func TestNormalizeFingerprint(t *testing.T) {
	got := NormalizeFingerprint("  AB:CD:ef  ")
	if got != "abcdef" {
		t.Fatalf("NormalizeFingerprint = %q, want abcdef", got)
	}
}
