/*
@Author: Franco Ribeiro Borba
@Description: SQL transaction boundary. UnitOfWork is the only place in the
service that opens and commits a transaction: it begins one, builds every
repository over it and hands them to the use case as a bundle, so the balance,
the ledger entry, the state of the operation, the inbox record and the outbox
events are written by the same transaction and commit together or not at all.
A repository can only be reached through this bundle, which is what keeps the
rule "an event is never published before the commit that produced it" a
property of the structure instead of a matter of discipline. The rollback is
deferred and unconditional: after a successful commit it is a no-op, and on any
error, panic or cancelled context it undoes everything the use case had
written.
@Date : 20/09/2026
@Update: -
*/
package postgres

import (
	"context"
	"errors"
	"fmt"

	"github.com/FrancoBorba/distributed-wallet-api/cmd/internal/usecase"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"
)

// uniqueViolation is the SQLSTATE PostgreSQL raises when a unique constraint
// refuses a row. It is how a race that slipped past an application check comes
// back to us, since the database is the authority on uniqueness.
const uniqueViolation = "23505"

// Querier is the subset of pgx used by the repositories. Both a pool and a
// transaction satisfy it, so the same repository code serves a use case inside
// a transaction and a worker reading outside of one.
type Querier interface {
	Exec(ctx context.Context, sql string, arguments ...any) (pgconn.CommandTag, error)
	Query(ctx context.Context, sql string, args ...any) (pgx.Rows, error)
	QueryRow(ctx context.Context, sql string, args ...any) pgx.Row
}

// UnitOfWork runs use cases inside a single SQL transaction.
type UnitOfWork struct {
	pool *pgxpool.Pool
}

// NewUnitOfWork builds the transaction boundary over a connection pool.
func NewUnitOfWork(pool *pgxpool.Pool) *UnitOfWork {
	return &UnitOfWork{pool: pool}
}

// Within runs fn in a transaction, committing when it returns nil and rolling
// back on any error. Read Committed is enough here because the wallet is taken
// with a row lock, which serializes the writers of the same wallet without
// blocking operations over other wallets.
func (u *UnitOfWork) Within(ctx context.Context, fn func(ctx context.Context, repos usecase.Repositories) error) error {
	transaction, err := u.pool.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.ReadCommitted})
	if err != nil {
		return fmt.Errorf("begin transaction: %w", err)
	}

	// Safe after a successful commit: rolling back a finished transaction
	// returns pgx.ErrTxClosed, which is exactly the situation we ignore.
	defer func() {
		_ = transaction.Rollback(context.WithoutCancel(ctx))
	}()

	if err := fn(ctx, repositoriesOf(transaction)); err != nil {
		return err
	}

	if err := transaction.Commit(ctx); err != nil {
		return fmt.Errorf("commit transaction: %w", err)
	}

	return nil
}

// repositoriesOf binds every repository to the same querier.
func repositoriesOf(querier Querier) usecase.Repositories {
	return usecase.Repositories{
		Wallets:      NewWalletRepository(querier),
		Transactions: NewTransactionRepository(querier),
		Ledger:       NewLedgerRepository(querier),
		Outbox:       NewOutboxRepository(querier),
		Inbox:        NewInboxRepository(querier),
	}
}

// isUniqueViolation reports whether an error is a unique constraint refusal,
// optionally of one specific constraint. It is how a concurrent writer that won
// the race is recognised and turned into a business answer.
func isUniqueViolation(err error, constraint string) bool {
	var pgErr *pgconn.PgError
	if !errors.As(err, &pgErr) || pgErr.Code != uniqueViolation {
		return false
	}

	return constraint == "" || pgErr.ConstraintName == constraint
}
