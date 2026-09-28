package provision

import (
	"errors"

	"github.com/suxen-project/suxen/internal/domain"
)

// ValidationError marks a provisioning failure whose detail is safe and useful
// to return to the caller. Storage and driver errors deliberately remain
// unmarked so the HTTP boundary can redact them.
type ValidationError struct{ Err error }

func (e *ValidationError) Error() string { return e.Err.Error() }
func (e *ValidationError) Unwrap() error { return e.Err }

func invalidInput(err error) error { return &ValidationError{Err: err} }

// IsValidationFailure distinguishes failed desired state from backend failures
// for startup retry decisions. The extra sentinels are deliberately separate
// from PublicError, whose list controls details exposed over HTTP.
func IsValidationFailure(err error) bool {
	if _, safe := PublicError(err); safe {
		return true
	}
	for _, known := range []error{
		domain.ErrInvalidBlobStoreConfig,
		domain.ErrInvalidBlobStoreDriver,
		domain.ErrInvalidBlobStoreIdentity,
		domain.ErrInvalidBlobStoreAttributes,
		domain.ErrBlobStoreConfigRequired,
		domain.ErrInvalidFormat,
		domain.ErrInvalidType,
		domain.ErrInvalidOCIEndpoint,
		domain.ErrOCIEndpointsNotSupported,
	} {
		if errors.Is(err, known) {
			return true
		}
	}
	return false
}

// PublicError returns only details explicitly classified as safe. Domain
// sentinels are returned verbatim, never with arbitrary wrapping text.
func PublicError(err error) (string, bool) {
	var validation *ValidationError
	if errors.As(err, &validation) {
		return validation.Error(), true
	}
	for _, known := range []error{
		domain.ErrNotFound,
		domain.ErrConflict,
		domain.ErrBlobStoreDefinitionImmutable,
		domain.ErrBlobStoreIdentityConflict,
		domain.ErrDefaultBlobStoreImmutable,
		domain.ErrActiveDrainTarget,
		domain.ErrBlobStoreInUse,
		domain.ErrRepositoryBlobStoreInUse,
		domain.ErrRepositoryInUseByGroup,
		domain.ErrRepositoryInUseByCleanupPolicy,
		domain.ErrRepositoryInUseByWebhook,
		domain.ErrImmutableRepositoryField,
		domain.ErrOCIEndpointConflict,
		domain.ErrNestedGroupMember,
	} {
		if errors.Is(err, known) {
			return known.Error(), true
		}
	}
	return "", false
}
