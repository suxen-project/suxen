// This implementation follows the parsing and comparison structure of
// Apache Maven's ComparableVersion (maven-artifact 3.9.16), licensed under
// the Apache License, Version 2.0. See the repository NOTICE and LICENSE.
// Source: https://github.com/apache/maven/blob/maven-3.9.16/maven-artifact/src/main/java/org/apache/maven/artifact/versioning/ComparableVersion.java
package maven

import (
	"sort"
	"strings"
	"unicode"
	"unicode/utf16"
	"unicode/utf8"

	"golang.org/x/text/cases"
	"golang.org/x/text/language"
)

// Maven's ComparableVersion nests items after hyphens and transitions between
// digits and letters. A nested numeric item sorts below a dotted numeric item:
// 1.0-2 < 1.0.1. Keep this tree rather than flattening separators away.
// Maven lowercases with the JVM's Unicode tables; this implementation uses
// Go/x/text's tables, so characters added after Java 21's Unicode repertoire
// can differ depending on the Go build toolchain.
type versionItem struct {
	kind       byte // 'n' number, 'q' qualifier, 'l' list
	value      string
	numberType byte // Maven chooses int/long/big by digit count after ASCII-zero stripping.
	children   []*versionItem
}

func compareVersions(left, right string) int {
	return compareVersionItems(parseVersion(left), parseVersion(right))
}

func parseVersion(version string) *versionItem {
	version = cases.Lower(language.English).String(version)
	characters := []rune(version)
	root := &versionItem{kind: 'l'}
	list := root
	stack := []*versionItem{root}
	nested := func() {
		child := &versionItem{kind: 'l'}
		list.children = append(list.children, child)
		list = child
		stack = append(stack, child)
	}
	add := func(item *versionItem) { list.children = append(list.children, item) }
	digit := false
	start := 0
	for i, character := range characters {
		switch {
		case character == '.' || character == '-':
			if i == start {
				add(numberItem("0"))
			} else {
				add(parsedVersionItem(string(characters[start:i]), digit, false))
			}
			start = i + 1
			if character == '-' {
				nested()
			}
		case mavenDigit(character):
			if !digit && i > start {
				if len(list.children) > 0 {
					nested()
				}
				add(parsedVersionItem(string(characters[start:i]), false, true))
				start = i
				nested()
			}
			digit = true
		default:
			if digit && i > start {
				add(parsedVersionItem(string(characters[start:i]), true, false))
				start = i
				nested()
			}
			digit = false
		}
	}
	if len(characters) > start {
		if !digit && len(list.children) > 0 {
			nested()
		}
		add(parsedVersionItem(string(characters[start:]), digit, false))
	}
	for i := len(stack) - 1; i >= 0; i-- {
		stack[i].normalize()
	}
	return root
}

// Maven uses Character.isDigit(char), so supplementary digit runes are not
// numeric separators: each of their UTF-16 surrogates is a qualifier char.
func mavenDigit(character rune) bool {
	return character <= 0xffff && unicode.IsDigit(character)
}

func parsedVersionItem(value string, numeric, followedByDigit bool) *versionItem {
	if numeric {
		return numberItem(value)
	}
	if followedByDigit {
		switch value {
		case "a":
			value = "alpha"
		case "b":
			value = "beta"
		case "m":
			value = "milestone"
		}
	}
	switch value {
	case "ga", "final", "release":
		value = ""
	case "cr":
		value = "rc"
	}
	return &versionItem{kind: 'q', value: value}
}

func numberItem(value string) *versionItem {
	// ComparableVersion strips only ASCII '0' before choosing IntItem (up to
	// nine chars), LongItem (up to eighteen), or BigIntegerItem. If every
	// character is ASCII zero, Maven retains the original token for this choice.
	// Non-ASCII zeroes likewise retain their character count despite value zero.
	if stripped := strings.TrimLeft(value, "0"); stripped != "" {
		value = stripped
	}
	count := utf8.RuneCountInString(value)
	numberType := byte(3)
	switch {
	case count <= 9:
		numberType = 1
	case count <= 18:
		numberType = 2
	}
	if len(value) == count { // All numeric runes are ASCII; avoid conversion.
		canonical := strings.TrimLeft(value, "0")
		if canonical == "" {
			canonical = "0"
		}
		return &versionItem{kind: 'n', value: canonical, numberType: numberType}
	}
	var digits strings.Builder
	digits.Grow(count)
	for _, character := range value {
		digits.WriteByte(mavenDecimalDigit(character))
	}
	canonical := strings.TrimLeft(digits.String(), "0")
	if canonical == "" {
		canonical = "0"
	}
	return &versionItem{kind: 'n', value: canonical, numberType: numberType}
}

// Each BMP decimal-digit range starts at zero and has ten values. Use the
// Unicode table already used by mavenDigit, and keep ASCII on the fast path.
func mavenDecimalDigit(character rune) byte {
	if character >= '0' && character <= '9' {
		return byte(character)
	}
	ranges := unicode.Digit.R16
	i := sort.Search(len(ranges), func(i int) bool { return rune(ranges[i].Hi) >= character })
	span := ranges[i] // mavenDigit already established that this BMP rune is Nd.
	return '0' + byte((character-rune(span.Lo))/rune(span.Stride)%10)
}

func (item *versionItem) isNull() bool {
	switch item.kind {
	case 'n':
		return item.value == "0"
	case 'q':
		return item.value == ""
	default:
		return len(item.children) == 0
	}
}

func (item *versionItem) normalize() {
	for i := len(item.children) - 1; i >= 0; i-- {
		child := item.children[i]
		if child.isNull() {
			item.children = append(item.children[:i], item.children[i+1:]...)
		} else if child.kind != 'l' {
			break
		}
	}
}

func compareVersionItems(left, right *versionItem) int {
	// Upstream metadata can contain long versions. Compare nested lists with
	// an explicit stack so an adversarial number of hyphens cannot exhaust the
	// Go call stack.
	type frame struct {
		left, right *versionItem
		next        int
	}
	stack := []frame{{left: left, right: right}}
	for len(stack) > 0 {
		current := &stack[len(stack)-1]
		l, r := current.left, current.right
		lList := l != nil && l.kind == 'l'
		rList := r != nil && r.kind == 'l'
		if (lList || rList) && (lList || l == nil) && (rList || r == nil) {
			var lLen, rLen int
			if lList {
				lLen = len(l.children)
			}
			if rList {
				rLen = len(r.children)
			}
			if current.next >= lLen && current.next >= rLen {
				stack = stack[:len(stack)-1]
				continue
			}
			var a, b *versionItem
			if current.next < lLen {
				a = l.children[current.next]
			}
			if current.next < rLen {
				b = r.children[current.next]
			}
			current.next++
			stack = append(stack, frame{left: a, right: b})
			continue
		}
		if comparison := compareVersionScalars(l, r); comparison != 0 {
			return comparison
		}
		stack = stack[:len(stack)-1]
	}
	return 0
}

func compareVersionScalars(left, right *versionItem) int {
	if left == nil {
		if right == nil {
			return 0
		}
		return -compareVersionScalars(right, nil)
	}
	if right == nil {
		if left.kind == 'n' {
			if left.value == "0" {
				return 0
			}
			return 1
		}
		return compareVersionQualifiers(left.value, "")
	}
	if left.kind != right.kind {
		// Qualifiers precede nested lists, and nested lists precede dotted
		// numbers. This is the distinction lost by a flat token comparator.
		rank := func(kind byte) int {
			switch kind {
			case 'q':
				return 0
			case 'l':
				return 1
			default:
				return 2
			}
		}
		return compareInts(rank(left.kind), rank(right.kind))
	}
	switch left.kind {
	case 'n':
		if comparison := compareInts(int(left.numberType), int(right.numberType)); comparison != 0 {
			return comparison
		}
		if comparison := compareInts(len(left.value), len(right.value)); comparison != 0 {
			return comparison
		}
		return strings.Compare(left.value, right.value)
	case 'q':
		return compareVersionQualifiers(left.value, right.value)
	default:
		return 0 // list/list pairs are traversed by compareVersionItems
	}
}

func compareInts(left, right int) int {
	switch {
	case left < right:
		return -1
	case left > right:
		return 1
	default:
		return 0
	}
}

func compareVersionQualifiers(left, right string) int {
	qualifierRank := func(value string) int {
		switch value {
		case "alpha":
			return 0
		case "beta":
			return 1
		case "milestone":
			return 2
		case "rc":
			return 3
		case "snapshot":
			return 4
		case "":
			return 5
		case "sp":
			return 6
		default:
			return 7
		}
	}
	if comparison := compareInts(qualifierRank(left), qualifierRank(right)); comparison != 0 {
		return comparison
	}
	return compareUTF16(left, right)
}

func compareUTF16(left, right string) int {
	a, b := utf16.Encode([]rune(left)), utf16.Encode([]rune(right))
	for i := 0; i < len(a) && i < len(b); i++ {
		if comparison := compareInts(int(a[i]), int(b[i])); comparison != 0 {
			return comparison
		}
	}
	return compareInts(len(a), len(b))
}
