package maven

import "testing"

// These equality groups and their order were generated with Apache Maven
// 3.9.16 ComparableVersion. To reproduce a pair, run:
// java -jar ${MAVEN_HOME}/lib/maven-artifact-3.9.16.jar 1.0-2 1.0.1
// Groups are ascending; every version in a group compares equal. The test
// checks every ordered pair without requiring Java during ordinary Go tests.
func TestCompareVersionsMavenReferenceCorpus(t *testing.T) {
	groups := [][]string{
		{"0", "0.0", "0-0"},
		{"1.0-alpha"},
		{"1.0-a1", "1.0-alpha-1"},
		{"1.0-beta"},
		{"1.0-b1"},
		{"1.0-m1"},
		{"1.0-RC"},
		{"1.0-cr1"},
		{"1.0.0.RC2"},
		{"1.0.0-RC3"},
		{"1.0-SNAPSHOT"},
		{"1", "1.0", "1.0.0", "1.ga", "1-final", "1-release", "1.0.0-0.0.0"},
		{"1-sp", "1.0-sp"},
		{"1-sp-1"},
		{"1-sp.1"},
		{"1_0"},
		{"1.0-a"},
		{"1.foo", "1-foo", "1.0.0-foo.0.0"},
		{"1.0-m"},
		{"1-ga-1"},
		{"1-1"},
		{"1-1.foo-bar1baz-.1"},
		{"1.0-2"},
		{"1.0.1"},
		{"1.1", "1.00000000000000000000000000000000000000000000000001"},
		{"1.2.0-rc1"},
		{"1.2.0", "1.00000000000000000000000000000000000000000000000002"},
		{"1.2+build"},
		{"1.2-foo2"},
		{"1.2-foo10"},
		{"1.2-20260927.123456-9"},
		{"1.2-20260927.123456-10"},
		{"1.2.1"},
	}
	for leftGroup, leftVersions := range groups {
		for rightGroup, rightVersions := range groups {
			for _, left := range leftVersions {
				for _, right := range rightVersions {
					got := compareVersions(left, right)
					if (leftGroup < rightGroup && got >= 0) ||
						(leftGroup > rightGroup && got <= 0) ||
						(leftGroup == rightGroup && got != 0) {
						t.Errorf("compareVersions(%q, %q) = %d; Maven groups %d and %d", left, right, got, leftGroup, rightGroup)
					}
				}
			}
		}
	}
}
