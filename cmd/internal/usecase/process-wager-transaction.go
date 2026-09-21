/*
@Author: Franco Ribeiro Borba
@Description: ProcessWagerTransaction use case. It is the single financial
entry point of the service: HTTP and SQS both call it, so both get the same
rules, the same idempotency and the same events. One call runs inside one SQL
transaction that locks the wallet, records the operation, moves the balance,
appends the ledger entry and writes the outbox events, which is what makes a
crash at any point leave a consistent state behind. An operation is identified
by (providerId, externalTransactionId), so a replay finds the stored result and
answers with it instead of moving money twice, while the same key carrying a
different body is a conflict. A reversal whose reference has not arrived is
parked in PENDING_REFERENCE rather than refused, and the same flow resumes it
later, because resuming and receiving are the same work seen twice.
@Date : 20/09/2026
@Update: -
*/
package usecase

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/FrancoBorba/distributed-wallet-api/cmd/internal/domain"
	"github.com/FrancoBorba/distributed-wallet-api/cmd/internal/events"
	"github.com/google/uuid"
)

// ErrInvalidCommand is a request that does not describe an operation at all,
// as opposed to one that is refused by a business rule.
var (
	ErrInvalidCommand = errors.New("invalid wager command")
	// ErrUnreversibleReference is a rollback pointing at an operation that moved
	// no balance, so there is no direction to invert.
	ErrUnreversibleReference = errors.New("referenced operation moves no balance")
)

// WagerCommand is one provider operation, already validated at the transport
// edge. PayloadHash is computed from the canonical JSON of the business fields,
// which is what makes the same operation over HTTP and over SQS comparable.
type WagerCommand struct {
	ProviderID            string
	ExternalTransactionID string
	IdempotencyKey        string
	PayloadHash           string
	PlayerID              uuid.UUID
	WalletID              uuid.UUID
	RoundID               string
	GameID                string
	Kind                  domain.TransactionKind
	Money                 domain.Money
	ReferenceExternalID   string
	Metadata              events.Metadata
}

// WagerResult is the outcome the caller reports back to the provider. Balance
// is only filled for a processed operation, since it is the balance observed
// when the money actually moved; a rejection stores no balance.
type WagerResult struct {
	TransactionID uuid.UUID
	State         domain.TransactionState
	Balance       domain.Money
	FailureCode   domain.FailureCode
	// Replayed reports that nothing was applied because the operation had
	// already reached a terminal state.
	Replayed bool
}

// ProcessWagerTransaction applies provider operations to wallets.
type ProcessWagerTransaction struct {
	uow   UnitOfWork
	clock Clock
	ids   IDGenerator
}

// NewProcessWagerTransaction builds the use case with the ports it depends on.
func NewProcessWagerTransaction(uow UnitOfWork, clock Clock, ids IDGenerator) *ProcessWagerTransaction {
	return &ProcessWagerTransaction{uow: uow, clock: clock, ids: ids}
}

// Execute runs the operation in one transaction. The whole flow is retried by
// the caller when it returns ErrConcurrentUpdate, since that means another
// writer moved the wallet and the decision was taken over a stale balance.
func (u *ProcessWagerTransaction) Execute(ctx context.Context, cmd WagerCommand) (WagerResult, error) {
	if err := cmd.validate(); err != nil {
		return WagerResult{}, err
	}

	var result WagerResult

	err := u.runWithRetry(ctx, func(ctx context.Context, repos Repositories) error {
		outcome, err := u.apply(ctx, repos, cmd)
		if err != nil {
			return err
		}

		result = outcome
		return nil
	})
	if err != nil {
		return WagerResult{}, err
	}

	return result, nil
}

// ExecuteFromMessage runs the operation of a consumed message. The inbox record
// and the conclusion of the handling share the transaction of the financial
// change, so a message is only ever marked as handled together with the work it
// caused. A redelivery of a completed message applies nothing.
func (u *ProcessWagerTransaction) ExecuteFromMessage(ctx context.Context, consumer string, cmd WagerCommand, messageID, messageHash string) (WagerResult, error) {
	if err := cmd.validate(); err != nil {
		return WagerResult{}, err
	}

	var result WagerResult

	err := u.runWithRetry(ctx, func(ctx context.Context, repos Repositories) error {
		now := u.clock.Now()

		status, err := repos.Inbox.Register(ctx, consumer, messageID, messageHash, now)
		if err != nil {
			return err
		}
		if status == InboxCompleted {
			// The message was already handled durably. Nothing is applied
			// again, and the caller may delete it from the queue.
			result = WagerResult{Replayed: true}
			return nil
		}

		// InboxInProgress falls through on purpose: the previous attempt died
		// before committing, so nothing it did is visible, and the domain
		// idempotency decides what is still left to do.
		outcome, err := u.apply(ctx, repos, cmd)
		if err != nil {
			return err
		}

		result = outcome

		return repos.Inbox.Complete(ctx, consumer, messageID, now)
	})
	if err != nil {
		return WagerResult{}, err
	}

	return result, nil
}

// apply is the flow itself, already inside a transaction.
func (u *ProcessWagerTransaction) apply(ctx context.Context, repos Repositories, cmd WagerCommand) (WagerResult, error) {
	now := u.clock.Now()
	meta := u.metadata(cmd.Metadata)

	transaction, replayed, err := u.record(ctx, repos, cmd, now)
	if err != nil {
		return WagerResult{}, err
	}
	if replayed {
		return replayOf(transaction), nil
	}

	// Locking the wallet is what serializes two operations over the same
	// wallet. Different wallets stay fully parallel, since the lock is a row
	// lock and not a table one.
	wallet, err := repos.Wallets.LockByID(ctx, cmd.WalletID)
	if errors.Is(err, ErrWalletNotFound) {
		return u.reject(ctx, repos, transaction, domain.FailureWalletNotFound, now, meta)
	}
	if err != nil {
		return WagerResult{}, err
	}

	// The wallet exists, but it is not this player's wallet. Telling the
	// caller it was not found is deliberate: a provider must not be able to
	// probe which wallets exist for players it does not own.
	if wallet.PlayerID() != cmd.PlayerID {
		return u.reject(ctx, repos, transaction, domain.FailureWalletNotFound, now, meta)
	}
	if wallet.Currency() != cmd.Money.Currency() {
		return u.reject(ctx, repos, transaction, domain.FailureCurrencyMismatch, now, meta)
	}

	reference, outcome, err := u.resolveReference(ctx, repos, transaction, now, meta)
	if err != nil || outcome != nil {
		return valueOrZero(outcome), err
	}

	return u.settle(ctx, repos, wallet, transaction, reference, now, meta)
}

// record finds the operation or creates it. The lookup is by provider and
// external identifier, never by the idempotency key alone, so the same
// operation replayed with a new key is still recognised as the same movement.
func (u *ProcessWagerTransaction) record(ctx context.Context, repos Repositories, cmd WagerCommand, now time.Time) (*domain.WagerTransaction, bool, error) {
	existing, err := repos.Transactions.FindByProviderAndExternalID(ctx, cmd.ProviderID, cmd.ExternalTransactionID)
	if err != nil && !errors.Is(err, ErrTransactionNotFound) {
		return nil, false, err
	}

	if existing != nil {
		// The same operation carrying a different body is a client error, not
		// a replay: answering with the stored result would hide the mistake.
		if existing.PayloadHash() != cmd.PayloadHash {
			return nil, false, fmt.Errorf("%w: %s", ErrPayloadConflict, cmd.ExternalTransactionID)
		}
		if existing.IsTerminal() {
			return existing, true, nil
		}

		// PENDING or PENDING_REFERENCE: the work was started and never
		// finished, so this delivery resumes it.
		return existing, false, nil
	}

	transaction, err := domain.NewExternalTransaction(domain.ExternalTransactionParams{
		ID:                    u.ids.New(),
		WalletID:              cmd.WalletID,
		PlayerID:              cmd.PlayerID,
		ProviderID:            cmd.ProviderID,
		ExternalTransactionID: cmd.ExternalTransactionID,
		IdempotencyKey:        cmd.IdempotencyKey,
		PayloadHash:           cmd.PayloadHash,
		RoundID:               cmd.RoundID,
		GameID:                cmd.GameID,
		ReferenceExternalID:   cmd.ReferenceExternalID,
		Kind:                  cmd.Kind,
		Money:                 cmd.Money,
		Now:                   now,
	})
	if err != nil {
		return nil, false, fmt.Errorf("%w: %v", ErrInvalidCommand, err)
	}

	// The record is durable before the money moves, so another instance can
	// resume the operation if this one dies mid flight.
	if err := repos.Transactions.Insert(ctx, transaction); err != nil {
		return nil, false, err
	}

	return transaction, false, nil
}

// resolveReference finds what a reversal undoes. It returns a result when the
// operation is already decided, either because it must wait for a reference
// that has not arrived or because the reference makes it impossible, and a nil
// result when the flow should carry on. Kinds that reverse nothing return
// immediately.
func (u *ProcessWagerTransaction) resolveReference(ctx context.Context, repos Repositories, transaction *domain.WagerTransaction, now time.Time, meta events.Metadata) (*domain.WagerTransaction, *WagerResult, error) {
	if !transaction.Kind().RequiresReference() {
		return nil, nil, nil
	}

	externalID, _ := transaction.ReferenceExternalID()

	reference, err := repos.Transactions.FindByProviderAndExternalID(ctx, transaction.ProviderID(), externalID)
	if err != nil && !errors.Is(err, ErrTransactionNotFound) {
		return nil, nil, err
	}

	// The reversal arrived before what it reverses. This is expected in an
	// at-least-once world with no ordering between producers, so the operation
	// waits instead of being refused, and a worker retries it with backoff.
	if reference == nil {
		result, err := u.park(ctx, repos, transaction, now, meta)
		return nil, &result, err
	}

	// The reference exists but has not concluded yet. Waiting is the only
	// correct answer: rejecting now would refuse an operation that is about to
	// become valid.
	if !reference.IsTerminal() {
		result, err := u.park(ctx, repos, transaction, now, meta)
		return nil, &result, err
	}

	// The reference concluded without success, so there is nothing to undo.
	if reference.State() != domain.StateProcessed {
		result, err := u.reject(ctx, repos, transaction, domain.FailureReferenceNotProcessed, now, meta)
		return nil, &result, err
	}

	if !agrees(transaction, reference) {
		result, err := u.reject(ctx, repos, transaction, domain.FailureReferenceMismatch, now, meta)
		return nil, &result, err
	}

	// A reference receives one successful reversal. The database enforces the
	// same rule with a partial unique index, so two concurrent reversals of the
	// same bet cannot both commit.
	reversed, err := repos.Transactions.HasProcessedReversal(ctx, reference.ID())
	if err != nil {
		return nil, nil, err
	}
	if reversed {
		result, err := u.reject(ctx, repos, transaction, domain.FailureAlreadyReversed, now, meta)
		return nil, &result, err
	}

	if err := transaction.ResolveReference(reference.ID(), now); err != nil {
		return nil, nil, err
	}

	return reference, nil, nil
}

// settle moves the balance and concludes the operation. LOSS goes through it
// as well, concluding with no movement, which is why it produces
// WagerTransactionProcessed but no ledger entry and no WalletBalanceChanged.
func (u *ProcessWagerTransaction) settle(ctx context.Context, repos Repositories, wallet *domain.Wallet, transaction *domain.WagerTransaction, reference *domain.WagerTransaction, now time.Time, meta events.Metadata) (WagerResult, error) {
	direction, moves, err := movementOf(transaction, reference)
	if err != nil {
		return WagerResult{}, err
	}

	if !moves {
		return u.conclude(ctx, repos, transaction, wallet.Balance(), nil, now, meta)
	}

	change, err := applyMovement(wallet, direction, transaction.Money(), now)
	if errors.Is(err, domain.ErrInsufficientFunds) {
		return u.reject(ctx, repos, transaction, insufficientFundsCode(transaction.Kind()), now, meta)
	}
	if err != nil {
		return WagerResult{}, err
	}

	// The update is conditioned on the version the wallet had when it was read,
	// so a writer that slipped in between is detected instead of overwritten.
	if err := repos.Wallets.UpdateBalance(ctx, wallet, change.WalletVersion-1); err != nil {
		return WagerResult{}, err
	}

	entry, err := domain.NewLedgerEntry(u.ids.New(), transaction.ID(), change)
	if err != nil {
		return WagerResult{}, err
	}
	if err := repos.Ledger.Insert(ctx, entry); err != nil {
		return WagerResult{}, err
	}

	return u.conclude(ctx, repos, transaction, wallet.Balance(), &change, now, meta)
}

// conclude marks the operation as processed and writes its events. The balance
// it stores is the one observed now, which is what a later replay answers with.
func (u *ProcessWagerTransaction) conclude(ctx context.Context, repos Repositories, transaction *domain.WagerTransaction, balance domain.Money, change *domain.BalanceChange, now time.Time, meta events.Metadata) (WagerResult, error) {
	if err := transaction.MarkAsProcessed(balance, now); err != nil {
		return WagerResult{}, err
	}
	if err := repos.Transactions.Update(ctx, transaction); err != nil {
		return WagerResult{}, err
	}

	builders := []func() (events.Envelope, error){
		func() (events.Envelope, error) { return events.NewWagerTransactionProcessed(transaction, meta) },
	}
	if change != nil {
		movement := *change
		builders = append(builders, func() (events.Envelope, error) {
			return events.NewWalletBalanceChanged(movement, transaction.ID(), meta)
		})
	}

	if err := saveEvents(ctx, repos.Outbox, builders...); err != nil {
		return WagerResult{}, err
	}

	return WagerResult{
		TransactionID: transaction.ID(),
		State:         transaction.State(),
		Balance:       balance,
	}, nil
}

// reject refuses the operation for good, with a stable code the provider can
// act upon, and publishes the refusal.
func (u *ProcessWagerTransaction) reject(ctx context.Context, repos Repositories, transaction *domain.WagerTransaction, code domain.FailureCode, now time.Time, meta events.Metadata) (WagerResult, error) {
	if err := transaction.Reject(code, now); err != nil {
		return WagerResult{}, err
	}
	if err := repos.Transactions.Update(ctx, transaction); err != nil {
		return WagerResult{}, err
	}

	err := saveEvents(ctx, repos.Outbox, func() (events.Envelope, error) {
		return events.NewWagerTransactionRejected(transaction, meta)
	})
	if err != nil {
		return WagerResult{}, err
	}

	return WagerResult{
		TransactionID: transaction.ID(),
		State:         transaction.State(),
		FailureCode:   code,
	}, nil
}

// park stores a reversal that is waiting for its reference and publishes the
// wait, so the provider learns the operation was neither lost nor refused. A
// transaction already parked is left as it is, since re-announcing the same
// wait on every retry would flood the consumer.
func (u *ProcessWagerTransaction) park(ctx context.Context, repos Repositories, transaction *domain.WagerTransaction, now time.Time, meta events.Metadata) (WagerResult, error) {
	if transaction.State() == domain.StatePendingRef {
		return WagerResult{TransactionID: transaction.ID(), State: transaction.State()}, nil
	}

	if err := transaction.MarkAsPendingReference(now); err != nil {
		return WagerResult{}, err
	}
	if err := repos.Transactions.Update(ctx, transaction); err != nil {
		return WagerResult{}, err
	}

	err := saveEvents(ctx, repos.Outbox, func() (events.Envelope, error) {
		return events.NewWagerTransactionPendingReference(transaction, meta)
	})
	if err != nil {
		return WagerResult{}, err
	}

	return WagerResult{
		TransactionID: transaction.ID(),
		State:         transaction.State(),
	}, nil
}

// metadata starts a correlation when the caller did not provide one, so every
// event this service publishes can always be traced back to its operation.
func (u *ProcessWagerTransaction) metadata(meta events.Metadata) events.Metadata {
	if meta.CorrelationID == uuid.Nil {
		meta.CorrelationID = u.ids.New()
	}

	return meta
}

// validate rejects a command that does not describe an operation. The business
// rules of each kind are checked again by the domain when the transaction is
// created; what is refused here is a request that could never be one.
func (c WagerCommand) validate() error {
	switch {
	case c.ProviderID == "":
		return fmt.Errorf("%w: providerId is required", ErrInvalidCommand)
	case c.ExternalTransactionID == "":
		return fmt.Errorf("%w: externalTransactionId is required", ErrInvalidCommand)
	case c.PayloadHash == "":
		return fmt.Errorf("%w: payloadHash is required", ErrInvalidCommand)
	case c.WalletID == uuid.Nil:
		return fmt.Errorf("%w: walletId is required", ErrInvalidCommand)
	case c.PlayerID == uuid.Nil:
		return fmt.Errorf("%w: playerId is required", ErrInvalidCommand)
	case !c.Kind.IsExternal():
		return fmt.Errorf("%w: %q is not a provider operation", ErrInvalidCommand, c.Kind)
	case c.Money.Currency() == "":
		return fmt.Errorf("%w: %v", ErrInvalidCommand, domain.ErrUninitializedMoney)
	}

	return nil
}

// movementOf returns how an operation moves the balance. A ROLLBACK is the only
// kind whose direction is not a property of itself: it is the opposite of what
// the referenced transaction did, and is therefore only known once that
// reference is resolved.
func movementOf(transaction *domain.WagerTransaction, reference *domain.WagerTransaction) (domain.Direction, bool, error) {
	if transaction.Kind() != domain.KindRollback {
		direction, moves := transaction.Kind().Direction()
		return direction, moves, nil
	}

	if reference == nil {
		return "", false, domain.ErrUnresolvedReference
	}

	original, moves := reference.Kind().Direction()
	if !moves {
		return "", false, fmt.Errorf("%w: %s", ErrUnreversibleReference, reference.Kind())
	}

	if original == domain.DirectionDebit {
		return domain.DirectionCredit, true, nil
	}

	return domain.DirectionDebit, true, nil
}

// applyMovement asks the aggregate to move its own balance. The wallet is what
// decides whether the movement is possible, so no balance arithmetic ever
// happens in this layer.
func applyMovement(wallet *domain.Wallet, direction domain.Direction, amount domain.Money, now time.Time) (domain.BalanceChange, error) {
	if direction == domain.DirectionDebit {
		return wallet.Debit(amount, now)
	}

	return wallet.Credit(amount, now)
}

// insufficientFundsCode tells apart a bet that exceeds the balance from a
// reversal that would, because the two are audited separately.
func insufficientFundsCode(kind domain.TransactionKind) domain.FailureCode {
	if kind.RequiresReference() {
		return domain.FailureReversalInsufficientFunds
	}

	return domain.FailureInsufficientFunds
}

// agrees reports whether a reversal and its reference describe the same money.
// Partial reversals are out of scope, so the amounts must be equal, and the
// operation must belong to the same player, wallet, currency and round.
func agrees(transaction *domain.WagerTransaction, reference *domain.WagerTransaction) bool {
	if transaction.PlayerID() != reference.PlayerID() ||
		transaction.WalletID() != reference.WalletID() ||
		transaction.RoundID() != reference.RoundID() {
		return false
	}

	// A REFUND returns a bet; a ROLLBACK undoes a bet, a win or a refund.
	switch transaction.Kind() {
	case domain.KindRefund:
		if reference.Kind() != domain.KindBet {
			return false
		}
	case domain.KindRollback:
		switch reference.Kind() {
		case domain.KindBet, domain.KindWin, domain.KindRefund:
		default:
			return false
		}
	}

	equal, err := transaction.Money().Equals(reference.Money())

	return err == nil && equal
}

// replayOf builds the answer of an operation that had already concluded.
func replayOf(transaction *domain.WagerTransaction) WagerResult {
	result := WagerResult{
		TransactionID: transaction.ID(),
		State:         transaction.State(),
		Replayed:      true,
	}

	if balance, ok := transaction.ResultBalance(); ok {
		result.Balance = balance
	}
	if code, ok := transaction.FailureCode(); ok {
		result.FailureCode = code
	}

	return result
}

// valueOrZero dereferences an optional result.
func valueOrZero(result *WagerResult) WagerResult {
	if result == nil {
		return WagerResult{}
	}

	return *result
}

// maxConflictRetries bounds how many times a whole operation is replayed after
// losing a race for the wallet. The limit exists because a retry is only worth
// it while the contention is momentary: past that, failing and letting the
// message come back later spreads the load instead of feeding it.
const maxConflictRetries = 3

// runWithRetry runs the transaction again when another writer moved the wallet
// under it. Retrying the whole transaction, and not only the write, is what
// makes it correct: the decision was taken over a balance that no longer
// exists, so it has to be taken again over the balance that does. Every attempt
// is a fresh transaction, and the idempotency of the operation is what keeps a
// replay from applying anything twice.
func (u *ProcessWagerTransaction) runWithRetry(ctx context.Context, fn func(ctx context.Context, repos Repositories) error) error {
	var err error

	for attempt := 0; attempt < maxConflictRetries; attempt++ {
		err = u.uow.Within(ctx, fn)
		if !errors.Is(err, ErrConcurrentUpdate) {
			return err
		}
		if ctx.Err() != nil {
			return ctx.Err()
		}
	}

	return err
}
