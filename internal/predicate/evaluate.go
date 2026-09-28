// Package predicate evaluates policy predicates against projected asset
// attributes.
package predicate

import (
	"reflect"
	"regexp"
	"strings"
	"time"

	"github.com/suxen-project/suxen/internal/assetattrs"
	"github.com/suxen-project/suxen/internal/domain"
)

// Match reports whether attributes satisfy one validated predicate. Invalid
// operators and incompatible runtime value types fail closed.
func Match(attributes map[string]any, condition domain.Predicate, now time.Time) bool {
	actual, found := assetattrs.Lookup(attributes, condition.Path)
	if !found {
		return condition.Op == "absent" || condition.Op == "not-in" && !invalidNumber(condition.Value)
	}

	switch condition.Op {
	case "exists":
		return true
	case "absent":
		return false
	case "=":
		equal, comparable := valuesEqual(actual, condition.Value)
		return comparable && equal
	case "!=":
		equal, comparable := valuesEqual(actual, condition.Value)
		return comparable && !equal
	case "<", "<=", ">", ">=":
		comparison, comparable := compareValues(actual, condition.Value)
		return comparable && orderedComparisonMatches(comparison, condition.Op)
	case "before", "after":
		actualTime, ok := predicateTime(actual)
		if !ok {
			return false
		}
		cutoff, ok := predicateCutoff(condition.Value, now)
		if !ok {
			return false
		}
		if condition.Op == "before" {
			return actualTime.Before(cutoff)
		}
		return actualTime.After(cutoff)
	case "matches":
		actualString, actualOK := actual.(string)
		pattern, valueOK := condition.Value.(string)
		if !actualOK || !valueOK {
			return false
		}
		compiled, err := regexp.Compile(pattern)
		return err == nil && compiled.MatchString(actualString)
	case "contains":
		return contains(actual, condition.Value)
	case "in", "not-in":
		contained, valid := in(actual, condition.Value)
		if !valid {
			return false
		}
		if condition.Op == "not-in" {
			return !contained
		}
		return contained
	default:
		return false
	}
}

// MatchAll reports whether attributes satisfy every predicate. Empty criteria
// match by convention; resource validators decide where empty criteria are
// permitted.
func MatchAll(attributes map[string]any, criteria []domain.Predicate, now time.Time) bool {
	for _, condition := range criteria {
		if !Match(attributes, condition, now) {
			return false
		}
	}
	return true
}

func valuesEqual(left any, right any) (bool, bool) {
	if invalidNumber(left) || invalidNumber(right) {
		return false, false
	}
	return valuesEqualValid(left, right)
}

func valuesEqualValid(left any, right any) (bool, bool) {
	leftNumber, leftNumeric, _ := number(left)
	rightNumber, rightNumeric, _ := number(right)
	if leftNumeric || rightNumeric {
		return leftNumeric && rightNumeric && leftNumber.Compare(rightNumber) == 0, leftNumeric && rightNumeric
	}
	if reflect.TypeOf(left) != reflect.TypeOf(right) {
		return false, false
	}
	// JSON numbers can have different Go representations and literal spellings.
	// Compare each value in JSON containers with the same rules as scalar values.
	switch typed := left.(type) {
	case []any:
		other := right.([]any)
		equal := (typed == nil) == (other == nil) && len(typed) == len(other)
		comparable := true
		for index, item := range typed {
			if index >= len(other) {
				break
			}
			itemEqual, itemComparable := valuesEqualValid(item, other[index])
			equal = equal && itemEqual
			comparable = comparable && itemComparable
		}
		return equal, comparable
	case map[string]any:
		other := right.(map[string]any)
		equal := (typed == nil) == (other == nil) && len(typed) == len(other)
		comparable := true
		for key, item := range typed {
			candidate, found := other[key]
			if !found {
				equal = false
				continue
			}
			itemEqual, itemComparable := valuesEqualValid(item, candidate)
			equal = equal && itemEqual
			comparable = comparable && itemComparable
		}
		return equal, comparable
	}
	return reflect.DeepEqual(left, right), true
}

func compareValues(left any, right any) (int, bool) {
	leftNumber, leftNumeric, leftValid := number(left)
	rightNumber, rightNumeric, rightValid := number(right)
	if leftNumeric || rightNumeric {
		if !leftNumeric || !rightNumeric || !leftValid || !rightValid {
			return 0, false
		}
		return leftNumber.Compare(rightNumber), true
	}
	leftString, leftOK := left.(string)
	rightString, rightOK := right.(string)
	if !leftOK || !rightOK {
		return 0, false
	}
	switch {
	case leftString < rightString:
		return -1, true
	case leftString > rightString:
		return 1, true
	default:
		return 0, true
	}
}

func orderedComparisonMatches(comparison int, operator string) bool {
	switch operator {
	case "<":
		return comparison < 0
	case "<=":
		return comparison <= 0
	case ">":
		return comparison > 0
	case ">=":
		return comparison >= 0
	default:
		return false
	}
}

func predicateTime(value any) (time.Time, bool) {
	switch typed := value.(type) {
	case time.Time:
		return typed, true
	case string:
		parsed, err := time.Parse(time.RFC3339, typed)
		return parsed, err == nil
	default:
		return time.Time{}, false
	}
}

func predicateCutoff(value any, now time.Time) (time.Time, bool) {
	text, ok := value.(string)
	if !ok {
		return time.Time{}, false
	}
	if absolute, err := time.Parse(time.RFC3339, text); err == nil {
		return absolute, true
	}
	age, err := domain.ParseRetentionDuration(text)
	if err != nil || age < 0 {
		return time.Time{}, false
	}
	return now.Add(-age), true
}

func contains(actual any, expected any) bool {
	if invalidNumber(actual) || invalidNumber(expected) {
		return false
	}
	if actualString, ok := actual.(string); ok {
		expectedString, ok := expected.(string)
		return ok && strings.Contains(actualString, expectedString)
	}
	value := reflect.ValueOf(actual)
	if !value.IsValid() || (value.Kind() != reflect.Array && value.Kind() != reflect.Slice) {
		return false
	}
	for index := 0; index < value.Len(); index++ {
		equal, comparable := valuesEqualValid(value.Index(index).Interface(), expected)
		if comparable && equal {
			return true
		}
	}
	return false
}

func in(actual any, choices any) (bool, bool) {
	if invalidNumber(actual) || invalidNumber(choices) {
		return false, false
	}
	value := reflect.ValueOf(choices)
	if !value.IsValid() || (value.Kind() != reflect.Array && value.Kind() != reflect.Slice) {
		return false, false
	}
	comparable := false
	for index := 0; index < value.Len(); index++ {
		equal, itemComparable := valuesEqualValid(actual, value.Index(index).Interface())
		comparable = comparable || itemComparable
		if itemComparable && equal {
			return true, true
		}
	}
	return false, comparable
}
