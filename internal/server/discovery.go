package server

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"path"
	"sort"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/suxen-project/suxen/internal/assetattrs"
	"github.com/suxen-project/suxen/internal/content"
	"github.com/suxen-project/suxen/internal/domain"
	"github.com/suxen-project/suxen/internal/httpx"
	"github.com/suxen-project/suxen/internal/identity"
	"github.com/suxen-project/suxen/internal/predicate"
)

// privilegeReads is the authorization read capability the whoami/discovery path
// needs: the effective privileges of a user (or the anonymous baseline) and the
// privileges granted by an explicit set of roles.
type privilegeReads interface {
	EffectivePrivileges(context.Context, string) ([]string, error)
	PrivilegesForRoles(context.Context, []string) ([]string, error)
	LocalAuthorization(context.Context, string, string) (domain.User, []string, error)
}

// privilegeReads narrows the metadata store to the authorization read
// capability. It reads s.metadata on each call so a reconfigured backend is
// honoured.
func (s *Server) privilegeReads() privilegeReads {
	return s.metadata
}

type identityResponse struct {
	Authenticated       bool     `json:"authenticated"`
	Username            string   `json:"username,omitempty"`
	Administrator       bool     `json:"admin"`
	AuthenticationKind  string   `json:"authenticationKind"`
	Roles               []string `json:"roles"`
	EffectivePrivileges []string `json:"effectivePrivileges"`
	// LoginProviders names the OIDC providers that accept interactive browser login,
	// so an anonymous UI visitor can start a login without administrative access.
	LoginProviders []string `json:"loginProviders"`
	// SessionProvider and ExpiresAt describe a browser OIDC session so the UI can
	// re-authenticate silently (prompt=none) before it lapses. Both are set only
	// for the "oidc-session" kind; ExpiresAt is Unix seconds.
	SessionProvider string `json:"sessionProvider,omitempty"`
	ExpiresAt       int64  `json:"expiresAt,omitempty"`
}

type repositoryBrowseItem struct {
	Repository  string         `json:"repository"`
	Format      string         `json:"format"`
	Component   string         `json:"component"`
	Version     string         `json:"version,omitempty"`
	Coordinates map[string]any `json:"coordinates"`
	Asset       domain.Asset   `json:"asset"`
}

type browseRepositoryDescriptor struct {
	Name   string `json:"name"`
	Format string `json:"format"`
	Type   string `json:"type"`
}

type searchFilter struct {
	query          string
	pathPrefix     string
	classification string
	attributes     []attributeFilter
	repositories   map[string]struct{}
}

type attributeFilter struct {
	path    string
	value   any
	encoded string
}

func (s *Server) handleWhoAmI(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		httpx.MethodNotAllowed(w, http.MethodGet)
		return
	}

	user, authenticated, authenticationKind := s.identity.AuthenticateWithKind(r)
	if !authenticated && requestSuppliesCredentials(r) {
		identity.RequestSessionAuthentication(w)
		return
	}
	if !authenticated {
		authenticationKind = "anonymous"
	}

	roles, privileges, err := s.identityAuthorization(r, &user, authenticated)
	if errors.Is(err, domain.ErrNotFound) {
		identity.RequestSessionAuthentication(w)
		return
	}
	if err != nil {
		httpx.WriteServerProblem(
			w,
			http.StatusInternalServerError,
			"authorization_error",
			"authorization failed",
			err,
		)
		return
	}
	loginProviders, err := s.loginProviderNames(r)
	if err != nil {
		httpx.WriteServerProblem(
			w,
			http.StatusInternalServerError,
			"login_provider_error",
			"list login providers failed",
			err,
		)
		return
	}
	response := identityResponse{
		Authenticated:       authenticated,
		Username:            user.Username,
		Administrator:       authenticated && user.Admin,
		AuthenticationKind:  authenticationKind,
		Roles:               roles,
		EffectivePrivileges: privileges,
		LoginProviders:      loginProviders,
	}
	if authenticated && authenticationKind == "oidc-session" {
		if provider, expiresAt, ok := s.identity.OIDCSessionHint(r); ok {
			response.SessionProvider = provider
			response.ExpiresAt = expiresAt
		}
	}
	httpx.WriteJSON(w, http.StatusOK, response)
}

// loginProviderNames lists the providers a browser can log in through. Interactive
// login needs the state secret that signs the login transaction, so without it no
// provider is offered even when some are configured for bearer authentication.
func (s *Server) loginProviderNames(r *http.Request) ([]string, error) {
	names := []string{}
	if s.cfg.OIDCStateSecret == "" {
		return names, nil
	}
	providers, err := s.oidcReads().OIDCProviders(r.Context())
	if err != nil {
		return nil, err
	}
	for _, provider := range providers {
		names = append(names, provider.Name)
	}
	return names, nil
}

func requestSuppliesCredentials(r *http.Request) bool {
	_, _, basic := r.BasicAuth()
	return basic || r.Header.Get("Authorization") != ""
}

func (s *Server) identityAuthorization(
	r *http.Request,
	user *domain.User,
	authenticated bool,
) ([]string, []string, error) {
	if !authenticated {
		privileges, err := s.privilegeReads().EffectivePrivileges(r.Context(), "")
		return []string{"anonymous"}, sortedUnique(privileges), err
	}

	var roles []string
	var privileges []string
	var err error
	if user.External {
		roles = append([]string(nil), user.Roles...)
		privileges, err = s.privilegeReads().PrivilegesForRoles(r.Context(), roles)
	} else if user.Username != "" {
		var current domain.User
		current, privileges, err = s.privilegeReads().LocalAuthorization(r.Context(), user.Username, user.Identity)
		roles = current.Roles
		user.Admin = current.Admin
	} else {
		roles = []string{"anonymous"}
		privileges, err = s.privilegeReads().EffectivePrivileges(r.Context(), "")
	}
	if err != nil {
		return nil, nil, err
	}
	if user.Admin {
		privileges = []string{"*"}
	}
	if len(user.TokenScopes) != 0 {
		privileges = effectiveScopedPrivileges(privileges, user.TokenScopes)
	}
	return sortedUnique(roles), sortedUnique(privileges), nil
}

func effectiveScopedPrivileges(privileges, scopes []string) []string {
	result := make([]string, 0, len(privileges)*len(scopes))
	for _, privilege := range privileges {
		for _, scope := range scopes {
			if intersection, ok := intersectPrivileges(privilege, scope); ok {
				result = append(result, intersection)
			}
		}
	}
	return result
}

func intersectPrivileges(left, right string) (string, bool) {
	leftParts := strings.Split(left, ":")
	rightParts := strings.Split(right, ":")
	leftVariable := leftParts[len(leftParts)-1] == "*"
	rightVariable := rightParts[len(rightParts)-1] == "*"
	if !leftVariable && rightVariable && len(leftParts) < len(rightParts) {
		return "", false
	}
	if leftVariable && !rightVariable && len(rightParts) < len(leftParts) {
		return "", false
	}
	length := max(len(leftParts), len(rightParts))
	if !leftVariable && !rightVariable && len(leftParts) != len(rightParts) {
		return "", false
	}
	if !leftVariable {
		length = len(leftParts)
	}
	if !rightVariable {
		if !leftVariable && length != len(rightParts) {
			return "", false
		}
		length = len(rightParts)
	}
	intersection := make([]string, length)
	for index := 0; index < length; index++ {
		leftPart, leftOK := privilegePart(leftParts, leftVariable, index)
		rightPart, rightOK := privilegePart(rightParts, rightVariable, index)
		if !leftOK || !rightOK {
			return "", false
		}
		switch {
		case leftPart == rightPart:
			intersection[index] = leftPart
		case leftPart == "*":
			intersection[index] = rightPart
		case rightPart == "*":
			intersection[index] = leftPart
		default:
			return "", false
		}
	}
	return strings.Join(intersection, ":"), true
}

func privilegePart(parts []string, variable bool, index int) (string, bool) {
	if index < len(parts) {
		return parts[index], true
	}
	if variable {
		return "*", true
	}
	return "", false
}

func sortedUnique(values []string) []string {
	seen := make(map[string]struct{}, len(values))
	result := make([]string, 0, len(values))
	for _, value := range values {
		if _, exists := seen[value]; exists {
			continue
		}
		seen[value] = struct{}{}
		result = append(result, value)
	}
	sort.Strings(result)
	return result
}

func (s *Server) handleBrowseRepositories(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		httpx.MethodNotAllowed(w, http.MethodGet)
		return
	}
	s.writeBrowseRepositoriesPage(w, r)
}

func (s *Server) handleRepositoryBrowse(
	w http.ResponseWriter,
	r *http.Request,
	repositoryName string,
) {
	if r.Method != http.MethodGet {
		httpx.MethodNotAllowed(w, http.MethodGet)
		return
	}
	repository, err := s.repositoryReads().Repository(r.Context(), repositoryName)
	if err != nil {
		httpx.WriteResult(w, nil, err)
		return
	}
	sources, err := s.discoverySources(r.Context(), []domain.Repository{repository})
	if err != nil {
		httpx.WriteResult(w, nil, err)
		return
	}
	component := r.URL.Query().Get("component")
	s.writeDiscoveryPage(w, r, sources, "", "repository-browse:"+repository.ID+":"+component,
		func(root domain.Repository, asset domain.Asset) (repositoryBrowseItem, bool) {
			asset.Repository = root.Name
			item := browseItems(root, []domain.Asset{asset})[0]
			return item, component == "" || item.Component == component
		})
}

func (s *Server) handleSearch(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		httpx.MethodNotAllowed(w, http.MethodGet)
		return
	}
	filter, err := parseSearchFilter(r)
	if err != nil {
		httpx.WriteProblem(w, http.StatusBadRequest, "invalid_search", err.Error())
		return
	}
	s.writeSearchPage(w, r, filter)
}

func (s *Server) browseRepositoryItems(
	ctx context.Context,
	repository domain.Repository,
	prefix string,
) ([]repositoryBrowseItem, error) {
	assets, err := s.logicalRepositoryAssets(
		ctx,
		repository,
		prefix,
		make(map[string]bool),
	)
	if err != nil {
		return nil, err
	}
	for index := range assets {
		assets[index].Repository = repository.Name
	}
	return browseItems(repository, assets), nil
}

func (s *Server) logicalRepositoryAssets(
	ctx context.Context,
	repository domain.Repository,
	prefix string,
	visited map[string]bool,
) ([]domain.Asset, error) {
	if visited[repository.Name] {
		return []domain.Asset{}, nil
	}
	visited[repository.Name] = true
	defer delete(visited, repository.Name)
	if repository.Type != "group" {
		return s.metadata.ForRepository(repository).Assets(ctx, prefix)
	}

	assets := make([]domain.Asset, 0)
	seenPaths := make(map[string]struct{})
	for _, memberName := range repository.Members {
		member, err := s.repositoryReads().Repository(ctx, memberName)
		if errors.Is(err, domain.ErrNotFound) {
			continue
		}
		if err != nil {
			return nil, err
		}
		if member.Format != repository.Format {
			continue
		}
		memberAssets, err := s.logicalRepositoryAssets(ctx, member, prefix, visited)
		if err != nil {
			return nil, err
		}
		for _, asset := range memberAssets {
			if _, exists := seenPaths[asset.Path]; exists {
				continue
			}
			seenPaths[asset.Path] = struct{}{}
			assets = append(assets, asset)
		}
	}
	return assets, nil
}

func (s *Server) readableRepositories(r *http.Request) ([]domain.Repository, error) {
	repositories, err := s.repositoryReads().Repositories(r.Context())
	if err != nil {
		return nil, err
	}
	user, authenticated := s.identity.Authenticate(r)
	readable := make([]domain.Repository, 0, len(repositories))
	for _, repository := range repositories {
		required := fmt.Sprintf("repository:%s:read", repository.Name)
		allowed, err := s.identity.UserHasPrivilege(r, user, authenticated, required)
		if err != nil {
			return nil, err
		}
		if allowed {
			readable = append(readable, repository)
		}
	}
	return readable, nil
}

func browseItems(repository domain.Repository, assets []domain.Asset) []repositoryBrowseItem {
	items := make([]repositoryBrowseItem, 0, len(assets))
	for _, asset := range assets {
		projected := content.ProjectAsset(asset, repository)
		coordinates, _ := projected.Attributes[repository.Format].(map[string]any)
		component, version := browseCoordinates(repository.Format, projected, coordinates)
		items = append(items, repositoryBrowseItem{
			Repository:  repository.Name,
			Format:      repository.Format,
			Component:   component,
			Version:     version,
			Coordinates: coordinates,
			Asset:       projected,
		})
	}
	return items
}

func browseCoordinates(
	format string,
	asset domain.Asset,
	coordinates map[string]any,
) (string, string) {
	if format == "oci" {
		component, _ := coordinates["image"].(string)
		if component == "" {
			component = asset.Path
		}
		if tag, ok := coordinates["tag"].(string); ok {
			return component, tag
		}
		if digest, ok := coordinates["digest"].(string); ok {
			return component, digest
		}
		return component, asset.Reference
	}
	publicPath := assetPublicPath(asset)
	directory := path.Dir(publicPath)
	if directory == "." {
		directory = "/"
	}
	return directory, path.Base(publicPath)
}

func filterBrowseComponent(
	items []repositoryBrowseItem,
	component string,
) []repositoryBrowseItem {
	filtered := make([]repositoryBrowseItem, 0)
	for _, item := range items {
		if item.Component == component {
			filtered = append(filtered, item)
		}
	}
	return filtered
}

func browseItemKey(item repositoryBrowseItem) string {
	return item.Repository + "\x00" + item.Component + "\x00" + item.Version + "\x00" + item.Asset.Path
}

func parseSearchFilter(r *http.Request) (searchFilter, error) {
	filter := searchFilter{
		query:          strings.TrimSpace(r.URL.Query().Get("q")),
		pathPrefix:     r.URL.Query().Get("pathPrefix"),
		classification: r.URL.Query().Get("classification"),
		repositories:   make(map[string]struct{}),
	}
	if utf8.RuneCountInString(filter.query) > 200 || len(filter.pathPrefix) > 1024 {
		return filter, fmt.Errorf("q or pathPrefix is too long")
	}
	for _, name := range r.URL.Query()["repository"] {
		if name == "" || len(name) > 128 {
			return filter, fmt.Errorf("repository must be a non-empty valid name")
		}
		filter.repositories[name] = struct{}{}
	}
	for _, encoded := range r.URL.Query()["attribute"] {
		attributePath, value, found := strings.Cut(encoded, "=")
		if !found || !validAttributePath(attributePath) || len(value) > 1024 {
			return filter, fmt.Errorf("attribute must use dot.path=value syntax")
		}
		filter.attributes = append(filter.attributes, attributeFilter{
			path:    attributePath,
			value:   searchPredicateValue(value),
			encoded: value,
		})
	}
	if len(filter.attributes) > 20 {
		return filter, fmt.Errorf("at most 20 attribute filters are allowed")
	}
	return filter, nil
}

func validAttributePath(value string) bool {
	if value == "" || len(value) > 256 {
		return false
	}
	for _, segment := range strings.Split(value, ".") {
		if segment == "" {
			return false
		}
		for _, character := range segment {
			if !(character >= 'a' && character <= 'z') &&
				!(character >= 'A' && character <= 'Z') &&
				!(character >= '0' && character <= '9') &&
				character != '-' && character != '_' {
				return false
			}
		}
	}
	return true
}

func searchPredicateValue(value string) any {
	var typed any
	decoder := json.NewDecoder(strings.NewReader(value))
	decoder.UseNumber()
	if err := decoder.Decode(&typed); err == nil {
		var trailing any
		if errors.Is(decoder.Decode(&trailing), io.EOF) {
			return typed
		}
	}
	return value
}

func searchMatches(item repositoryBrowseItem, filter searchFilter) bool {
	attributes := item.Asset.Attributes
	if filter.classification != "" {
		if !assetattrs.ClassificationHasValue(attributes, filter.classification) {
			return false
		}
	}
	for _, expected := range filter.attributes {
		if !predicate.Match(attributes, domain.Predicate{
			Path:  expected.path,
			Op:    "=",
			Value: expected.value,
		}, time.Now().UTC()) {
			return false
		}
	}
	if filter.query == "" {
		return true
	}
	query := strings.ToLower(filter.query)
	if strings.Contains(strings.ToLower(item.Repository), query) ||
		strings.Contains(strings.ToLower(assetPublicPath(item.Asset)), query) ||
		strings.Contains(strings.ToLower(item.Component), query) ||
		strings.Contains(strings.ToLower(item.Version), query) {
		return true
	}
	encoded, err := json.Marshal(attributes)
	return err == nil && strings.Contains(strings.ToLower(string(encoded)), query)
}

func (filter searchFilter) identity() string {
	repositories := make([]string, 0, len(filter.repositories))
	for repository := range filter.repositories {
		repositories = append(repositories, repository)
	}
	sort.Strings(repositories)
	attributes := make([]string, 0, len(filter.attributes))
	for _, attribute := range filter.attributes {
		attributes = append(attributes, attribute.path+"="+attribute.encoded)
	}
	encoded, err := json.Marshal(struct {
		Query          string   `json:"query"`
		PathPrefix     string   `json:"pathPrefix"`
		Classification string   `json:"classification"`
		Attributes     []string `json:"attributes"`
		Repositories   []string `json:"repositories"`
	}{
		Query:          filter.query,
		PathPrefix:     filter.pathPrefix,
		Classification: filter.classification,
		Attributes:     attributes,
		Repositories:   repositories,
	})
	if err != nil {
		panic(fmt.Sprintf("encode search filter: %v", err))
	}
	digest := sha256.Sum256(encoded)
	return base64.RawURLEncoding.EncodeToString(digest[:])
}
