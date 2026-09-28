package store

import (
	"context"
	"database/sql"
	"fmt"
	"strings"
)

const (
	dialectSQLite   = "sqlite"
	dialectPostgres = "postgres"
)

type dialectDB struct {
	database *sql.DB
	dialect  string
}

func newDialectDB(database *sql.DB, dialect string) *dialectDB {
	return &dialectDB{database: database, dialect: dialect}
}

func (db *dialectDB) ExecContext(
	ctx context.Context,
	query string,
	arguments ...any,
) (sql.Result, error) {
	return db.database.ExecContext(ctx, db.bind(query), arguments...)
}

func (db *dialectDB) QueryContext(
	ctx context.Context,
	query string,
	arguments ...any,
) (*sql.Rows, error) {
	return db.database.QueryContext(ctx, db.bind(query), arguments...)
}

func (db *dialectDB) QueryRowContext(
	ctx context.Context,
	query string,
	arguments ...any,
) *sql.Row {
	return db.database.QueryRowContext(ctx, db.bind(query), arguments...)
}

func (db *dialectDB) BeginTx(
	ctx context.Context,
	options *sql.TxOptions,
) (*dialectTx, error) {
	transaction, err := db.database.BeginTx(ctx, options)
	if err != nil {
		return nil, err
	}
	return &dialectTx{transaction: transaction, dialect: db.dialect}, nil
}

func (db *dialectDB) PingContext(ctx context.Context) error {
	return db.database.PingContext(ctx)
}

func (db *dialectDB) Close() error {
	return db.database.Close()
}

func (db *dialectDB) bind(query string) string {
	if db.dialect != dialectPostgres {
		return query
	}
	return bindPostgres(query)
}

type dialectTx struct {
	transaction *sql.Tx
	dialect     string
}

func (transaction *dialectTx) ExecContext(
	ctx context.Context,
	query string,
	arguments ...any,
) (sql.Result, error) {
	return transaction.transaction.ExecContext(
		ctx,
		transaction.bind(query),
		arguments...,
	)
}

func (transaction *dialectTx) QueryContext(
	ctx context.Context,
	query string,
	arguments ...any,
) (*sql.Rows, error) {
	return transaction.transaction.QueryContext(
		ctx,
		transaction.bind(query),
		arguments...,
	)
}

func (transaction *dialectTx) QueryRowContext(
	ctx context.Context,
	query string,
	arguments ...any,
) *sql.Row {
	return transaction.transaction.QueryRowContext(
		ctx,
		transaction.bind(query),
		arguments...,
	)
}

func (transaction *dialectTx) Commit() error {
	return transaction.transaction.Commit()
}

func (transaction *dialectTx) Rollback() error {
	return transaction.transaction.Rollback()
}

func (transaction *dialectTx) bind(query string) string {
	if transaction.dialect != dialectPostgres {
		return query
	}
	return bindPostgres(query)
}

func bindPostgres(query string) string {
	var result strings.Builder
	result.Grow(len(query) + 16)
	parameter := 1
	inSingleQuote := false
	for index := 0; index < len(query); index++ {
		character := query[index]
		if character == '\'' {
			result.WriteByte(character)
			if inSingleQuote && index+1 < len(query) && query[index+1] == '\'' {
				result.WriteByte(query[index+1])
				index++
				continue
			}
			inSingleQuote = !inSingleQuote
			continue
		}
		if character == '?' && !inSingleQuote {
			_, _ = fmt.Fprintf(&result, "$%d", parameter)
			parameter++
			continue
		}
		result.WriteByte(character)
	}
	return result.String()
}
