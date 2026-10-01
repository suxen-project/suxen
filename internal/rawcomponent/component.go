// Package rawcomponent derives component identities for Raw assets from the
// ordered path patterns a repository declares in its formatConfig.
//
// A pattern is an RE2 expression with the named groups "name" and "version".
// The first pattern matching an asset path decides the asset's component and
// version; paths matching no pattern keep the Raw defaults.
package rawcomponent

import (
	"errors"
	"fmt"
	"regexp"
	"strings"
	"sync"
)

// ConfigKey is the formatConfig entry that holds the ordered component rules.
const ConfigKey = "components"

const (
	maxRules         = 32
	maxPatternLength = 1024
)

// Rule is one compiled component pattern with its optional anchor.
type Rule struct {
	pattern *regexp.Regexp
	anchor  *regexp.Regexp
	name    int
	version int
}

// Rules is the ordered component configuration of one repository.
type Rules []Rule

// Match is the component identity a rule assigns to an asset path.
type Match struct {
	Name    string
	Version string
	// Directory is the version directory when the version capture is the
	// whole parent segment of the path, and empty for a single-file version.
	Directory string
	// Anchor reports whether the path may seed a directory unit. It is true
	// for every matched path when the rule declares no anchor.
	Anchor bool
}

// compiled caches expressions by source so attribute projection does not
// recompile a repository's patterns for every asset it evaluates.
var compiled sync.Map

func compile(source string) (*regexp.Regexp, error) {
	if cached, ok := compiled.Load(source); ok {
		return cached.(*regexp.Regexp), nil
	}
	expression, err := regexp.Compile(source)
	if err != nil {
		return nil, err
	}
	compiled.Store(source, expression)
	return expression, nil
}

// Validate checks a Raw repository's formatConfig.
func Validate(config map[string]any) error {
	_, err := Parse(config)
	return err
}

// Parse compiles the component rules of a Raw repository's formatConfig. An
// empty configuration yields no rules.
func Parse(config map[string]any) (Rules, error) {
	for key := range config {
		if key != ConfigKey {
			return nil, fmt.Errorf("unknown raw formatConfig field %q", key)
		}
	}
	raw, present := config[ConfigKey]
	if !present || raw == nil {
		return nil, nil
	}
	entries, ok := raw.([]any)
	if !ok {
		return nil, errors.New("raw formatConfig components must be a list")
	}
	if len(entries) > maxRules {
		return nil, fmt.Errorf("raw formatConfig accepts at most %d components", maxRules)
	}
	rules := make(Rules, 0, len(entries))
	for index, entry := range entries {
		rule, err := parseRule(entry)
		if err != nil {
			return nil, fmt.Errorf("raw formatConfig components[%d]: %w", index, err)
		}
		rules = append(rules, rule)
	}
	return rules, nil
}

func parseRule(entry any) (Rule, error) {
	fields, ok := entry.(map[string]any)
	if !ok {
		return Rule{}, errors.New("must be an object")
	}
	for key := range fields {
		if key != "pattern" && key != "anchor" {
			return Rule{}, fmt.Errorf("unknown field %q", key)
		}
	}
	source, _ := fields["pattern"].(string)
	if source == "" || len(source) > maxPatternLength {
		return Rule{}, fmt.Errorf("pattern must be a nonempty string of at most %d bytes", maxPatternLength)
	}
	pattern, err := compile(source)
	if err != nil {
		return Rule{}, fmt.Errorf("invalid pattern: %w", err)
	}
	rule := Rule{pattern: pattern, name: pattern.SubexpIndex("name"), version: pattern.SubexpIndex("version")}
	if rule.name < 0 || rule.version < 0 {
		return Rule{}, errors.New("pattern must contain the named groups name and version")
	}
	if anchor, present := fields["anchor"]; present && anchor != nil {
		anchorSource, ok := anchor.(string)
		if !ok || anchorSource == "" || len(anchorSource) > maxPatternLength {
			return Rule{}, fmt.Errorf("anchor must be a nonempty string of at most %d bytes", maxPatternLength)
		}
		if rule.anchor, err = compile(anchorSource); err != nil {
			return Rule{}, fmt.Errorf("invalid anchor: %w", err)
		}
	}
	return rule, nil
}

// ForConfig returns the rules of a stored configuration, or nil when it holds
// none. Stored configurations were validated on write, so a parse failure is
// treated as no rules rather than surfaced on every read.
func ForConfig(config map[string]any) Rules {
	rules, err := Parse(config)
	if err != nil {
		return nil
	}
	return rules
}

// Match returns the component identity of the first rule matching assetPath.
// A rule whose name or version capture is empty does not match.
func (rules Rules) Match(assetPath string) (Match, bool) {
	for _, rule := range rules {
		indexes := rule.pattern.FindStringSubmatchIndex(assetPath)
		if indexes == nil {
			continue
		}
		nameStart, nameEnd := indexes[2*rule.name], indexes[2*rule.name+1]
		versionStart, versionEnd := indexes[2*rule.version], indexes[2*rule.version+1]
		if nameStart < 0 || versionStart < 0 || nameStart == nameEnd || versionStart == versionEnd {
			continue
		}
		match := Match{
			Name:    assetPath[nameStart:nameEnd],
			Version: assetPath[versionStart:versionEnd],
			Anchor:  rule.anchor == nil || rule.anchor.MatchString(assetPath),
		}
		parent := strings.LastIndex(assetPath, "/")
		if versionEnd == parent && (versionStart == 0 || assetPath[versionStart-1] == '/') {
			match.Directory = assetPath[:parent]
		}
		return match, true
	}
	return Match{}, false
}
