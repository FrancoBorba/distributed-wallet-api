/*
@Author: Franco Ribeiro Borba
@Description: Ports of the application layer. Every interface here is defined
by the use cases that consume it and implemented by the infrastructure, so the
business flow depends on what it needs and never on PostgreSQL, SQS or Fx. The
central one is UnitOfWork: it hands the use case a bundle of repositories that
are already bound to a single SQL transaction, which is what makes the balance,
the ledger entry, the state of the operation, the inbox record and the outbox
events commit together or not at all. Nothing in this package opens or commits
a transaction by itself, and no repository is reachable outside of one, which
is how the rule "an event is never published before the commit that produced
it" is kept structural instead of a matter of discipline.
@Date : 20/09/2026
@Update: -
*/
package usecase

import (
	"context"
	"errors"
	"time"

	"github.com/FrancoBorba/distributed-wallet-api/cmd/internal/domain"
	"github.com/FrancoBorba/distributed-wallet-api/cmd/internal/events"
	"github.com/google/uuid"
)

var (
	// ErrWalletNotFound is returned by a repository when no wallet matches.
	ErrWalletNotFound = errors.New("wallet not found")
	// ErrTransactionNotFound is returned when no transaction matches.
	ErrTransactionNotFound = errors.New("transaction not found")
	// ErrConcurrentUpdate means another writer moved the wallet between the
	// read and the write. The caller retries the whole operation.
	ErrConcurrentUpdate = errors.New("wallet was changed by another writer")
	// ErrWalletAlreadyExists is the conflict of opening a second wallet for
	// the same player and currency.
	ErrWalletAlreadyExists = errors.New("wallet already exists for this player and currency")
	// ErrPayloadConflict is the same idempotency key reused with a different
	// body, which is a client error and never a replay.
	ErrPayloadConflict = errors.New("idempotency key was already used with a different payload")
	// ErrMessageConflict is the same messageId redelivered with a different
	// body, which the inbox refuses instead of treating as a duplicate.
	ErrMessageConflict = errors.New("message id was already received with a different payload")
)

// WalletRepository reads and writes wallets inside the current transaction.
type WalletRepository interface {
	// LockByID loads a wallet and holds it until the transaction ends, so two
	// operations over the same wallet are serialized instead of racing.
	LockByID(ctx context.Context, id uuid.UUID) (*domain.Wallet, error)
	// FindByPlayerAndCurrency answers whether a player already has a wallet in
	// a currency, which is what makes a second opening a conflict.
	FindByPlayerAndCurrency(ctx context.Context, playerID uuid.UUID, currency string) (*domain.Wallet, error)
	Insert(ctx context.Context, wallet *domain.Wallet) error
	// UpdateBalance writes a balance that was moved by the aggregate. It is
	// conditioned on the version the wallet had when it was read, so a lost
	// update surfaces as ErrConcurrentUpdate instead of silent corruption.
	UpdateBalance(ctx context.Context, wallet *domain.Wallet, expectedVersion int64) error
}

// TransactionRepository reads and writes wager transactions inside the current
// transaction.
type TransactionRepository interface {
	FindByID(ctx context.Context, id uuid.UUID) (*domain.WagerTransaction, error)
	// FindByProviderAndExternalID is the idempotency lookup: an operation is
	// identified by the provider and the identifier the provider gave it,
	// whatever transport or key it arrived with.
	FindByProviderAndExternalID(ctx context.Context, providerID, externalID string) (*domain.WagerTransaction, error)
	Insert(ctx context.Context, transaction *domain.WagerTransaction) error
	Update(ctx context.Context, transaction *domain.WagerTransaction) error
	// HasProcessedReversal reports whether a transaction was already reversed
	// successfully, which is what stops the same debit being returned twice.
	HasProcessedReversal(ctx context.Context, referenceID uuid.UUID) (bool, error)
}

// LedgerRepository appends entries to the immutable wallet history.
type LedgerRepository interface {
	Insert(ctx context.Context, entry *domain.WalletLedgerEntry) error
}

// OutboxRepository stores an event so that it is committed with the state that
// justifies it. Publication happens later, from a separate worker.
type OutboxRepository interface {
	Save(ctx context.Context, envelope events.Envelope) error
}

// InboxStatus is what the inbox knew about a message before this delivery.
type InboxStatus int

const (
	// InboxNew is the first time this consumer sees the message.
	InboxNew InboxStatus = iota
	// InboxInProgress is a message that was accepted but whose handling never
	// completed, typically because the process died halfway. The handler runs
	// again, and the domain idempotency decides what is still left to do.
	InboxInProgress
	// InboxCompleted is a redelivery of a message already handled. Nothing is
	// applied again and the message is dropped.
	InboxCompleted
)

// InboxRepository records the identity of consumed messages, which is the
// durable side of at-least-once deduplication.
type InboxRepository interface {
	// Register claims the message for this consumer. It reports what was known
	// about it before, and returns ErrMessageConflict when the same id comes
	// back carrying a different payload.
	Register(ctx context.Context, consumer, messageID, payloadHash string, now time.Time) (InboxStatus, error)
	// Complete marks the handling as durably concluded. It runs in the same
	// transaction as the domain changes, so a message is only ever completed
	// together with the work it caused.
	Complete(ctx context.Context, consumer, messageID string, now time.Time) error
}

// Repositories is the set of repositories bound to one SQL transaction. They
// are handed to the use case together, because every one of them writes part of
// the same atomic financial change.
type Repositories struct {
	Wallets      WalletRepository
	Transactions TransactionRepository
	Ledger       LedgerRepository
	Outbox       OutboxRepository
	Inbox        InboxRepository
}

// UnitOfWork runs a function inside a single SQL transaction, committing when
// it returns nil and rolling back on any error. It is the only way the use
// cases reach a repository, so no write can escape the transaction boundary.
type UnitOfWork interface {
	Within(ctx context.Context, fn func(ctx context.Context, repos Repositories) error) error
}

// Publisher delivers an event to the message broker. It is used only by the
// outbox worker, after the event was committed.
type Publisher interface {
	Publish(ctx context.Context, envelope events.Envelope) error
}

// Clock is the source of time of the use cases. It is a port so that tests can
// pin an instant instead of depending on the wall clock.
type Clock interface {
	Now() time.Time
}

// SystemClock is the production clock, always in UTC.
type SystemClock struct{}

// Now returns the current instant in UTC.
func (SystemClock) Now() time.Time { return time.Now().UTC() }

// IDGenerator produces the identifiers of new records. It is a port for the
// same reason as the clock: a test wants predictable identities.
type IDGenerator interface {
	New() uuid.UUID
}

// UUIDGenerator is the production generator.
type UUIDGenerator struct{}

// New returns a random identifier.
func (UUIDGenerator) New() uuid.UUID { return uuid.New() }
