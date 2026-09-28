package content

import (
	"net/http"
	"strings"
	"time"
)

// checkReadPreconditions evaluates validators before selecting any byte range.
// Authorization and download policy must already have succeeded.
func checkReadPreconditions(w http.ResponseWriter, r *http.Request, modified time.Time) bool {
	etag := w.Header().Get("ETag")
	modified = modified.Truncate(time.Second)
	if match := strings.Join(r.Header.Values("If-Match"), ", "); match != "" {
		if !etagListMatches(match, etag, false) {
			w.WriteHeader(http.StatusPreconditionFailed)
			return false
		}
	} else if value := r.Header.Get("If-Unmodified-Since"); value != "" && !modified.IsZero() {
		if date, err := http.ParseTime(value); err == nil && modified.After(date) {
			w.WriteHeader(http.StatusPreconditionFailed)
			return false
		}
	}
	if match := strings.Join(r.Header.Values("If-None-Match"), ", "); match != "" {
		if etagListMatches(match, etag, true) {
			writeNotModified(w)
			return false
		}
	} else if value := r.Header.Get("If-Modified-Since"); value != "" && !modified.IsZero() {
		if date, err := http.ParseTime(value); err == nil && !modified.After(date) {
			writeNotModified(w)
			return false
		}
	}
	return true
}

func writeNotModified(w http.ResponseWriter) {
	w.Header().Del("Content-Type")
	w.Header().Del("Content-Length")
	w.Header().Del("Content-Encoding")
	w.WriteHeader(http.StatusNotModified)
}

func ifRangeMatches(value, etag string) bool {
	if value == "" {
		return true
	}
	value = strings.TrimSpace(value)
	// Last-Modified has second precision, while a stored artifact can be replaced
	// twice within one second. We cannot establish that a date is a strong
	// validator, so only an exact strong ETag can authorize a partial response.
	return strings.HasPrefix(value, `"`) && value == etag
}

func etagListMatches(value, etag string, weak bool) bool {
	return matchETagList(value, etag, weak, true)
}

// StrongETagListContains checks an If-Match list for the current generation.
// Unlike a read precondition, annotation writes require a concrete digest:
// neither a weak validator nor the wildcard can authorize them.
func StrongETagListContains(value, etag string) bool {
	return matchETagList(value, etag, false, false)
}

func matchETagList(value, etag string, weak, allowWildcard bool) bool {
	if strings.TrimSpace(value) == "*" {
		return allowWildcard
	}
	matched := false
	for value != "" {
		value = strings.TrimLeft(value, " \t,")
		if value == "" {
			break
		}
		candidate := value
		isWeak := strings.HasPrefix(candidate, "W/")
		if isWeak {
			candidate = candidate[2:]
		}
		if !strings.HasPrefix(candidate, `"`) {
			return false
		}
		end := strings.IndexByte(candidate[1:], '"')
		if end < 0 {
			return false
		}
		end += 2
		tag := candidate[:end]
		if (weak || !isWeak) && tag == etag {
			matched = true
		}
		value = candidate[end:]
		if value != "" && !strings.HasPrefix(strings.TrimLeft(value, " \t"), ",") {
			return false
		}
	}
	return matched
}
