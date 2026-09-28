package httpx

import "net/http"

// OCIErrorEnvelope is the Docker Distribution error body.
type OCIErrorEnvelope struct {
	Errors []OCIError `json:"errors"`
}

// OCIError is one Docker Distribution error object.
type OCIError struct {
	Code    string `json:"code"`
	Message string `json:"message"`
}

// WriteOCIError writes a Docker Distribution error envelope.
func WriteOCIError(w http.ResponseWriter, status int, code, message string) {
	WriteJSON(w, status, OCIErrorEnvelope{
		Errors: []OCIError{{
			Code:    code,
			Message: message,
		}},
	})
}

// WriteOCIInternalError logs err and writes a generic OCI UNKNOWN error.
func WriteOCIInternalError(w http.ResponseWriter, err error) {
	WriteServerProblem(
		w,
		http.StatusInternalServerError,
		"internal_error",
		"internal server error",
		err,
	)
}

// OCIErrorCode maps a problem code / HTTP status onto a Distribution error code.
func OCIErrorCode(status int, problemCode string) string {
	switch {
	case problemCode == "digest_mismatch":
		return "DIGEST_INVALID"
	case problemCode == "invalid_json":
		return "MANIFEST_INVALID"
	case status == http.StatusUnauthorized:
		return "UNAUTHORIZED"
	case status == http.StatusForbidden:
		return "DENIED"
	case status == http.StatusNotFound:
		return "NAME_UNKNOWN"
	case status == http.StatusMethodNotAllowed:
		return "UNSUPPORTED"
	case status == http.StatusRequestedRangeNotSatisfiable:
		return "UNSUPPORTED"
	default:
		return "UNKNOWN"
	}
}
