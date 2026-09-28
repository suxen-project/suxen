package npm

import (
	"errors"
	"fmt"

	"github.com/suxen-project/suxen/plugins/format/internal/jsonbudget"
)

// These caps mirror the content runtime's npm rendered and hosted source
// limits. Check before json.Marshal, which otherwise allocates the full output.
const (
	packumentRenderedLimit = 256 << 20
	hostedPackumentLimit   = 128 << 20
)

var errPackumentBudget = errors.New("npm packument exceeds rendered content limit")

func jsonEncodedSize(value any, limit int64) (int64, error) {
	size, err := jsonbudget.EncodedSize(value, limit)
	if errors.Is(err, jsonbudget.ErrLimit) {
		return 0, errPackumentBudget
	}
	if err != nil {
		return 0, fmt.Errorf("npm JSON size: %w", err)
	}
	return size, nil
}

func jsonStringSize(value string, limit int64) (int64, error) {
	size, err := jsonbudget.StringSize(value, limit)
	if errors.Is(err, jsonbudget.ErrLimit) {
		return 0, errPackumentBudget
	}
	return size, err
}
