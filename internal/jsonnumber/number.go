// Package jsonnumber parses and compares JSON numbers with work proportional
// to their literal length, including numbers with very large exponents.
package jsonnumber

import (
	"strconv"
	"strings"
)

// Number keeps the decimal point position separate from the digits.
// Its storage and comparison cost depend on literal length, never on the
// magnitude of an exponent such as 1e1000000.
type Number struct {
	sign   int
	digits string          // nonzero significant digits, with no leading zeroes
	order  decimalExponent // base-10 exponent of the first significant digit
}

// decimalExponent is a signed decimal integer. Keeping it in base ten makes
// even a very long exponent spelling linear to parse and compare.
type decimalExponent struct {
	negative bool
	digits   string // unsigned magnitude, no leading zeroes; zero is "0"
}

func (left decimalExponent) cmp(right decimalExponent) int {
	if left.negative != right.negative {
		if left.negative {
			return -1
		}
		return 1
	}
	comparison := compareDecimalDigits(left.digits, right.digits)
	if left.negative {
		return -comparison
	}
	return comparison
}

func compareDecimalDigits(left, right string) int {
	if len(left) < len(right) {
		return -1
	}
	if len(left) > len(right) {
		return 1
	}
	return strings.Compare(left, right)
}

func (value decimalExponent) addOffset(offset int) decimalExponent {
	if offset == 0 {
		return value
	}
	negative := offset < 0
	if negative {
		offset = -offset
	}
	digits := strconv.Itoa(offset)
	if value.negative == negative {
		return decimalExponent{negative: negative, digits: addDecimalDigits(value.digits, digits)}
	}
	comparison := compareDecimalDigits(value.digits, digits)
	if comparison == 0 {
		return decimalExponent{digits: "0"}
	}
	if comparison > 0 {
		return decimalExponent{negative: value.negative, digits: subtractDecimalDigits(value.digits, digits)}
	}
	return decimalExponent{negative: negative, digits: subtractDecimalDigits(digits, value.digits)}
}

func addDecimalDigits(left, right string) string {
	width := max(len(left), len(right))
	result := make([]byte, width+1)
	carry := 0
	for index := 0; index < width; index++ {
		sum := carry
		if index < len(left) {
			sum += int(left[len(left)-1-index] - '0')
		}
		if index < len(right) {
			sum += int(right[len(right)-1-index] - '0')
		}
		result[width-index] = byte(sum%10) + '0'
		carry = sum / 10
	}
	if carry != 0 {
		result[0] = '1'
		return string(result)
	}
	return string(result[1:])
}

// subtractDecimalDigits requires left > right.
func subtractDecimalDigits(left, right string) string {
	result := make([]byte, len(left))
	borrow := 0
	for index := 0; index < len(left); index++ {
		difference := int(left[len(left)-1-index]-'0') - borrow
		if index < len(right) {
			difference -= int(right[len(right)-1-index] - '0')
		}
		borrow = 0
		if difference < 0 {
			difference += 10
			borrow = 1
		}
		result[len(left)-1-index] = byte(difference) + '0'
	}
	return strings.TrimLeft(string(result), "0")
}

// Compare returns the exact ordering of two parsed JSON numbers.
func (left Number) Compare(right Number) int {
	if left.sign != right.sign {
		if left.sign < right.sign {
			return -1
		}
		return 1
	}
	if left.sign == 0 {
		return 0
	}
	comparison := left.order.cmp(right.order)
	if comparison == 0 {
		for index := 0; index < len(left.digits) || index < len(right.digits); index++ {
			leftDigit, rightDigit := byte('0'), byte('0')
			if index < len(left.digits) {
				leftDigit = left.digits[index]
			}
			if index < len(right.digits) {
				rightDigit = right.digits[index]
			}
			if leftDigit < rightDigit {
				comparison = -1
				break
			}
			if leftDigit > rightDigit {
				comparison = 1
				break
			}
		}
	}
	return left.sign * comparison
}

// Int64 returns the exact integer value when it fits in a signed 64-bit integer.
// It inspects at most 19 integer digits, regardless of exponent magnitude.
func (value Number) Int64() (int64, bool) {
	if value.sign == 0 {
		return 0, true
	}
	if value.order.negative || value.order.cmp(decimalExponent{digits: "18"}) > 0 {
		return 0, false
	}
	order, _ := strconv.Atoi(value.order.digits) // already known to be 0..18
	integerDigits := order + 1
	for index := integerDigits; index < len(value.digits); index++ {
		if value.digits[index] != '0' {
			return 0, false
		}
	}
	literal := make([]byte, 0, integerDigits+1)
	if value.sign < 0 {
		literal = append(literal, '-')
	}
	for index := 0; index < integerDigits; index++ {
		if index < len(value.digits) {
			literal = append(literal, value.digits[index])
		} else {
			literal = append(literal, '0')
		}
	}
	parsed, err := strconv.ParseInt(string(literal), 10, 64)
	if err != nil {
		return 0, false
	}
	return parsed, true
}

// Parse accepts exactly the JSON number grammar. In particular it does
// not accept the extra fraction and slash spellings supported by big.Rat.
func Parse(literal string) (Number, bool) {
	var result Number
	result.order.digits = "0"
	index := 0
	result.sign = 1
	if len(literal) > 0 && literal[0] == '-' {
		result.sign = -1
		index++
	}
	if index == len(literal) {
		return Number{}, false
	}
	integerStart := index
	if literal[index] == '0' {
		index++
	} else if literal[index] >= '1' && literal[index] <= '9' {
		for index < len(literal) && literal[index] >= '0' && literal[index] <= '9' {
			index++
		}
	} else {
		return Number{}, false
	}
	integerEnd := index
	fractionStart := index
	if index < len(literal) && literal[index] == '.' {
		index++
		fractionStart = index
		for index < len(literal) && literal[index] >= '0' && literal[index] <= '9' {
			index++
		}
		if index == fractionStart {
			return Number{}, false
		}
	}
	fraction := literal[fractionStart:index]
	if index < len(literal) && (literal[index] == 'e' || literal[index] == 'E') {
		index++
		exponentStart := index
		if index < len(literal) && (literal[index] == '+' || literal[index] == '-') {
			index++
		}
		digitsStart := index
		for index < len(literal) && literal[index] >= '0' && literal[index] <= '9' {
			index++
		}
		if index == digitsStart {
			return Number{}, false
		}
		exponent := literal[exponentStart:index]
		result.order.negative = exponent[0] == '-'
		if exponent[0] == '+' || exponent[0] == '-' {
			exponent = exponent[1:]
		}
		result.order.digits = strings.TrimLeft(exponent, "0")
		if result.order.digits == "" {
			result.order.digits = "0"
			result.order.negative = false
		}
	}
	if index != len(literal) {
		return Number{}, false
	}
	digits := literal[integerStart:integerEnd] + fraction
	digits = strings.TrimLeft(digits, "0")
	if digits == "" {
		return Number{}, true // positive and negative zero are equal
	}
	result.digits = digits
	result.order = result.order.addOffset(len(digits) - len(fraction) - 1)
	return result, true
}
