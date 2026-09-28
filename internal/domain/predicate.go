package domain

import (
	"encoding/json"
	"fmt"
	"reflect"
	"regexp"
	"time"
)

var predicatePathPattern = regexp.MustCompile(`^[A-Za-z0-9_-]+(?:\.[A-Za-z0-9_-]+)*$`)

var predicateOperators = map[string]struct{}{
	"=":        {},
	"!=":       {},
	"<":        {},
	"<=":       {},
	">":        {},
	">=":       {},
	"before":   {},
	"after":    {},
	"matches":  {},
	"contains": {},
	"in":       {},
	"not-in":   {},
	"exists":   {},
	"absent":   {},
}

// Validate checks the predicate path, operator, and literal before a policy is
// persisted.
func (predicate Predicate) Validate() error {
	if !predicatePathPattern.MatchString(predicate.Path) {
		return fmt.Errorf("%w: %q", ErrInvalidPredicatePath, predicate.Path)
	}
	if _, found := predicateOperators[predicate.Op]; !found {
		return fmt.Errorf("%w: %q", ErrInvalidPredicateOperator, predicate.Op)
	}

	switch predicate.Op {
	case "exists", "absent":
		if predicate.Value != nil {
			return fmt.Errorf("%w: %s does not accept a value", ErrInvalidPredicateValue, predicate.Op)
		}
	case "matches":
		pattern, ok := predicate.Value.(string)
		if !ok {
			return fmt.Errorf("%w: matches requires a string", ErrInvalidPredicateValue)
		}
		if _, err := regexp.Compile(pattern); err != nil {
			return fmt.Errorf("%w: %v", ErrInvalidPredicatePattern, err)
		}
	case "before", "after":
		value, ok := predicate.Value.(string)
		if !ok || !validPredicateTime(value) {
			return fmt.Errorf("%w: %s requires RFC3339 or a non-negative age", ErrInvalidPredicateDuration, predicate.Op)
		}
	case "in", "not-in":
		value := reflect.ValueOf(predicate.Value)
		if !value.IsValid() ||
			(value.Kind() != reflect.Array && value.Kind() != reflect.Slice) ||
			value.Len() == 0 {
			return fmt.Errorf("%w: %s requires a non-empty array", ErrInvalidPredicateValue, predicate.Op)
		}
	case "<", "<=", ">", ">=":
		if !isOrderedPredicateLiteral(predicate.Value) {
			return fmt.Errorf("%w: %s requires a number or string", ErrInvalidPredicateValue, predicate.Op)
		}
	case "contains":
		if predicate.Value == nil {
			return fmt.Errorf("%w: contains requires a value", ErrInvalidPredicateValue)
		}
	}
	return nil
}

func validPredicateTime(value string) bool {
	if _, err := time.Parse(time.RFC3339, value); err == nil {
		return true
	}
	duration, err := ParseRetentionDuration(value)
	return err == nil && duration >= 0
}

func isOrderedPredicateLiteral(value any) bool {
	if _, ok := value.(string); ok {
		return true
	}
	if _, ok := value.(json.Number); ok {
		return true
	}
	valueType := reflect.TypeOf(value)
	if valueType == nil {
		return false
	}
	switch valueType.Kind() {
	case reflect.Int, reflect.Int8, reflect.Int16, reflect.Int32, reflect.Int64,
		reflect.Uint, reflect.Uint8, reflect.Uint16, reflect.Uint32, reflect.Uint64,
		reflect.Float32, reflect.Float64:
		return true
	default:
		return false
	}
}
