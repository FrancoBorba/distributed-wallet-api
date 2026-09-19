/*
@Author: Franco Ribeiro Borba
@Description: Wallet aggregate root. A wallet belongs to a single player and
holds its balance in one currency, so the pair (playerID, currency) identifies
it. The balance can only change through Debit and Credit, which reject non
positive amounts and currencies different from the wallet currency, and a
debit is never allowed to leave the balance below zero. Every accepted
movement returns a BalanceChange with the balances before and after it, which
is the source for the ledger entry and the WalletBalanceChanged event. The
version starts at 1 on creation and is incremented only when the balance
changes, which lets the persistence layer detect concurrent writers. NewWallet
creates a new wallet, while RestoreWallet rebuilds one from storage without
reapplying movements, rejecting any persisted state that breaks an invariant.
@Date : 19/09/2026
@Update: -
*/
package domain

import (
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"
)

// initialVersion is the version of a wallet right after its creation.
const initialVersion int64 = 1

var (
	ErrInsufficientFunds      = errors.New("insufficient funds")
	ErrInvalidDebit           = errors.New("debit amount must be greater than zero")
	ErrInvalidCredit          = errors.New("credit amount must be greater than zero")
	ErrInvalidWalletID        = errors.New("wallet id is required")
	ErrInvalidPlayerID        = errors.New("player id is required")
	ErrInvalidTimestamp       = errors.New("timestamp is required")
	ErrNegativeInitialBalance = errors.New("initial balance cannot be negative")
	ErrInvalidWalletState     = errors.New("invalid persisted wallet state")
)

// Direction tells whether a movement takes money out of or puts money into a
// wallet.
type Direction string

const (
	DirectionDebit  Direction = "DEBIT"
	DirectionCredit Direction = "CREDIT"
)

// BalanceChange describes a movement accepted by a wallet. It carries every
// value the ledger entry and the WalletBalanceChanged event need, so callers
// never have to capture the previous balance on their own.
type BalanceChange struct {
	WalletID      uuid.UUID
	Direction     Direction
	Amount        Money
	BalanceBefore Money
	BalanceAfter  Money
	WalletVersion int64
	OccurredAt    time.Time
}

type Wallet struct {
	id        uuid.UUID
	playerID  uuid.UUID
	balance   Money
	version   int64
	createdAt time.Time
	updatedAt time.Time
}

// NewWallet creates a wallet with version 1. The initial balance may be zero
// or positive, and its currency becomes the wallet currency.
func NewWallet(id uuid.UUID, playerID uuid.UUID, initialBalance Money, now time.Time) (*Wallet, error) {
	if err := validateIdentity(id, playerID); err != nil {
		return nil, err
	}
	if err := initialBalance.validate(); err != nil {
		return nil, err
	}
	if initialBalance.IsNegative() {
		return nil, ErrNegativeInitialBalance
	}
	if now.IsZero() {
		return nil, ErrInvalidTimestamp
	}

	return &Wallet{
		id:        id,
		playerID:  playerID,
		balance:   initialBalance,
		version:   initialVersion,
		createdAt: now.UTC(),
		updatedAt: now.UTC(),
	}, nil
}

// RestoreWallet rebuilds a wallet from its persisted state, keeping the stored
// balance and version as they are. It rejects a state that breaks an invariant
// instead of loading it, so corrupted rows never reach the business rules.
func RestoreWallet(id uuid.UUID, playerID uuid.UUID, balance Money, version int64, createdAt, updatedAt time.Time) (*Wallet, error) {
	if err := validateIdentity(id, playerID); err != nil {
		return nil, err
	}
	if err := balance.validate(); err != nil {
		return nil, err
	}
	if balance.IsNegative() {
		return nil, fmt.Errorf("%w: negative balance %s", ErrInvalidWalletState, balance)
	}
	if version < initialVersion {
		return nil, fmt.Errorf("%w: version %d is lower than %d", ErrInvalidWalletState, version, initialVersion)
	}
	if createdAt.IsZero() || updatedAt.IsZero() {
		return nil, ErrInvalidTimestamp
	}
	if updatedAt.Before(createdAt) {
		return nil, fmt.Errorf("%w: updatedAt is before createdAt", ErrInvalidWalletState)
	}

	return &Wallet{
		id:        id,
		playerID:  playerID,
		balance:   balance,
		version:   version,
		createdAt: createdAt.UTC(),
		updatedAt: updatedAt.UTC(),
	}, nil
}

// validateIdentity rejects missing wallet or player identifiers.
func validateIdentity(id uuid.UUID, playerID uuid.UUID) error {
	if id == uuid.Nil {
		return ErrInvalidWalletID
	}
	if playerID == uuid.Nil {
		return ErrInvalidPlayerID
	}
	return nil
}

// Debit withdraws a positive amount from the balance, increments the version
// and returns the resulting BalanceChange. It fails without changing the
// wallet when the currency differs or the balance is not enough.
func (w *Wallet) Debit(amount Money, now time.Time) (BalanceChange, error) {
	if amount.IsZero() || amount.IsNegative() {
		return BalanceChange{}, ErrInvalidDebit
	}
	if now.IsZero() {
		return BalanceChange{}, ErrInvalidTimestamp
	}

	// Compare first so that a lack of funds is reported as a business error.
	comparison, err := w.balance.Compare(amount)
	if err != nil {
		return BalanceChange{}, err // Currency mismatch or uninitialized value.
	}
	if comparison < 0 {
		return BalanceChange{}, ErrInsufficientFunds
	}

	newBalance, err := w.balance.Subtract(amount)
	if err != nil {
		return BalanceChange{}, err // Arithmetic overflow.
	}

	return w.apply(DirectionDebit, amount, newBalance, now), nil
}

// Credit adds a positive amount to the balance, increments the version and
// returns the resulting BalanceChange. It fails without changing the wallet
// when the currency differs or the sum overflows.
func (w *Wallet) Credit(amount Money, now time.Time) (BalanceChange, error) {
	if amount.IsZero() || amount.IsNegative() {
		return BalanceChange{}, ErrInvalidCredit
	}
	if now.IsZero() {
		return BalanceChange{}, ErrInvalidTimestamp
	}

	newBalance, err := w.balance.Add(amount)
	if err != nil {
		return BalanceChange{}, err
	}

	return w.apply(DirectionCredit, amount, newBalance, now), nil
}

// apply stores an already validated balance, increments the version and
// describes the movement. The update instant never moves backwards, so clock
// skew between instances cannot place updatedAt before a previous change.
func (w *Wallet) apply(direction Direction, amount Money, newBalance Money, now time.Time) BalanceChange {
	occurredAt := now.UTC()
	if occurredAt.Before(w.updatedAt) {
		occurredAt = w.updatedAt
	}

	change := BalanceChange{
		WalletID:      w.id,
		Direction:     direction,
		Amount:        amount,
		BalanceBefore: w.balance,
		BalanceAfter:  newBalance,
		WalletVersion: w.version + 1,
		OccurredAt:    occurredAt,
	}

	w.balance = newBalance
	w.version = change.WalletVersion
	w.updatedAt = occurredAt

	return change
}

// ID returns the wallet identifier.
func (w *Wallet) ID() uuid.UUID {
	return w.id
}

// PlayerID returns the identifier of the player that owns the wallet.
func (w *Wallet) PlayerID() uuid.UUID {
	return w.playerID
}

// Currency returns the ISO 4217 code of the wallet, which together with the
// player identifies it.
func (w *Wallet) Currency() string {
	return w.balance.Currency()
}

// Balance returns the current balance. Money is immutable, so the caller
// cannot change the wallet through it.
func (w *Wallet) Balance() Money {
	return w.balance
}

// Version returns the optimistic concurrency version of the wallet.
func (w *Wallet) Version() int64 {
	return w.version
}

// CreatedAt returns the creation instant in UTC.
func (w *Wallet) CreatedAt() time.Time {
	return w.createdAt
}

// UpdatedAt returns the instant of the last balance change in UTC.
func (w *Wallet) UpdatedAt() time.Time {
	return w.updatedAt
}
