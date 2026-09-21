/*
@Author: Franco Ribeiro Borba
@Description: In-memory doubles for the use case tests
*/
package usecase

import (
	"context"
	"fmt"
	"sync"
	"time"

	"github.com/FrancoBorba/distributed-wallet-api/cmd/internal/domain"
	"github.com/FrancoBorba/distributed-wallet-api/cmd/internal/events"
	"github.com/google/uuid"
)

// store holds everything the repositories would keep in PostgreSQL. The fake
// unit of work commits as soon as the function returns, since these tests are
// about the decisions of the flow and not about the atomicity, which belongs to
// the integration tests against a real database.
type store struct {
	mu           sync.Mutex
	wallets      map[uuid.UUID]walletRow
	transactions map[uuid.UUID]*domain.WagerTransaction
	ledger       []*domain.WalletLedgerEntry
	outbox       []events.Envelope
	inbox        map[string]*inboxRow
}

// walletRow is the persisted state of a wallet, kept as plain values so that
// every read rebuilds a fresh aggregate, the way a real repository does.
type walletRow struct {
	id        uuid.UUID
	playerID  uuid.UUID
	balance   domain.Money
	version   int64
	createdAt time.Time
	updatedAt time.Time
}

type inboxRow struct {
	hash        string
	completedAt *time.Time
}

func newStore() *store {
	return &store{
		wallets:      map[uuid.UUID]walletRow{},
		transactions: map[uuid.UUID]*domain.WagerTransaction{},
		inbox:        map[string]*inboxRow{},
	}
}

// Within runs the function with repositories bound to this store.
func (s *store) Within(ctx context.Context, fn func(ctx context.Context, repos Repositories) error) error {
	return fn(ctx, Repositories{
		Wallets:      (*walletRepo)(s),
		Transactions: (*transactionRepo)(s),
		Ledger:       (*ledgerRepo)(s),
		Outbox:       (*outboxRepo)(s),
		Inbox:        (*inboxRepo)(s),
	})
}

// putWallet seeds a wallet directly, as an earlier opening would have left it.
func (s *store) putWallet(wallet *domain.Wallet) {
	s.wallets[wallet.ID()] = walletRow{
		id:        wallet.ID(),
		playerID:  wallet.PlayerID(),
		balance:   wallet.Balance(),
		version:   wallet.Version(),
		createdAt: wallet.CreatedAt(),
		updatedAt: wallet.UpdatedAt(),
	}
}

// eventsOfType returns every stored event of one type.
func (s *store) eventsOfType(eventType events.EventType) []events.Envelope {
	var found []events.Envelope

	for _, envelope := range s.outbox {
		if envelope.Type() == eventType {
			found = append(found, envelope)
		}
	}

	return found
}

// balanceOf returns the stored balance of a wallet.
func (s *store) balanceOf(id uuid.UUID) domain.Money {
	return s.wallets[id].balance
}

type walletRepo store

func (r *walletRepo) LockByID(_ context.Context, id uuid.UUID) (*domain.Wallet, error) {
	row, found := r.wallets[id]
	if !found {
		return nil, ErrWalletNotFound
	}

	return domain.RestoreWallet(row.id, row.playerID, row.balance, row.version, row.createdAt, row.updatedAt)
}

func (r *walletRepo) FindByPlayerAndCurrency(_ context.Context, playerID uuid.UUID, currency string) (*domain.Wallet, error) {
	for _, row := range r.wallets {
		if row.playerID == playerID && row.balance.Currency() == currency {
			return domain.RestoreWallet(row.id, row.playerID, row.balance, row.version, row.createdAt, row.updatedAt)
		}
	}

	return nil, ErrWalletNotFound
}

func (r *walletRepo) Insert(_ context.Context, wallet *domain.Wallet) error {
	if _, exists := r.wallets[wallet.ID()]; exists {
		return ErrWalletAlreadyExists
	}

	(*store)(r).putWallet(wallet)

	return nil
}

func (r *walletRepo) UpdateBalance(_ context.Context, wallet *domain.Wallet, expectedVersion int64) error {
	row, found := r.wallets[wallet.ID()]
	if !found {
		return ErrWalletNotFound
	}

	// The same condition the SQL UPDATE carries: a writer that moved the
	// wallet in between makes this fail instead of overwriting it.
	if row.version != expectedVersion {
		return ErrConcurrentUpdate
	}

	(*store)(r).putWallet(wallet)

	return nil
}

type transactionRepo store

func (r *transactionRepo) FindByID(_ context.Context, id uuid.UUID) (*domain.WagerTransaction, error) {
	transaction, found := r.transactions[id]
	if !found {
		return nil, ErrTransactionNotFound
	}

	return transaction, nil
}

func (r *transactionRepo) FindByProviderAndExternalID(_ context.Context, providerID, externalID string) (*domain.WagerTransaction, error) {
	for _, transaction := range r.transactions {
		if transaction.ProviderID() == providerID && transaction.ExternalTransactionID() == externalID {
			return transaction, nil
		}
	}

	return nil, ErrTransactionNotFound
}

func (r *transactionRepo) Insert(_ context.Context, transaction *domain.WagerTransaction) error {
	if _, exists := r.transactions[transaction.ID()]; exists {
		return ErrConcurrentUpdate
	}

	r.transactions[transaction.ID()] = transaction

	return nil
}

func (r *transactionRepo) Update(_ context.Context, transaction *domain.WagerTransaction) error {
	if _, exists := r.transactions[transaction.ID()]; !exists {
		return ErrTransactionNotFound
	}

	r.transactions[transaction.ID()] = transaction

	return nil
}

func (r *transactionRepo) HasProcessedReversal(_ context.Context, referenceID uuid.UUID) (bool, error) {
	for _, transaction := range r.transactions {
		resolved, ok := transaction.ReferenceTransactionID()
		if !ok || resolved != referenceID {
			continue
		}
		if transaction.State() == domain.StateProcessed && transaction.Kind().RequiresReference() {
			return true, nil
		}
	}

	return false, nil
}

type ledgerRepo store

func (r *ledgerRepo) Insert(_ context.Context, entry *domain.WalletLedgerEntry) error {
	for _, stored := range r.ledger {
		// The uniqueness of (walletId, transactionId), which is what makes a
		// duplicated movement impossible.
		if stored.WalletID() == entry.WalletID() && stored.TransactionID() == entry.TransactionID() {
			return ErrConcurrentUpdate
		}
	}

	r.ledger = append(r.ledger, entry)

	return nil
}

type outboxRepo store

func (r *outboxRepo) Save(_ context.Context, envelope events.Envelope) error {
	r.outbox = append(r.outbox, envelope)

	return nil
}

type inboxRepo store

func (r *inboxRepo) Register(_ context.Context, consumer, messageID, payloadHash string, now time.Time) (InboxStatus, error) {
	key := consumer + "/" + messageID

	row, found := r.inbox[key]
	if !found {
		r.inbox[key] = &inboxRow{hash: payloadHash}
		return InboxNew, nil
	}
	if row.hash != payloadHash {
		return InboxNew, fmt.Errorf("%w: %s", ErrMessageConflict, messageID)
	}
	if row.completedAt != nil {
		return InboxCompleted, nil
	}

	return InboxInProgress, nil
}

func (r *inboxRepo) Complete(_ context.Context, consumer, messageID string, now time.Time) error {
	key := consumer + "/" + messageID

	row, found := r.inbox[key]
	if !found {
		return nil
	}

	moment := now
	row.completedAt = &moment

	return nil
}

// fixedClock pins the instant, so no test depends on the wall clock.
type fixedClock struct {
	now time.Time
}

func (c fixedClock) Now() time.Time { return c.now }

// sequentialIDs hands out predictable identifiers, which keeps a failure
// message readable.
type sequentialIDs struct {
	next uint32
}

func (g *sequentialIDs) New() uuid.UUID {
	g.next++

	var id uuid.UUID
	id[0] = byte(g.next >> 24)
	id[1] = byte(g.next >> 16)
	id[2] = byte(g.next >> 8)
	id[3] = byte(g.next)
	id[15] = 1

	return id
}
