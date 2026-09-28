package httpx

import (
	"context"
	"crypto/rand"
	"encoding/base64"
	"fmt"
	"log/slog"
	"net/http"
	"strings"
)

// RequestIDHeader is the canonical inbound/outbound request correlation header.
const RequestIDHeader = "X-Request-ID"

type requestLogContextKey struct{}

// RequestLog is the request-scoped correlation fields attached to the context
// by the compositor and read by identity, OCI, and the control plane.
type RequestLog struct {
	RequestID  string
	Subject    string
	Repository string
	Format     string
}

// WithRequestLog stores log on the request context.
func WithRequestLog(r *http.Request, log *RequestLog) *http.Request {
	return r.WithContext(context.WithValue(r.Context(), requestLogContextKey{}, log))
}

// RequestUsesTLS reports whether the inbound request was received over TLS.
func RequestUsesTLS(r *http.Request) bool {
	return r.TLS != nil
}

// RequestOrigin returns the scheme and host the client used to reach this
// request, honouring a reverse proxy's X-Forwarded-Proto. It is the base for
// URLs handed back to clients (rewritten indexes, token realms), which must
// point at the same hostname the client already resolved.
func RequestOrigin(r *http.Request) string {
	scheme := "http"
	if RequestUsesTLS(r) || forwardedHTTPS(r) {
		scheme = "https"
	}
	host := r.Host
	if host == "" {
		host = "suxen"
	}
	return scheme + "://" + host
}

func forwardedHTTPS(r *http.Request) bool {
	proto := strings.ToLower(strings.TrimSpace(r.Header.Get("X-Forwarded-Proto")))
	if comma := strings.IndexByte(proto, ','); comma >= 0 {
		proto = strings.TrimSpace(proto[:comma])
	}
	return proto == "https"
}

// RequestLogFrom returns the request log stored by WithRequestLog, if any.
func RequestLogFrom(ctx context.Context) *RequestLog {
	log, _ := ctx.Value(requestLogContextKey{}).(*RequestLog)
	return log
}

// SetAuthenticatedSubject records the authenticated username for access logs.
func SetAuthenticatedSubject(r *http.Request, subject string) {
	log := RequestLogFrom(r.Context())
	if log == nil {
		return
	}
	log.Subject = subject
}

// RequestLogger adds request correlation fields to base.
func RequestLogger(base *slog.Logger, r *http.Request) *slog.Logger {
	log := RequestLogFrom(r.Context())
	if log == nil {
		return base
	}
	return base.With(
		"request_id", log.RequestID,
		"subject", log.Subject,
		"repository", log.Repository,
		"format", log.Format,
	)
}

// RequestID returns a validated inbound request id or a generated one.
func RequestID(values []string) string {
	if len(values) != 1 {
		return randomSecret(12)
	}
	candidate := strings.TrimSpace(values[0])
	if validRequestID(candidate) {
		return candidate
	}
	return randomSecret(12)
}

// RandomSecret returns size bytes of cryptographic randomness, encoded as raw URL base64.
func RandomSecret(size int) string {
	return randomSecret(size)
}

func randomSecret(size int) string {
	bytes := make([]byte, size)
	if _, err := rand.Read(bytes); err != nil {
		panic(fmt.Sprintf("read cryptographic randomness: %v", err))
	}
	return base64.RawURLEncoding.EncodeToString(bytes)
}

func validRequestID(candidate string) bool {
	if candidate == "" || len(candidate) > 128 {
		return false
	}
	for _, character := range candidate {
		if character >= 'a' && character <= 'z' ||
			character >= 'A' && character <= 'Z' ||
			character >= '0' && character <= '9' {
			continue
		}
		switch character {
		case '-', '_', '.':
			continue
		default:
			return false
		}
	}
	return true
}
