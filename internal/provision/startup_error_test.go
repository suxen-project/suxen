package provision

import (
	"errors"
	"testing"

	"github.com/suxen-project/suxen/internal/domain"
)

func TestReportFailurePreservesResourceCause(t *testing.T) {
	cause := errors.New("backend unavailable")
	report := Report{Results: []Result{{
		Kind: "role", Name: "reader", Status: StatusFailed,
		Error: cause.Error(), cause: cause,
	}}}
	if err := report.Failure(); !errors.Is(err, cause) {
		t.Fatalf("Failure() = %v, want wrapped cause", err)
	}
	if report.HasValidationFailure() {
		t.Fatal("backend failure was classified as validation")
	}
	report.Results[0].cause = domain.ErrInvalidBlobStoreConfig
	if !report.HasValidationFailure() {
		t.Fatal("invalid blob-store configuration was classified as a dependency outage")
	}
}
