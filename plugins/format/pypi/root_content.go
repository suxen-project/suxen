package pypi

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"html"
	"io"
	"sort"
	"strings"

	"github.com/suxen-project/suxen/spi/format"
	htmltoken "golang.org/x/net/html"
)

// Root indexes can contain hundreds of thousands of projects. Keep their
// entries compact; project pages still use the richer file-entry model.
type rootEntry struct {
	name        string
	url         string
	originalURL string
	raw         []byte // JSON source object, borrowed from the bounded input buffer
}

type rootPage struct {
	baseHref string
	asJSON   bool
	fields   map[string]json.RawMessage
	entries  []rootEntry
	seen     map[string]struct{}
}

const rootRenderedLimit = renderedIndexLimit

func (page *rootPage) add(entry rootEntry) {
	key := normalizeProjectName(entry.name)
	if key == "" {
		return
	}
	if _, exists := page.seen[key]; exists {
		return
	}
	page.seen[key] = struct{}{}
	page.entries = append(page.entries, entry)
}

func parseRootPage(body []byte) (*rootPage, error) {
	page := &rootPage{seen: make(map[string]struct{})}
	if isJSON("", body) {
		page.asJSON = true
		decoder := json.NewDecoder(bytes.NewReader(body))
		opening, err := decoder.Token()
		if err != nil || opening != json.Delim('{') {
			return nil, errors.New("invalid JSON simple root index")
		}
		page.fields = make(map[string]json.RawMessage)
		for decoder.More() {
			keyToken, err := decoder.Token()
			if err != nil {
				return nil, err
			}
			key, ok := keyToken.(string)
			if !ok {
				return nil, errors.New("invalid JSON simple root key")
			}
			if key != "projects" {
				var value json.RawMessage
				if err := decoder.Decode(&value); err != nil {
					return nil, err
				}
				page.fields[key] = value
				continue
			}
			opening, err := decoder.Token()
			if err != nil || opening != json.Delim('[') {
				return nil, errors.New("invalid JSON simple root projects")
			}
			for decoder.More() {
				start := decoder.InputOffset()
				var item struct {
					Name string `json:"name"`
					URL  string `json:"url"`
				}
				if err := decoder.Decode(&item); err != nil {
					return nil, err
				}
				end := decoder.InputOffset()
				objectStart := bytes.IndexByte(body[start:end], '{')
				if objectStart < 0 {
					continue
				}
				page.add(rootEntry{name: item.Name, url: item.URL, originalURL: item.URL, raw: body[start+int64(objectStart) : end]})
			}
			if closing, err := decoder.Token(); err != nil || closing != json.Delim(']') {
				return nil, errors.New("invalid JSON simple root projects closing delimiter")
			}
		}
		if closing, err := decoder.Token(); err != nil || closing != json.Delim('}') {
			return nil, errors.New("invalid JSON simple root closing delimiter")
		}
		if _, err := decoder.Token(); err != io.EOF {
			if err != nil {
				return nil, err
			}
			return nil, errors.New("unexpected data after JSON simple root")
		}
		return page, nil
	}

	tokenizer := htmltoken.NewTokenizer(bytes.NewReader(body))
	var anchor *htmltoken.Token
	var baseSeen bool
	var label strings.Builder
	for {
		switch tokenizer.Next() {
		case htmltoken.ErrorToken:
			if err := tokenizer.Err(); err != nil && err != io.EOF {
				return nil, err
			}
			if len(page.entries) == 0 && !bytes.Contains(bytes.ToLower(body), []byte("<html")) && !bytes.Contains(body, []byte("<a")) {
				return nil, errors.New("not a simple index page")
			}
			return page, nil
		case htmltoken.StartTagToken, htmltoken.SelfClosingTagToken:
			token := tokenizer.Token()
			if token.Data == "base" && !baseSeen {
				for _, attr := range token.Attr {
					if strings.EqualFold(attr.Key, "href") {
						page.baseHref = attr.Val
						baseSeen = true
						break
					}
				}
			}
			if token.Data == "a" {
				anchor = &token
				label.Reset()
			}
		case htmltoken.TextToken:
			if anchor != nil {
				label.WriteString(tokenizer.Token().Data)
			}
		case htmltoken.EndTagToken:
			if tokenizer.Token().Data == "a" && anchor != nil {
				entry := rootEntry{name: strings.TrimSpace(label.String())}
				for _, attr := range anchor.Attr {
					if strings.EqualFold(attr.Key, "href") {
						entry.url = attr.Val
						break
					}
				}
				page.add(entry)
				anchor = nil
			}
		}
	}
}

func (page *rootPage) render() ([]byte, string, error) {
	var output bytes.Buffer
	write := func(part []byte) error {
		if len(part) > rootRenderedLimit-output.Len() {
			return fmt.Errorf("rendered PyPI root index exceeds %d bytes", rootRenderedLimit)
		}
		_, _ = output.Write(part)
		return nil
	}
	if page.asJSON {
		if page.fields == nil {
			page.fields = make(map[string]json.RawMessage)
		}
		var meta map[string]json.RawMessage
		if raw, exists := page.fields["meta"]; exists {
			if err := json.Unmarshal(raw, &meta); err != nil {
				return nil, "", err
			}
		}
		if meta == nil {
			meta = make(map[string]json.RawMessage)
		}
		if _, exists := meta["api-version"]; !exists {
			meta["api-version"] = json.RawMessage(`"1.0"`)
		}
		metaBudget := newOutputBudget()
		if err := jsonSize(meta, &metaBudget); err != nil {
			return nil, "", err
		}
		page.fields["meta"], _ = json.Marshal(meta)
		output.WriteByte('{')
		first := true
		keys := make([]string, 0, len(page.fields))
		for key := range page.fields {
			keys = append(keys, key)
		}
		sort.Strings(keys)
		for _, key := range keys {
			value := page.fields[key]
			keyBudget := outputBudget{remaining: int64(rootRenderedLimit - output.Len())}
			if err := jsonStringSize(key, &keyBudget); err != nil {
				return nil, "", err
			}
			encodedKey, _ := json.Marshal(key)
			if !first {
				output.WriteByte(',')
			}
			first = false
			if err := write(encodedKey); err != nil {
				return nil, "", err
			}
			output.WriteByte(':')
			if err := write(value); err != nil {
				return nil, "", err
			}
		}
		if !first {
			output.WriteByte(',')
		}
		output.WriteString(`"projects":[`)
		for index, entry := range page.entries {
			if index != 0 {
				output.WriteByte(',')
			}
			if len(entry.raw) != 0 {
				if entry.url == entry.originalURL {
					if err := write(entry.raw); err != nil {
						return nil, "", err
					}
					continue
				}
				var object map[string]json.RawMessage
				if err := json.Unmarshal(entry.raw, &object); err != nil {
					return nil, "", err
				}
				delete(object, "url")
				// Size the preserved RawMessages with encoding/json's compact
				// and HTML-escaping rules before Marshal allocates the object.
				entryBudget := outputBudget{remaining: int64(rootRenderedLimit - output.Len())}
				if err := jsonSize(object, &entryBudget); err != nil {
					return nil, "", err
				}
				urlField := int64(len(`"url":`))
				if len(object) != 0 {
					urlField++
				}
				if err := entryBudget.charge(urlField); err != nil {
					return nil, "", err
				}
				if err := jsonStringSize(entry.url, &entryBudget); err != nil {
					return nil, "", err
				}
				object["url"], _ = json.Marshal(entry.url)
				encoded, err := json.Marshal(object)
				if err != nil {
					return nil, "", err
				}
				if err := write(encoded); err != nil {
					return nil, "", err
				}
				continue
			}
			entryBudget := outputBudget{remaining: int64(rootRenderedLimit - output.Len())}
			if err := entryBudget.charge(int64(len(`{"name":,"url":}`))); err != nil {
				return nil, "", err
			}
			if err := jsonStringSize(entry.name, &entryBudget); err != nil {
				return nil, "", err
			}
			if entry.url != "" {
				if err := jsonStringSize(entry.url, &entryBudget); err != nil {
					return nil, "", err
				}
			}
			encoded, err := json.Marshal(struct {
				Name string `json:"name"`
				URL  string `json:"url,omitempty"`
			}{entry.name, entry.url})
			if err != nil {
				return nil, "", err
			}
			if err := write(encoded); err != nil {
				return nil, "", err
			}
		}
		output.WriteString(`]}`)
		if output.Len() > rootRenderedLimit {
			return nil, "", fmt.Errorf("rendered PyPI root index exceeds %d bytes", rootRenderedLimit)
		}
		return output.Bytes(), jsonContentType, nil
	}
	output.WriteString("<!DOCTYPE html><html><head><meta name=\"pypi:repository-version\" content=\"1.0\"></head><body>\n")
	for _, entry := range page.entries {
		link := entry.url
		if link == "" {
			link = normalizeProjectName(entry.name) + "/"
		}
		entryBudget := outputBudget{remaining: int64(rootRenderedLimit - output.Len())}
		if err := entryBudget.charge(int64(len(`<a href=""></a>`) + 1)); err != nil {
			return nil, "", err
		}
		if err := htmlStringSize(link, &entryBudget); err != nil {
			return nil, "", err
		}
		if err := htmlStringSize(entry.name, &entryBudget); err != nil {
			return nil, "", err
		}
		line := fmt.Sprintf("<a href=\"%s\">%s</a>\n", html.EscapeString(link), html.EscapeString(entry.name))
		if err := write([]byte(line)); err != nil {
			return nil, "", err
		}
	}
	output.WriteString("</body></html>\n")
	if output.Len() > rootRenderedLimit {
		return nil, "", fmt.Errorf("rendered PyPI root index exceeds %d bytes", rootRenderedLimit)
	}
	return output.Bytes(), htmlContentType, nil
}

func rewriteRootIndex(repository format.Repository, body []byte, repositoryURL string, asJSON *bool) ([]byte, string, error) {
	page, err := parseRootPage(body)
	if err != nil {
		return nil, "", fmt.Errorf("upstream simple index: %w", err)
	}
	if asJSON != nil {
		page.asJSON = *asJSON
	}
	base := pageDocumentURL(repository.Upstream, pathInfo{kind: kindRoot}, page.baseHref)
	budget := newOutputBudget()
	for index := range page.entries {
		if page.entries[index].url != "" {
			rewritten, err := rewriteLinkWithBudget(page.entries[index].url, base, repositoryURL, kindRoot, &budget)
			if err != nil {
				return nil, "", err
			}
			page.entries[index].url = rewritten
		} else {
			name := normalizeProjectName(page.entries[index].name)
			if err := budget.charge(int64(len(repositoryURL) + len(name) + len("/simple//"))); err != nil {
				return nil, "", err
			}
			page.entries[index].url = repositoryURL + "/simple/" + name + "/"
		}
	}
	return page.render()
}

func normalizeRootSource(repository format.Repository, body []byte) ([]byte, error) {
	page, err := parseRootPage(body)
	if err != nil {
		return nil, err
	}
	base := pageDocumentURL(repository.Upstream, pathInfo{kind: kindRoot}, page.baseHref)
	baseLength := int64(len(base.String()))
	budget := newOutputBudget()
	for index := range page.entries {
		link := page.entries[index].url
		if link == "" {
			continue
		}
		resolved, valid, err := resolveSourceLinkWithBudget(base, baseLength, link, &budget)
		if err != nil {
			return nil, err
		}
		if valid {
			page.entries[index].url = resolved
		}
	}
	content, _, err := page.render()
	return content, err
}

func mergeRootIndexes(sources [][]byte) ([]byte, string, error) {
	var merged *rootPage
	for _, source := range sources {
		page, err := parseRootPage(source)
		if err != nil {
			continue
		}
		if merged == nil {
			merged = page
			continue
		}
		for _, entry := range page.entries {
			merged.add(entry)
		}
	}
	if merged == nil {
		return nil, "", errors.New("no member of the group serves a valid simple index page")
	}
	merged.asJSON = true
	return merged.render()
}
