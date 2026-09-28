package identity

import (
	"github.com/suxen-project/suxen/internal/httpx"
	"net/http"
	"net/url"
	"strings"
)

func (s *Service) ProtectCookieAuthenticatedMutation(
	w http.ResponseWriter,
	r *http.Request,
) bool {
	if isSafeMethod(r.Method) || r.Header.Get("Authorization") != "" {
		return true
	}
	if _, err := r.Cookie(oidcSessionCookieName); err != nil {
		return true
	}
	if SameOriginStrings(r.Header.Get("Origin"), s.expectedOrigin(r)) {
		return true
	}

	httpx.WriteProblem(
		w,
		http.StatusForbidden,
		"csrf_validation_failed",
		"cookie-authenticated mutations require a same-origin request",
	)
	return false
}

func isSafeMethod(method string) bool {
	switch method {
	case http.MethodGet, http.MethodHead, http.MethodOptions, http.MethodTrace:
		return true
	default:
		return false
	}
}

func (s *Service) expectedOrigin(r *http.Request) string {
	if s.Config.PublicURL != "" {
		return s.Config.PublicURL
	}
	scheme := "http"
	if httpx.RequestUsesTLS(r) {
		scheme = "https"
	}
	return scheme + "://" + r.Host
}

func SameOriginStrings(actual string, expected string) bool {
	actualURL, err := url.Parse(actual)
	if err != nil || !isOriginURL(actualURL) {
		return false
	}
	expectedURL, err := url.Parse(expected)
	if err != nil || !isOriginURL(expectedURL) {
		return false
	}
	return sameOrigin(actualURL, expectedURL)
}

func isOriginURL(candidate *url.URL) bool {
	if candidate == nil {
		return false
	}
	scheme := strings.ToLower(candidate.Scheme)
	return (scheme == "http" || scheme == "https") &&
		candidate.Host != "" &&
		candidate.Hostname() != "" &&
		candidate.User == nil &&
		candidate.Path == "" &&
		candidate.RawQuery == "" &&
		!candidate.ForceQuery &&
		candidate.Fragment == "" &&
		candidate.Opaque == ""
}

func sameOrigin(first, second *url.URL) bool {
	return strings.EqualFold(first.Scheme, second.Scheme) &&
		strings.EqualFold(
			strings.TrimSuffix(first.Hostname(), "."),
			strings.TrimSuffix(second.Hostname(), "."),
		) &&
		effectivePort(first) == effectivePort(second)
}

func effectivePort(target *url.URL) string {
	if port := target.Port(); port != "" {
		return port
	}
	switch strings.ToLower(target.Scheme) {
	case "http":
		return "80"
	case "https":
		return "443"
	default:
		return ""
	}
}
