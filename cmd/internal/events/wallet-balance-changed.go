/*
@Author: Franco Ribeiro Borba
@Description: WalletBalanceChanged integration event. It is produced whenever a
balance actually moves, which means one event per accepted debit or credit and
no event at all for LOSS, for a rejected operation or for a wallet opened with
zero balance. Its payload is built from the BalanceChange the aggregate returned
when it accepted the movement, so the balances it publishes are the ones the
wallet computed and committed, never a recalculation made by the caller. The
walletVersion it carries is the version the wallet reached with this movement,
which lets a consumer order the events of a wallet and discard a redelivery of
an older one.
@Date : 20/09/2026
@Update: -
*/
package events

import (
	"errors"
	"fmt"

	"github.com/FrancoBorba/distributed-wallet-api/cmd/internal/domain"
	"github.com/google/uuid"
)

// versionWalletBalanceChanged is the schema version of the event. It is set by
// the constructor and only changes when the payload stops being compatible with
// what consumers already read.
const versionWalletBalanceChanged = 1

var (
	ErrInvalidWalletVersion = errors.New("wallet version must be greater than zero")
	ErrInvalidChangeAmount  = errors.New("balance change amount must be greater than zero")
	ErrNegativeEventBalance = errors.New("published balances cannot be negative")
)

// WalletBalanceChanged is the payload of the event. Monetary values are
// serialized by Money as decimal strings with their currency, so no consumer
// ever has to guess a scale.
type WalletBalanceChanged struct {
	WalletID      uuid.UUID        `json:"walletId"`
	TransactionID uuid.UUID        `json:"transactionId"`
	Direction     domain.Direction `json:"direction"`
	Money         domain.Money     `json:"money"`
	BalanceBefore domain.Money     `json:"balanceBefore"`
	BalanceAfter  domain.Money     `json:"balanceAfter"`
	WalletVersion int64            `json:"walletVersion"`
}

// NewWalletBalanceChanged builds the event of a movement the wallet has just
// accepted. It takes the BalanceChange returned by Debit or Credit together
// with the transaction that caused it, and validates both again because
// BalanceChange is a plain struct that any caller could fill in. The instant of
// the event is the one the aggregate produced, not the moment of publication.
func NewWalletBalanceChanged(change domain.BalanceChange, transactionID uuid.UUID, meta Metadata) (Envelope, error) {
	if change.WalletID == uuid.Nil {
		return Envelope{}, domain.ErrInvalidWalletID
	}
	if transactionID == uuid.Nil {
		return Envelope{}, domain.ErrInvalidTransactionID
	}
	if change.Direction != domain.DirectionDebit && change.Direction != domain.DirectionCredit {
		return Envelope{}, fmt.Errorf("%w: %q", domain.ErrInvalidDirection, change.Direction)
	}
	if err := requireInitialized(change.Amount, change.BalanceBefore, change.BalanceAfter); err != nil {
		return Envelope{}, err
	}

	// An event of this type means money moved, so a zero amount would describe
	// a change that did not happen.
	if !change.Amount.IsPositive() {
		return Envelope{}, fmt.Errorf("%w: %s", ErrInvalidChangeAmount, change.Amount)
	}
	if change.BalanceBefore.IsNegative() || change.BalanceAfter.IsNegative() {
		return Envelope{}, ErrNegativeEventBalance
	}
	if change.WalletVersion < 1 {
		return Envelope{}, fmt.Errorf("%w: %d", ErrInvalidWalletVersion, change.WalletVersion)
	}

	data := WalletBalanceChanged{
		WalletID:      change.WalletID,
		TransactionID: transactionID,
		Direction:     change.Direction,
		Money:         change.Amount,
		BalanceBefore: change.BalanceBefore,
		BalanceAfter:  change.BalanceAfter,
		WalletVersion: change.WalletVersion,
	}

	return newEnvelope(
		TypeWalletBalanceChanged,
		versionWalletBalanceChanged,
		change.WalletID,
		change.OccurredAt,
		meta,
		data,
	)
}

// requireInitialized rejects a Money that was never built through a domain
// constructor. Its currency would be empty and it would fail to serialize in
// the middle of a publication, long after the transaction that produced it was
// committed.
func requireInitialized(values ...domain.Money) error {
	for _, value := range values {
		if value.Currency() == "" {
			return domain.ErrUninitializedMoney
		}
	}

	return nil
}
