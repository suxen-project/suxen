package oci

import (
	"errors"
	"io"
	"net/http/httptest"
	"strings"
	"testing"
	"testing/iotest"
)

func TestParseContentRange(t *testing.T) {
	tests := []struct {
		name   string
		header string
		start  int64
		end    int64
		valid  bool
	}{
		{name: "OCI numeric", header: "0-4", start: 0, end: 4, valid: true},
		{name: "OCI numeric with total", header: "5-9/10", start: 5, end: 9, valid: true},
		{name: "legacy bytes", header: "bytes 0-4/*", start: 0, end: 4, valid: true},
		{name: "legacy case insensitive", header: "BYTES 5-9", start: 5, end: 9, valid: true},
		{name: "wrong unit", header: "items 0-4", valid: false},
		{name: "missing end", header: "0-", valid: false},
		{name: "backwards", header: "4-0", valid: false},
		{name: "extra field", header: "bytes 0-4 extra", valid: false},
		{name: "overflowing end", header: "0-9223372036854775807", valid: false},
		{name: "invalid total", header: "0-4/nonsense", valid: false},
		{name: "missing total", header: "0-4/", valid: false},
		{name: "total before end", header: "0-4/4", valid: false},
		{name: "multiple totals", header: "0-4/5/6", valid: false},
		{name: "signed start", header: "+0-4", valid: false},
		{name: "signed end", header: "0-+4", valid: false},
		{name: "signed total", header: "0-4/+5", valid: false},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			start, end, err := parseOCIContentRange(test.header)
			if test.valid && (err != nil || start != test.start || end != test.end) {
				t.Fatalf("content range = %d-%d err=%v", start, end, err)
			}
			if !test.valid && err == nil {
				t.Fatalf("parseOCIContentRange(%q) unexpectedly succeeded", test.header)
			}
		})
	}
}

func TestOCIRequestContentRangeRejectsDuplicateAndEmptyHeaders(t *testing.T) {
	for _, values := range [][]string{{"0-2", "0-2"}, {""}} {
		r := httptest.NewRequest("PATCH", "/", nil)
		for _, value := range values {
			r.Header.Add("Content-Range", value)
		}
		_, _, hasRange, err := ociRequestContentRange(r)
		if !hasRange || err == nil {
			t.Fatalf("Content-Range values %q = hasRange %t, error %v", values, hasRange, err)
		}
	}
}

func TestOCIUploadLengthReader(t *testing.T) {
	for _, test := range []struct {
		name     string
		body     string
		expected int64
		want     error
	}{
		{name: "exact", body: "abc", expected: 3, want: nil},
		{name: "short", body: "ab", expected: 3, want: errOCIUploadLengthMismatch},
		{name: "long", body: "abcd", expected: 3, want: errOCIUploadLengthMismatch},
		{name: "empty exact", body: "", expected: 0, want: nil},
		{name: "empty long", body: "a", expected: 0, want: errOCIUploadLengthMismatch},
	} {
		t.Run(test.name, func(t *testing.T) {
			reader := &ociUploadLengthReader{source: strings.NewReader(test.body), remaining: test.expected}
			_, err := io.Copy(io.Discard, reader)
			if !errors.Is(err, test.want) {
				t.Fatalf("io.Copy error = %v, want %v", err, test.want)
			}
		})
	}
	truncated := &ociUploadLengthReader{
		source:    io.MultiReader(strings.NewReader("ab"), unexpectedEOFReader{}),
		remaining: 3,
	}
	_, err := io.Copy(io.Discard, truncated)
	if !errors.Is(err, errOCIUploadLengthMismatch) || !errors.Is(err, io.ErrUnexpectedEOF) {
		t.Fatalf("truncated body error = %v, want length mismatch and unexpected EOF", err)
	}
	for _, test := range []struct {
		name      string
		expected  int64
		wantError error
	}{
		{name: "EOF with exact data", expected: 3},
		{name: "EOF with short data", expected: 4, wantError: errOCIUploadLengthMismatch},
	} {
		t.Run(test.name, func(t *testing.T) {
			reader := &ociUploadLengthReader{source: iotest.DataErrReader(strings.NewReader("abc")), remaining: test.expected}
			_, err := io.Copy(io.Discard, reader)
			if !errors.Is(err, test.wantError) {
				t.Fatalf("io.Copy error = %v, want %v", err, test.wantError)
			}
		})
	}
	sourceErr := errors.New("source failed")
	for _, expected := range []int64{1, 2} {
		reader := &ociUploadLengthReader{
			source:    dataErrorReader{data: "ab", err: sourceErr},
			remaining: expected,
		}
		_, err := io.Copy(io.Discard, reader)
		if !errors.Is(err, sourceErr) {
			t.Fatalf("expected %d: source error = %v, want %v", expected, err, sourceErr)
		}
	}
	extraWithError := &ociUploadLengthReader{
		source:    io.MultiReader(strings.NewReader("a"), dataErrorReader{data: "b", err: sourceErr}),
		remaining: 1,
	}
	_, err = io.Copy(io.Discard, extraWithError)
	if !errors.Is(err, errOCIUploadLengthMismatch) || !errors.Is(err, sourceErr) {
		t.Fatalf("extra byte source error = %v, want length mismatch and source error", err)
	}
}

type unexpectedEOFReader struct{}

func (unexpectedEOFReader) Read([]byte) (int, error) {
	return 0, io.ErrUnexpectedEOF
}

type dataErrorReader struct {
	data string
	err  error
}

func (reader dataErrorReader) Read(p []byte) (int, error) {
	return copy(p, reader.data), reader.err
}
