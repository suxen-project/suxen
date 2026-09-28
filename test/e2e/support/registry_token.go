package main

import (
	"crypto"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/base32"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"encoding/pem"
	"fmt"
	"math/big"
	"net/http"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"time"
)

const (
	registryTokenIssuer  = "suxen-e2e-auth"
	registryTokenService = "suxen-e2e-registry"
)

type registryTokenIssuerServer struct {
	privateKey *rsa.PrivateKey
	keyID      string
	mu         sync.Mutex
	requests   int
}

type registryTokenAccess struct {
	Type    string   `json:"type"`
	Name    string   `json:"name"`
	Actions []string `json:"actions"`
}

type registryTokenClaims struct {
	Issuer    string                `json:"iss"`
	Subject   string                `json:"sub"`
	Audience  string                `json:"aud"`
	ExpiresAt int64                 `json:"exp"`
	NotBefore int64                 `json:"nbf"`
	IssuedAt  int64                 `json:"iat"`
	ID        string                `json:"jti"`
	Access    []registryTokenAccess `json:"access"`
}

func serveRegistryTokenIssuer() error {
	stateDirectory := environment("E2E_STATE", "/state")
	if err := os.MkdirAll(stateDirectory, 0o750); err != nil {
		return fmt.Errorf("create registry token state: %w", err)
	}

	privateKey, certificate, err := generateRegistryTokenKeyPair()
	if err != nil {
		return err
	}
	certificatePath := environment(
		"E2E_TOKEN_CERTIFICATE",
		filepath.Join(stateDirectory, "registry-token-cert.pem"),
	)
	if err := os.WriteFile(certificatePath, certificate, 0o644); err != nil {
		return fmt.Errorf("write registry token certificate: %w", err)
	}
	keyID, err := registryTokenKeyID(&privateKey.PublicKey)
	if err != nil {
		return err
	}

	issuer := &registryTokenIssuerServer{
		privateKey: privateKey,
		keyID:      keyID,
	}
	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", healthHandler)
	mux.HandleFunc("GET /requests", issuer.handleRequests)
	mux.HandleFunc("GET /token", issuer.handleToken)
	return listen(mux)
}

func generateRegistryTokenKeyPair() (*rsa.PrivateKey, []byte, error) {
	privateKey, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		return nil, nil, fmt.Errorf("generate registry token key: %w", err)
	}
	serialNumber, err := rand.Int(rand.Reader, new(big.Int).Lsh(big.NewInt(1), 128))
	if err != nil {
		return nil, nil, fmt.Errorf("generate registry token certificate serial: %w", err)
	}
	now := time.Now().UTC()
	template := &x509.Certificate{
		SerialNumber:          serialNumber,
		Subject:               pkix.Name{CommonName: registryTokenIssuer},
		NotBefore:             now.Add(-time.Minute),
		NotAfter:              now.Add(24 * time.Hour),
		KeyUsage:              x509.KeyUsageDigitalSignature | x509.KeyUsageCertSign,
		BasicConstraintsValid: true,
		IsCA:                  true,
	}
	certificateDER, err := x509.CreateCertificate(
		rand.Reader,
		template,
		template,
		&privateKey.PublicKey,
		privateKey,
	)
	if err != nil {
		return nil, nil, fmt.Errorf("create registry token certificate: %w", err)
	}
	certificate := pem.EncodeToMemory(&pem.Block{
		Type:  "CERTIFICATE",
		Bytes: certificateDER,
	})
	return privateKey, certificate, nil
}

func (issuer *registryTokenIssuerServer) handleToken(
	w http.ResponseWriter,
	request *http.Request,
) {
	service := request.URL.Query().Get("service")
	if service == "" {
		service = registryTokenService
	}
	if service != registryTokenService {
		http.Error(w, "unsupported token service", http.StatusBadRequest)
		return
	}

	now := time.Now().UTC()
	identifier := make([]byte, 16)
	if _, err := rand.Read(identifier); err != nil {
		http.Error(w, "create token identifier", http.StatusInternalServerError)
		return
	}
	claims := registryTokenClaims{
		Issuer:    registryTokenIssuer,
		Subject:   "suxen-e2e-client",
		Audience:  service,
		ExpiresAt: now.Add(5 * time.Minute).Unix(),
		NotBefore: now.Add(-time.Second).Unix(),
		IssuedAt:  now.Unix(),
		ID:        hex.EncodeToString(identifier),
		Access:    registryTokenAccessFromScopes(request.URL.Query()["scope"]),
	}
	token, err := issuer.sign(claims)
	if err != nil {
		http.Error(w, "sign registry token", http.StatusInternalServerError)
		return
	}

	issuer.mu.Lock()
	issuer.requests++
	issuer.mu.Unlock()
	writeJSON(w, http.StatusOK, map[string]any{
		"token":        token,
		"access_token": token,
		"expires_in":   300,
		"issued_at":    now.Format(time.RFC3339),
	})
}

func registryTokenAccessFromScopes(scopes []string) []registryTokenAccess {
	access := make([]registryTokenAccess, 0, len(scopes))
	for _, scopeValue := range scopes {
		for _, scope := range strings.Fields(scopeValue) {
			parts := strings.SplitN(scope, ":", 3)
			if len(parts) != 3 || parts[0] == "" || parts[1] == "" {
				continue
			}
			actions := strings.Split(parts[2], ",")
			actions = slices.DeleteFunc(actions, func(action string) bool {
				return action == ""
			})
			access = append(access, registryTokenAccess{
				Type:    parts[0],
				Name:    parts[1],
				Actions: actions,
			})
		}
	}
	return access
}

func (issuer *registryTokenIssuerServer) sign(claims registryTokenClaims) (string, error) {
	header := map[string]string{
		"alg": "RS256",
		"kid": issuer.keyID,
		"typ": "JWT",
	}
	headerJSON, err := json.Marshal(header)
	if err != nil {
		return "", err
	}
	claimsJSON, err := json.Marshal(claims)
	if err != nil {
		return "", err
	}
	encodedHeader := base64.RawURLEncoding.EncodeToString(headerJSON)
	encodedClaims := base64.RawURLEncoding.EncodeToString(claimsJSON)
	signed := encodedHeader + "." + encodedClaims
	digest := sha256.Sum256([]byte(signed))
	signature, err := rsa.SignPKCS1v15(rand.Reader, issuer.privateKey, crypto.SHA256, digest[:])
	if err != nil {
		return "", err
	}
	return signed + "." + base64.RawURLEncoding.EncodeToString(signature), nil
}

// registryTokenKeyID uses the Docker libtrust fingerprint format expected by
// Distribution 2.x when it loads trusted signing keys from a certificate bundle.
func registryTokenKeyID(publicKey *rsa.PublicKey) (string, error) {
	encoded, err := x509.MarshalPKIXPublicKey(publicKey)
	if err != nil {
		return "", fmt.Errorf("marshal registry token public key: %w", err)
	}
	digest := sha256.Sum256(encoded)
	fingerprint := base32.StdEncoding.WithPadding(base32.NoPadding).EncodeToString(digest[:30])
	groups := make([]string, 0, len(fingerprint)/4)
	for offset := 0; offset < len(fingerprint); offset += 4 {
		groups = append(groups, fingerprint[offset:offset+4])
	}
	return strings.Join(groups, ":"), nil
}

func (issuer *registryTokenIssuerServer) handleRequests(
	w http.ResponseWriter,
	_ *http.Request,
) {
	issuer.mu.Lock()
	requests := issuer.requests
	issuer.mu.Unlock()
	writeJSON(w, http.StatusOK, map[string]int{"requests": requests})
}
