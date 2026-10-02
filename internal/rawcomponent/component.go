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
	"sync/atomic"
)

// ConfigKey is the formatConfig entry that holds the ordered component rules.
const ConfigKey = "components"

const (
	maxRules         = 32
	maxPatternLength = 1024
)

// Rule is one compiled component pattern with its optional anchor.
type Rule struct {
	source  string
	pattern *regexp.Regexp
	anchor  *regexp.Regexp
	name    int
	version int
}

// Rules is the ordered component configuration of one repository.
type Rules []Rule

// Match is the component identity a rule assigns to an asset path.
type Match struct {
	// Rule is the index of the matching rule, or the number of rules for the
	// implicit rule; units never span rules.
	Rule    int
	Name    string
	Version string
	// Directory is the version directory when the version capture is a whole
	// path segment followed by more path and preceded by the name capture, so
	// the directory and everything below it form one version. It is empty
	// when the version is part of the file name.
	Directory string
	// Anchor reports whether the path may establish its version. It is true
	// for every matched path when the rule declares no anchor.
	Anchor bool
	// Implicit marks a path no pattern matched, whose component is its
	// directory. Its versions never share retention with pattern versions of
	// a component of the same name.
	Implicit bool
}

// Unit identifies the version a matched path belongs to.
type Unit struct {
	Rule                     int
	Name, Version, Directory string
}

// Unit returns the version unit of the match.
func (match Match) Unit() Unit {
	return Unit{Rule: match.Rule, Name: match.Name, Version: match.Version, Directory: match.Directory}
}

// RootComponent names the implicit component of files at the repository root.
const RootComponent = "/"

// compiled caches expressions by source so attribute projection does not
// recompile a repository's patterns for every asset it evaluates. Admins can
// submit any number of distinct patterns, so the cache stops growing at
// maxCompiled entries and later sources compile on each use.
var (
	compiled      sync.Map
	compiledCount atomic.Int64
)

const maxCompiled = 1024

func compile(source string) (*regexp.Regexp, error) {
	if cached, ok := compiled.Load(source); ok {
		return cached.(*regexp.Regexp), nil
	}
	expression, err := regexp.Compile(source)
	if err != nil {
		return nil, err
	}
	if compiledCount.Load() < maxCompiled {
		if _, loaded := compiled.LoadOrStore(source, expression); !loaded {
			compiledCount.Add(1)
		}
	}
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
	rule := Rule{source: source, pattern: pattern, name: pattern.SubexpIndex("name"), version: pattern.SubexpIndex("version")}
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
// A rule whose name or version capture is empty, or whose captures overlap,
// does not match. A path that
// no rule matches falls back to the implicit rule: its parent directory is the
// component (RootComponent at the root) and its file name the version, so
// every file is a one-file version.
func (rules Rules) Match(assetPath string) Match {
	for index, rule := range rules {
		indexes := rule.pattern.FindStringSubmatchIndex(assetPath)
		if indexes == nil {
			continue
		}
		nameStart, nameEnd := indexes[2*rule.name], indexes[2*rule.name+1]
		versionStart, versionEnd := indexes[2*rule.version], indexes[2*rule.version+1]
		// Overlapping captures would let one path fill both indexed columns.
		if nameStart < 0 || versionStart < 0 || nameStart == nameEnd || versionStart == versionEnd ||
			nameStart < versionEnd && versionStart < nameEnd {
			continue
		}
		match := Match{
			Rule:    index,
			Name:    assetPath[nameStart:nameEnd],
			Version: assetPath[versionStart:versionEnd],
			Anchor:  rule.anchor == nil || rule.anchor.MatchString(assetPath),
		}
		// The version directory owns its subtree only when the component name
		// precedes it; otherwise components would share the directory.
		if versionEnd < len(assetPath) && assetPath[versionEnd] == '/' && nameEnd <= versionStart &&
			(versionStart == 0 || assetPath[versionStart-1] == '/') {
			match.Directory = assetPath[:versionEnd]
		}
		return match
	}
	match := Match{Rule: len(rules), Name: RootComponent, Version: assetPath, Anchor: true, Implicit: true}
	if parent := strings.LastIndex(assetPath, "/"); parent >= 0 {
		match.Name, match.Version = assetPath[:parent], assetPath[parent+1:]
	}
	return match
}
