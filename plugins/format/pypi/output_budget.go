package pypi

import (
	"errors"
	"net/url"

	"github.com/suxen-project/suxen/plugins/format/internal/jsonbudget"
)

// The root also accepts 128 MiB sources. A single output cap keeps the
// expanded project and root representations bounded without excluding a
// normal full PyPI root index.
const renderedIndexLimit = 128 << 20

var errIndexBudget = errors.New("rendered PyPI index exceeds 128 MiB")

type outputBudget struct{ remaining int64 }

func newOutputBudget() outputBudget { return outputBudget{remaining: renderedIndexLimit} }

func (b *outputBudget) charge(size int64) error {
	if size < 0 || size > b.remaining {
		return errIndexBudget
	}
	b.remaining -= size
	return nil
}

// jsonSize delegates exact JSON accounting to the shared built-in-format helper.
func jsonSize(value any, budget *outputBudget) error {
	size, err := jsonbudget.EncodedSize(value, budget.remaining)
	if errors.Is(err, jsonbudget.ErrLimit) {
		return errIndexBudget
	}
	if err != nil {
		return err
	}
	return budget.charge(size)
}

func jsonStringSize(value string, budget *outputBudget) error {
	size, err := jsonbudget.StringSize(value, budget.remaining)
	if errors.Is(err, jsonbudget.ErrLimit) {
		return errIndexBudget
	}
	if err != nil {
		return err
	}
	return budget.charge(size)
}

func htmlStringSize(value string, budget *outputBudget) error {
	for i := 0; i < len(value); i++ {
		size := int64(1)
		switch value[i] {
		case '&':
			size = 5
		case '<', '>':
			size = 4
		case '"':
			size = 5
		case '\'':
			size = 5
		}
		if err := budget.charge(size); err != nil {
			return err
		}
	}
	return nil
}

// escapedPathSize matches filePath's per-segment url.PathEscape work without
// producing the expanded path (and, crucially, repeated request-host URLs).
func escapedPathSize(path string) int64 {
	var size int64
	var widths [256]uint8
	for i := 0; i < len(path); i++ {
		if path[i] == '/' {
			size++
			continue
		}
		width := widths[path[i]]
		if width == 0 {
			width = uint8(len(url.PathEscape(path[i : i+1])))
			widths[path[i]] = width
		}
		size += int64(width)
	}
	return size
}

func resolveSourceLinkWithBudget(base *url.URL, baseLength int64, link string, budget *outputBudget) (string, bool, error) {
	reference, err := url.Parse(link)
	if err != nil {
		return "", false, nil
	}
	// Resolving a relative link can repeat a large HTML base href for every
	// entry. Reserve its worst-case size before ResolveReference allocates it.
	// Relative Unicode and reserved characters can become %XX escapes.
	maximum := 3 * int64(len(link))
	if !reference.IsAbs() {
		maximum += baseLength
	}
	if err := budget.charge(maximum); err != nil {
		return "", false, err
	}
	resolved := base.ResolveReference(reference)
	if (resolved.Scheme != "http" && resolved.Scheme != "https") || resolved.Host == "" {
		return "", false, nil
	}
	return resolved.String(), true, nil
}
