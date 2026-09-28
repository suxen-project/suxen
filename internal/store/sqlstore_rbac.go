package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/suxen-project/suxen/internal/domain"
)

// CreateRole inserts a named privilege bundle.
func (s *SQLStore) CreateRole(ctx context.Context, role domain.Role) error {
	if err := role.Validate(); err != nil {
		return err
	}
	transaction, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer transaction.Rollback()
	if err := insertRoleRow(ctx, transaction, role); err != nil {
		return err
	}
	return transaction.Commit()
}

// UpdateRole replaces a role's description and privileges.
func (s *SQLStore) UpdateRole(ctx context.Context, role domain.Role) error {
	if err := role.Validate(); err != nil {
		return err
	}
	transaction, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer transaction.Rollback()
	if err := updateRoleRow(ctx, transaction, role); err != nil {
		return err
	}
	return transaction.Commit()
}

// RoleSave is an atomic role mutation and its ownership effect. Create inserts a
// new privilege bundle; an update replaces the description and privileges.
type RoleSave struct {
	Role      domain.Role
	Create    bool
	Ownership Ownership
}

// SaveRole commits a role's rows and its ownership record in one transaction
// serialized on ("role", name). An imperative mutation of a provisioning-managed
// role returns domain.ErrManaged unless Force is set.
func (s *SQLStore) SaveRole(ctx context.Context, save RoleSave) error {
	if err := save.Role.Validate(); err != nil {
		return err
	}
	return s.writeOwned(ctx, "role", save.Role.Name, save.Ownership, !save.Create, false,
		func(ctx context.Context, transaction *dialectTx) error {
			if save.Create {
				return insertRoleRow(ctx, transaction, save.Role)
			}
			return updateRoleRow(ctx, transaction, save.Role)
		})
}

// DeleteRole removes a role, its assignments, and its ownership record in one
// transaction serialized on ("role", name). A role still referenced by an OIDC
// provider mapping is rejected; direct user assignments cascade with the role.
func (s *SQLStore) DeleteRole(ctx context.Context, name string, ownership Ownership) error {
	return s.writeOwned(ctx, "role", name, ownership, true, true,
		func(ctx context.Context, transaction *dialectTx) error {
			referenced, err := roleReferencedByProviderTx(ctx, transaction, name)
			if err != nil {
				return err
			}
			if referenced {
				return domain.ErrRoleReferencedByProvider
			}
			result, err := transaction.ExecContext(ctx, `DELETE FROM roles WHERE name = ?`, name)
			if err != nil {
				return err
			}
			return requireAffectedRow(result)
		})
}

func insertRoleRow(ctx context.Context, executor sqlExecer, role domain.Role) error {
	_, err := executor.ExecContext(
		ctx,
		`INSERT INTO roles (name, description, created_at) VALUES (?, ?, ?)`,
		role.Name,
		role.Description,
		formatTime(time.Now()),
	)
	if isUniqueConstraint(err) {
		return domain.ErrConflict
	}
	if err != nil {
		return err
	}
	return replaceRolePrivileges(ctx, executor, role)
}

func updateRoleRow(ctx context.Context, executor sqlExecer, role domain.Role) error {
	result, err := executor.ExecContext(
		ctx,
		`UPDATE roles SET description = ? WHERE name = ?`,
		role.Description,
		role.Name,
	)
	if err != nil {
		return err
	}
	if err := requireAffectedRow(result); err != nil {
		return err
	}
	if _, err := executor.ExecContext(
		ctx,
		`DELETE FROM role_privileges WHERE role = ?`,
		role.Name,
	); err != nil {
		return err
	}
	return replaceRolePrivileges(ctx, executor, role)
}

// roleReferencedByProviderTx reports whether any OIDC provider's default or group
// role mappings still name role, resolved inside the delete transaction. The
// mappings are stored as JSON, so this scans providers rather than joining.
func roleReferencedByProviderTx(ctx context.Context, transaction *dialectTx, role string) (bool, error) {
	rows, err := transaction.QueryContext(ctx, `SELECT default_roles, group_roles FROM oidc_providers`)
	if err != nil {
		return false, err
	}
	defer rows.Close()
	for rows.Next() {
		var defaultRolesJSON, groupRolesJSON string
		if err := rows.Scan(&defaultRolesJSON, &groupRolesJSON); err != nil {
			return false, err
		}
		var defaultRoles []string
		if err := json.Unmarshal([]byte(defaultRolesJSON), &defaultRoles); err != nil {
			return false, err
		}
		for _, mapped := range defaultRoles {
			if mapped == role {
				return true, nil
			}
		}
		var groupRoles map[string][]string
		if err := json.Unmarshal([]byte(groupRolesJSON), &groupRoles); err != nil {
			return false, err
		}
		for _, mapped := range groupRoles {
			for _, name := range mapped {
				if name == role {
					return true, nil
				}
			}
		}
	}
	return false, rows.Err()
}

// Role returns one role by name.
func (s *SQLStore) Role(ctx context.Context, name string) (domain.Role, error) {
	const query = `SELECT name, description, created_at FROM roles WHERE name = ?`
	var role domain.Role
	var createdAt string
	err := s.db.QueryRowContext(ctx, query, name).Scan(
		&role.Name,
		&role.Description,
		&createdAt,
	)
	if errors.Is(err, sql.ErrNoRows) {
		return role, domain.ErrNotFound
	}
	if err != nil {
		return role, err
	}
	role.CreatedAt, err = parseTime(createdAt)
	if err != nil {
		return role, err
	}
	role.Privileges, err = queryStrings(
		ctx,
		s.db,
		`SELECT privilege FROM role_privileges WHERE role = ? ORDER BY privilege`,
		name,
	)
	return role, err
}

// Roles returns every role ordered by name.
func (s *SQLStore) Roles(ctx context.Context) ([]domain.Role, error) {
	names, err := queryStrings(ctx, s.db, `SELECT name FROM roles ORDER BY name`)
	if err != nil {
		return nil, err
	}
	roles := make([]domain.Role, 0, len(names))
	for _, name := range names {
		role, err := s.Role(ctx, name)
		if err != nil {
			return nil, err
		}
		roles = append(roles, role)
	}
	return roles, nil
}

// SetUserRoles replaces the roles directly assigned to a local user.
func (s *SQLStore) SetUserRoles(
	ctx context.Context,
	username string,
	roles []string,
) error {
	transaction, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer transaction.Rollback()
	if err := setUserRolesTx(ctx, transaction, username, roles); err != nil {
		return err
	}
	return transaction.Commit()
}

func setUserRolesTx(ctx context.Context, transaction *dialectTx, username string, roles []string) error {
	var exists int
	err := transaction.QueryRowContext(
		ctx,
		`SELECT 1 FROM users WHERE username = ?`,
		username,
	).Scan(&exists)
	if errors.Is(err, sql.ErrNoRows) {
		return domain.ErrNotFound
	}
	if err != nil {
		return err
	}
	if _, err := transaction.ExecContext(
		ctx,
		`DELETE FROM user_roles WHERE username = ?`,
		username,
	); err != nil {
		return err
	}
	for _, role := range uniqueStrings(roles) {
		if _, err := transaction.ExecContext(
			ctx,
			`INSERT INTO user_roles (username, role) VALUES (?, ?)`,
			username,
			role,
		); err != nil {
			// A role can be deleted after the API validates its name but
			// before this transaction assigns it. The foreign key keeps the
			// mutation atomic; report the missing reference as a 404.
			if strings.Contains(strings.ToLower(err.Error()), "foreign key") {
				return fmt.Errorf("assign role %q: %w", role, domain.ErrNotFound)
			}
			return fmt.Errorf("assign role %q: %w", role, err)
		}
	}
	return nil
}

// UserRoles returns roles directly assigned to a local user.
func (s *SQLStore) UserRoles(ctx context.Context, username string) ([]string, error) {
	return queryStrings(
		ctx,
		s.db,
		`SELECT role FROM user_roles WHERE username = ? ORDER BY role`,
		username,
	)
}

// EffectivePrivileges resolves a local user's directly assigned roles plus the
// anonymous role. Roles are flat: a role is a named set of privileges, so no
// role-to-role expansion is needed.
func (s *SQLStore) EffectivePrivileges(
	ctx context.Context,
	username string,
) ([]string, error) {
	const query = `
        SELECT DISTINCT role_privileges.privilege
        FROM role_privileges
        WHERE role_privileges.role IN (
            SELECT role FROM user_roles WHERE username = ?
            UNION
            SELECT 'anonymous'
        )
        ORDER BY role_privileges.privilege`
	return queryStrings(ctx, s.db, query, username)
}

// LocalAuthorization reads all authorization state from one account generation
// in one statement. A concurrent delete and same-name create cannot redirect
// role or administrator access to the replacement account.
func (s *SQLStore) LocalAuthorization(
	ctx context.Context,
	username string,
	identity string,
) (domain.User, []string, error) {
	if identity == "" {
		return domain.User{}, nil, domain.ErrNotFound
	}
	const query = `
		SELECT users.admin, user_roles.role, role_privileges.privilege
		FROM users
		LEFT JOIN user_roles ON user_roles.username = users.username
		LEFT JOIN role_privileges ON role_privileges.role = user_roles.role
			OR role_privileges.role = 'anonymous'
		WHERE users.username = ? AND users.identity = ?`
	rows, err := s.db.QueryContext(ctx, query, username, identity)
	if err != nil {
		return domain.User{}, nil, err
	}
	defer rows.Close()

	user := domain.User{Username: username, Identity: identity}
	roleSet := make(map[string]struct{})
	privilegeSet := make(map[string]struct{})
	found := false
	for rows.Next() {
		found = true
		var role, privilege sql.NullString
		if err := rows.Scan(&user.Admin, &role, &privilege); err != nil {
			return domain.User{}, nil, err
		}
		if role.Valid {
			roleSet[role.String] = struct{}{}
		}
		if privilege.Valid {
			privilegeSet[privilege.String] = struct{}{}
		}
	}
	if err := rows.Err(); err != nil {
		return domain.User{}, nil, err
	}
	if !found {
		return domain.User{}, nil, domain.ErrNotFound
	}
	for role := range roleSet {
		user.Roles = append(user.Roles, role)
	}
	sort.Strings(user.Roles)
	privileges := make([]string, 0, len(privilegeSet))
	for privilege := range privilegeSet {
		privileges = append(privileges, privilege)
	}
	sort.Strings(privileges)
	return user, privileges, nil
}

// PrivilegesForRoles resolves the privileges of the roles an external identity
// carries, plus the anonymous role. Roles are flat, so the supplied set is
// exactly the roles that grant privileges.
func (s *SQLStore) PrivilegesForRoles(
	ctx context.Context,
	roles []string,
) ([]string, error) {
	roles = uniqueStrings(append(append([]string{}, roles...), "anonymous"))
	placeholders := make([]string, len(roles))
	arguments := make([]any, len(roles))
	for index, role := range roles {
		placeholders[index] = "?"
		arguments[index] = role
	}
	query := `
        SELECT DISTINCT role_privileges.privilege
        FROM role_privileges
        WHERE role_privileges.role IN (` + strings.Join(placeholders, ",") + `)
        ORDER BY role_privileges.privilege`
	return queryStrings(ctx, s.db, query, arguments...)
}

func replaceRolePrivileges(
	ctx context.Context,
	executor sqlExecer,
	role domain.Role,
) error {
	for _, privilege := range uniqueStrings(role.Privileges) {
		if _, err := executor.ExecContext(
			ctx,
			`INSERT INTO role_privileges (role, privilege) VALUES (?, ?)`,
			role.Name,
			privilege,
		); err != nil {
			return fmt.Errorf("insert privilege %q: %w", privilege, err)
		}
	}
	return nil
}

type stringQueryer interface {
	QueryContext(context.Context, string, ...any) (*sql.Rows, error)
}

func queryStrings(
	ctx context.Context,
	queryer stringQueryer,
	query string,
	arguments ...any,
) ([]string, error) {
	rows, err := queryer.QueryContext(ctx, query, arguments...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	values := make([]string, 0)
	for rows.Next() {
		var value string
		if err := rows.Scan(&value); err != nil {
			return nil, err
		}
		values = append(values, value)
	}
	return values, rows.Err()
}

func uniqueStrings(values []string) []string {
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

func safeSQLIdentifier(value string) bool {
	if value == "" {
		return false
	}
	for _, character := range value {
		if !((character >= 'a' && character <= 'z') || character == '_') {
			return false
		}
	}
	return !strings.Contains(value, "__")
}
