package domain

import (
	"encoding/hex"
	"encoding/json"
	"fmt"
	"math"
	"net"
	"net/url"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/suxen-project/suxen/internal/jsonnumber"
	"github.com/suxen-project/suxen/internal/rawcomponent"
	spiblob "github.com/suxen-project/suxen/spi/blob"
	spiformat "github.com/suxen-project/suxen/spi/format"
)

// InstanceDefaultsName is the reserved resource name that addresses the
// instance-wide policy default for the classification, trustPolicy, and
// downloadGate kinds. A resource of one of those kinds named this way sets the
// singleton default that repositories inherit; every other name is a per-repository
// resource. The name is reserved for repositories so the two never collide on one
// provisioning-record key.
const InstanceDefaultsName = "default"

// These byte limits keep names and paths below PostgreSQL's B-tree index entry
// limit, including the repository identity stored alongside each asset path.
const (
	MaxResourceNameBytes = 255
	MaxUsernameBytes     = 255
	MaxAssetPathBytes    = 2048
)

// ValidUsername accepts addressable local account names. A leading letter or
// digit excludes dot path segments; the remaining characters are safe in a
// URL path segment and do not conflict with HTTP Basic's colon separator.
func ValidUsername(username string) bool {
	if username == "" || len(username) > MaxUsernameBytes {
		return false
	}
	for index := 0; index < len(username); index++ {
		char := username[index]
		alphanumeric := char >= 'a' && char <= 'z' || char >= 'A' && char <= 'Z' || char >= '0' && char <= '9'
		if alphanumeric {
			continue
		}
		if index == 0 || char != '.' && char != '_' && char != '@' && char != '+' && char != '-' {
			return false
		}
	}
	return true
}

// Repository describes one logical artifact endpoint.
//
// Format and Type are independent: repositories of any format can be hosted,
// proxy, or group repositories.
type Repository struct {
	// ID is the opaque internal identity, durable for the repository's lifetime
	// and fresh on any delete+recreate. It is never part of the public surface:
	// it is not serialized, and no API, provisioning, group-member, or SPI input
	// accepts it. Populated when the store loads a repository.
	ID        string   `json:"-"`
	Name      string   `json:"name"`
	Format    string   `json:"format"`
	Type      string   `json:"type"`
	BlobStore string   `json:"blobStore,omitempty"`
	Upstream  string   `json:"upstream,omitempty"`
	Members   []string `json:"members,omitempty"`
	Writable  bool     `json:"writable"`
	// AllowOverwrite controls replacement of hosted artifacts. Nil means omitted
	// on input; stored hosted repositories always return an effective value.
	AllowOverwrite *bool `json:"allowOverwrite,omitempty"`
	Managed        bool  `json:"managed"`
	// FormatConfig carries format-owned repository settings (for example the
	// Maven version policy). Its schema is validated by the format plugin;
	// formats without a validator accept no configuration.
	FormatConfig map[string]any `json:"formatConfig,omitempty"`
	// Endpoints bind an OCI repository to extra registry roots. Hosts match the
	// request Host header (without port). Ports bind additional process listeners
	// so clients can use host:port without a unique DNS name. The path form
	// /repository/{name}/v2/ remains available on every listener.
	Endpoints *RepositoryEndpoints `json:"endpoints,omitempty"`
	CreatedAt time.Time            `json:"createdAt"`
}

// DefaultAllowOverwrite retains existing publication behavior when a new
// hosted repository does not declare an overwrite policy.
func DefaultAllowOverwrite(format string) bool {
	switch format {
	case "npm", "cargo", "pypi":
		return false
	default:
		return true
	}
}

// RepositoryEndpoints declares optional host- and port-based OCI registry roots.
type RepositoryEndpoints struct {
	Hosts []string `json:"hosts,omitempty"`
	Ports []int    `json:"ports,omitempty"`
}

func (endpoints *RepositoryEndpoints) Empty() bool {
	return endpoints == nil || (len(endpoints.Hosts) == 0 && len(endpoints.Ports) == 0)
}

// Validate checks the repository's format-independent configuration, then
// delegates format-owned validation to the registered format plugin.
func (r Repository) Validate() error {
	if r.Name == "" || !validName(r.Name) {
		return ErrInvalidRepository
	}
	if r.Name == InstanceDefaultsName {
		return ErrReservedRepositoryName
	}
	if !spiformat.Registered(r.Format) {
		return ErrInvalidFormat
	}
	if r.Type != "hosted" && r.Type != "proxy" && r.Type != "group" {
		return ErrInvalidType
	}
	if r.Type != "hosted" && r.AllowOverwrite != nil {
		return ErrInvalidOverwritePolicy
	}
	if r.BlobStore != "" && !validName(r.BlobStore) {
		return ErrInvalidBlobStore
	}
	if r.Type == "proxy" {
		if r.Upstream == "" {
			return ErrUpstreamRequired
		}
		upstream, err := url.Parse(r.Upstream)
		if err != nil || upstream.Host == "" ||
			(upstream.Scheme != "http" && upstream.Scheme != "https") {
			return ErrInvalidUpstream
		}
	}
	if r.Type == "group" && len(r.Members) == 0 {
		return ErrMembersRequired
	}
	if err := r.validateEndpoints(); err != nil {
		return err
	}
	return r.validateFormatConfig()
}

// SameUpstreamEndpoint reports whether two upstream URLs address the same
// origin. Scheme, host, port, path, and query must match; the embedded
// credentials (URL userinfo) are ignored so they can rotate freely. Empty
// upstreams (hosted and group repositories) compare equal only to each other.
func SameUpstreamEndpoint(a, b string) (bool, error) {
	if a == b {
		return true, nil
	}
	if a == "" || b == "" {
		return false, nil
	}
	parsedA, err := url.Parse(a)
	if err != nil {
		return false, fmt.Errorf("parse upstream URL: %w", err)
	}
	parsedB, err := url.Parse(b)
	if err != nil {
		return false, fmt.Errorf("parse upstream URL: %w", err)
	}
	parsedA.User = nil
	parsedB.User = nil
	return parsedA.String() == parsedB.String(), nil
}

func (r Repository) validateEndpoints() error {
	if r.Endpoints.Empty() {
		return nil
	}
	if r.Format != "oci" {
		return ErrOCIEndpointsNotSupported
	}
	seenHosts := make(map[string]struct{}, len(r.Endpoints.Hosts))
	for _, host := range r.Endpoints.Hosts {
		normalized, err := CanonicalOCIEndpointHost(host)
		if err != nil {
			return err
		}
		if _, exists := seenHosts[normalized]; exists {
			return ErrInvalidOCIEndpoint
		}
		seenHosts[normalized] = struct{}{}
	}
	seenPorts := make(map[int]struct{}, len(r.Endpoints.Ports))
	for _, port := range r.Endpoints.Ports {
		if port < 1 || port > 65535 {
			return ErrInvalidOCIEndpoint
		}
		if _, exists := seenPorts[port]; exists {
			return ErrInvalidOCIEndpoint
		}
		seenPorts[port] = struct{}{}
	}
	return nil
}

func CanonicalOCIEndpointHost(host string) (string, error) {
	host = strings.ToLower(strings.TrimSpace(host))
	if host == "" || strings.ContainsAny(host, "/:\\ ") {
		return "", ErrInvalidOCIEndpoint
	}
	parsed, err := url.Parse("http://" + host)
	if err != nil || parsed.Host != host || parsed.User != nil ||
		parsed.Path != "" || parsed.RawQuery != "" || parsed.Fragment != "" {
		return "", ErrInvalidOCIEndpoint
	}
	return host, nil
}

// CanonicalOCIRequestHost extracts the hostname from a request Host header.
func CanonicalOCIRequestHost(hostHeader string) string {
	host := strings.TrimSpace(hostHeader)
	if host == "" {
		return ""
	}
	if hostname, _, err := net.SplitHostPort(host); err == nil {
		host = hostname
	}
	canonical, err := CanonicalOCIEndpointHost(host)
	if err != nil {
		return strings.ToLower(host)
	}
	return canonical
}

// OCIEndpointsOverlap reports whether two endpoint bindings share a host or port.
func OCIEndpointsOverlap(left *RepositoryEndpoints, right *RepositoryEndpoints) bool {
	if left.Empty() || right.Empty() {
		return false
	}
	hosts := make(map[string]struct{}, len(left.Hosts))
	for _, host := range left.Hosts {
		canonical, err := CanonicalOCIEndpointHost(host)
		if err != nil {
			continue
		}
		hosts[canonical] = struct{}{}
	}
	for _, host := range right.Hosts {
		canonical, err := CanonicalOCIEndpointHost(host)
		if err != nil {
			continue
		}
		if _, exists := hosts[canonical]; exists {
			return true
		}
	}
	ports := make(map[int]struct{}, len(left.Ports))
	for _, port := range left.Ports {
		ports[port] = struct{}{}
	}
	for _, port := range right.Ports {
		if _, exists := ports[port]; exists {
			return true
		}
	}
	return false
}

// validateFormatConfig hands the repository to the owning format plugin.
// Formats without a validator hook accept no configuration, keeping
// unvalidated settings out of the stored resource.
func (r Repository) validateFormatConfig() error {
	if r.Format == "raw" {
		return r.validateRawFormatConfig()
	}
	registered, found := spiformat.Lookup(r.Format)
	if !found {
		if len(r.FormatConfig) != 0 {
			return &spiformat.PolicyViolation{
				Code:    "invalid_format_config",
				Message: "format " + r.Format + " does not accept formatConfig",
			}
		}
		return nil
	}
	validator, ok := registered.(spiformat.RepositoryValidator)
	if !ok {
		if len(r.FormatConfig) != 0 {
			return &spiformat.PolicyViolation{
				Code:    "invalid_format_config",
				Message: "format " + r.Format + " does not accept formatConfig",
			}
		}
		return nil
	}
	return validator.ValidateRepository(r.FormatView())
}

// validateRawFormatConfig accepts the core Raw component rules. A group stores
// no assets of its own, so component rules would never apply to it.
func (r Repository) validateRawFormatConfig() error {
	if len(r.FormatConfig) == 0 {
		return nil
	}
	if r.Type == "group" {
		return &spiformat.PolicyViolation{
			Code:    "invalid_format_config",
			Message: "raw group repositories do not accept formatConfig",
		}
	}
	if err := rawcomponent.Validate(r.FormatConfig); err != nil {
		return &spiformat.PolicyViolation{Code: "invalid_format_config", Message: err.Error()}
	}
	return nil
}

// FormatView projects the asset onto the format SPI's read-only view.
func (a Asset) FormatView() spiformat.Asset {
	formatPath := a.Path
	if a.FormatPath != "" {
		formatPath = a.FormatPath
	}
	return spiformat.Asset{
		Repository:  a.Repository,
		Path:        formatPath,
		Digest:      a.Digest,
		Size:        a.Size,
		ContentType: a.ContentType,
		Kind:        a.Kind,
		Reference:   a.Reference,
		UpdatedAt:   a.UpdatedAt,
		ValidatedAt: a.ValidatedAt,
	}
}

// FormatView projects the repository onto the format SPI's read-only view.
func (r Repository) FormatView() spiformat.Repository {
	return spiformat.Repository{
		Name:           r.Name,
		Format:         r.Format,
		Type:           r.Type,
		Upstream:       r.Upstream,
		Config:         r.FormatConfig,
		AllowOverwrite: r.AllowOverwrite != nil && *r.AllowOverwrite,
	}
}

// ConfigurationReference identifies one server-side source for driver configuration.
// Exactly one of Env or File must be set. The referenced value may contain credentials;
// the reference is safe to return from the administration API, while the resolved value
// is never included in API responses.
type ConfigurationReference struct {
	Env  string `json:"env,omitempty"`
	File string `json:"file,omitempty"`
}

// Blob-store lifecycle states. A store is Active until an operator drains it
// onto a target; migration copies its data across and, once the source holds no
// referenced blobs, marks it Drained (safe to remove). Draining is reversible.
const (
	BlobStoreStateActive   = "active"
	BlobStoreStateDraining = "draining"
	BlobStoreStateDrained  = "drained"
)

// BlobStore describes a named blob storage backend shared by repositories.
type BlobStore struct {
	Name             string                  `json:"name"`
	Driver           string                  `json:"driver"`
	ConfigurationRef *ConfigurationReference `json:"configurationRef,omitempty"`
	Attributes       map[string]any          `json:"attributes,omitempty"`
	PhysicalIdentity string                  `json:"-"`
	Managed          bool                    `json:"managed"`
	// State is the lifecycle state; DrainTarget names the store data migrates
	// to while State is draining. Both are set only through the drain action,
	// never a resource update, so an ordinary edit preserves them.
	State       string    `json:"state"`
	DrainTarget string    `json:"drainTarget,omitempty"`
	CreatedAt   time.Time `json:"createdAt"`
}

// Validate checks the blob store name, driver, and configuration source syntax.
func (store BlobStore) Validate() error {
	if store.Name == "" || !validName(store.Name) {
		return ErrInvalidBlobStore
	}
	if store.Driver == "" || !validName(store.Driver) {
		return ErrInvalidBlobStoreDriver
	}
	if store.ConfigurationRef == nil {
		return ErrBlobStoreConfigRequired
	}
	if len(store.PhysicalIdentity) != 64 ||
		store.PhysicalIdentity != strings.ToLower(store.PhysicalIdentity) {
		return ErrInvalidBlobStoreIdentity
	}
	if _, err := hex.DecodeString(store.PhysicalIdentity); err != nil {
		return ErrInvalidBlobStoreIdentity
	}
	if store.ConfigurationRef != nil {
		hasEnvironment := store.ConfigurationRef.Env != ""
		hasFile := store.ConfigurationRef.File != ""
		if hasEnvironment == hasFile {
			return ErrInvalidBlobStoreConfig
		}
		if hasEnvironment && !validEnvironmentName(store.ConfigurationRef.Env) {
			return ErrInvalidBlobStoreConfig
		}
		if hasFile && !filepath.IsAbs(store.ConfigurationRef.File) {
			return ErrInvalidBlobStoreConfig
		}
	}
	if err := validateBlobStoreAttributes(store.Attributes); err != nil {
		return err
	}
	return nil
}

func validateBlobStoreAttributes(attributes map[string]any) error {
	uploadSessions, configured := attributes["uploadSessions"]
	if !configured {
		return nil
	}
	policy, ok := uploadSessions.(map[string]any)
	if !ok {
		return ErrInvalidBlobStoreAttributes
	}
	allowed := map[string]bool{
		"staleAfter":              true,
		"maxStagedBytes":          true,
		"maxPrincipalStagedBytes": true,
		"maxPrincipalSessions":    true,
	}
	for name := range policy {
		if !allowed[name] {
			return fmt.Errorf(
				"%w: unknown uploadSessions field %q",
				ErrInvalidBlobStoreAttributes,
				name,
			)
		}
	}
	if value, found := policy["staleAfter"]; found {
		text, ok := value.(string)
		if !ok {
			return fmt.Errorf(
				"%w: uploadSessions.staleAfter must be a duration",
				ErrInvalidBlobStoreAttributes,
			)
		}
		duration, err := time.ParseDuration(text)
		if err != nil || duration <= 0 {
			return fmt.Errorf(
				"%w: uploadSessions.staleAfter must be a positive duration",
				ErrInvalidBlobStoreAttributes,
			)
		}
	}
	for _, name := range []string{
		"maxStagedBytes",
		"maxPrincipalStagedBytes",
		"maxPrincipalSessions",
	} {
		if value, found := policy[name]; found {
			if _, ok := PositiveInt64(value); !ok {
				return fmt.Errorf(
					"%w: uploadSessions.%s must be a positive integer",
					ErrInvalidBlobStoreAttributes,
					name,
				)
			}
		}
	}
	return nil
}

// PositiveInt64 converts an exact positive upload-session limit. Validation and
// runtime quota enforcement share this conversion so accepted limits take effect.
func PositiveInt64(value any) (int64, bool) {
	switch number := value.(type) {
	case int:
		return int64(number), number > 0
	case int8:
		return int64(number), number > 0
	case int16:
		return int64(number), number > 0
	case int32:
		return int64(number), number > 0
	case int64:
		return number, number > 0
	case uint:
		return positiveUint64(uint64(number))
	case uint8:
		return int64(number), number > 0
	case uint16:
		return int64(number), number > 0
	case uint32:
		return int64(number), number > 0
	case uint64:
		return positiveUint64(number)
	case float32:
		return positiveFloat64(float64(number))
	case float64:
		return positiveFloat64(number)
	case json.Number:
		parsed, ok := jsonnumber.Parse(string(number))
		if !ok {
			return 0, false
		}
		integer, ok := parsed.Int64()
		return integer, ok && integer > 0
	default:
		return 0, false
	}
}

func positiveUint64(value uint64) (int64, bool) {
	return int64(value), value > 0 && value <= math.MaxInt64
}

func positiveFloat64(value float64) (int64, bool) {
	if value < 1 || value >= 0x1p63 || math.Trunc(value) != value {
		return 0, false
	}
	return int64(value), true
}

func validEnvironmentName(value string) bool {
	for index, character := range value {
		if character == '_' || character >= 'A' && character <= 'Z' ||
			index > 0 && character >= '0' && character <= '9' {
			continue
		}
		return false
	}
	return value != ""
}

// RedactURLSecrets removes URL components that may carry credentials before a
// configured endpoint is disclosed in an API response or diagnostic.
func RedactURLSecrets(value string) string {
	parsed, err := url.Parse(value)
	if err != nil {
		return ""
	}
	parsed.User = nil
	parsed.RawQuery = ""
	parsed.ForceQuery = false
	parsed.Fragment = ""
	return parsed.String()
}

// MarshalJSON removes upstream URL credentials from API responses.
func (r Repository) MarshalJSON() ([]byte, error) {
	type repositoryAlias Repository
	redacted := repositoryAlias(r)
	if redacted.Upstream != "" {
		redacted.Upstream = RedactURLSecrets(redacted.Upstream)
	}
	return json.Marshal(redacted)
}

func validName(s string) bool {
	if s == "" || len(s) > MaxResourceNameBytes {
		return false
	}
	for _, r := range s {
		if !((r >= 'a' && r <= 'z') || (r >= '0' && r <= '9') || r == '-' || r == '_') {
			return false
		}
	}
	return true
}

// ValidAssetPath reports whether a repository-relative path fits every
// supported metadata backend. Format-specific grammar is checked separately.
func ValidAssetPath(path string) bool {
	return path != "" && len(path) <= MaxAssetPathBytes && utf8.ValidString(path)
}

// Asset maps a repository-relative path to an immutable content-addressed blob.
type Asset struct {
	ID         int64  `json:"id"`
	Repository string `json:"repository"`
	// RepositoryID pins the write to one repository identity captured at the
	// start of the operation. When set, the store writes against it instead of
	// re-resolving the name, so a delete+recreate under the same name cannot
	// redirect a stale write into the replacement. Never persisted or exposed.
	RepositoryID string `json:"-"`
	// ProxyFetch is an internal publication precondition for an upstream result.
	// It is never persisted or exposed to clients.
	ProxyFetch ProxyFetchToken `json:"-"`
	Path       string          `json:"path"`
	// FormatPath is the client-facing path for a proxy asset whose stored Path
	// is an opaque cache identity. Empty means Path is also the format path.
	FormatPath string `json:"formatPath,omitempty"`
	Digest     string `json:"digest"`
	Size       int64  `json:"size"`
	// BlobStore names the blob store physically holding this asset's bytes. It
	// is the asset's own home store, not derived from its repository, so an
	// asset can be relocated independently of where its repository points.
	BlobStore     string         `json:"blobStore,omitempty"`
	ContentType   string         `json:"contentType"`
	Kind          string         `json:"kind"`
	Reference     string         `json:"reference,omitempty"`
	SubjectDigest string         `json:"subjectDigest,omitempty"`
	Attributes    map[string]any `json:"attributes,omitempty"`
	CreatedAt     time.Time      `json:"createdAt"`
	UpdatedAt     time.Time      `json:"updatedAt"`
	ValidatedAt   time.Time      `json:"validatedAt"`
	LastAccessed  *time.Time     `json:"lastAccessed,omitempty"`
	Dependencies  []string       `json:"-"`
	// Component and ComponentVersion are a Raw asset's stored component
	// identity; other formats leave them empty. ComponentStored marks a row
	// read from the store, whose columns are then authoritative.
	Component        string `json:"-"`
	ComponentVersion string `json:"-"`
	ComponentStored  bool   `json:"-"`
	// Immutable is a caller publication precondition for proxy cache paths and
	// distinguishes per-version metadata from mutable hosted indexes. Hosted
	// artifact replacement follows the repository policy. Never persisted.
	Immutable bool `json:"-"`
	// ImmutableIdentity protects a stable format-owned claim even when a hosted
	// repository permits artifact replacement. Identical retries remain valid.
	ImmutableIdentity bool `json:"-"`
}

// ProxyFetchToken orders concurrent upstream results for one cache path.
type ProxyFetchToken struct {
	Epoch    string
	Sequence int64
}

// BlobInfo is the metadata returned by a content-addressed blob store. It is
// the public blob-store SPI type, aliased so internal code and third-party
// drivers exchange identical values.
type BlobInfo = spiblob.Info

// User is an authenticated local account.
type User struct {
	Username string `json:"username"`
	// Identity is the immutable local account generation, kept off the API.
	Identity    string    `json:"-"`
	Admin       bool      `json:"admin"`
	Managed     bool      `json:"managed"`
	CreatedAt   time.Time `json:"createdAt"`
	TokenScopes []string  `json:"-"`
	External    bool      `json:"-"`
	Roles       []string  `json:"-"`
}

// Role is a named set of privileges. Composition happens at the assignment
// boundary: a subject holds multiple roles rather than a role including others.
type Role struct {
	Name        string    `json:"name"`
	Description string    `json:"description,omitempty"`
	Privileges  []string  `json:"privileges"`
	Managed     bool      `json:"managed"`
	CreatedAt   time.Time `json:"createdAt"`
}

// APIToken describes a revocable token without exposing its secret or hash.
type APIToken struct {
	ID        int64     `json:"id"`
	Username  string    `json:"username"`
	Name      string    `json:"name"`
	Scopes    []string  `json:"scopes,omitempty"`
	CreatedAt time.Time `json:"createdAt"`
}

// OIDCProvider configures JWT validation and external group-to-role mapping.
type OIDCProvider struct {
	Name         string              `json:"name"`
	Issuer       string              `json:"issuer"`
	ClientID     string              `json:"clientId"`
	ClientSecret string              `json:"-"`
	Scopes       []string            `json:"scopes"`
	GroupsClaim  string              `json:"groupsClaim"`
	DefaultRoles []string            `json:"defaultRoles,omitempty"`
	GroupRoles   map[string][]string `json:"groupRoles,omitempty"`
	// AllowPasswordGrant enables the OAuth2 Resource Owner Password Credentials
	// grant for this provider, letting `docker login` username/password reach the
	// IdP directly. Only safe for an internal IdP you control (no MFA, deprecated
	// grant); keep it off for remote providers.
	AllowPasswordGrant bool      `json:"allowPasswordGrant"`
	Managed            bool      `json:"managed"`
	CreatedAt          time.Time `json:"createdAt"`
}

// Validate checks the required OIDC provider configuration.
func (p OIDCProvider) Validate() error {
	if p.Name == "" || !validName(p.Name) {
		return ErrInvalidOIDCProvider
	}
	parsed, err := url.Parse(p.Issuer)
	if err != nil || parsed.Host == "" || parsed.User != nil ||
		parsed.RawQuery != "" || parsed.Fragment != "" ||
		(parsed.Scheme != "https" && parsed.Scheme != "http") {
		return ErrInvalidOIDCIssuer
	}
	if p.ClientID == "" {
		return ErrOIDCClientIDRequired
	}
	return nil
}

// Validate checks that a role name and all privileges use valid syntax.
func (r Role) Validate() error {
	if r.Name == "" || !validName(r.Name) {
		return ErrInvalidRole
	}
	for _, privilege := range r.Privileges {
		if privilege == "" {
			return ErrInvalidPrivilege
		}
	}
	return nil
}

// classificationKeyPattern bounds a classification label key to one namespace
// segment, so the assigned label lands at classification.<key>.
var classificationKeyPattern = regexp.MustCompile(`^[A-Za-z0-9_-]+$`)

// ClassificationRule assigns the namespaced label classification.<Key> = Value to
// an asset whose projected attributes satisfy every predicate in When. When is an
// AND of conditions; an empty When always matches.
type ClassificationRule struct {
	When  []Predicate `json:"when,omitempty"`
	Key   string      `json:"key"`
	Value string      `json:"value"`
}

// ClassificationConfig holds the ordered classification rules for one repository.
// Rules are evaluated in order against each asset; every matching rule sets its
// classification.<Key> label and a later rule overwrites an earlier one on the
// same key. An asset matched by no rule receives no classification.* labels.
//
// A config with an empty Repository is the instance-wide default: InheritGlobal
// is unused there, and a repository config whose InheritGlobal is true prepends
// the default's rules to its own (so a repository rule wins on key collision).
type ClassificationConfig struct {
	Repository    string               `json:"repository"`
	Rules         []ClassificationRule `json:"rules"`
	InheritGlobal bool                 `json:"inheritGlobal"`
	Managed       bool                 `json:"managed"`
	UpdatedAt     time.Time            `json:"updatedAt"`
}

// Validate checks classification rule keys, values, and predicate conditions.
func (config ClassificationConfig) Validate() error {
	if config.Repository == "" || !validName(config.Repository) {
		return ErrInvalidRepository
	}
	return config.validateRules()
}

// ValidateDefaults checks the instance-wide classification default, which carries
// no repository.
func (config ClassificationConfig) ValidateDefaults() error {
	if config.Repository != "" {
		return ErrInvalidRepository
	}
	return config.validateRules()
}

func (config ClassificationConfig) validateRules() error {
	for _, rule := range config.Rules {
		if !classificationKeyPattern.MatchString(rule.Key) {
			return ErrClassificationKeyRequired
		}
		if rule.Value == "" {
			return ErrClassificationValueRequired
		}
		for _, condition := range rule.When {
			if err := condition.Validate(); err != nil {
				return err
			}
		}
	}
	return nil
}

// Predicate is one typed condition evaluated against an asset's projected
// attribute view. Policy criteria evaluate predicates in their listed order
// and require every predicate to match.
type Predicate struct {
	Path  string `json:"path"`
	Op    string `json:"op"`
	Value any    `json:"value,omitempty"`
}

// CleanupCriteria is an ordered list of predicates that an asset must satisfy
// for deletion.
type CleanupCriteria []Predicate

// CleanupPolicy defines a reusable retention policy attached to repositories.
type CleanupPolicy struct {
	Name         string          `json:"name"`
	Repositories []string        `json:"repositories"`
	Criteria     CleanupCriteria `json:"criteria"`
	KeepLast     int             `json:"keepLast,omitempty"`
	// Order ranks keepLast candidates within a component: CleanupOrderUpdatedAt
	// (newest update first) or CleanupOrderVersion (highest version first).
	Order     string    `json:"order"`
	Action    string    `json:"action"`
	Enabled   bool      `json:"enabled"`
	Managed   bool      `json:"managed"`
	CreatedAt time.Time `json:"createdAt"`
	UpdatedAt time.Time `json:"updatedAt"`
}

// Cleanup policy orders. An empty order means CleanupOrderUpdatedAt.
const (
	CleanupOrderUpdatedAt = "updatedAt"
	CleanupOrderVersion   = "version"
)

// Validate checks cleanup policy syntax without resolving repository references.
func (policy CleanupPolicy) Validate() error {
	if policy.Name == "" || !validName(policy.Name) {
		return ErrInvalidCleanupPolicy
	}
	if len(policy.Repositories) == 0 {
		return ErrCleanupRepositoriesRequired
	}
	if len(policy.Criteria) == 0 {
		return ErrCleanupCriteriaRequired
	}
	if policy.KeepLast < 0 {
		return ErrInvalidKeepLast
	}
	if policy.Action != "" && policy.Action != "delete" {
		return ErrInvalidCleanupAction
	}
	if policy.Order != "" && policy.Order != CleanupOrderUpdatedAt && policy.Order != CleanupOrderVersion {
		return ErrInvalidCleanupOrder
	}
	for _, predicate := range policy.Criteria {
		if err := predicate.Validate(); err != nil {
			return err
		}
	}
	return nil
}

// ParseRetentionDuration parses Go durations and cleanup-friendly day, week,
// month, and year suffixes. Months and years are fixed approximations of 30 and
// 365 days respectively.
func ParseRetentionDuration(value string) (time.Duration, error) {
	multiplier := time.Duration(0)
	numeric := ""
	switch {
	case strings.HasSuffix(value, "mo"):
		multiplier = 30 * 24 * time.Hour
		numeric = strings.TrimSuffix(value, "mo")
	case strings.HasSuffix(value, "y"):
		multiplier = 365 * 24 * time.Hour
		numeric = strings.TrimSuffix(value, "y")
	case strings.HasSuffix(value, "d"):
		multiplier = 24 * time.Hour
		numeric = strings.TrimSuffix(value, "d")
	case strings.HasSuffix(value, "w"):
		multiplier = 7 * 24 * time.Hour
		numeric = strings.TrimSuffix(value, "w")
	default:
		return time.ParseDuration(value)
	}
	amount, err := strconv.ParseFloat(numeric, 64)
	if err != nil || amount < 0 {
		return 0, fmt.Errorf("invalid retention duration %q", value)
	}
	return time.Duration(amount * float64(multiplier)), nil
}

// Task records one administrative background or manual operation.
type Task struct {
	ID          int64          `json:"id"`
	Type        string         `json:"type"`
	Status      string         `json:"status"`
	Policy      string         `json:"policy,omitempty"`
	Repository  string         `json:"repository,omitempty"`
	DryRun      bool           `json:"dryRun"`
	Result      map[string]any `json:"result,omitempty"`
	Error       string         `json:"error,omitempty"`
	CreatedAt   time.Time      `json:"createdAt"`
	StartedAt   *time.Time     `json:"startedAt,omitempty"`
	CompletedAt *time.Time     `json:"completedAt,omitempty"`
}

// Lease describes the current owner and expiry of a singleton background job.
type Lease struct {
	Name      string    `json:"name"`
	Holder    string    `json:"holder"`
	ExpiresAt time.Time `json:"expiresAt"`
}

const (
	// WebhookAssetUploaded identifies a newly created or replaced asset.
	WebhookAssetUploaded = "asset.uploaded"
	// WebhookAssetDeleted identifies an asset metadata deletion.
	WebhookAssetDeleted = "asset.deleted"
	// WebhookAssetDownloaded identifies a successfully streamed asset.
	WebhookAssetDownloaded = "asset.downloaded"
	// WebhookComponentCreated identifies the first asset in a logical component.
	WebhookComponentCreated = "component.created"
	// WebhookCleanupCompleted identifies a completed cleanup task.
	WebhookCleanupCompleted = "cleanup.completed"
)

// Webhook subscribes an external HTTP endpoint to repository events. Name is
// the stable primary identifier used by storage, APIs, and provisioning.
type Webhook struct {
	Name         string    `json:"name"`
	URL          string    `json:"url"`
	Secret       string    `json:"-"`
	Events       []string  `json:"events"`
	Repositories []string  `json:"repositories,omitempty"`
	Enabled      bool      `json:"enabled"`
	Managed      bool      `json:"managed"`
	CreatedAt    time.Time `json:"createdAt"`
	UpdatedAt    time.Time `json:"updatedAt"`
}

// Validate checks webhook names, targets, event types, and repository filters.
func (webhook Webhook) Validate() error {
	if webhook.Name == "" || !validName(webhook.Name) {
		return ErrInvalidWebhook
	}
	target, err := url.Parse(webhook.URL)
	if err != nil || target.Host == "" || target.User != nil ||
		(target.Scheme != "http" && target.Scheme != "https") {
		return ErrInvalidWebhookURL
	}
	if len(webhook.Secret) < 16 {
		return ErrWebhookSecretRequired
	}
	if len(webhook.Events) == 0 {
		return ErrWebhookEventsRequired
	}
	for _, event := range webhook.Events {
		if !IsWebhookEventType(event) {
			return ErrInvalidWebhookEvent
		}
	}
	for _, repository := range webhook.Repositories {
		if repository == "" || !validName(repository) {
			return ErrInvalidRepository
		}
	}
	return nil
}

// IsWebhookEventType reports whether event is supported by webhook subscriptions.
func IsWebhookEventType(event string) bool {
	switch event {
	case WebhookAssetUploaded,
		WebhookAssetDeleted,
		WebhookAssetDownloaded,
		WebhookComponentCreated,
		WebhookCleanupCompleted:
		return true
	default:
		return false
	}
}

// WebhookEvent is the signed payload sent to an external subscriber.
type WebhookEvent struct {
	ID         string         `json:"id"`
	Type       string         `json:"type"`
	Repository string         `json:"repository"`
	OccurredAt time.Time      `json:"occurredAt"`
	Asset      *Asset         `json:"asset,omitempty"`
	Details    map[string]any `json:"details,omitempty"`
}

// WebhookDelivery records one at-least-once delivery attempt stream and names
// the subscription that owns it.
type WebhookDelivery struct {
	ID            int64           `json:"id"`
	WebhookName   string          `json:"webhookName"`
	Event         string          `json:"event"`
	Repository    string          `json:"repository"`
	Payload       json.RawMessage `json:"payload"`
	Status        string          `json:"status"`
	Attempts      int             `json:"attempts"`
	NextAttemptAt time.Time       `json:"nextAttemptAt"`
	LastError     string          `json:"lastError,omitempty"`
	CreatedAt     time.Time       `json:"createdAt"`
	UpdatedAt     time.Time       `json:"updatedAt"`
	DeliveredAt   *time.Time      `json:"deliveredAt,omitempty"`
	TargetURL     string          `json:"-"`
	Secret        string          `json:"-"`
}

// DownloadGate withholds repository assets until all configured predicates match.
//
// A gate with an empty Repository is the instance-wide default: InheritGlobal is
// unused there, and a repository gate whose InheritGlobal is true AND-extends its
// criteria with the default's.
type DownloadGate struct {
	Repository    string      `json:"repository"`
	Criteria      []Predicate `json:"criteria"`
	Enabled       bool        `json:"enabled"`
	InheritGlobal bool        `json:"inheritGlobal"`
	Managed       bool        `json:"managed"`
	UpdatedAt     time.Time   `json:"updatedAt"`
}

// TrustIdentity allows a signer identity issued by one certificate authority.
type TrustIdentity struct {
	Issuer  string `json:"issuer"`
	Subject string `json:"subject"`
}

// TrustPolicy controls signature verification and enforcement for one repository.
type TrustPolicy struct {
	Repository             string          `json:"repository"`
	Mode                   string          `json:"mode"`
	PublicKeys             []string        `json:"publicKeys,omitempty"`
	CertificateAuthorities []string        `json:"certificateAuthorities,omitempty"`
	AllowedIdentities      []TrustIdentity `json:"allowedIdentities,omitempty"`
	DeniedFingerprints     []string        `json:"deniedFingerprints,omitempty"`
	Managed                bool            `json:"managed"`
	UpdatedAt              time.Time       `json:"updatedAt"`
}

// Validate checks trust-policy syntax before parsing its PEM trust material.
func (policy TrustPolicy) Validate() error {
	if policy.Repository == "" || !validName(policy.Repository) {
		return ErrInvalidRepository
	}
	return policy.validateModeAndMaterial()
}

// ValidateDefaults checks the instance-wide trust-policy default, which carries
// no repository. A repository inherits it only when it has no policy of its own.
func (policy TrustPolicy) ValidateDefaults() error {
	if policy.Repository != "" {
		return ErrInvalidRepository
	}
	return policy.validateModeAndMaterial()
}

func (policy TrustPolicy) validateModeAndMaterial() error {
	if policy.Mode != "audit" && policy.Mode != "verify-on-pull" &&
		policy.Mode != "verify-on-push" {
		return ErrInvalidTrustPolicy
	}
	if len(policy.PublicKeys) == 0 && len(policy.CertificateAuthorities) == 0 {
		return ErrTrustMaterialRequired
	}
	for _, identity := range policy.AllowedIdentities {
		if identity.Issuer == "" || identity.Subject == "" {
			return ErrInvalidTrustIdentity
		}
	}
	return nil
}

// ProvenanceResult is persisted in the reserved provenance attribute namespace.
type ProvenanceResult struct {
	Status      string    `json:"status"`
	Format      string    `json:"format"`
	Digest      string    `json:"digest"`
	PolicyAt    time.Time `json:"policyUpdatedAt"`
	Fingerprint string    `json:"fingerprint,omitempty"`
	Identity    string    `json:"identity,omitempty"`
	Issuer      string    `json:"issuer,omitempty"`
	Reason      string    `json:"reason,omitempty"`
	VerifiedAt  time.Time `json:"verifiedAt"`
}

// DSSESignature is one base64-encoded signature in a DSSE envelope.
type DSSESignature struct {
	KeyID     string `json:"keyid,omitempty"`
	Signature string `json:"sig"`
}

// DSSEEnvelope carries a signed in-toto or SLSA statement.
type DSSEEnvelope struct {
	PayloadType string          `json:"payloadType"`
	Payload     string          `json:"payload"`
	Signatures  []DSSESignature `json:"signatures"`
}

// VerificationRequest supplies a detached signature or DSSE envelope for an asset.
type VerificationRequest struct {
	Signature        string        `json:"signature,omitempty"`
	Payload          string        `json:"payload,omitempty"`
	Certificate      string        `json:"certificate,omitempty"`
	CertificateChain []string      `json:"certificateChain,omitempty"`
	DSSE             *DSSEEnvelope `json:"dsse,omitempty"`
}

// Validate checks that a download gate belongs to a repository and contains
// valid projected-attribute predicates. Empty criteria are allowed: such a row
// gates nothing on its own and exists to carry InheritGlobal=false, the explicit
// opt-out from the instance-wide default.
func (gate DownloadGate) Validate() error {
	if gate.Repository == "" || !validName(gate.Repository) {
		return ErrInvalidDownloadGate
	}
	return gate.validateCriteria()
}

// ValidateDefaults checks the instance-wide download-gate default. It carries no
// repository and may hold no criteria (an empty default gates nothing).
func (gate DownloadGate) ValidateDefaults() error {
	if gate.Repository != "" {
		return ErrInvalidDownloadGate
	}
	return gate.validateCriteria()
}

func (gate DownloadGate) validateCriteria() error {
	for _, predicate := range gate.Criteria {
		if err := predicate.Validate(); err != nil {
			return err
		}
	}
	return nil
}
