package jsonbudget

import (
	"bytes"
	"encoding/json"
	"errors"
	"math"
	"strings"
	"testing"
)

func assertMatchesMarshal(t *testing.T, value any) {
	t.Helper()
	encoded, err := json.Marshal(value)
	if err != nil {
		t.Fatal(err)
	}
	want := int64(len(encoded))
	for _, limit := range []int64{want, want + 19} {
		got, err := EncodedSize(value, limit)
		if err != nil || got != want {
			t.Fatalf("%T (%q): size(%d) = %d, %v; Marshal size = %d", value, encoded, limit, got, err, want)
		}
	}
	if _, err := EncodedSize(value, want-1); !errors.Is(err, ErrLimit) {
		t.Fatalf("%T (%q): one-byte-short budget error = %v", value, encoded, err)
	}
}

func TestEncodedSizeMatchesJSONAtExactBoundary(t *testing.T) {
	allBytes := make([]byte, 256)
	for i := range allBytes {
		allBytes[i] = byte(i)
	}
	values := []any{
		nil, true, false, "", string(allBytes), "世界\u2028\u2029😀",
		int(-42), int64(math.MinInt64), math.Copysign(0, -1), 1e-9, 1e21,
		json.Number(""), json.Number("-0"), json.Number("1.000e+100000"),
		json.Number(strings.Repeat("9", 4096)),
		map[string]any(nil), map[string]any{},
		[]any(nil), []any{}, []string(nil), []string{"", "<&>"},
		[]map[string]any(nil), []map[string]any{{"file": "a.whl", "size": int64(100)}},
		map[string]any{"<&>\"": []any{nil, true, "\xff", json.Number("9e99")}},
		json.RawMessage(nil), json.RawMessage(` { "escaped": "\u2028\\\"", "html": "<>&" } `),
		json.RawMessage("{\"raw\":\"\u2028\u2029\xff\"}"),
		map[string]json.RawMessage(nil), map[string]json.RawMessage{},
		map[string]json.RawMessage{"<&>": json.RawMessage(" [ 1, \"<>&\u2028\" ] "), "null": nil},
	}
	for _, value := range values {
		assertMatchesMarshal(t, value)
	}
}

func TestEncodedSizeRejectsInvalidNumericValues(t *testing.T) {
	for _, value := range []any{json.Number("01"), json.Number("NaN"), math.Inf(1), math.NaN()} {
		if _, err := EncodedSize(value, 1024); err == nil {
			t.Errorf("accepted invalid numeric value %v", value)
		}
	}
}

// Compare against the serializer rather than a second copy of the counting
// algorithm, including unknown extension fields retained as RawMessage.
func FuzzEncodedSizeMatchesJSON(f *testing.F) {
	for _, seed := range []string{
		`null`, `[]`, `{}`, `{"files":[{"url":"https://host/a?x=1&y=2","size":123}]}`,
		`{"extension":{"value":"<>&\\\"","number":1.234e+100}}`,
		"{\"unicode\":\"\u2028\u2029\xff\"}",
	} {
		f.Add([]byte(seed))
	}
	f.Fuzz(func(t *testing.T, source []byte) {
		if len(source) > 64<<10 || !json.Valid(source) {
			return
		}
		decoder := json.NewDecoder(bytes.NewReader(source))
		decoder.UseNumber()
		var decoded any
		if err := decoder.Decode(&decoded); err != nil {
			t.Fatal(err)
		}
		assertMatchesMarshal(t, decoded)
		assertMatchesMarshal(t, json.RawMessage(source))
	})
}
