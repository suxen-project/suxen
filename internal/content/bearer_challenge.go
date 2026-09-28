package content

import "strings"

// parseBearerChallenges searches all WWW-Authenticate field values. A comma
// separates challenges only when the following item is not an auth parameter.
func parseBearerChallenges(headers []string) (bearerChallenge, bool) {
	var current challengeParameters
	for _, header := range headers {
		parts, ok := splitChallengeItems(header)
		if !ok {
			// A malformed parameter can invalidate the preceding challenge;
			// a new malformed challenge cannot change an earlier one.
			if !challengeParameterPrefix(strings.TrimLeft(header, " \t,")) {
				if challenge, ok := current.bearer(); ok {
					return challenge, true
				}
			}
			current = challengeParameters{}
			continue
		}
		for _, part := range parts {
			part = trimChallengeSpace(part)
			if part == "" {
				continue
			}
			if challengeParameterPrefix(part) {
				key, value, ok := parseChallengeParameter(part)
				if !ok {
					current.invalid = true
				} else {
					current.add(key, value)
				}
				continue
			}
			if challenge, ok := current.bearer(); ok {
				return challenge, true
			}
			current = challengeParameters{}
			scheme, rest, ok := parseChallengeScheme(part)
			if !ok || !strings.EqualFold(scheme, "Bearer") {
				continue
			}
			current.bearerScheme = true
			if rest != "" {
				key, value, ok := parseChallengeParameter(rest)
				if !ok {
					current.invalid = true
				} else {
					current.add(key, value)
				}
			}
		}
	}
	return current.bearer()
}

// Retain the single-value entry point for callers which already have one field.
func parseBearerChallenge(header string) (bearerChallenge, bool) {
	return parseBearerChallenges([]string{header})
}

type challengeParameters struct {
	bearerScheme bool
	invalid      bool
	realm        string
	service      string
	scope        string
	seen         map[string]bool
}

func (p *challengeParameters) add(key, value string) {
	if !p.bearerScheme {
		return
	}
	key = strings.ToLower(key)
	if p.seen == nil {
		p.seen = make(map[string]bool)
	}
	if p.seen[key] {
		p.invalid = true // Duplicate parameters are ambiguous.
		return
	}
	p.seen[key] = true
	switch key {
	case "realm":
		p.realm = value
	case "service":
		p.service = value
	case "scope":
		p.scope = value
	}
}

func (p challengeParameters) bearer() (bearerChallenge, bool) {
	if !p.bearerScheme || p.invalid || p.realm == "" {
		return bearerChallenge{}, false
	}
	return bearerChallenge{Realm: p.realm, Service: p.service, Scope: p.scope}, true
}

func splitChallengeItems(header string) ([]string, bool) {
	var items []string
	start := 0
	quoted := false
	escaped := false
	for i := 0; i < len(header); i++ {
		switch {
		case escaped:
			escaped = false
		case quoted && header[i] == '\\':
			escaped = true
		case header[i] == '"':
			quoted = !quoted
		case header[i] == ',' && !quoted:
			items = append(items, header[start:i])
			start = i + 1
		}
	}
	if quoted || escaped {
		return nil, false
	}
	return append(items, header[start:]), true
}

func parseChallengeScheme(part string) (string, string, bool) {
	i := 0
	for i < len(part) && challengeTokenByte(part[i]) {
		i++
	}
	if i == 0 || (i < len(part) && part[i] != ' ' && part[i] != '\t') {
		return "", "", false
	}
	return part[:i], trimChallengeSpace(part[i:]), true
}

func parseChallengeParameter(part string) (string, string, bool) {
	i := 0
	for i < len(part) && challengeTokenByte(part[i]) {
		i++
	}
	if i == 0 {
		return "", "", false
	}
	key := part[:i]
	for i < len(part) && (part[i] == ' ' || part[i] == '\t') {
		i++
	}
	if i == len(part) || part[i] != '=' {
		return "", "", false
	}
	i++
	for i < len(part) && (part[i] == ' ' || part[i] == '\t') {
		i++
	}
	if i == len(part) {
		return "", "", false
	}
	if part[i] != '"' {
		start := i
		for i < len(part) && challengeTokenByte(part[i]) {
			i++
		}
		if i == start || trimChallengeSpace(part[i:]) != "" {
			return "", "", false
		}
		return key, part[start:i], true
	}
	i++
	var value strings.Builder
	for i < len(part) {
		c := part[i]
		i++
		if c == '"' {
			if trimChallengeSpace(part[i:]) != "" {
				return "", "", false
			}
			return key, value.String(), true
		}
		if c == '\\' {
			if i == len(part) || !challengeQuotedByte(part[i]) {
				return "", "", false
			}
			c = part[i]
			i++
		} else if !challengeQuotedByte(c) || c == '\\' {
			return "", "", false
		}
		value.WriteByte(c)
	}
	return "", "", false
}

func challengeParameterPrefix(part string) bool {
	i := 0
	for i < len(part) && challengeTokenByte(part[i]) {
		i++
	}
	if i == 0 {
		return false
	}
	for i < len(part) && (part[i] == ' ' || part[i] == '\t') {
		i++
	}
	return i < len(part) && part[i] == '='
}

func trimChallengeSpace(value string) string {
	return strings.Trim(value, " \t")
}

func challengeQuotedByte(c byte) bool {
	return c == '\t' || c >= ' ' && c <= '~' || c >= 0x80
}

func challengeTokenByte(c byte) bool {
	return c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' ||
		c >= '0' && c <= '9' || strings.IndexByte("!#$%&'*+-.^_`|~", c) >= 0
}
