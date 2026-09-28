package content

import "testing"

func TestParseSingleByteRange(t *testing.T) {
	start, end, err := parseSingleByteRange("bytes=2-4", 11)
	if err != nil || start != 2 || end != 4 {
		t.Fatalf("byte range = %d-%d err=%v", start, end, err)
	}
	_, _, err = parseSingleByteRange("bytes=20-30", 11)
	if err != errUnsatisfiableRange {
		t.Fatalf("unsatisfiable range err = %v", err)
	}
}
