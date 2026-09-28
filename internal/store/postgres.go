package store

import (
	"database/sql"
	"fmt"
	"time"

	_ "github.com/jackc/pgx/v5/stdlib"
)

// postgresMigrationLockID is an application-specific advisory lock. PostgreSQL
// holds it for the migration transaction, which prevents multiple replicas from
// applying schema changes concurrently during a rolling deployment.
const postgresMigrationLockID int64 = 0x535558454e

// Postgres is a shared metadata store backed by PostgreSQL. It embeds SQLStore
// so both dialects share one implementation.
type Postgres struct {
	*SQLStore
}

// OpenPostgres opens a PostgreSQL metadata store using a pgx database/sql connection pool.
func OpenPostgres(dataSourceName string) (*Postgres, error) {
	if dataSourceName == "" {
		return nil, fmt.Errorf("PostgreSQL data source name is empty")
	}
	database, err := sql.Open("pgx", dataSourceName)
	if err != nil {
		return nil, fmt.Errorf("open PostgreSQL database: %w", err)
	}
	database.SetMaxOpenConns(20)
	database.SetMaxIdleConns(10)
	database.SetConnMaxLifetime(time.Hour)
	return &Postgres{
		SQLStore: &SQLStore{
			db:      newDialectDB(database, dialectPostgres),
			dialect: dialectPostgres,
		},
	}, nil
}

var _ Store = (*Postgres)(nil)
