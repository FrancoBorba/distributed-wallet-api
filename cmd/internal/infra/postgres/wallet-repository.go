/*
@Author: Franco Ribeiro Borba
@Description: Wallet repository. Money crosses this boundary as BIGINT minor
units and an ISO 4217 code, which is the exact shape of the Money value object,
so no conversion and no rounding ever happens on the way in or out. LockByID
takes the row with SELECT FOR UPDATE, which is what serializes two operations
over the same wallet while leaving different wallets fully parallel, and
UpdateBalance is still conditioned on the version the wallet had when it was
read. The two together are deliberate: the lock prevents the race inside one
instance and the version check turns any lost update that still happens, from a
lock taken in the wrong order or a connection reset, into a visible
ErrConcurrentUpdate instead of a silently overwritten balance.
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
)

// walletColumns is the projection every read of a wallet uses, in the order
// scanWallet expects.
const walletColumns = `id, player_id, currency, balance_cents, version, created_at, updated_at`

// WalletRepository reads and writes wallets.
type WalletRepository struct {
	querier Querier
}

// NewWalletRepository binds the repository to a querier, which is the current
// transaction when it comes from the unit of work.
func NewWalletRepository(querier Querier) *WalletRepository {
	return &WalletRepository{querier: querier}
}

// LockByID loads a wallet and holds its row until the transaction ends. Every
// writer of the same wallet queues here, which is what makes two simultaneous
// bets over one balance decide one after the other instead of both reading the
// same funds.
func (r *WalletRepository) LockByID(ctx context.Context, id uuid.UUID) (*domain.Wallet, error) {
	row := r.querier.QueryRow(ctx, `SELECT `+walletColumns+` FROM wallets WHERE id = $1 FOR UPDATE`, id)

	wallet, err := scanWallet(row)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, usecase.ErrWalletNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("lock wallet %s: %w", id, err)
	}

	return wallet, nil
}

// FindByPlayerAndCurrency loads the wallet a player holds in one currency. The
// pair identifies a wallet, so at most one row can match.
func (r *WalletRepository) FindByPlayerAndCurrency(ctx context.Context, playerID uuid.UUID, currency string) (*domain.Wallet, error) {
	row := r.querier.QueryRow(ctx,
		`SELECT `+walletColumns+` FROM wallets WHERE player_id = $1 AND currency = $2`,
		playerID, currency,
	)

	wallet, err := scanWallet(row)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, usecase.ErrWalletNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("find wallet of player %s in %s: %w", playerID, currency, err)
	}

	return wallet, nil
}

// Insert writes a new wallet. A second opening for the same player and currency
// is refused by the database, which is the answer that counts when two
// instances try it at the same instant.
func (r *WalletRepository) Insert(ctx context.Context, wallet *domain.Wallet) error {
	_, err := r.querier.Exec(ctx,
		`INSERT INTO wallets (id, player_id, currency, balance_cents, version, created_at, updated_at)
		 VALUES ($1, $2, $3, $4, $5, $6, $7)`,
		wallet.ID(), wallet.PlayerID(), wallet.Currency(),
		wallet.Balance().Cents(), wallet.Version(),
		wallet.CreatedAt(), wallet.UpdatedAt(),
	)
	if isUniqueViolation(err, "wallets_player_currency_key") {
		return usecase.ErrWalletAlreadyExists
	}
	if err != nil {
		return fmt.Errorf("insert wallet %s: %w", wallet.ID(), err)
	}

	return nil
}

// UpdateBalance writes a balance the aggregate has already moved. The version
// the wallet was read at is part of the WHERE clause, so an update that matches
// no row means somebody else moved the wallet first and the decision was taken
// over a balance that no longer exists.
func (r *WalletRepository) UpdateBalance(ctx context.Context, wallet *domain.Wallet, expectedVersion int64) error {
	tag, err := r.querier.Exec(ctx,
		`UPDATE wallets
		    SET balance_cents = $1, version = $2, updated_at = $3
		  WHERE id = $4 AND version = $5`,
		wallet.Balance().Cents(), wallet.Version(), wallet.UpdatedAt(),
		wallet.ID(), expectedVersion,
	)
	if err != nil {
		return fmt.Errorf("update balance of wallet %s: %w", wallet.ID(), err)
	}
	if tag.RowsAffected() == 0 {
		return usecase.ErrConcurrentUpdate
	}

	return nil
}

// scanWallet rebuilds a wallet from a row. It goes through RestoreWallet, so a
// stored state that breaks an invariant is refused instead of loaded.
func scanWallet(row pgx.Row) (*domain.Wallet, error) {
	var (
		id, playerID          uuid.UUID
		currency              string
		balanceCents, version int64
		createdAt, updatedAt  = time.Time{}, time.Time{}
	)

	if err := row.Scan(&id, &playerID, &currency, &balanceCents, &version, &createdAt, &updatedAt); err != nil {
		return nil, err
	}

	balance, err := domain.NewMoney(balanceCents, currency)
	if err != nil {
		return nil, err
	}

	return domain.RestoreWallet(id, playerID, balance, version, createdAt, updatedAt)
}
