package content

import (
	"errors"
	"fmt"
	"net/http"
	"testing"

	"github.com/suxen-project/suxen/internal/domain"
	spiformat "github.com/suxen-project/suxen/spi/format"
)

func TestWireToolErrorCategoriesPreserveCauses(t *testing.T) {
	for _, test := range []struct {
		name, category string
		cause          error
		want           error
	}{
		{"conflict", "conflict", fmt.Errorf("persist: %w", domain.ErrConflict), spiformat.ErrConflict},
		{"policy", "policy", fmt.Errorf("verify: %w", domain.ErrProvenanceRejected), spiformat.ErrPolicyRejected},
		{"plugin policy", "policy", &spiformat.PolicyViolation{Code: "test", Message: "rejected"}, spiformat.ErrPolicyRejected},
		{"limit", "limit", fmt.Errorf("stage: %w", &http.MaxBytesError{Limit: 4}), spiformat.ErrUploadLimit},
	} {
		t.Run(test.name, func(t *testing.T) {
			got := wireToolError(test.cause)
			if !errors.Is(got, test.want) || !errors.Is(got, test.cause) {
				t.Fatalf("category %s did not retain both identities: %v", test.category, got)
			}
		})
	}
	unknown := errors.New("database unavailable")
	if got := wireToolError(unknown); got != unknown || errors.Is(got, spiformat.ErrConflict) {
		t.Fatalf("unknown infrastructure error was reclassified: %v", got)
	}
}
