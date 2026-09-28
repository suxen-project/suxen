package httpx

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/url"
	"strings"

	"github.com/suxen-project/suxen/internal/domain"
	spiformat "github.com/suxen-project/suxen/spi/format"
)

// ProblemDetails is the RFC 7807 body written for control-plane errors.
type ProblemDetails struct {
	Type   string `json:"type"`
	Title  string `json:"title"`
	Status int    `json:"status"`
	Detail string `json:"detail"`
	Code   string `json:"code"`
}

type problemMapping struct {
	target error
	status int
	code   string
}

var domainProblemMappings = []problemMapping{
	{domain.ErrNotFound, http.StatusNotFound, "not_found"},
	{domain.ErrConflict, http.StatusConflict, "conflict"},
	{domain.ErrManaged, http.StatusConflict, "managed_resource"},
	{domain.ErrInvalidUsername, http.StatusBadRequest, "invalid_username"},
	{domain.ErrInvalidAssetPath, http.StatusBadRequest, "invalid_path"},
	{domain.ErrPasswordRequired, http.StatusBadRequest, "password_required"},
	{domain.ErrReservedRepositoryName, http.StatusBadRequest, "reserved_repository_name"},
	{domain.ErrNestedRolesUnsupported, http.StatusBadRequest, "nested_roles_unsupported"},
	{domain.ErrRoleReferencedByProvider, http.StatusConflict, "role_in_use_by_provider"},
	{domain.ErrReadOnly, http.StatusMethodNotAllowed, "read_only"},
	{domain.ErrDigestMismatch, http.StatusBadRequest, "digest_mismatch"},
	{domain.ErrProvenanceRejected, http.StatusForbidden, "provenance_rejected"},
	{domain.ErrInvalidRepository, http.StatusBadRequest, "invalid_repository"},
	{domain.ErrInvalidFormat, http.StatusBadRequest, "invalid_format"},
	{domain.ErrInvalidType, http.StatusBadRequest, "invalid_type"},
	{domain.ErrInvalidOverwritePolicy, http.StatusBadRequest, "invalid_overwrite_policy"},
	{domain.ErrInvalidBlobStore, http.StatusBadRequest, "invalid_blob_store"},
	{domain.ErrInvalidBlobStoreDriver, http.StatusBadRequest, "invalid_blob_store_driver"},
	{domain.ErrInvalidBlobStoreConfig, http.StatusBadRequest, "invalid_blob_store_config"},
	{domain.ErrInvalidBlobStoreAttributes, http.StatusBadRequest, "invalid_blob_store_attributes"},
	{domain.ErrBlobStoreConfigRequired, http.StatusBadRequest, "blob_store_config_required"},
	{domain.ErrBlobStoreInUse, http.StatusConflict, "blob_store_in_use"},
	{domain.ErrBlobStoreDefinitionImmutable, http.StatusConflict, "blob_store_definition_immutable"},
	{domain.ErrBlobStoreIdentityConflict, http.StatusConflict, "blob_store_identity_conflict"},
	{domain.ErrInvalidBlobStoreIdentity, http.StatusBadRequest, "invalid_blob_store_identity"},
	{domain.ErrDefaultBlobStoreImmutable, http.StatusConflict, "default_blob_store_immutable"},
	{domain.ErrBlobStoreDraining, http.StatusConflict, "blob_store_draining"},
	{domain.ErrBlobStoreNotDrainable, http.StatusConflict, "blob_store_not_drainable"},
	{domain.ErrInvalidDrainTarget, http.StatusBadRequest, "invalid_drain_target"},
	{domain.ErrActiveDrainTarget, http.StatusConflict, "active_drain_target"},
	{domain.ErrRepositoryBlobStoreInUse, http.StatusConflict, "repository_blob_store_in_use"},
	{domain.ErrBlobStoreConfigChanged, http.StatusConflict, "blob_store_config_changed"},
	{domain.ErrUpstreamRequired, http.StatusBadRequest, "upstream_required"},
	{domain.ErrInvalidUpstream, http.StatusBadRequest, "invalid_upstream"},
	{domain.ErrMembersRequired, http.StatusBadRequest, "members_required"},
	{domain.ErrImmutableRepositoryField, http.StatusConflict, "immutable_repository_field"},
	{domain.ErrNestedGroupMember, http.StatusBadRequest, "nested_group_member"},
	{domain.ErrRepositoryInUseByGroup, http.StatusConflict, "repository_in_use_by_group"},
	{domain.ErrRepositoryInUseByCleanupPolicy, http.StatusConflict, "repository_in_use_by_cleanup_policy"},
	{domain.ErrRepositoryInUseByWebhook, http.StatusConflict, "repository_in_use_by_webhook"},
	{domain.ErrRepositoryHasUploadSessions, http.StatusConflict, "repository_has_upload_sessions"},
	{domain.ErrOCIEndpointsNotSupported, http.StatusBadRequest, "oci_endpoints_not_supported"},
	{domain.ErrInvalidOCIEndpoint, http.StatusBadRequest, "invalid_oci_endpoint"},
	{domain.ErrOCIEndpointConflict, http.StatusConflict, "oci_endpoint_conflict"},
	{domain.ErrInvalidRole, http.StatusBadRequest, "invalid_role"},
	{domain.ErrInvalidPrivilege, http.StatusBadRequest, "invalid_privilege"},
	{domain.ErrInvalidOIDCProvider, http.StatusBadRequest, "invalid_oidc_provider"},
	{domain.ErrInvalidOIDCIssuer, http.StatusBadRequest, "invalid_oidc_issuer"},
	{domain.ErrOIDCClientIDRequired, http.StatusBadRequest, "oidc_client_id_required"},
	{domain.ErrClassificationKeyRequired, http.StatusBadRequest, "classification_key_required"},
	{domain.ErrClassificationValueRequired, http.StatusBadRequest, "classification_value_required"},
	{domain.ErrInvalidCleanupPolicy, http.StatusBadRequest, "invalid_cleanup_policy"},
	{domain.ErrCleanupRepositoriesRequired, http.StatusBadRequest, "cleanup_repositories_required"},
	{domain.ErrCleanupCriteriaRequired, http.StatusBadRequest, "cleanup_criteria_required"},
	{domain.ErrInvalidKeepLast, http.StatusBadRequest, "invalid_keep_last"},
	{domain.ErrInvalidCleanupAction, http.StatusBadRequest, "invalid_cleanup_action"},
	{domain.ErrInvalidPredicatePath, http.StatusBadRequest, "invalid_predicate_path"},
	{domain.ErrInvalidPredicateOperator, http.StatusBadRequest, "invalid_predicate_operator"},
	{domain.ErrInvalidPredicateValue, http.StatusBadRequest, "invalid_predicate_value"},
	{domain.ErrInvalidPredicatePattern, http.StatusBadRequest, "invalid_predicate_pattern"},
	{domain.ErrInvalidPredicateDuration, http.StatusBadRequest, "invalid_predicate_duration"},
	{domain.ErrInvalidWebhook, http.StatusBadRequest, "invalid_webhook"},
	{domain.ErrInvalidWebhookURL, http.StatusBadRequest, "invalid_webhook_url"},
	{domain.ErrWebhookSecretRequired, http.StatusBadRequest, "webhook_secret_required"},
	{domain.ErrWebhookEventsRequired, http.StatusBadRequest, "webhook_events_required"},
	{domain.ErrInvalidWebhookEvent, http.StatusBadRequest, "invalid_webhook_event"},
	{domain.ErrInvalidDownloadGate, http.StatusBadRequest, "invalid_download_gate"},
	{domain.ErrInvalidTrustPolicy, http.StatusBadRequest, "invalid_trust_policy"},
	{domain.ErrTrustMaterialRequired, http.StatusBadRequest, "trust_material_required"},
	{domain.ErrInvalidTrustMaterial, http.StatusBadRequest, "invalid_trust_material"},
	{domain.ErrInvalidTrustIdentity, http.StatusBadRequest, "invalid_trust_identity"},
}

// DecodeJSON reads a single JSON object of at most 1 MiB into destination.
func DecodeJSON(w http.ResponseWriter, r *http.Request, destination any) bool {
	defer r.Body.Close()

	const maximumJSONBodyBytes = 1 << 20
	body, err := io.ReadAll(io.LimitReader(r.Body, maximumJSONBodyBytes+1))
	if err != nil {
		WriteProblem(w, http.StatusBadRequest, "invalid_json", err.Error())
		return false
	}
	if len(body) > maximumJSONBodyBytes {
		WriteProblem(
			w,
			http.StatusBadRequest,
			"invalid_json",
			"request body exceeds the 1 MiB JSON limit",
		)
		return false
	}

	decoder := json.NewDecoder(bytes.NewReader(body))
	var value json.RawMessage
	if err := decoder.Decode(&value); err != nil {
		WriteProblem(w, http.StatusBadRequest, "invalid_json", err.Error())
		return false
	}
	if bytes.Equal(bytes.TrimSpace(value), []byte("null")) {
		WriteProblem(w, http.StatusBadRequest, "invalid_json", "request body must not be null")
		return false
	}
	var trailing json.RawMessage
	if err := decoder.Decode(&trailing); !errors.Is(err, io.EOF) {
		if err == nil {
			err = errors.New("request body must contain exactly one JSON value")
		}
		WriteProblem(w, http.StatusBadRequest, "invalid_json", err.Error())
		return false
	}

	decoder = json.NewDecoder(bytes.NewReader(value))
	decoder.DisallowUnknownFields()
	decoder.UseNumber()
	if err := decoder.Decode(destination); err != nil {
		WriteProblem(w, http.StatusBadRequest, "invalid_json", err.Error())
		return false
	}
	return true
}

// WriteResult writes value or maps err onto a problem / OCI envelope.
func WriteResult(w http.ResponseWriter, value any, err error) {
	if err == nil {
		WriteJSON(w, http.StatusOK, value)
		return
	}

	var violation *spiformat.PolicyViolation
	if errors.As(err, &violation) {
		code := violation.Code
		if code == "" {
			code = "format_policy"
		}
		WriteProblem(w, http.StatusBadRequest, code, violation.Message)
		return
	}

	for _, mapping := range domainProblemMappings {
		if errors.Is(err, mapping.target) {
			WriteProblem(w, mapping.status, mapping.code, err.Error())
			return
		}
	}

	WriteServerProblem(
		w,
		http.StatusInternalServerError,
		"internal_error",
		"internal server error",
		err,
	)
}

// WriteCreated writes a 201 response with a Location header.
func WriteCreated(w http.ResponseWriter, resourcePath string, value any) {
	if parsed, err := url.Parse(resourcePath); err == nil {
		w.Header().Set("Location", parsed.EscapedPath())
	} else {
		w.Header().Set("Location", resourcePath)
	}
	WriteJSON(w, http.StatusCreated, value)
}

// WriteJSON encodes value as JSON.
func WriteJSON(w http.ResponseWriter, status int, value any) {
	if w.Header().Get("Content-Type") == "" {
		w.Header().Set("Content-Type", "application/json")
	}
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(value)
}

// WriteProblem writes problem+json or an OCI error envelope, depending on format.
func WriteProblem(w http.ResponseWriter, status int, code, message string) {
	if currentErrorResponseFormat(w) == ErrorResponseFormatOCI {
		WriteOCIError(w, status, OCIErrorCode(status, code), message)
		return
	}

	w.Header().Set("Content-Type", "application/problem+json")
	WriteJSON(w, status, ProblemDetails{
		Type:   "urn:suxen:problem:" + code,
		Title:  problemTitle(code),
		Status: status,
		Detail: message,
		Code:   code,
	})
}

// WriteServerProblem logs err internally then writes a public problem response.
func WriteServerProblem(
	w http.ResponseWriter,
	status int,
	code string,
	detail string,
	err error,
) {
	reportInternalError(w, err)
	WriteProblem(w, status, code, detail)
}

func problemTitle(code string) string {
	title := strings.ReplaceAll(code, "_", " ")
	if title == "" {
		return "Request failed"
	}
	return strings.ToUpper(title[:1]) + title[1:]
}

// MethodNotAllowed writes a 405 with an Allow header.
func MethodNotAllowed(w http.ResponseWriter, methods ...string) {
	w.Header().Set("Allow", strings.Join(methods, ", "))
	WriteProblem(
		w,
		http.StatusMethodNotAllowed,
		"method_not_allowed",
		"method not allowed",
	)
}

// AllowReadOnlyMethod accepts GET/HEAD or writes 405.
func AllowReadOnlyMethod(w http.ResponseWriter, r *http.Request) bool {
	if r.Method == http.MethodGet || r.Method == http.MethodHead {
		return true
	}
	MethodNotAllowed(w, http.MethodGet, http.MethodHead)
	return false
}

// SplitPath splits a URL path into non-empty segments.
func SplitPath(value string) []string {
	rawParts := strings.Split(strings.Trim(value, "/"), "/")
	parts := make([]string, 0, len(rawParts))
	for _, part := range rawParts {
		if part != "" {
			parts = append(parts, part)
		}
	}
	return parts
}
