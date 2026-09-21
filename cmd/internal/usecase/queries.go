/*
@Author: Franco Ribeiro Borba
@Description: Read ports of the application layer. Reads are kept apart from
the write flow on purpose: a query never opens a business transaction, never
takes a lock and never produces an event, so it cannot slow down or interfere
with an operation that is moving money. The ledger page carries the sequence of
each entry because the ordering of the history is a property of the storage and
not of the domain: no invariant depends on it, it exists so that pagination has
a stable order, and the sequence is what the opaque cursor is built from. The
reconciliation is the one read that needs a consistent view, since comparing a
balance against a history that moved while it was being read would report a
difference that never existed.
@Date : 20/09/2026
@Update: -
*/
package usecase

import (
	"context"

	"github.com/FrancoBorba/distributed-wallet-api/cmd/internal/domain"
	"github.com/google/uuid"
)

// LedgerEntryRow is one line of the history together with its storage
// sequence, which is the ordering the cursor walks.
type LedgerEntryRow struct {
	Sequence int64
	Entry    *domain.WalletLedgerEntry
}

// WalletQueries reads wallets and their history.
type WalletQueries interface {
	FindWallet(ctx context.Context, id uuid.UUID) (*domain.Wallet, error)
	// PageLedger returns entries after a sequence, in a stable order. Asking
	// for one more than the page size is what tells the caller whether there
	// is a next page without a second query.
	PageLedger(ctx context.Context, walletID uuid.UUID, afterSequence int64, limit int) ([]LedgerEntryRow, error)
	// LedgerSnapshot reads the stored balance and every entry of a wallet in
	// one consistent view of the data, which is what the reconciliation
	// compares. It exists as a single call because the consistency is a
	// property of reading both together, not of reading each one well.
	LedgerSnapshot(ctx context.Context, walletID uuid.UUID) (domain.Money, []*domain.WalletLedgerEntry, error)
}

// TransactionQueries reads wager transactions.
type TransactionQueries interface {
	FindTransaction(ctx context.Context, id uuid.UUID) (*domain.WagerTransaction, error)
	FindTransactionByProvider(ctx context.Context, providerID, externalID string) (*domain.WagerTransaction, error)
}

// Reconciliation compares the stored balance of a wallet against the balance
// rebuilt from its ledger. Difference is the stored balance minus the
// calculated one, so a positive value means the wallet holds more money than
// its history justifies.
type Reconciliation struct {
	WalletID          uuid.UUID
	StoredBalance     domain.Money
	CalculatedBalance domain.Money
	Difference        domain.Money
	Consistent        bool
	CheckedEntries    int
}

// ReconcileWallet rebuilds a balance from the ledger and compares it with the
// stored one. It is read only: a divergence is reported, never corrected,
// because an automatic correction would destroy the evidence of whatever
// caused it.
type ReconcileWallet struct {
	queries WalletQueries
}

// NewReconcileWallet builds the use case.
func NewReconcileWallet(queries WalletQueries) *ReconcileWallet {
	return &ReconcileWallet{queries: queries}
}

// Execute reconciles one wallet. The replay includes the opening credit, since
// it is an ordinary ledger entry, which is why a wallet opened with a positive
// balance reconciles to exactly that balance.
func (u *ReconcileWallet) Execute(ctx context.Context, walletID uuid.UUID) (Reconciliation, error) {
	storedBalance, entries, err := u.queries.LedgerSnapshot(ctx, walletID)
	if err != nil {
		return Reconciliation{}, err
	}

	calculated, err := domain.ReplayLedgerBalance(storedBalance.Currency(), entries)
	if err != nil {
		return Reconciliation{}, err
	}

	difference, err := storedBalance.Subtract(calculated)
	if err != nil {
		return Reconciliation{}, err
	}

	return Reconciliation{
		WalletID:          walletID,
		StoredBalance:     storedBalance,
		CalculatedBalance: calculated,
		Difference:        difference,
		Consistent:        difference.IsZero(),
		CheckedEntries:    len(entries),
	}, nil
}
