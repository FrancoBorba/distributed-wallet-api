/*
@Author: Franco Ribeiro Borba
@Description: WalletLedgerEntry value object. Each entry records one balance
movement of a wallet: the transaction that caused it, its direction, the amount
and the balances immediately before and after it. An entry is immutable and is
only accepted when balanceAfter equals balanceBefore plus or minus the amount,
according to the direction, so a chain of entries that does not add up cannot
be created. The amount must be greater than zero, which is what keeps LOSS and
every rejected operation out of the ledger. NewLedgerEntry builds an entry from
the BalanceChange returned by the wallet, so the balances always come from the
aggregate instead of a calculation made by the caller, while RestoreLedgerEntry
rebuilds one from storage under the same validation. The ledger is append-only:
a financial correction is a new entry, never an edit, and ReplayLedgerBalance
rebuilds a balance from the entries for reconciliation.
@Date : 20/09/2026
@Update: -
*/
package domain

import (
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"
)

var (
	ErrInvalidLedgerEntryID  = errors.New("ledger entry id is required")
	ErrInvalidDirection      = errors.New("direction must be DEBIT or CREDIT")
	ErrInvalidLedgerAmount   = errors.New("ledger entry amount must be greater than zero")
	ErrNegativeLedgerBalance = errors.New("ledger balances cannot be negative")
	ErrLedgerBalanceMismatch = errors.New("balance after does not match balance before and amount")
	ErrNilLedgerEntry        = errors.New("ledger entry is nil")
)

// WalletLedgerEntry is one immutable line of the wallet history. Every field is
// stored by value, so an entry can never be changed after it is built.
//
// The persistence layer adds its own ordering column for cursor pagination;
// that sequence is not part of the entry, because no domain invariant depends
// on it.
type WalletLedgerEntry struct {
	id            uuid.UUID
	walletID      uuid.UUID
	transactionID uuid.UUID
	direction     Direction
	amount        Money
	balanceBefore Money
	balanceAfter  Money
	createdAt     time.Time
}

// NewLedgerEntry builds the entry of a movement the wallet has just accepted.
// It takes the BalanceChange returned by Debit or Credit, so the balances and
// the instant are the ones the aggregate produced, and validates them again
// because BalanceChange is a plain struct that any caller could fill in.
func NewLedgerEntry(id uuid.UUID, transactionID uuid.UUID, change BalanceChange) (*WalletLedgerEntry, error) {
	return buildLedgerEntry(RestoredLedgerEntryParams{
		ID:            id,
		WalletID:      change.WalletID,
		TransactionID: transactionID,
		Direction:     change.Direction,
		Amount:        change.Amount,
		BalanceBefore: change.BalanceBefore,
		BalanceAfter:  change.BalanceAfter,
		CreatedAt:     change.OccurredAt,
	})
}

// RestoredLedgerEntryParams carries the persisted columns of an entry.
type RestoredLedgerEntryParams struct {
	ID            uuid.UUID
	WalletID      uuid.UUID
	TransactionID uuid.UUID
	Direction     Direction
	Amount        Money
	BalanceBefore Money
	BalanceAfter  Money
	CreatedAt     time.Time
}

// RestoreLedgerEntry rebuilds an entry from its persisted state. It applies no
// movement and recomputes no balance, it only refuses a row whose arithmetic
// does not hold, so a corrupted line never takes part in a reconciliation.
func RestoreLedgerEntry(params RestoredLedgerEntryParams) (*WalletLedgerEntry, error) {
	return buildLedgerEntry(params)
}

// buildLedgerEntry validates the fields shared by creation and rehydration.
// Both paths run the same rules, since a stored entry that breaks them is as
// wrong as a new one.
func buildLedgerEntry(params RestoredLedgerEntryParams) (*WalletLedgerEntry, error) {
	if params.ID == uuid.Nil {
		return nil, ErrInvalidLedgerEntryID
	}
	if params.WalletID == uuid.Nil {
		return nil, ErrInvalidWalletID
	}
	if params.TransactionID == uuid.Nil {
		return nil, ErrInvalidTransactionID
	}
	if params.Direction != DirectionDebit && params.Direction != DirectionCredit {
		return nil, fmt.Errorf("%w: %q", ErrInvalidDirection, params.Direction)
	}

	for _, value := range []Money{params.Amount, params.BalanceBefore, params.BalanceAfter} {
		if err := value.validate(); err != nil {
			return nil, err
		}
	}

	// An entry always moves money, so LOSS and rejected operations never
	// produce one.
	if !params.Amount.IsPositive() {
		return nil, fmt.Errorf("%w: %s", ErrInvalidLedgerAmount, params.Amount)
	}
	if params.BalanceBefore.IsNegative() || params.BalanceAfter.IsNegative() {
		return nil, ErrNegativeLedgerBalance
	}

	if err := validateLedgerArithmetic(params); err != nil {
		return nil, err
	}
	if params.CreatedAt.IsZero() {
		return nil, ErrInvalidTimestamp
	}

	return &WalletLedgerEntry{
		id:            params.ID,
		walletID:      params.WalletID,
		transactionID: params.TransactionID,
		direction:     params.Direction,
		amount:        params.Amount,
		balanceBefore: params.BalanceBefore,
		balanceAfter:  params.BalanceAfter,
		createdAt:     params.CreatedAt.UTC(),
	}, nil
}

// validateLedgerArithmetic checks balanceAfter = balanceBefore ± amount. It
// relies on Money, so a mismatch of currencies between the amount and the
// balances is reported as ErrCurrencyMismatch and an overflow is reported
// instead of wrapping around.
func validateLedgerArithmetic(params RestoredLedgerEntryParams) error {
	var (
		expected Money
		err      error
	)

	if params.Direction == DirectionCredit {
		expected, err = params.BalanceBefore.Add(params.Amount)
	} else {
		expected, err = params.BalanceBefore.Subtract(params.Amount)
	}
	if err != nil {
		return err
	}

	equal, err := expected.Equals(params.BalanceAfter)
	if err != nil {
		return err
	}
	if !equal {
		return fmt.Errorf("%w: %s %s %s should be %s, got %s",
			ErrLedgerBalanceMismatch, params.BalanceBefore, params.Direction,
			params.Amount, expected, params.BalanceAfter)
	}
	return nil
}

// ReplayLedgerBalance rebuilds a balance from the entries, adding credits and
// subtracting debits from zero. It is what the reconciliation compares against
// the stored balance, and it never changes anything.
//
// The entries do not need to be ordered, because addition is commutative, but
// they must all belong to the same currency.
func ReplayLedgerBalance(currency string, entries []*WalletLedgerEntry) (Money, error) {
	balance, err := Zero(currency)
	if err != nil {
		return Money{}, err
	}

	for _, entry := range entries {
		if entry == nil {
			return Money{}, ErrNilLedgerEntry
		}

		signed, err := entry.SignedAmount()
		if err != nil {
			return Money{}, err
		}
		balance, err = balance.Add(signed)
		if err != nil {
			return Money{}, err
		}
	}

	return balance, nil
}

// SignedAmount returns the amount as it affects the balance: positive for a
// credit and negative for a debit.
func (e *WalletLedgerEntry) SignedAmount() (Money, error) {
	switch e.direction {
	case DirectionCredit:
		return e.amount, nil
	case DirectionDebit:
		return e.amount.Negate()
	default:
		return Money{}, fmt.Errorf("%w: %q", ErrInvalidDirection, e.direction)
	}
}

// IsDebit reports whether the entry took money out of the wallet.
func (e *WalletLedgerEntry) IsDebit() bool { return e.direction == DirectionDebit }

// IsCredit reports whether the entry put money into the wallet.
func (e *WalletLedgerEntry) IsCredit() bool { return e.direction == DirectionCredit }

// ID returns the entry identifier.
func (e *WalletLedgerEntry) ID() uuid.UUID { return e.id }

// WalletID returns the wallet the entry belongs to.
func (e *WalletLedgerEntry) WalletID() uuid.UUID { return e.walletID }

// TransactionID returns the transaction that caused the movement. Together with
// the wallet it is unique, so one transaction never produces two entries.
func (e *WalletLedgerEntry) TransactionID() uuid.UUID { return e.transactionID }

// Direction returns whether the entry is a DEBIT or a CREDIT.
func (e *WalletLedgerEntry) Direction() Direction { return e.direction }

// Amount returns the amount moved, always greater than zero.
func (e *WalletLedgerEntry) Amount() Money { return e.amount }

// BalanceBefore returns the wallet balance immediately before the movement.
func (e *WalletLedgerEntry) BalanceBefore() Money { return e.balanceBefore }

// BalanceAfter returns the wallet balance immediately after the movement.
func (e *WalletLedgerEntry) BalanceAfter() Money { return e.balanceAfter }

// Currency returns the ISO 4217 code shared by the amount and the balances.
func (e *WalletLedgerEntry) Currency() string { return e.amount.Currency() }

// CreatedAt returns the instant of the movement in UTC.
func (e *WalletLedgerEntry) CreatedAt() time.Time { return e.createdAt }
