package server

import (
	"fmt"
	"net/http"
	"net/url"
	"sort"
	"strings"
)

func (s *Server) validateRoles(r *http.Request, roles []string) error {
	for _, name := range roles {
		if _, err := s.roleReads().Role(r.Context(), name); err != nil {
			return fmt.Errorf("resolve role %q: %w", name, err)
		}
	}
	return nil
}

func collectionItemLocation(r *http.Request, name string) string {
	return strings.TrimSuffix(r.URL.Path, "/") + "/" + url.PathEscape(name)
}

func uniqueSortedStrings(values []string) []string {
	set := make(map[string]struct{}, len(values))
	for _, value := range values {
		set[value] = struct{}{}
	}
	result := make([]string, 0, len(set))
	for value := range set {
		result = append(result, value)
	}
	sort.Strings(result)
	return result
}
