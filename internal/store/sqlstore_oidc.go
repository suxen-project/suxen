package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/suxen-project/suxen/internal/domain"
)

const oidcProviderColumns = `
    name,
    issuer,
    client_id,
    client_secret,
    scopes,
    groups_claim,
    default_roles,
    group_roles,
    allow_password_grant,
    created_at`

// CreateOIDCProvider inserts an external identity provider configuration.
func (s *SQLStore) CreateOIDCProvider(
	ctx context.Context,
	provider domain.OIDCProvider,
) error {
	if err := provider.Validate(); err != nil {
		return err
	}
	normalizeOIDCProvider(&provider)
	encoded, err := encodeOIDCCollections(provider)
	if err != nil {
		return err
	}
	transaction, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer transaction.Rollback()
	if err := insertOIDCProviderTx(ctx, transaction, provider, encoded); err != nil {
		return err
	}
	if err := transaction.Commit(); err != nil {
		return err
	}
	return s.invalidateProvisionSecret(ctx, "oidcProvider", provider.Name)
}

// OIDCSave is an atomic provider mutation and its ownership effect. Create
// inserts a new provider; an empty ClientSecret keeps the existing secret on
// update.
type OIDCSave struct {
	Provider  domain.OIDCProvider
	Create    bool
	Ownership Ownership
}

// SaveOIDCProvider commits a provider configuration and its ownership record in
// one transaction serialized on ("oidcProvider", name). An imperative mutation
// of a provisioning-managed provider returns domain.ErrManaged unless Force is
// set. Verifier-cache invalidation is a post-commit effect owned by the caller.
func (s *SQLStore) SaveOIDCProvider(ctx context.Context, save OIDCSave) error {
	provider := save.Provider
	if err := provider.Validate(); err != nil {
		return err
	}
	normalizeOIDCProvider(&provider)
	encoded, err := encodeOIDCCollections(provider)
	if err != nil {
		return err
	}
	transaction, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer transaction.Rollback()
	managed, err := s.prepareOwnershipTx(ctx, transaction, "oidcProvider", provider.Name, save.Ownership, !save.Create)
	if err != nil {
		return err
	}
	if err := lockAndVerifyMappedRolesTx(ctx, transaction, provider); err != nil {
		return err
	}
	if save.Create {
		if err := insertOIDCProviderTx(ctx, transaction, provider, encoded); err != nil {
			return err
		}
	} else if _, err := updateOIDCProviderTx(ctx, transaction, provider, encoded); err != nil {
		return err
	}
	if err := applyOwnershipTx(ctx, transaction, "oidcProvider", provider.Name, save.Ownership, managed, false); err != nil {
		return err
	}
	return transaction.Commit()
}

// lockAndVerifyMappedRolesTx serializes a provider save against concurrent role
// deletions: it locks each mapped role's ownership key (sorted, to avoid
// deadlock) and verifies the role still exists inside this transaction. Combined
// with DeleteRole holding the same role key while it rejects a referenced role,
// a provider mapping and a role deletion cannot both commit and strand a mapping
// to a deleted role, even across replicas.
func lockAndVerifyMappedRolesTx(ctx context.Context, transaction *dialectTx, provider domain.OIDCProvider) error {
	names := append([]string{}, provider.DefaultRoles...)
	for _, roles := range provider.GroupRoles {
		names = append(names, roles...)
	}
	for _, name := range uniqueStrings(names) {
		if err := lockOwnershipTx(ctx, transaction, "role", name); err != nil {
			return err
		}
		var present int
		err := transaction.QueryRowContext(ctx, `SELECT 1 FROM roles WHERE name = ?`, name).Scan(&present)
		if errors.Is(err, sql.ErrNoRows) {
			return fmt.Errorf("mapped role %q: %w", name, domain.ErrNotFound)
		}
		if err != nil {
			return err
		}
	}
	return nil
}

func insertOIDCProviderTx(
	ctx context.Context,
	transaction *dialectTx,
	provider domain.OIDCProvider,
	encoded encodedOIDCCollections,
) error {
	const query = `
        INSERT INTO oidc_providers (
            name, issuer, client_id, client_secret, scopes, groups_claim,
            default_roles, group_roles, allow_password_grant, created_at
        ) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`
	_, err := transaction.ExecContext(
		ctx,
		query,
		provider.Name,
		provider.Issuer,
		provider.ClientID,
		provider.ClientSecret,
		encoded.scopes,
		provider.GroupsClaim,
		encoded.defaultRoles,
		encoded.groupRoles,
		provider.AllowPasswordGrant,
		formatTime(provider.CreatedAt),
	)
	if isUniqueConstraint(err) {
		return domain.ErrConflict
	}
	return err
}

// UpdateOIDCProvider replaces an external identity provider configuration.
func (s *SQLStore) UpdateOIDCProvider(
	ctx context.Context,
	provider domain.OIDCProvider,
) error {
	if err := provider.Validate(); err != nil {
		return err
	}
	normalizeOIDCProvider(&provider)
	encoded, err := encodeOIDCCollections(provider)
	if err != nil {
		return err
	}
	transaction, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer transaction.Rollback()
	secretChanged, err := updateOIDCProviderTx(ctx, transaction, provider, encoded)
	if err != nil {
		return err
	}
	if err := transaction.Commit(); err != nil {
		return err
	}
	if secretChanged {
		return s.invalidateProvisionSecret(ctx, "oidcProvider", provider.Name)
	}
	return nil
}

// updateOIDCProviderTx applies the column changes. An empty ClientSecret keeps
// the existing secret; the returned flag reports whether the secret changed.
func updateOIDCProviderTx(
	ctx context.Context,
	transaction *dialectTx,
	provider domain.OIDCProvider,
	encoded encodedOIDCCollections,
) (secretChanged bool, err error) {
	if provider.ClientSecret != "" {
		var existingSecret string
		err := transaction.QueryRowContext(
			ctx,
			`SELECT client_secret FROM oidc_providers WHERE name = ?`,
			provider.Name,
		).Scan(&existingSecret)
		if errors.Is(err, sql.ErrNoRows) {
			return false, domain.ErrNotFound
		}
		if err != nil {
			return false, err
		}
		secretChanged = !secretValuesEqual(existingSecret, provider.ClientSecret)
	}
	query := `
		UPDATE oidc_providers
		SET issuer = ?, client_id = ?, scopes = ?, groups_claim = ?,
			default_roles = ?, group_roles = ?, allow_password_grant = ?`
	arguments := []any{
		provider.Issuer,
		provider.ClientID,
		encoded.scopes,
		provider.GroupsClaim,
		encoded.defaultRoles,
		encoded.groupRoles,
		provider.AllowPasswordGrant,
	}
	if provider.ClientSecret != "" {
		query += `, client_secret = ?`
		arguments = append(arguments, provider.ClientSecret)
	}
	query += ` WHERE name = ?`
	arguments = append(arguments, provider.Name)
	result, err := transaction.ExecContext(ctx, query, arguments...)
	if isUniqueConstraint(err) {
		return secretChanged, domain.ErrConflict
	}
	if err != nil {
		return secretChanged, err
	}
	if err := requireAffectedRow(result); err != nil {
		return secretChanged, err
	}
	return secretChanged, nil
}

// DeleteOIDCProvider removes a provider configuration and its ownership record
// in one transaction serialized on ("oidcProvider", name). An imperative delete
// of a provisioning-managed provider returns domain.ErrManaged unless the
// ownership carries Force.
func (s *SQLStore) DeleteOIDCProvider(ctx context.Context, name string, ownership Ownership) error {
	return s.writeOwned(ctx, "oidcProvider", name, ownership, true, true,
		func(ctx context.Context, transaction *dialectTx) error {
			result, err := transaction.ExecContext(ctx, `DELETE FROM oidc_providers WHERE name = ?`, name)
			if err != nil {
				return err
			}
			return requireAffectedRow(result)
		})
}

// OIDCProvider returns one external identity provider by name.
func (s *SQLStore) OIDCProvider(
	ctx context.Context,
	name string,
) (domain.OIDCProvider, error) {
	query := `SELECT ` + oidcProviderColumns + ` FROM oidc_providers WHERE name = ?`
	return scanOIDCProvider(s.db.QueryRowContext(ctx, query, name))
}

// OIDCProviderByIssuer returns one external identity provider by issuer URL.
func (s *SQLStore) OIDCProviderByIssuer(
	ctx context.Context,
	issuer string,
) (domain.OIDCProvider, error) {
	query := `SELECT ` + oidcProviderColumns + ` FROM oidc_providers WHERE issuer = ?`
	return scanOIDCProvider(s.db.QueryRowContext(ctx, query, issuer))
}

// OIDCProviders returns all configured external identity providers.
func (s *SQLStore) OIDCProviders(ctx context.Context) ([]domain.OIDCProvider, error) {
	query := `SELECT ` + oidcProviderColumns + ` FROM oidc_providers ORDER BY name`
	rows, err := s.db.QueryContext(ctx, query)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	providers := make([]domain.OIDCProvider, 0)
	for rows.Next() {
		provider, err := scanOIDCProvider(rows)
		if err != nil {
			return nil, err
		}
		providers = append(providers, provider)
	}
	return providers, rows.Err()
}

type encodedOIDCCollections struct {
	scopes       string
	defaultRoles string
	groupRoles   string
}

func encodeOIDCCollections(
	provider domain.OIDCProvider,
) (encodedOIDCCollections, error) {
	scopes, err := json.Marshal(provider.Scopes)
	if err != nil {
		return encodedOIDCCollections{}, fmt.Errorf("encode OIDC scopes: %w", err)
	}
	defaultRoles, err := json.Marshal(provider.DefaultRoles)
	if err != nil {
		return encodedOIDCCollections{}, fmt.Errorf("encode OIDC default roles: %w", err)
	}
	groupRoles, err := json.Marshal(provider.GroupRoles)
	if err != nil {
		return encodedOIDCCollections{}, fmt.Errorf("encode OIDC group roles: %w", err)
	}
	return encodedOIDCCollections{
		scopes:       string(scopes),
		defaultRoles: string(defaultRoles),
		groupRoles:   string(groupRoles),
	}, nil
}

func scanOIDCProvider(source scanner) (domain.OIDCProvider, error) {
	var provider domain.OIDCProvider
	var scopes string
	var defaultRoles string
	var groupRoles string
	var createdAt string
	err := source.Scan(
		&provider.Name,
		&provider.Issuer,
		&provider.ClientID,
		&provider.ClientSecret,
		&scopes,
		&provider.GroupsClaim,
		&defaultRoles,
		&groupRoles,
		&provider.AllowPasswordGrant,
		&createdAt,
	)
	if errors.Is(err, sql.ErrNoRows) {
		return provider, domain.ErrNotFound
	}
	if err != nil {
		return provider, err
	}
	if err := json.Unmarshal([]byte(scopes), &provider.Scopes); err != nil {
		return provider, fmt.Errorf("decode OIDC scopes: %w", err)
	}
	if err := json.Unmarshal([]byte(defaultRoles), &provider.DefaultRoles); err != nil {
		return provider, fmt.Errorf("decode OIDC default roles: %w", err)
	}
	if err := json.Unmarshal([]byte(groupRoles), &provider.GroupRoles); err != nil {
		return provider, fmt.Errorf("decode OIDC group roles: %w", err)
	}
	provider.CreatedAt, err = parseTime(createdAt)
	return provider, err
}

func normalizeOIDCProvider(provider *domain.OIDCProvider) {
	if len(provider.Scopes) == 0 {
		provider.Scopes = []string{"openid", "profile", "email", "groups"}
	}
	if provider.GroupsClaim == "" {
		provider.GroupsClaim = "groups"
	}
	if provider.DefaultRoles == nil {
		provider.DefaultRoles = []string{}
	}
	if provider.GroupRoles == nil {
		provider.GroupRoles = make(map[string][]string)
	}
	if provider.CreatedAt.IsZero() {
		provider.CreatedAt = time.Now().UTC()
	}
}
