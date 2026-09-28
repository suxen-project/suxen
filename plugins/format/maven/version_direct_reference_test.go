package maven

import (
	"os"
	"strings"
	"testing"
)

// This is a direct-pair oracle, not a sorted list: Maven's comparator has
// non-transitive relations for some separator/qualifier combinations. The
// matrix was generated with Apache Maven 3.9.16 ComparableVersion on Java 21:
//
//	JAVA_HOME=/path/to/jdk-21 MAVEN_HOME=/path/to/maven-3.9.16 \
//	  sh testdata/generate-maven-oracle.sh > testdata/maven-3.9.16-java21-pairs.txt
//
// The corpus covers ASCII separator/numeric/qualifier cases and Unicode
// decimal digits, contextual lowercasing, and UTF-16 lexical ordering from
// the Java 21 Unicode repertoire. Newer scripts can differ between the JVM's
// Unicode tables and those selected by the Go/x/text build toolchain.
func TestMavenVersionDirectPairOracle(t *testing.T) {
	versionsRaw, err := os.ReadFile("testdata/maven-3.9.16-java21-versions.txt")
	if err != nil {
		t.Fatal(err)
	}
	pairsRaw, err := os.ReadFile("testdata/maven-3.9.16-java21-pairs.txt")
	if err != nil {
		t.Fatal(err)
	}
	versions := strings.Split(strings.TrimSuffix(string(versionsRaw), "\n"), "\n")
	rows := strings.Split(strings.TrimSuffix(string(pairsRaw), "\n"), "\n")
	if len(rows) != len(versions) {
		t.Fatalf("oracle has %d rows for %d versions", len(rows), len(versions))
	}
	for leftIndex, left := range versions {
		if len(rows[leftIndex]) != len(versions) {
			t.Fatalf("oracle row %d has %d comparisons, want %d", leftIndex, len(rows[leftIndex]), len(versions))
		}
		for rightIndex, right := range versions {
			code := rows[leftIndex][rightIndex]
			if code < '0' || code > '2' {
				t.Fatalf("invalid oracle code %q at %d,%d", code, leftIndex, rightIndex)
			}
			want := int(code) - '1'
			if got := compareInts(compareVersions(left, right), 0); got != want {
				t.Fatalf("compareVersions(%q, %q) = %d; Maven 3.9.16/Java 21 = %d", left, right, got, want)
			}
		}
	}
}

func TestMavenVersionUnicodeAndNumericTypes(t *testing.T) {
	for _, test := range []struct {
		name, left, right string
		want              int
	}{
		{"Arabic-Indic digits", "1.٢", "1.2", 0},
		{"fullwidth digits", "1.１２", "1.12", 0},
		{"ASCII zero stripping", "01", "1.٠", 0},
		{"non-ASCII zero retains long item", "1." + strings.Repeat("٠", 9) + "١", "1.1", 1},
		{"non-ASCII zero retains big item", "1." + strings.Repeat("٠", 18) + "١", "1." + strings.Repeat("٠", 17) + "١", 1},
		{"numeric zero is null at any item size", "1." + strings.Repeat("٠", 10), "1", 0},
		{"ten ASCII zeroes retain long item", "1." + strings.Repeat("0", 10) + ".1", "1.0.1", 1},
		{"nineteen ASCII zeroes retain big item", "1." + strings.Repeat("0", 19) + ".1", "1." + strings.Repeat("0", 18) + ".1", 1},
		{"trailing long zero item is null", "1." + strings.Repeat("0", 10), "1", 0},
		{"dotted I expands", "1.İ", "1.i\u0307", 0},
		{"Greek final sigma", "1.ΟΣ", "1.ος", 0},
		{"Greek final and medial sigma differ", "1.ΟΣ", "1.οσ", -1},
		{"UTF-16 unknown qualifier order", "1.\ue000", "1.𝟙", 1},
		{"supplementary digit stays a qualifier", "1.𝟙", "1.1", -1},
	} {
		t.Run(test.name, func(t *testing.T) {
			if got := compareInts(compareVersions(test.left, test.right), 0); got != test.want {
				t.Fatalf("compareVersions(%q, %q) = %d, want %d", test.left, test.right, got, test.want)
			}
		})
	}
}
