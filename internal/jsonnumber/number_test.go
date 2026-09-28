package jsonnumber

import (
	"encoding/json"
	"math/big"
	"math/rand"
	"strconv"
	"strings"
	"testing"
)

func TestInt64(t *testing.T) {
	for _, trial := range []struct {
		literal string
		want    int64
		ok      bool
	}{
		{"0", 0, true},
		{"-0e-" + strings.Repeat("9", 200), 0, true},
		{"1e1", 10, true},
		{"10.0", 10, true},
		{"1.000e0", 1, true},
		{"100e-2", 1, true},
		{"10000000000000000000e-1", 1000000000000000000, true},
		{"9223372036854775807", 9223372036854775807, true},
		{"-9223372036854775808", -9223372036854775808, true},
		{"9223372036854775808", 0, false},
		{"-9223372036854775809", 0, false},
		{"1.2", 0, false},
		{"1e-1", 0, false},
		{"1e1000000", 0, false},
		{"1e" + strings.Repeat("9", 200), 0, false},
		{"1e-" + strings.Repeat("9", 200), 0, false},
	} {
		number, parsed := Parse(trial.literal)
		if !parsed {
			t.Fatalf("valid number rejected: %q", trial.literal)
		}
		got, ok := number.Int64()
		if got != trial.want || ok != trial.ok {
			t.Errorf("%q Int64 = (%d, %t), want (%d, %t)", trial.literal, got, ok, trial.want, trial.ok)
		}
	}
}

func TestDecimalNumberBoundaryCases(t *testing.T) {
	longExponent := strings.Repeat("9", 300)
	for _, trial := range []struct {
		left, right string
		want        int
	}{
		{"1e1000000", "10e999999", 0},
		{"1e-1000000", "0.1e-999999", 0},
		{"1e9223372036854775808", "10e9223372036854775807", 0},
		{"1e-9223372036854775809", "0.1e-9223372036854775808", 0},
		{"1e" + longExponent, "10e" + longExponent[:299] + "8", 0},
		{"1e-" + longExponent, "0.1e-" + longExponent[:299] + "8", 0},
		{"-0e" + longExponent, "0e-" + longExponent, 0},
		{"1e+0000010", "10000000000", 0},
		{"9.999e99", "1e100", -1},
		{"1.0001e-100", "1e-100", 1},
		{"-1e100", "-9e99", -1},
		{"1.2e-1", "0.12", 0},
		{"0.00001200e+5", "1.2", 0},
	} {
		left, leftOK := Parse(trial.left)
		right, rightOK := Parse(trial.right)
		if !leftOK || !rightOK {
			t.Fatalf("valid pair rejected: %q, %q", trial.left, trial.right)
		}
		if got := left.Compare(right); got != trial.want {
			t.Errorf("compare %q and %q = %d, want %d", trial.left, trial.right, got, trial.want)
		}
	}
}

func TestDecimalNumberMatchesRationalOracle(t *testing.T) {
	random := rand.New(rand.NewSource(1))
	makeLiteral := func() string {
		var result strings.Builder
		if random.Intn(2) == 0 {
			result.WriteByte('-')
		}
		result.WriteByte(byte('0' + random.Intn(10)))
		if random.Intn(2) == 0 {
			result.WriteByte('.')
			for count := random.Intn(30) + 1; count > 0; count-- {
				result.WriteByte(byte('0' + random.Intn(10)))
			}
		}
		if random.Intn(2) == 0 {
			result.WriteByte('e')
			result.WriteString(big.NewInt(int64(random.Intn(201) - 100)).String())
		}
		return result.String()
	}
	for index := 0; index < 2000; index++ {
		leftLiteral, rightLiteral := makeLiteral(), makeLiteral()
		left, leftOK := Parse(leftLiteral)
		right, rightOK := Parse(rightLiteral)
		if !leftOK || !rightOK {
			t.Fatalf("generated pair rejected: %q, %q", leftLiteral, rightLiteral)
		}
		leftRat, leftOK := new(big.Rat).SetString(leftLiteral)
		rightRat, rightOK := new(big.Rat).SetString(rightLiteral)
		if !leftOK || !rightOK {
			t.Fatalf("oracle rejected generated pair: %q, %q", leftLiteral, rightLiteral)
		}
		if got, want := left.Compare(right), leftRat.Cmp(rightRat); got != want {
			t.Fatalf("compare %q and %q = %d, oracle %d", leftLiteral, rightLiteral, got, want)
		}
	}
}

func TestParseDecimalUsesJSONNumberGrammar(t *testing.T) {
	for _, literal := range []string{"", "+1", "01", "-01", ".1", "1.", "1e", "1e+", "1/2", "NaN", "Infinity", "1_0", "1e2x", " 1", "1 ", "0x10"} {
		if _, ok := Parse(literal); ok {
			t.Errorf("accepted invalid JSON number %q", literal)
		}
	}
	for _, literal := range []string{"0", "-0", "1", "-1.2", "0.1", "1e+001", "1E-1000"} {
		if _, ok := Parse(literal); !ok {
			t.Errorf("rejected valid JSON number %q", literal)
		}
	}
}

func BenchmarkDecimalNumberLargeExponent(b *testing.B) {
	for _, literal := range []string{"1e1000000", "1e" + strings.Repeat("9", 1000), strings.Repeat("9", 1000) + "e-1000000"} {
		b.Run(literal[:min(len(literal), 20)], func(b *testing.B) {
			for index := 0; index < b.N; index++ {
				left, _ := Parse(literal)
				right, _ := Parse("2")
				_ = left.Compare(right)
			}
		})
	}
}

func FuzzDecimalNumber(f *testing.F) {
	for _, pair := range [][2]string{
		{"1e1000000", "10e999999"},
		{"1e-9223372036854775809", "0.1e-9223372036854775808"},
		{"-0e999999999999999999999", "0"},
		{"1.00", "1"},
		{"01", "1"},
		{"1e+", "1"},
		{"0", "0 "},
	} {
		f.Add(pair[0], pair[1])
	}
	f.Fuzz(func(t *testing.T, leftLiteral, rightLiteral string) {
		if len(leftLiteral) > 256 || len(rightLiteral) > 256 {
			return
		}
		left, leftOK := Parse(leftLiteral)
		right, rightOK := Parse(rightLiteral)
		isNumber := func(literal string) bool {
			return len(literal) > 0 && literal == strings.TrimSpace(literal) && (literal[0] == '-' || literal[0] >= '0' && literal[0] <= '9') && json.Valid([]byte(literal))
		}
		if leftOK != isNumber(leftLiteral) || rightOK != isNumber(rightLiteral) {
			t.Fatalf("JSON grammar disagreement for %q, %q", leftLiteral, rightLiteral)
		}
		if !leftOK || !rightOK {
			return
		}
		if left.Compare(right) != -right.Compare(left) || left.Compare(left) != 0 {
			t.Fatalf("comparison is not antisymmetric for %q, %q", leftLiteral, rightLiteral)
		}
		bounded := func(literal string) bool {
			index := strings.IndexAny(literal, "eE")
			if index < 0 {
				return true
			}
			exponent, err := strconv.Atoi(literal[index+1:])
			return err == nil && exponent >= -100 && exponent <= 100
		}
		if !bounded(leftLiteral) || !bounded(rightLiteral) {
			return
		}
		leftRat, leftRatOK := new(big.Rat).SetString(leftLiteral)
		rightRat, rightRatOK := new(big.Rat).SetString(rightLiteral)
		if !leftRatOK || !rightRatOK || left.Compare(right) != leftRat.Cmp(rightRat) {
			t.Fatalf("rational oracle disagreement for %q, %q", leftLiteral, rightLiteral)
		}
	})
}
