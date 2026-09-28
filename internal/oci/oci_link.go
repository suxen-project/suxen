package oci

import "strings"

// parseOCINextLinkHeaders reads Link field values without treating delimiters
// inside URI references or quoted parameter values as list separators.
func parseOCINextLinkHeaders(headers []string) (string, bool) {
	for _, header := range headers {
		if next, ok := parseOCINextLink(header); ok {
			return next, true
		}
	}
	return "", false
}

func parseOCINextLink(header string) (string, bool) {
	for len(header) > 0 {
		member, rest := nextOCILinkMember(header)
		if target, ok := ociNextLinkMember(member); ok {
			return target, true
		}
		header = rest
	}
	return "", false
}

func nextOCILinkMember(header string) (string, string) {
	inURI, inQuote, escaped := false, false, false
	for i := 0; i < len(header); i++ {
		switch {
		case escaped:
			escaped = false
		case inQuote && header[i] == '\\':
			escaped = true
		case !inURI && header[i] == '"':
			inQuote = !inQuote
		case !inQuote && header[i] == '<':
			inURI = true
		case !inQuote && header[i] == '>':
			inURI = false
		case !inURI && !inQuote && header[i] == ',':
			return header[:i], header[i+1:]
		}
	}
	return header, ""
}

func ociNextLinkMember(member string) (string, bool) {
	member = strings.TrimSpace(member)
	if !strings.HasPrefix(member, "<") {
		return "", false
	}
	end := strings.IndexByte(member, '>')
	if end < 2 {
		return "", false
	}
	target := member[1:end]
	params := member[end+1:]
	seenRel, hasNext, hasAnchor := false, false, false
	for len(strings.TrimSpace(params)) > 0 {
		params = strings.TrimSpace(params)
		if params[0] != ';' {
			return "", false
		}
		params = strings.TrimSpace(params[1:])
		nameEnd := strings.IndexAny(params, "=; \t")
		if nameEnd < 0 {
			nameEnd = len(params)
		}
		name := params[:nameEnd]
		if !ociLinkToken(name) {
			return "", false
		}
		params = strings.TrimLeft(params[nameEnd:], " \t")
		value := ""
		if strings.HasPrefix(params, "=") {
			params = strings.TrimLeft(params[1:], " \t")
			if strings.HasPrefix(params, `"`) {
				var ok bool
				value, params, ok = ociQuotedLinkValue(params)
				if !ok {
					return "", false
				}
			} else {
				valueEnd := strings.IndexByte(params, ';')
				if valueEnd < 0 {
					valueEnd = len(params)
				}
				value = strings.TrimSpace(params[:valueEnd])
				params = params[valueEnd:]
				if !ociLinkToken(value) {
					return "", false
				}
			}
		} else if params != "" && !strings.HasPrefix(params, ";") {
			return "", false
		}
		if strings.EqualFold(name, "anchor") {
			hasAnchor = true
		}
		if !strings.EqualFold(name, "rel") || seenRel {
			continue
		}
		seenRel = true
		for _, relation := range strings.Fields(value) {
			if strings.EqualFold(relation, "next") {
				hasNext = true
			}
		}
	}
	if hasNext && !hasAnchor {
		return target, true
	}
	return "", false
}

func ociLinkToken(value string) bool {
	if value == "" {
		return false
	}
	for i := 0; i < len(value); i++ {
		char := value[i]
		if char >= '0' && char <= '9' || char >= 'A' && char <= 'Z' || char >= 'a' && char <= 'z' {
			continue
		}
		if !strings.ContainsRune("!#$%&'*+-.^_`|~", rune(char)) {
			return false
		}
	}
	return true
}

func ociQuotedLinkValue(input string) (string, string, bool) {
	var value strings.Builder
	for i := 1; i < len(input); i++ {
		switch input[i] {
		case '\\':
			i++
			if i >= len(input) {
				return "", "", false
			}
			value.WriteByte(input[i])
		case '"':
			return value.String(), input[i+1:], true
		default:
			value.WriteByte(input[i])
		}
	}
	return "", "", false
}
