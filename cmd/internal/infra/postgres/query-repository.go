/*
@Author: Franco Ribeiro Borba
@Description: Read side of the persistence. Every query here runs on the pool
and outside any business transaction, so a page of the ledger or a replay can
never block an operation that is moving money, and none of them takes a lock.
The exception that proves the rule is the reconciliation: it reads the balance
and the whole history inside a REPEATABLE READ transaction, because comparing a
balance taken at one instant against entries read at another would report a
difference that never existed. Reads go through the domain constructors, so a
row that breaks an invariant is refused instead of being served to a caller.
@Date : 20/09/2026
@Update: -
*/
package postgres

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/FrancoBorba/distributed-wallet-api/cmd/internal/domain"
	"github.com/FrancoBorba/distributed-wallet-api/cmd/internal/usecase"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// ledgerColumns is the projection of a history read, in the order
// scanLedgerEntry expects.
const ledgerColumns = `seq, id, wallet_id, transaction_id, direction, currency,
	amount_cents, balance_before_cents, balance_after_cents, created_at`

// QueryRepository answers reads.
type QueryRepository struct {
	pool *pgxpool.Pool
}

// NewQueryRepository builds the read side over the connection pool.
func NewQueryRepository(pool *pgxpool.Pool) *QueryRepository {
	return &QueryRepository{pool: pool}
}

// FindWallet reads a wallet without locking it.
func (r *QueryRepository) FindWallet(ctx context.Context, id uuid.UUID) (*domain.Wallet, error) {
	wallet, err := scanWallet(r.pool.QueryRow(ctx, `SELECT `+walletColumns+` FROM wallets WHERE id = $1`, id))
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, usecase.ErrWalletNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("read wallet %s: %w", id, err)
	}

	return wallet, nil
}

// FindTransaction reads a transaction by its internal identifier.
func (r *QueryRepository) FindTransaction(ctx context.Context, id uuid.UUID) (*domain.WagerTransaction, error) {
	transaction, err := scanTransaction(r.pool.QueryRow(ctx, `SELECT `+transactionColumns+` FROM wager_transactions WHERE id = $1`, id))
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, usecase.ErrTransactionNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("read transaction %s: %w", id, err)
	}

	return transaction, nil
}

// FindTransactionByProvider reads a transaction the way the provider knows it.
func (r *QueryRepository) FindTransactionByProvider(ctx context.Context, providerID, externalID string) (*domain.WagerTransaction, error) {
	row := r.pool.QueryRow(ctx,
		`SELECT `+transactionColumns+`
		   FROM wager_transactions
		  WHERE provider_id = $1 AND external_transaction_id = $2`,
		providerID, externalID,
	)

	transaction, err := scanTransaction(row)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, usecase.ErrTransactionNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("read transaction %s of %s: %w", externalID, providerID, err)
	}

	return transaction, nil
}

// PageLedger returns the entries of a wallet after a sequence. The ordering is
// the sequence itself, which never changes once a row is written, so a cursor
// taken now still points at the same place after new entries are appended.
func (r *QueryRepository) PageLedger(ctx context.Context, walletID uuid.UUID, afterSequence int64, limit int) ([]usecase.LedgerEntryRow, error) {
	rows, err := r.pool.Query(ctx,
		`SELECT `+ledgerColumns+`
		   FROM wallet_ledger
		  WHERE wallet_id = $1 AND seq > $2
		  ORDER BY seq
		  LIMIT $3`,
		walletID, afterSequence, limit,
	)
	if err != nil {
		return nil, fmt.Errorf("page ledger of wallet %s: %w", walletID, err)
	}
	defer rows.Close()

	var page []usecase.LedgerEntryRow

	for rows.Next() {
		sequence, entry, err := scanLedgerEntry(rows)
		if err != nil {
			return nil, err
		}

		page = append(page, usecase.LedgerEntryRow{Sequence: sequence, Entry: entry})
	}

	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("read ledger page: %w", err)
	}

	return page, nil
}

// LedgerSnapshot reads the balance and the whole history of a wallet in one
// consistent view.
//
// REPEATABLE READ is what makes the comparison meaningful: every statement in
// the transaction sees the same snapshot of the database, so an operation that
// commits while the entries are being read is invisible to both the balance
// and the history, and the reconciliation reports the state as it was at one
// instant instead of a mix of two.
func (r *QueryRepository) LedgerSnapshot(ctx context.Context, walletID uuid.UUID) (domain.Money, []*domain.WalletLedgerEntry, error) {
	transaction, err := r.pool.BeginTx(ctx, pgx.TxOptions{
		IsoLevel:   pgx.RepeatableRead,
		AccessMode: pgx.ReadOnly,
	})
	if err != nil {
		return domain.Money{}, nil, fmt.Errorf("begin reconciliation snapshot: %w", err)
	}

	defer func() {
		_ = transaction.Rollback(context.WithoutCancel(ctx))
	}()

	wallet, err := scanWallet(transaction.QueryRow(ctx, `SELECT `+walletColumns+` FROM wallets WHERE id = $1`, walletID))
	if errors.Is(err, pgx.ErrNoRows) {
		return domain.Money{}, nil, usecase.ErrWalletNotFound
	}
	if err != nil {
		return domain.Money{}, nil, fmt.Errorf("read wallet %s: %w", walletID, err)
	}

	rows, err := transaction.Query(ctx,
		`SELECT `+ledgerColumns+` FROM wallet_ledger WHERE wallet_id = $1 ORDER BY seq`,
		walletID,
	)
	if err != nil {
		return domain.Money{}, nil, fmt.Errorf("read ledger of wallet %s: %w", walletID, err)
	}
	defer rows.Close()

	var entries []*domain.WalletLedgerEntry

	for rows.Next() {
		_, entry, err := scanLedgerEntry(rows)
		if err != nil {
			return domain.Money{}, nil, err
		}

		entries = append(entries, entry)
	}

	if err := rows.Err(); err != nil {
		return domain.Money{}, nil, fmt.Errorf("read ledger of wallet %s: %w", walletID, err)
	}

	return wallet.Balance(), entries, nil
}

// scanLedgerEntry rebuilds one entry and returns its storage sequence.
func scanLedgerEntry(row pgx.Row) (int64, *domain.WalletLedgerEntry, error) {
	var (
		sequence                                     int64
		id, walletID, transactionID                  uuid.UUID
		direction, currency                          string
		amountCents, balanceBefore, balanceAfterCent int64
		createdAt                                    = time.Time{}
	)

	err := row.Scan(
		&sequence, &id, &walletID, &transactionID, &direction, &currency,
		&amountCents, &balanceBefore, &balanceAfterCent, &createdAt,
	)
	if err != nil {
		return 0, nil, fmt.Errorf("scan ledger entry: %w", err)
	}

	amount, err := domain.NewMoney(amountCents, currency)
	if err != nil {
		return 0, nil, err
	}
	before, err := domain.NewMoney(balanceBefore, currency)
	if err != nil {
		return 0, nil, err
	}
	after, err := domain.NewMoney(balanceAfterCent, currency)
	if err != nil {
		return 0, nil, err
	}

	entry, err := domain.RestoreLedgerEntry(domain.RestoredLedgerEntryParams{
		ID:            id,
		WalletID:      walletID,
		TransactionID: transactionID,
		Direction:     domain.Direction(direction),
		Amount:        amount,
		BalanceBefore: before,
		BalanceAfter:  after,
		CreatedAt:     createdAt,
	})
	if err != nil {
		return 0, nil, err
	}

	return sequence, entry, nil
}
