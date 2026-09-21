/*
@Author: Franco Ribeiro Borba:
@Description: OpenWallet use case. It creates the wallet of a player in one
currency and, when the initial balance is positive, everything that justifies
that money being there: the internal OPENING transaction already PROCESSED, its
credit entry in the ledger and the WagerTransactionProcessed and
WalletBalanceChanged events in the outbox, all in the same commit as the wallet
itself. A wallet opened with zero balance creates none of those records, because
no money moved. The pair (playerId, currency) identifies a wallet, so a second
opening is a conflict and not a new wallet, and the database enforces the same
rule with a unique constraint in case two instances try it at the same instant.
@Date : 20/09/2026
@Update: -
*/
package usecase

import (
	"context"
	"errors"

	"github.com/FrancoBorba/distributed-wallet-api/cmd/internal/domain"
	"github.com/FrancoBorba/distributed-wallet-api/cmd/internal/events"
	"github.com/google/uuid"
)

// OpenWalletCommand is the request to open a wallet. The currency of the wallet
// is the currency of the initial balance.
type OpenWalletCommand struct {
	PlayerID       uuid.UUID
	InitialBalance domain.Money
	// CorrelationID ties the opening to the caller trace. When absent, one is
	// started here.
	CorrelationID uuid.UUID
}

// OpenWallet creates wallets.
type OpenWallet struct {
	uow   UnitOfWork
	clock Clock
	ids   IDGenerator
}

// NewOpenWallet builds the use case with the ports it depends on.
func NewOpenWallet(uow UnitOfWork, clock Clock, ids IDGenerator) *OpenWallet {
	return &OpenWallet{uow: uow, clock: clock, ids: ids}
}

// Execute opens the wallet and returns it. Everything it writes belongs to a
// single transaction, so a crash halfway leaves neither a wallet without its
// opening record nor an opening record without its wallet.
func (u *OpenWallet) Execute(ctx context.Context, cmd OpenWalletCommand) (*domain.Wallet, error) {
	now := u.clock.Now()

	if cmd.InitialBalance.Currency() == "" {
		return nil, domain.ErrUninitializedMoney
	}
	if cmd.InitialBalance.IsNegative() {
		return nil, domain.ErrNegativeInitialBalance
	}

	meta := events.NewMetadata(u.correlation(cmd.CorrelationID))

	var wallet *domain.Wallet

	err := u.uow.Within(ctx, func(ctx context.Context, repos Repositories) error {
		existing, err := repos.Wallets.FindByPlayerAndCurrency(ctx, cmd.PlayerID, cmd.InitialBalance.Currency())
		if err != nil && !errors.Is(err, ErrWalletNotFound) {
			return err
		}
		if existing != nil {
			return ErrWalletAlreadyExists
		}

		wallet, err = domain.NewWallet(u.ids.New(), cmd.PlayerID, cmd.InitialBalance, now)
		if err != nil {
			return err
		}
		if err := repos.Wallets.Insert(ctx, wallet); err != nil {
			return err
		}

		// Zero is a valid initial balance and moves nothing, so it produces no
		// transaction, no ledger entry and no financial event.
		if !cmd.InitialBalance.IsPositive() {
			return nil
		}

		return u.recordOpening(ctx, repos, wallet, meta)
	})
	if err != nil {
		return nil, err
	}

	return wallet, nil
}

// recordOpening writes the internal OPENING and everything it justifies. The
// BalanceChange is assembled here because the wallet was born with the balance
// already applied: it never went through Credit, so there is no movement for
// the aggregate to describe. The values are the only ones an opening can have,
// and both the ledger entry and the event validate them again.
func (u *OpenWallet) recordOpening(ctx context.Context, repos Repositories, wallet *domain.Wallet, meta events.Metadata) error {
	opening, err := domain.NewOpeningTransaction(u.ids.New(), wallet.ID(), wallet.PlayerID(), wallet.Balance(), wallet.CreatedAt())
	if err != nil {
		return err
	}
	if err := repos.Transactions.Insert(ctx, opening); err != nil {
		return err
	}

	zero, err := domain.Zero(wallet.Currency())
	if err != nil {
		return err
	}

	change := domain.BalanceChange{
		WalletID:      wallet.ID(),
		Direction:     domain.DirectionCredit,
		Amount:        wallet.Balance(),
		BalanceBefore: zero,
		BalanceAfter:  wallet.Balance(),
		// An opening is the first version of the wallet, by definition.
		WalletVersion: wallet.Version(),
		OccurredAt:    wallet.CreatedAt(),
	}

	entry, err := domain.NewLedgerEntry(u.ids.New(), opening.ID(), change)
	if err != nil {
		return err
	}
	if err := repos.Ledger.Insert(ctx, entry); err != nil {
		return err
	}

	return saveEvents(ctx, repos.Outbox,
		func() (events.Envelope, error) { return events.NewWagerTransactionProcessed(opening, meta) },
		func() (events.Envelope, error) { return events.NewWalletBalanceChanged(change, opening.ID(), meta) },
	)
}

// correlation returns the trace identifier of the operation, starting one when
// the caller did not provide it.
func (u *OpenWallet) correlation(correlationID uuid.UUID) uuid.UUID {
	if correlationID != uuid.Nil {
		return correlationID
	}

	return u.ids.New()
}

// saveEvents builds and stores events in order, stopping at the first failure.
// Building and storing are kept together so that an event can never be created
// and then forgotten before reaching the outbox.
func saveEvents(ctx context.Context, outbox OutboxRepository, builders ...func() (events.Envelope, error)) error {
	for _, build := range builders {
		envelope, err := build()
		if err != nil {
			return err
		}
		if err := outbox.Save(ctx, envelope); err != nil {
			return err
		}
	}

	return nil
}
