// Package jsonbudget counts encoding/json output before allocating it.
package jsonbudget

import (
	"encoding/json"
	"errors"
	"fmt"
	"unicode/utf8"
)

var ErrLimit = errors.New("JSON output exceeds limit")

// encoding/json's representation of replacement characters differs between
// supported Go releases. Measure that constant once using the active encoder.
var invalidUTF8Size = func() int64 {
	encoded, _ := json.Marshal(string([]byte{0xff}))
	return int64(len(encoded) - 2)
}()

// EncodedSize supports the concrete JSON values produced by decoding with
// UseNumber and by the built-in npm and PyPI synthesizers.
func EncodedSize(value any, limit int64) (int64, error) {
	remaining := limit
	charge := func(size int64) error {
		if size < 0 || size > remaining {
			return ErrLimit
		}
		remaining -= size
		return nil
	}
	var count func(any) error
	count = func(value any) error {
		switch typed := value.(type) {
		case map[string]any:
			if typed == nil {
				return charge(4)
			}
			if err := charge(2); err != nil {
				return err
			}
			first := true
			for key, child := range typed {
				if !first {
					if err := charge(1); err != nil {
						return err
					}
				}
				first = false
				if err := count(key); err != nil {
					return err
				}
				if err := charge(1); err != nil {
					return err
				}
				if err := count(child); err != nil {
					return err
				}
			}
		case map[string]json.RawMessage:
			if typed == nil {
				return charge(4)
			}
			if err := charge(2); err != nil {
				return err
			}
			first := true
			for key, raw := range typed {
				if !first {
					if err := charge(1); err != nil {
						return err
					}
				}
				first = false
				if err := count(key); err != nil {
					return err
				}
				if err := charge(1); err != nil {
					return err
				}
				if err := count(raw); err != nil {
					return err
				}
			}
		case json.RawMessage:
			if typed == nil {
				return charge(4)
			}
			size, err := RawSize(typed, remaining)
			if err != nil {
				return err
			}
			return charge(size)
		case []map[string]any:
			if typed == nil {
				return charge(4)
			}
			if err := charge(2); err != nil {
				return err
			}
			for i, child := range typed {
				if i != 0 {
					if err := charge(1); err != nil {
						return err
					}
				}
				if err := count(child); err != nil {
					return err
				}
			}
		case []any:
			if typed == nil {
				return charge(4)
			}
			if err := charge(2); err != nil {
				return err
			}
			for i, child := range typed {
				if i != 0 {
					if err := charge(1); err != nil {
						return err
					}
				}
				if err := count(child); err != nil {
					return err
				}
			}
		case []string:
			if typed == nil {
				return charge(4)
			}
			if err := charge(2); err != nil {
				return err
			}
			for i, child := range typed {
				if i != 0 {
					if err := charge(1); err != nil {
						return err
					}
				}
				if err := count(child); err != nil {
					return err
				}
			}
		case string:
			size, err := StringSize(typed, remaining)
			if err != nil {
				return err
			}
			return charge(size)
		case nil:
			return charge(4)
		case bool:
			if typed {
				return charge(4)
			}
			return charge(5)
		case json.Number:
			// A decoded number serializes verbatim. Reserve before Marshal
			// validates it so a caller-supplied long number cannot allocate
			// past the output budget.
			length := len(typed)
			if length == 0 {
				length = 1 // encoding/json treats the zero Number value as 0
			}
			if err := charge(int64(length)); err != nil {
				return err
			}
			_, err := json.Marshal(typed)
			return err
		case float64, int, int64:
			encoded, err := json.Marshal(typed)
			if err != nil {
				return err
			}
			return charge(int64(len(encoded)))
		default:
			return fmt.Errorf("unsupported JSON value %T", value)
		}
		return nil
	}
	if err := count(value); err != nil {
		return 0, err
	}
	return limit - remaining, nil
}

// RawSize counts the compacted, HTML-escaped form that encoding/json emits
// for a valid RawMessage. Callers get RawMessages from a JSON decoder.
func RawSize(raw json.RawMessage, limit int64) (int64, error) {
	var size int64
	inString, escaped := false, false
	for i := 0; i < len(raw); {
		c := raw[i]
		width, encoded := 1, int64(1)
		if inString {
			switch {
			case escaped:
				escaped = false
			case c == '\\':
				escaped = true
			case c == '"':
				inString = false
			case c == '<' || c == '>' || c == '&':
				encoded = 6
			case c >= utf8.RuneSelf:
				r, n := utf8.DecodeRune(raw[i:])
				width = n
				if r == '\u2028' || r == '\u2029' {
					encoded = 6
				} else {
					encoded = int64(n)
				}
			}
		} else if c == '"' {
			inString = true
		} else if c == ' ' || c == '\t' || c == '\r' || c == '\n' {
			encoded = 0
		}
		if encoded > limit-size {
			return 0, ErrLimit
		}
		size += encoded
		i += width
	}
	return size, nil
}

// StringSize exactly matches encoding/json's default string escaping.
func StringSize(value string, limit int64) (int64, error) {
	if int64(len(value)) > limit || limit < 2 {
		return 0, ErrLimit
	}
	size := int64(2)
	for index := 0; index < len(value); {
		character := value[index]
		width, encoded := 1, int64(1)
		switch {
		case character == '\\' || character == '"' || character == '\b' || character == '\f' || character == '\n' || character == '\r' || character == '\t':
			encoded = 2
		case character < 0x20 || character == '<' || character == '>' || character == '&':
			encoded = 6
		case character >= utf8.RuneSelf:
			r, n := utf8.DecodeRuneInString(value[index:])
			width, encoded = n, int64(n)
			if r == utf8.RuneError && n == 1 {
				encoded = invalidUTF8Size
			} else if r == '\u2028' || r == '\u2029' {
				encoded = 6
			}
		}
		if encoded > limit-size {
			return 0, ErrLimit
		}
		size += encoded
		index += width
	}
	return size, nil
}
