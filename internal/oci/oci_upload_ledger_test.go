package oci

import (
	"encoding/json"
	"testing"
)

func TestPositiveInt64AcceptsExactJSONNumbers(t *testing.T) {
	for _, test := range []struct {
		value json.Number
		want  int64
		valid bool
	}{
		{value: "9007199254740993", want: 9007199254740993, valid: true},
		{value: "1e1", want: 10, valid: true},
		{value: "10.0", want: 10, valid: true},
		{value: "9223372036854775807", want: 9223372036854775807, valid: true},
		{value: "0"},
		{value: "-1"},
		{value: "1.5"},
		{value: "9223372036854775808"},
		{value: "1e1000000"},
		{value: "1e+"},
	} {
		got, valid := positiveInt64(test.value)
		if valid != test.valid || (valid && got != test.want) {
			t.Errorf("positiveInt64(%q) = (%d, %t), want (%d, %t)", test.value, got, valid, test.want, test.valid)
		}
	}
}
