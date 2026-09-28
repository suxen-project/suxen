package predicate

import (
	"encoding/json"
	"reflect"
	"strconv"

	"github.com/suxen-project/suxen/internal/jsonnumber"
)

// number returns whether value is a numeric Go type separately from whether
// its literal is valid. Invalid numeric values must never reach type or spelling
// equality as if they were ordinary nonnumeric values.
func number(value any) (jsonnumber.Number, bool, bool) {
	var literal string
	switch typed := value.(type) {
	case int:
		literal = strconv.FormatInt(int64(typed), 10)
	case int8:
		literal = strconv.FormatInt(int64(typed), 10)
	case int16:
		literal = strconv.FormatInt(int64(typed), 10)
	case int32:
		literal = strconv.FormatInt(int64(typed), 10)
	case int64:
		literal = strconv.FormatInt(typed, 10)
	case uint:
		literal = strconv.FormatUint(uint64(typed), 10)
	case uint8:
		literal = strconv.FormatUint(uint64(typed), 10)
	case uint16:
		literal = strconv.FormatUint(uint64(typed), 10)
	case uint32:
		literal = strconv.FormatUint(uint64(typed), 10)
	case uint64:
		literal = strconv.FormatUint(typed, 10)
	case float32:
		literal = strconv.FormatFloat(float64(typed), 'g', -1, 32)
	case float64:
		literal = strconv.FormatFloat(typed, 'g', -1, 64)
	case json.Number:
		literal = string(typed)
	default:
		return jsonnumber.Number{}, false, false
	}
	parsed, ok := jsonnumber.Parse(literal)
	return parsed, true, ok
}

func invalidNumber(value any) bool {
	_, numeric, valid := number(value)
	if numeric {
		return !valid
	}
	container := reflect.ValueOf(value)
	if !container.IsValid() {
		return false
	}
	switch container.Kind() {
	case reflect.Array, reflect.Slice:
		for index := 0; index < container.Len(); index++ {
			if invalidNumber(container.Index(index).Interface()) {
				return true
			}
		}
	case reflect.Map:
		iterator := container.MapRange()
		for iterator.Next() {
			if invalidNumber(iterator.Value().Interface()) {
				return true
			}
		}
	}
	return false
}
