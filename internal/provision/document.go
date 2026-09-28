// Package provision reconciles declarative control-plane resources against a
// metadata store. It contains no HTTP or command-line transport behavior.
package provision

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	"github.com/suxen-project/suxen/internal/domain"
	"go.yaml.in/yaml/v3"
)

const (
	// APIVersion identifies the supported desired-state schema.
	APIVersion = "suxen.io/v1"
	// MaxDocumentSize bounds both individual inputs and combined directories.
	MaxDocumentSize = 8 << 20
)

// SecretReference selects exactly one local source for a provisioned secret.
type SecretReference struct {
	Env  string `json:"env,omitempty" yaml:"env,omitempty"`
	File string `json:"file,omitempty" yaml:"file,omitempty"`
}

// Resource is one typed desired-state entry. Spec contains only non-secret
// fields after parsing; Secret is deliberately excluded from serialization.
type Resource struct {
	Kind         string
	Name         string
	Spec         map[string]any
	Secret       string
	SecretSource string
}

// Document is one validated desired-state input.
type Document struct {
	APIVersion string
	Resources  []Resource
}

type documentEnvelope struct {
	APIVersion string        `yaml:"apiVersion"`
	Resources  []rawResource `yaml:"resources"`
}

type rawResource struct {
	Kind string         `json:"kind" yaml:"kind"`
	Name string         `json:"name" yaml:"name"`
	Spec map[string]any `json:"spec" yaml:"spec"`
}

// Parse reads either an apiVersion/resources document or a top-level resource list.
func Parse(reader io.Reader) (Document, error) {
	contents, err := readDocumentContents(reader)
	if err != nil {
		return Document{}, err
	}
	if jsonLines, recognized, err := recognizeJSONLines(contents); recognized {
		if err != nil {
			return Document{}, err
		}
		return normalizeDocumentResources(APIVersion, jsonLines)
	}

	root, err := decodeSingleYAMLDocument(contents)
	if err != nil {
		jsonLines, jsonLinesErr := parseJSONLines(contents)
		if jsonLinesErr != nil {
			return Document{}, fmt.Errorf("decode provisioning YAML: %w", err)
		}
		return normalizeDocumentResources(APIVersion, jsonLines)
	}
	if len(root.Content) != 1 {
		return Document{}, errors.New("provisioning document must contain one YAML value")
	}

	document := Document{APIVersion: APIVersion}
	var rawResources []rawResource
	switch root.Content[0].Kind {
	case yaml.SequenceNode:
		for index, resourceNode := range root.Content[0].Content {
			if err := validateRawResourceNode(resourceNode); err != nil {
				return Document{}, fmt.Errorf("resource %d: %w", index+1, err)
			}
		}
		var err error
		rawResources, err = decodeRawResources(root.Content[0].Content)
		if err != nil {
			return Document{}, err
		}
	case yaml.MappingNode:
		if mappingHasKey(root.Content[0], "kind") {
			if err := validateRawResourceNode(root.Content[0]); err != nil {
				return Document{}, err
			}
			var err error
			rawResources, err = decodeRawResources([]*yaml.Node{root.Content[0]})
			if err != nil {
				return Document{}, err
			}
			break
		}
		if err := validateMappingKeys(root.Content[0], "apiVersion", "resources"); err != nil {
			return Document{}, fmt.Errorf("decode provisioning document: %w", err)
		}
		if resourcesNode := mappingValue(root.Content[0], "resources"); resourcesNode != nil {
			if resourcesNode.Kind != yaml.SequenceNode {
				return Document{}, errors.New("decode provisioning document: resources must be a list")
			}
			for index, resourceNode := range resourcesNode.Content {
				if err := validateRawResourceNode(resourceNode); err != nil {
					return Document{}, fmt.Errorf("resource %d: %w", index+1, err)
				}
			}
		}
		var envelope struct {
			APIVersion string `yaml:"apiVersion"`
		}
		if err := root.Content[0].Decode(&envelope); err != nil {
			return Document{}, fmt.Errorf("decode provisioning document: %w", err)
		}
		if envelope.APIVersion != "" && envelope.APIVersion != APIVersion {
			return Document{}, fmt.Errorf("unsupported provisioning apiVersion %q", envelope.APIVersion)
		}
		if resourcesNode := mappingValue(root.Content[0], "resources"); resourcesNode != nil {
			var err error
			rawResources, err = decodeRawResources(resourcesNode.Content)
			if err != nil {
				return Document{}, err
			}
		}
	default:
		return Document{}, errors.New("provisioning document must be a mapping or list")
	}
	return normalizeDocumentResources(document.APIVersion, rawResources)
}

// ParseCanonical reads the envelope form accepted by the provisioning HTTP API.
// Parse remains intentionally more flexible for local files and command-line
// workflows, where top-level lists, single resources, and JSONL are useful.
func ParseCanonical(reader io.Reader) (Document, error) {
	contents, err := readDocumentContents(reader)
	if err != nil {
		return Document{}, err
	}
	return parseCanonicalContents(contents)
}

// ParseCanonicalJSON reads a canonical HTTP envelope encoded as JSON.
func ParseCanonicalJSON(reader io.Reader) (Document, error) {
	contents, err := readDocumentContents(reader)
	if err != nil {
		return Document{}, err
	}
	if !json.Valid(contents) {
		return Document{}, errors.New("provisioning application/json body is not valid JSON")
	}
	return parseCanonicalContents(contents)
}

func parseCanonicalContents(contents []byte) (Document, error) {
	root, err := decodeSingleYAMLDocument(contents)
	if err != nil {
		return Document{}, fmt.Errorf("decode provisioning document: %w", err)
	}
	if len(root.Content) != 1 || root.Content[0].Kind != yaml.MappingNode {
		return Document{}, errors.New("provisioning API document must be an envelope mapping")
	}
	resourcesNode := mappingValue(root.Content[0], "resources")
	if resourcesNode == nil || resourcesNode.Kind != yaml.SequenceNode {
		return Document{}, errors.New("provisioning API document requires a resources list")
	}
	for index, resourceNode := range resourcesNode.Content {
		if err := validateCanonicalResourceNode(resourceNode); err != nil {
			return Document{}, fmt.Errorf("resource %d: %w", index+1, err)
		}
	}
	return Parse(bytes.NewReader(contents))
}

func decodeSingleYAMLDocument(contents []byte) (yaml.Node, error) {
	decoder := yaml.NewDecoder(bytes.NewReader(contents))
	var root yaml.Node
	if err := decoder.Decode(&root); err != nil {
		return yaml.Node{}, err
	}
	var trailing yaml.Node
	if err := decoder.Decode(&trailing); !errors.Is(err, io.EOF) {
		if err == nil {
			err = errors.New("multiple YAML documents are not supported")
		}
		return yaml.Node{}, err
	}
	return root, nil
}

func readDocumentContents(reader io.Reader) ([]byte, error) {
	contents, err := io.ReadAll(io.LimitReader(reader, MaxDocumentSize+1))
	if err != nil {
		return nil, fmt.Errorf("read provisioning document: %w", err)
	}
	if len(contents) > MaxDocumentSize {
		return nil, fmt.Errorf("provisioning document exceeds %d bytes", MaxDocumentSize)
	}
	if len(bytes.TrimSpace(contents)) == 0 {
		return nil, errors.New("provisioning document is empty")
	}
	return contents, nil
}

func validateCanonicalResourceNode(node *yaml.Node) error {
	if err := validateRawResourceNode(node); err != nil {
		return err
	}
	for _, field := range []string{"kind", "name"} {
		if mappingValue(node, field) == nil {
			return fmt.Errorf("%s is required", field)
		}
	}
	kindNode := mappingValue(node, "kind")
	if kindNode.Kind != yaml.ScalarNode || !supportedKind(kindNode.Value) {
		return fmt.Errorf("kind must be a canonical supported kind, got %q", kindNode.Value)
	}
	if specNode := mappingValue(node, "spec"); specNode != nil &&
		specNode.Kind != yaml.MappingNode && specNode.Tag != "!!null" {
		return errors.New("spec must be a mapping or null")
	}
	return nil
}

func validateRawResourceNode(node *yaml.Node) error {
	if node.Kind != yaml.MappingNode {
		return errors.New("provisioning resource must be a mapping")
	}
	return validateMappingKeys(node, "kind", "name", "spec")
}

func validateMappingKeys(node *yaml.Node, allowed ...string) error {
	known := make(map[string]struct{}, len(allowed))
	for _, key := range allowed {
		known[key] = struct{}{}
	}
	for index := 0; index < len(node.Content); index += 2 {
		key := node.Content[index].Value
		if _, found := known[key]; !found {
			return fmt.Errorf("unknown field %q", key)
		}
	}
	return nil
}

func mappingHasKey(node *yaml.Node, key string) bool {
	return mappingValue(node, key) != nil
}

func mappingValue(node *yaml.Node, key string) *yaml.Node {
	for index := 0; index < len(node.Content); index += 2 {
		if node.Content[index].Value == key {
			return node.Content[index+1]
		}
	}
	return nil
}

func recognizeJSONLines(contents []byte) ([]rawResource, bool, error) {
	lines := bytes.Split(contents, []byte("\n"))
	nonEmpty := make([][]byte, 0, len(lines))
	for _, line := range lines {
		line = bytes.TrimSpace(line)
		if len(line) > 0 {
			nonEmpty = append(nonEmpty, line)
		}
	}
	if len(nonEmpty) == 0 {
		return nil, false, nil
	}
	for _, line := range nonEmpty {
		if !json.Valid(line) {
			return nil, false, nil
		}
	}
	if len(nonEmpty) == 1 {
		var resource rawResource
		if err := decodeStrictJSON(nonEmpty[0], &resource); err != nil || resource.Kind == "" {
			return nil, false, nil
		}
	}
	resources, err := parseJSONLines(contents)
	if err != nil {
		return nil, true, err
	}
	return resources, true, nil
}

func parseJSONLines(contents []byte) ([]rawResource, error) {
	lines := bytes.Split(contents, []byte("\n"))
	resources := make([]rawResource, 0, len(lines))
	for lineNumber, line := range lines {
		line = bytes.TrimSpace(line)
		if len(line) == 0 {
			continue
		}
		decoder := json.NewDecoder(bytes.NewReader(line))
		decoder.DisallowUnknownFields()
		decoder.UseNumber()
		var resource rawResource
		if err := decoder.Decode(&resource); err != nil {
			return nil, fmt.Errorf("decode JSONL line %d: %w", lineNumber+1, err)
		}
		var trailing any
		if err := decoder.Decode(&trailing); !errors.Is(err, io.EOF) {
			return nil, fmt.Errorf("decode JSONL line %d: multiple values", lineNumber+1)
		}
		resources = append(resources, resource)
	}
	if len(resources) == 0 {
		return nil, errors.New("JSONL document has no resources")
	}
	return resources, nil
}

func normalizeDocumentResources(
	apiVersion string,
	rawResources []rawResource,
) (Document, error) {
	document := Document{
		APIVersion: apiVersion,
		Resources:  make([]Resource, 0, len(rawResources)),
	}
	seen := make(map[string]struct{}, len(rawResources))
	for index, raw := range rawResources {
		resource, err := normalizeResource(raw)
		if err != nil {
			return Document{}, fmt.Errorf("resource %d: %w", index+1, err)
		}
		key := resource.Kind + "\x00" + resource.Name
		if _, exists := seen[key]; exists {
			return Document{}, fmt.Errorf("duplicate %s %q", resource.Kind, resource.Name)
		}
		seen[key] = struct{}{}
		document.Resources = append(document.Resources, resource)
	}
	return document, nil
}

func normalizeResource(raw rawResource) (Resource, error) {
	kind := normalizeKind(raw.Kind)
	if !supportedKind(kind) {
		return Resource{}, fmt.Errorf("unsupported kind %q", raw.Kind)
	}
	name := strings.TrimSpace(raw.Name)
	if name == "" {
		return Resource{}, errors.New("name is required")
	}
	if kind == "user" && !domain.ValidUsername(raw.Name) {
		return Resource{}, domain.ErrInvalidUsername
	}
	spec, err := cloneSpec(raw.Spec)
	if err != nil {
		return Resource{}, fmt.Errorf("invalid spec: %w", err)
	}
	if spec == nil {
		spec = make(map[string]any)
	}

	resource := Resource{Kind: kind, Name: name, Spec: spec}
	secretField := kindSecretField(kind)
	if secretField == "" {
		for _, unsupported := range []string{"secret", "secretRef"} {
			if _, found := spec[unsupported]; found {
				return Resource{}, fmt.Errorf("%s resources do not support spec.%s", kind, unsupported)
			}
		}
	}
	if value, found := spec[secretField]; found {
		secret, ok := value.(string)
		if !ok {
			return Resource{}, fmt.Errorf("spec.%s must be a string", secretField)
		}
		resource.Secret = secret
		resource.SecretSource = "inline"
		delete(spec, secretField)
	}
	if secretField != "secret" {
		if value, found := spec["secret"]; found {
			if resource.SecretSource != "" {
				return Resource{}, errors.New("spec.secret and the kind-specific secret are mutually exclusive")
			}
			secret, ok := value.(string)
			if !ok {
				return Resource{}, errors.New("spec.secret must be a string")
			}
			resource.Secret = secret
			resource.SecretSource = "inline"
			delete(spec, "secret")
		}
	}
	if value, found := spec["secretRef"]; found {
		if resource.SecretSource != "" {
			return Resource{}, errors.New("spec.secret and spec.secretRef are mutually exclusive")
		}
		reference, err := decodeSecretReference(value)
		if err != nil {
			return Resource{}, err
		}
		resource.SecretSource = referenceSource(reference)
		delete(spec, "secretRef")
		encoded, err := json.Marshal(reference)
		if err != nil {
			return Resource{}, err
		}
		resource.Spec["__secretRef"] = string(encoded)
	}
	if resource.SecretSource == "inline" && strings.TrimSpace(resource.Secret) == "" {
		return Resource{}, errors.New("inline secret cannot be empty")
	}
	validationResource := resource
	validationResource.Spec, err = cloneSpec(resource.Spec)
	if err != nil {
		return Resource{}, fmt.Errorf("invalid spec: %w", err)
	}
	delete(validationResource.Spec, "__secretRef")
	if err := validateResourceSpec(validationResource); err != nil {
		return Resource{}, fmt.Errorf("invalid %s spec: %w", resource.Kind, err)
	}
	return resource, nil
}

// ResolveSecrets resolves every remaining secretRef with the supplied resolver.
// Resolution validates the entire document before reconciliation starts.
func ResolveSecrets(document Document, resolver SecretResolver) (Document, error) {
	resolved := Document{APIVersion: document.APIVersion, Resources: make([]Resource, len(document.Resources))}
	for index, resource := range document.Resources {
		var err error
		resource.Spec, err = cloneSpec(resource.Spec)
		if err != nil {
			return Document{}, fmt.Errorf("resolve %s %q spec: %w", resource.Kind, resource.Name, err)
		}
		encoded, hasReference := resource.Spec["__secretRef"]
		delete(resource.Spec, "__secretRef")
		if hasReference {
			encodedReference, ok := encoded.(string)
			if !ok {
				return Document{}, fmt.Errorf(
					"resolve %s %q secretRef: invalid internal representation",
					resource.Kind,
					resource.Name,
				)
			}
			var reference SecretReference
			if err := decodeStrictJSON([]byte(encodedReference), &reference); err != nil {
				return Document{}, fmt.Errorf("resolve %s %q secretRef: %w", resource.Kind, resource.Name, err)
			}
			secret, err := resolver.Resolve(reference)
			if err != nil {
				return Document{}, fmt.Errorf("resolve %s %q secretRef: %w", resource.Kind, resource.Name, err)
			}
			if strings.TrimSpace(secret) == "" {
				return Document{}, fmt.Errorf("resolve %s %q secretRef: resolved secret is empty", resource.Kind, resource.Name)
			}
			resource.Secret = secret
		}
		resolved.Resources[index] = resource
	}
	return resolved, nil
}

// SecretResolver resolves references in the environment where apply runs.
type SecretResolver interface {
	// Resolve reads exactly one environment or absolute-file reference. It returns
	// trimmed contents and fails when the source is absent or unreadable.
	Resolve(SecretReference) (string, error)
}

// EnvironmentResolver resolves environment variables and absolute mounted files.
type EnvironmentResolver struct{}

// Resolve reads and trims a secret from one environment variable or absolute file.
func (EnvironmentResolver) Resolve(reference SecretReference) (string, error) {
	if (reference.Env == "") == (reference.File == "") {
		return "", errors.New("secretRef must set exactly one of env or file")
	}
	if reference.Env != "" {
		value, found := os.LookupEnv(reference.Env)
		if !found {
			return "", fmt.Errorf("environment variable %s is not set", reference.Env)
		}
		return strings.TrimSpace(value), nil
	}
	if !filepath.IsAbs(reference.File) {
		return "", errors.New("secretRef.file must be an absolute path")
	}
	contents, err := os.ReadFile(reference.File)
	if err != nil {
		return "", fmt.Errorf("read secret file %s: %w", reference.File, err)
	}
	return strings.TrimSpace(string(contents)), nil
}

// MarshalResolved encodes a document with resolved secrets for a trusted TLS
// transport. Secret references are not retained in the emitted payload. JSON
// preserves json.Number values that YAML would otherwise quote as strings.
func MarshalResolved(document Document) ([]byte, error) {
	resources := make([]rawResource, 0, len(document.Resources))
	for _, resource := range document.Resources {
		spec, err := cloneSpec(resource.Spec)
		if err != nil {
			return nil, fmt.Errorf("marshal %s %q spec: %w", resource.Kind, resource.Name, err)
		}
		if spec == nil {
			spec = make(map[string]any)
		}
		delete(spec, "__secretRef")
		if resource.Secret != "" {
			secretField := kindSecretField(resource.Kind)
			if secretField == "" {
				return nil, fmt.Errorf("%s resources do not support secrets", resource.Kind)
			}
			spec[secretField] = resource.Secret
		}
		resources = append(resources, rawResource{
			Kind: resource.Kind,
			Name: resource.Name,
			Spec: spec,
		})
	}
	return json.Marshal(struct {
		APIVersion string        `json:"apiVersion"`
		Resources  []rawResource `json:"resources"`
	}{APIVersion: APIVersion, Resources: resources})
}

func decodeSecretReference(value any) (SecretReference, error) {
	encoded, err := json.Marshal(value)
	if err != nil {
		return SecretReference{}, errors.New("spec.secretRef must be a mapping")
	}
	var reference SecretReference
	if err := decodeStrictJSON(encoded, &reference); err != nil {
		return SecretReference{}, fmt.Errorf(
			"spec.secretRef must contain only env or file: %w",
			err,
		)
	}
	if (reference.Env == "") == (reference.File == "") {
		return SecretReference{}, errors.New("spec.secretRef must set exactly one of env or file")
	}
	return reference, nil
}

func referenceSource(reference SecretReference) string {
	if reference.Env != "" {
		return "env:" + reference.Env
	}
	return "file:" + reference.File
}

func normalizeKind(kind string) string {
	normalized := strings.ToLower(strings.TrimSpace(kind))
	normalized = strings.ReplaceAll(normalized, "-", "")
	normalized = strings.ReplaceAll(normalized, "_", "")
	switch normalized {
	case "blobstore", "blobstores":
		return "blobStore"
	case "repository", "repositories":
		return "repository"
	case "oidc", "oidcprovider", "oidcproviders":
		return "oidcProvider"
	case "role", "roles":
		return "role"
	case "user", "users":
		return "user"
	case "cleanuppolicy", "cleanuppolicies":
		return "cleanupPolicy"
	case "classification", "classifications":
		return "classification"
	case "trustpolicy", "trustpolicies":
		return "trustPolicy"
	case "downloadgate", "downloadgates":
		return "downloadGate"
	case "webhook", "webhooks":
		return "webhook"
	default:
		return normalized
	}
}

func supportedKind(kind string) bool {
	_, found := kindPriorities[kind]
	return found
}

func kindSecretField(kind string) string {
	switch kind {
	case "user":
		return "password"
	case "oidcProvider":
		return "clientSecret"
	case "repository":
		return "upstream"
	case "webhook":
		return "secret"
	default:
		return ""
	}
}

func decodeStrictJSON(encoded []byte, destination any) error {
	decoder := json.NewDecoder(bytes.NewReader(encoded))
	decoder.DisallowUnknownFields()
	decoder.UseNumber()
	if err := decoder.Decode(destination); err != nil {
		return err
	}
	var trailing any
	if err := decoder.Decode(&trailing); !errors.Is(err, io.EOF) {
		if err == nil {
			return errors.New("multiple JSON values")
		}
		return err
	}
	return nil
}

func cloneSpec(spec map[string]any) (map[string]any, error) {
	if spec == nil {
		return nil, nil
	}
	encoded, err := json.Marshal(spec)
	if err != nil {
		return nil, err
	}
	var clone map[string]any
	if err := decodeStrictJSON(encoded, &clone); err != nil {
		return nil, err
	}
	return clone, nil
}
