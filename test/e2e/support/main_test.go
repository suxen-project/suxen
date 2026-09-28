package main

import (
	"crypto"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"reflect"
	"regexp"
	"strings"
	"testing"
)

func TestRegistryTokenKeyIDUsesLibtrustFingerprintFormat(t *testing.T) {
	privateKey, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatalf("generate key: %v", err)
	}

	keyID, err := registryTokenKeyID(&privateKey.PublicKey)
	if err != nil {
		t.Fatalf("create key ID: %v", err)
	}

	pattern := regexp.MustCompile(`^(?:[A-Z2-7]{4}:){11}[A-Z2-7]{4}$`)
	if !pattern.MatchString(keyID) {
		t.Fatalf("key ID %q does not use the libtrust fingerprint format", keyID)
	}
}

func TestRegistryTokenAccessFromScopes(t *testing.T) {
	access := registryTokenAccessFromScopes([]string{
		"repository:library/alpine:pull,push repository:team/example:pull",
		"registry:catalog:* malformed",
	})

	expected := []registryTokenAccess{
		{Type: "repository", Name: "library/alpine", Actions: []string{"pull", "push"}},
		{Type: "repository", Name: "team/example", Actions: []string{"pull"}},
		{Type: "registry", Name: "catalog", Actions: []string{"*"}},
	}
	if !reflect.DeepEqual(access, expected) {
		t.Fatalf("unexpected access claims: got %#v, want %#v", access, expected)
	}
}

func TestRegistryTokenSignatureAndClaims(t *testing.T) {
	privateKey, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatalf("generate key: %v", err)
	}
	keyID, err := registryTokenKeyID(&privateKey.PublicKey)
	if err != nil {
		t.Fatalf("create key ID: %v", err)
	}
	issuer := registryTokenIssuerServer{
		privateKey: privateKey,
		keyID:      keyID,
	}
	claims := registryTokenClaims{
		Issuer:   registryTokenIssuer,
		Subject:  "test-client",
		Audience: registryTokenService,
		Access: []registryTokenAccess{
			{Type: "repository", Name: "fixture/image", Actions: []string{"pull"}},
		},
	}

	token, err := issuer.sign(claims)
	if err != nil {
		t.Fatalf("sign token: %v", err)
	}
	parts := strings.Split(token, ".")
	if len(parts) != 3 {
		t.Fatalf("token has %d parts, want 3", len(parts))
	}

	var header map[string]string
	decodeTokenPart(t, parts[0], &header)
	if header["alg"] != "RS256" || header["kid"] != keyID {
		t.Fatalf("unexpected token header: %#v", header)
	}

	var signedClaims registryTokenClaims
	decodeTokenPart(t, parts[1], &signedClaims)
	if !reflect.DeepEqual(signedClaims, claims) {
		t.Fatalf("unexpected token claims: got %#v, want %#v", signedClaims, claims)
	}

	signature, err := base64.RawURLEncoding.DecodeString(parts[2])
	if err != nil {
		t.Fatalf("decode token signature: %v", err)
	}
	digest := sha256.Sum256([]byte(parts[0] + "." + parts[1]))
	if err := rsa.VerifyPKCS1v15(
		&privateKey.PublicKey,
		crypto.SHA256,
		digest[:],
		signature,
	); err != nil {
		t.Fatalf("verify token signature: %v", err)
	}
}

func decodeTokenPart(t *testing.T, part string, destination any) {
	t.Helper()
	decoded, err := base64.RawURLEncoding.DecodeString(part)
	if err != nil {
		t.Fatalf("decode token part: %v", err)
	}
	if err := json.Unmarshal(decoded, destination); err != nil {
		t.Fatalf("unmarshal token part: %v", err)
	}
}
