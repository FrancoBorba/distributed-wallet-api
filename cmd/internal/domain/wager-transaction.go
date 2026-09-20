/*
@Author: Franco Ribeiro Borba
@Description: WagerTransaction entity. It records one financial operation over
a wallet, either an internal OPENING that justifies a positive initial balance
or an external BET, WIN, LOSS, REFUND or ROLLBACK sent by a provider through
HTTP or SQS. An external transaction is born PENDING and walks a validated
state machine towards one of the terminal states PROCESSED, REJECTED or FAILED;
an OPENING is born PROCESSED because it is applied in the same commit as the
wallet it opens. REFUND and ROLLBACK carry the external reference they reverse
and can wait in PENDING_REFERENCE until that reference is resolved to an
internal transaction, which must happen before they are processed. Because a
terminal transaction is never reapplied, it also stores the balance returned to
the provider, so an idempotent replay answers with the balance observed at the
original processing. NewOpeningTransaction and NewExternalTransaction create
transactions, while RestoreTransaction rebuilds one from storage without
replaying any transition, rejecting persisted rows that break an invariant.
@Date : 19/09/2026
@Update: 20/09/2026
*/
package domain

import (
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/google/uuid"
)

var (
	ErrUnknownTransactionKind   = errors.New("unknown transaction kind")
	ErrUnknownTransactionState  = errors.New("unknown transaction state")
	ErrInvalidExternalKind      = errors.New("external transactions cannot use OPENING kind")
	ErrTransactionTerminal      = errors.New("transaction is already in a terminal state and cannot be changed")
	ErrInvalidTransactionID     = errors.New("transaction id is required")
	ErrInvalidTransactionAmount = errors.New("invalid amount for transaction kind")
	ErrMissingExternalMetadata  = errors.New("external transaction metadata is required")
	ErrMissingReference         = errors.New("reference is required for this transaction kind")
	ErrReferenceNotApplicable   = errors.New("transaction kind does not accept a reference")
	ErrReferenceAlreadyResolved = errors.New("reference was already resolved to another transaction")
	ErrUnresolvedReference      = errors.New("reversal cannot be processed before its reference is resolved")
	ErrMissingFailureCode       = errors.New("failure code is required")
	ErrInvalidTransactionRecord = errors.New("invalid persisted transaction state")
)

// TransactionKind is the operation the provider asked for, plus the internal
// OPENING used when a wallet is created with a positive balance.
type TransactionKind string

const (
	KindOpening  TransactionKind = "OPENING"
	KindBet      TransactionKind = "BET"
	KindWin      TransactionKind = "WIN"
	KindLoss     TransactionKind = "LOSS"
	KindRefund   TransactionKind = "REFUND"
	KindRollback TransactionKind = "ROLLBACK"
)

// IsValid reports whether the kind is one of the supported ones, so that an
// arbitrary string coming from a payload or from a database row is rejected
// instead of becoming an unknown operation.
func (k TransactionKind) IsValid() bool {
	switch k {
	case KindOpening, KindBet, KindWin, KindLoss, KindRefund, KindRollback:
		return true
	default:
		return false
	}
}

// IsExternal reports whether the kind may originate from a provider. OPENING is
// the only internal kind and the schema must keep both origins apart.
func (k TransactionKind) IsExternal() bool {
	return k.IsValid() && k != KindOpening
}

// Direction returns how the kind moves the balance. The second result is false
// for LOSS, which is recorded and acknowledged but produces no ledger entry and
// does not change the wallet version. ROLLBACK is absent as well, because its
// direction is the opposite of the referenced transaction and is only known
// once that reference is resolved.
func (k TransactionKind) Direction() (Direction, bool) {
	switch k {
	case KindBet:
		return DirectionDebit, true
	case KindOpening, KindWin, KindRefund:
		return DirectionCredit, true
	default:
		return "", false
	}
}

// RequiresReference reports whether the kind can only exist as a reversal of an
// earlier transaction, which makes referenceExternalTransactionId mandatory.
func (k TransactionKind) RequiresReference() bool {
	return k == KindRefund || k == KindRollback
}

// AcceptsReference reports whether the kind may carry a reference. WIN may
// point at a bet of the same round without being a reversal of it.
func (k TransactionKind) AcceptsReference() bool {
	return k.RequiresReference() || k == KindWin
}

// requiresPositiveAmount reports whether the kind moves money. LOSS is the only
// kind that requires exactly "0.00"; every other kind requires more than zero,
// including OPENING, since a wallet opened with zero balance creates no
// transaction at all.
func (k TransactionKind) requiresPositiveAmount() bool {
	return k != KindLoss
}

// TransactionState is the state machine of a transaction. PENDING and
// PENDING_REFERENCE are resumable by any instance after an interruption, while
// PROCESSED, REJECTED and FAILED are terminal and are only read back by a
// replay.
type TransactionState string

const (
	// StatePending marks a record that was accepted but not concluded yet.
	StatePending TransactionState = "PENDING"
	// StatePendingRef marks a reversal waiting for a reference that has not
	// arrived, retried by a worker with backoff.
	StatePendingRef TransactionState = "PENDING_REFERENCE"
	// StateProcessed marks a successfully concluded operation.
	StateProcessed TransactionState = "PROCESSED"
	// StateRejected marks an operation refused by a business rule.
	StateRejected TransactionState = "REJECTED"
	// StateFailed marks a permanent infrastructure failure kept for auditing.
	// Transient failures must be retried instead of reaching this state.
	StateFailed TransactionState = "FAILED"
)

// IsValid reports whether the state is one of the supported ones, so that a
// zero value or a corrupted row never transitions as if it were PENDING.
func (s TransactionState) IsValid() bool {
	switch s {
	case StatePending, StatePendingRef, StateProcessed, StateRejected, StateFailed:
		return true
	default:
		return false
	}
}

// IsTerminal reports whether the state accepts no further transition.
func (s TransactionState) IsTerminal() bool {
	return s == StateProcessed || s == StateRejected || s == StateFailed
}

// FailureCode is the stable identifier returned to the provider when an
// operation is rejected or fails, so that a caller can tell a correctable input
// from a definitive result without parsing messages.
type FailureCode string

const (
	// FailureInsufficientFunds is a bet that exceeds the available balance.
	FailureInsufficientFunds FailureCode = "INSUFFICIENT_FUNDS"
	// FailureReversalInsufficientFunds is a reversal that would debit more
	// than the available balance. It is intentionally different from
	// FailureInsufficientFunds, because the two cases are audited apart.
	FailureReversalInsufficientFunds FailureCode = "REVERSAL_INSUFFICIENT_FUNDS"
	// FailureReferenceNotFound is a reversal whose reference never arrived
	// within the retry budget.
	FailureReferenceNotFound FailureCode = "REFERENCE_NOT_FOUND"
	// FailureReferenceNotProcessed is a reversal whose reference exists but
	// did not conclude successfully.
	FailureReferenceNotProcessed FailureCode = "REFERENCE_NOT_PROCESSED"
	// FailureReferenceMismatch is a reversal that disagrees with its
	// reference on provider, player, wallet, currency, round or amount.
	FailureReferenceMismatch FailureCode = "REFERENCE_MISMATCH"
	// FailureAlreadyReversed is a second successful reversal of the same
	// reference, which would return the same debit twice.
	FailureAlreadyReversed FailureCode = "ALREADY_REVERSED"
	// FailureWalletNotFound is an operation over a wallet that does not exist.
	FailureWalletNotFound FailureCode = "WALLET_NOT_FOUND"
	// FailureCurrencyMismatch is an operation whose currency differs from the
	// wallet currency.
	FailureCurrencyMismatch FailureCode = "CURRENCY_MISMATCH"
	// FailureInternalError is a permanent infrastructure failure recorded for
	// auditing after the retries were exhausted.
	FailureInternalError FailureCode = "INTERNAL_ERROR"
)

type WagerTransaction struct {
	// Internal identifiers.
	id       uuid.UUID
	walletID uuid.UUID
	playerID uuid.UUID

	// Provider metadata, empty on an internally originated OPENING.
	providerID            string
	externalTransactionID string
	idempotencyKey        string
	payloadHash           string

	// Game context, empty on an internally originated OPENING.
	roundID string
	gameID  string

	// Reversal reference. referenceExternalID is what the provider sent and
	// referenceTransactionID is the internal transaction it resolved to. Both
	// are stored by value so that no caller can change them afterwards.
	referenceExternalID    string
	referenceTransactionID uuid.UUID

	// Financial core.
	kind  TransactionKind
	money Money

	// State control.
	state       TransactionState
	failureCode FailureCode

	// resultBalance is the wallet balance observed when the operation was
	// processed. A replay answers with it instead of reading the current
	// balance, which may already have moved.
	resultBalance Money

	createdAt time.Time
	updatedAt time.Time
}

// NewOpeningTransaction creates the internal transaction that justifies a
// positive initial balance. It is born PROCESSED because it is written in the
// same commit as the wallet, its ledger entry and its outbox records, and it
// carries none of the provider metadata, which does not apply to this origin.
func NewOpeningTransaction(id, walletID, playerID uuid.UUID, amount Money, now time.Time) (*WagerTransaction, error) {
	if err := validateTransactionIdentity(id, walletID, playerID); err != nil {
		return nil, err
	}
	if err := validateAmountForKind(KindOpening, amount); err != nil {
		return nil, err
	}
	if now.IsZero() {
		return nil, ErrInvalidTimestamp
	}

	moment := now.UTC()

	return &WagerTransaction{
		id:       id,
		walletID: walletID,
		playerID: playerID,
		kind:     KindOpening,
		money:    amount,
		state:    StateProcessed,
		// The wallet was just created with this credit, so the balance
		// returned for this transaction is the amount itself.
		resultBalance: amount,
		createdAt:     moment,
		updatedAt:     moment,
	}, nil
}

// ExternalTransactionParams carries the fields of an operation received from a
// provider. It is a struct instead of a long parameter list because the
// identifiers and the metadata are same typed values that would otherwise be
// easy to swap silently at the call site.
type ExternalTransactionParams struct {
	ID                    uuid.UUID
	WalletID              uuid.UUID
	PlayerID              uuid.UUID
	ProviderID            string
	ExternalTransactionID string
	IdempotencyKey        string
	PayloadHash           string
	RoundID               string
	GameID                string
	// ReferenceExternalID is empty when the operation reverses nothing.
	ReferenceExternalID string
	Kind                TransactionKind
	Money               Money
	Now                 time.Time
}

// NewExternalTransaction creates a transaction from a provider command received
// over HTTP or SQS. It starts in PENDING, so a durable record exists before the
// operation is applied and another instance can resume it after a restart.
func NewExternalTransaction(params ExternalTransactionParams) (*WagerTransaction, error) {
	if !params.Kind.IsValid() {
		return nil, fmt.Errorf("%w: %q", ErrUnknownTransactionKind, params.Kind)
	}
	if params.Kind == KindOpening {
		return nil, ErrInvalidExternalKind
	}
	if err := validateTransactionIdentity(params.ID, params.WalletID, params.PlayerID); err != nil {
		return nil, err
	}
	if err := validateExternalMetadata(params); err != nil {
		return nil, err
	}
	if err := validateAmountForKind(params.Kind, params.Money); err != nil {
		return nil, err
	}
	if err := validateReferenceForKind(params.Kind, params.ReferenceExternalID); err != nil {
		return nil, err
	}
	if params.Now.IsZero() {
		return nil, ErrInvalidTimestamp
	}

	moment := params.Now.UTC()

	return &WagerTransaction{
		id:                    params.ID,
		walletID:              params.WalletID,
		playerID:              params.PlayerID,
		providerID:            params.ProviderID,
		externalTransactionID: params.ExternalTransactionID,
		idempotencyKey:        params.IdempotencyKey,
		payloadHash:           params.PayloadHash,
		roundID:               params.RoundID,
		gameID:                params.GameID,
		referenceExternalID:   params.ReferenceExternalID,
		kind:                  params.Kind,
		money:                 params.Money,
		state:                 StatePending,
		createdAt:             moment,
		updatedAt:             moment,
	}, nil
}

// RestoredTransactionParams carries the persisted columns of a transaction.
// Optional columns use their zero value to mean absent: an empty string, a nil
// UUID or an uninitialized Money.
type RestoredTransactionParams struct {
	ID                     uuid.UUID
	WalletID               uuid.UUID
	PlayerID               uuid.UUID
	ProviderID             string
	ExternalTransactionID  string
	IdempotencyKey         string
	PayloadHash            string
	RoundID                string
	GameID                 string
	ReferenceExternalID    string
	ReferenceTransactionID uuid.UUID
	Kind                   TransactionKind
	Money                  Money
	State                  TransactionState
	FailureCode            FailureCode
	ResultBalance          Money
	CreatedAt              time.Time
	UpdatedAt              time.Time
}

// RestoreTransaction rebuilds a transaction from its persisted state. It
// replays no transition and emits no event, it only refuses a row that breaks
// an invariant, so corrupted data never reaches the business rules.
func RestoreTransaction(params RestoredTransactionParams) (*WagerTransaction, error) {
	if !params.Kind.IsValid() {
		return nil, fmt.Errorf("%w: %q", ErrUnknownTransactionKind, params.Kind)
	}
	if !params.State.IsValid() {
		return nil, fmt.Errorf("%w: %q", ErrUnknownTransactionState, params.State)
	}
	if err := validateTransactionIdentity(params.ID, params.WalletID, params.PlayerID); err != nil {
		return nil, err
	}
	if err := validateRestoredOrigin(params); err != nil {
		return nil, err
	}
	if err := validateAmountForKind(params.Kind, params.Money); err != nil {
		return nil, err
	}
	if err := validateReferenceForKind(params.Kind, params.ReferenceExternalID); err != nil {
		return nil, err
	}
	if err := validateRestoredOutcome(params); err != nil {
		return nil, err
	}
	if params.CreatedAt.IsZero() || params.UpdatedAt.IsZero() {
		return nil, ErrInvalidTimestamp
	}
	if params.UpdatedAt.Before(params.CreatedAt) {
		return nil, fmt.Errorf("%w: updatedAt is before createdAt", ErrInvalidTransactionRecord)
	}

	return &WagerTransaction{
		id:                     params.ID,
		walletID:               params.WalletID,
		playerID:               params.PlayerID,
		providerID:             params.ProviderID,
		externalTransactionID:  params.ExternalTransactionID,
		idempotencyKey:         params.IdempotencyKey,
		payloadHash:            params.PayloadHash,
		roundID:                params.RoundID,
		gameID:                 params.GameID,
		referenceExternalID:    params.ReferenceExternalID,
		referenceTransactionID: params.ReferenceTransactionID,
		kind:                   params.Kind,
		money:                  params.Money,
		state:                  params.State,
		failureCode:            params.FailureCode,
		resultBalance:          params.ResultBalance,
		createdAt:              params.CreatedAt.UTC(),
		updatedAt:              params.UpdatedAt.UTC(),
	}, nil
}

// validateTransactionIdentity rejects missing internal identifiers.
func validateTransactionIdentity(id, walletID, playerID uuid.UUID) error {
	if id == uuid.Nil {
		return ErrInvalidTransactionID
	}
	if walletID == uuid.Nil {
		return ErrInvalidWalletID
	}
	if playerID == uuid.Nil {
		return ErrInvalidPlayerID
	}
	return nil
}

// validateExternalMetadata requires every field a provider operation is
// identified and audited by. The idempotency key and the payload hash are
// mandatory because without them a repeated delivery cannot be recognised.
func validateExternalMetadata(params ExternalTransactionParams) error {
	fields := []struct {
		name  string
		value string
	}{
		{"providerId", params.ProviderID},
		{"externalTransactionId", params.ExternalTransactionID},
		{"idempotencyKey", params.IdempotencyKey},
		{"payloadHash", params.PayloadHash},
		{"roundId", params.RoundID},
		{"gameId", params.GameID},
	}

	for _, field := range fields {
		if strings.TrimSpace(field.value) == "" {
			return fmt.Errorf("%w: %s is empty", ErrMissingExternalMetadata, field.name)
		}
	}
	return nil
}

// validateRestoredOrigin keeps both origins apart, the same way the schema
// does: an external row carries the provider metadata and an internal OPENING
// carries none of it.
func validateRestoredOrigin(params RestoredTransactionParams) error {
	if params.Kind.IsExternal() {
		return validateExternalMetadata(ExternalTransactionParams{
			ProviderID:            params.ProviderID,
			ExternalTransactionID: params.ExternalTransactionID,
			IdempotencyKey:        params.IdempotencyKey,
			PayloadHash:           params.PayloadHash,
			RoundID:               params.RoundID,
			GameID:                params.GameID,
		})
	}

	internal := params.ProviderID == "" &&
		params.ExternalTransactionID == "" &&
		params.IdempotencyKey == "" &&
		params.PayloadHash == "" &&
		params.RoundID == "" &&
		params.GameID == ""

	if !internal {
		return fmt.Errorf("%w: OPENING cannot carry external metadata", ErrInvalidTransactionRecord)
	}
	return nil
}

// validateRestoredOutcome checks the columns that only exist once a transaction
// reaches a terminal state, so that a row cannot claim a result it never
// produced or hide the one it did.
func validateRestoredOutcome(params RestoredTransactionParams) error {
	if params.ReferenceTransactionID != uuid.Nil {
		if !params.Kind.AcceptsReference() {
			return fmt.Errorf("%w: %s cannot carry a resolved reference", ErrInvalidTransactionRecord, params.Kind)
		}
		if params.ReferenceExternalID == "" {
			return fmt.Errorf("%w: resolved reference without an external reference", ErrInvalidTransactionRecord)
		}
		if params.ReferenceTransactionID == params.ID {
			return fmt.Errorf("%w: transaction references itself", ErrInvalidTransactionRecord)
		}
	}

	switch {
	case params.State == StateRejected || params.State == StateFailed:
		if params.FailureCode == "" {
			return fmt.Errorf("%w: %s requires a failure code", ErrInvalidTransactionRecord, params.State)
		}
	case params.FailureCode != "":
		return fmt.Errorf("%w: %s cannot carry a failure code", ErrInvalidTransactionRecord, params.State)
	}

	if params.State != StateProcessed {
		if err := params.ResultBalance.validate(); err == nil {
			return fmt.Errorf("%w: %s cannot carry a result balance", ErrInvalidTransactionRecord, params.State)
		}
		return nil
	}

	if err := params.ResultBalance.validate(); err != nil {
		return fmt.Errorf("%w: PROCESSED requires a result balance", ErrInvalidTransactionRecord)
	}
	if params.ResultBalance.Currency() != params.Money.Currency() {
		return ErrCurrencyMismatch
	}
	if params.ResultBalance.IsNegative() {
		return fmt.Errorf("%w: result balance is negative", ErrInvalidTransactionRecord)
	}
	if params.Kind.RequiresReference() && params.ReferenceTransactionID == uuid.Nil {
		return fmt.Errorf("%w: %w", ErrInvalidTransactionRecord, ErrUnresolvedReference)
	}
	return nil
}

// validateAmountForKind applies the zero policy of each kind: LOSS is recorded
// with exactly "0.00" and moves nothing, while every other kind requires an
// amount greater than zero.
func validateAmountForKind(kind TransactionKind, amount Money) error {
	if err := amount.validate(); err != nil {
		return err
	}
	if amount.IsNegative() {
		return fmt.Errorf("%w: %s cannot be negative", ErrInvalidTransactionAmount, kind)
	}
	if kind.requiresPositiveAmount() && !amount.IsPositive() {
		return fmt.Errorf("%w: %s requires an amount greater than zero", ErrInvalidTransactionAmount, kind)
	}
	if !kind.requiresPositiveAmount() && !amount.IsZero() {
		return fmt.Errorf("%w: %s requires an amount equal to zero", ErrInvalidTransactionAmount, kind)
	}
	return nil
}

// validateReferenceForKind requires a reference on the reversals and refuses
// one on the kinds that cannot reverse anything.
func validateReferenceForKind(kind TransactionKind, referenceExternalID string) error {
	present := strings.TrimSpace(referenceExternalID) != ""

	if kind.RequiresReference() && !present {
		return fmt.Errorf("%w: %s", ErrMissingReference, kind)
	}
	if !kind.AcceptsReference() && present {
		return fmt.Errorf("%w: %s", ErrReferenceNotApplicable, kind)
	}
	return nil
}

// --- STATE MACHINE ---

// IsTerminal reports whether the transaction already concluded. A terminal
// transaction is never reapplied, a replay only reads its stored result.
func (t *WagerTransaction) IsTerminal() bool {
	return t.state.IsTerminal()
}

// IsExternal reports whether the transaction came from a provider.
func (t *WagerTransaction) IsExternal() bool {
	return t.kind.IsExternal()
}

// ensureTransition guards every transition. It refuses an unknown state, which
// is what an uninitialized value would carry, a state that already concluded
// and a missing instant.
func (t *WagerTransaction) ensureTransition(now time.Time) error {
	if !t.state.IsValid() {
		return fmt.Errorf("%w: %q", ErrUnknownTransactionState, t.state)
	}
	if t.state.IsTerminal() {
		return ErrTransactionTerminal
	}
	if now.IsZero() {
		return ErrInvalidTimestamp
	}
	return nil
}

// touch records the instant of a transition. The instant never moves backwards,
// so clock skew between instances cannot place updatedAt before a previous
// transition or before the creation.
func (t *WagerTransaction) touch(now time.Time) {
	moment := now.UTC()
	if moment.Before(t.updatedAt) {
		moment = t.updatedAt
	}
	t.updatedAt = moment
}

// MarkAsProcessed concludes the operation successfully and stores the wallet
// balance returned to the provider, which a later replay answers with. A
// reversal cannot be processed before its reference is resolved, so that the
// amount it moves is always tied to a known transaction.
func (t *WagerTransaction) MarkAsProcessed(resultBalance Money, now time.Time) error {
	if err := t.ensureTransition(now); err != nil {
		return err
	}
	if err := resultBalance.validate(); err != nil {
		return err
	}
	if resultBalance.Currency() != t.money.Currency() {
		return ErrCurrencyMismatch
	}
	if resultBalance.IsNegative() {
		return fmt.Errorf("%w: result balance is negative", ErrInvalidTransactionAmount)
	}
	if t.kind.RequiresReference() && t.referenceTransactionID == uuid.Nil {
		return ErrUnresolvedReference
	}

	t.state = StateProcessed
	t.resultBalance = resultBalance
	t.touch(now)
	return nil
}

// Reject refuses the operation by a business rule, such as a bet without funds
// or a reference that never arrived. The failure code is what the provider
// reads to tell a correctable input from a definitive result.
func (t *WagerTransaction) Reject(failureCode FailureCode, now time.Time) error {
	if err := t.ensureTransition(now); err != nil {
		return err
	}
	if strings.TrimSpace(string(failureCode)) == "" {
		return ErrMissingFailureCode
	}

	t.state = StateRejected
	t.failureCode = failureCode
	t.touch(now)
	return nil
}

// Fail records a permanent infrastructure failure for auditing. A transient
// failure must be retried instead, since this state accepts no new attempt.
func (t *WagerTransaction) Fail(failureCode FailureCode, now time.Time) error {
	if err := t.ensureTransition(now); err != nil {
		return err
	}
	if strings.TrimSpace(string(failureCode)) == "" {
		return ErrMissingFailureCode
	}

	t.state = StateFailed
	t.failureCode = failureCode
	t.touch(now)
	return nil
}

// MarkAsPendingReference parks a reversal whose reference has not arrived yet.
// The record is durable, so the reference worker retries it with backoff even
// after a restart of every process.
func (t *WagerTransaction) MarkAsPendingReference(now time.Time) error {
	if err := t.ensureTransition(now); err != nil {
		return err
	}
	if !t.kind.AcceptsReference() {
		return fmt.Errorf("%w: %s", ErrReferenceNotApplicable, t.kind)
	}
	if t.referenceExternalID == "" {
		return fmt.Errorf("%w: %s", ErrMissingReference, t.kind)
	}

	t.state = StatePendingRef
	t.touch(now)
	return nil
}

// ResolveReference records the internal transaction that the external reference
// resolved to. It does not conclude the operation, the caller still applies it
// and then marks it as processed or rejected. Resolving again to the same
// transaction is accepted, so a retried worker is harmless, but pointing an
// already resolved reversal at a different transaction is refused.
func (t *WagerTransaction) ResolveReference(referenceID uuid.UUID, now time.Time) error {
	if err := t.ensureTransition(now); err != nil {
		return err
	}
	if !t.kind.AcceptsReference() {
		return fmt.Errorf("%w: %s", ErrReferenceNotApplicable, t.kind)
	}
	if t.referenceExternalID == "" {
		return fmt.Errorf("%w: %s", ErrMissingReference, t.kind)
	}
	if referenceID == uuid.Nil || referenceID == t.id {
		return ErrInvalidTransactionID
	}
	if t.referenceTransactionID != uuid.Nil && t.referenceTransactionID != referenceID {
		return ErrReferenceAlreadyResolved
	}

	t.referenceTransactionID = referenceID
	t.touch(now)
	return nil
}

// --- ACCESSORS ---

// ID returns the internal transaction identifier.
func (t *WagerTransaction) ID() uuid.UUID { return t.id }

// WalletID returns the wallet the operation moves.
func (t *WagerTransaction) WalletID() uuid.UUID { return t.walletID }

// PlayerID returns the player that owns the wallet.
func (t *WagerTransaction) PlayerID() uuid.UUID { return t.playerID }

// ProviderID returns the provider that sent the operation, empty on OPENING.
func (t *WagerTransaction) ProviderID() string { return t.providerID }

// ExternalTransactionID returns the identifier given by the provider, empty on
// OPENING. Together with the provider it identifies the operation.
func (t *WagerTransaction) ExternalTransactionID() string { return t.externalTransactionID }

// IdempotencyKey returns the key received from the caller, empty on OPENING.
func (t *WagerTransaction) IdempotencyKey() string { return t.idempotencyKey }

// PayloadHash returns the deterministic hash of the business fields, used to
// tell a replay from a reused key with a different content.
func (t *WagerTransaction) PayloadHash() string { return t.payloadHash }

// RoundID returns the round of the operation, empty on OPENING.
func (t *WagerTransaction) RoundID() string { return t.roundID }

// GameID returns the game of the operation, empty on OPENING.
func (t *WagerTransaction) GameID() string { return t.gameID }

// ReferenceExternalID returns the external reference of a reversal. The second
// result is false when the operation reverses nothing.
func (t *WagerTransaction) ReferenceExternalID() (string, bool) {
	return t.referenceExternalID, t.referenceExternalID != ""
}

// ReferenceTransactionID returns the internal transaction the reference
// resolved to. The second result is false while it is still unresolved.
func (t *WagerTransaction) ReferenceTransactionID() (uuid.UUID, bool) {
	return t.referenceTransactionID, t.referenceTransactionID != uuid.Nil
}

// Kind returns the operation type.
func (t *WagerTransaction) Kind() TransactionKind { return t.kind }

// Money returns the amount of the operation.
func (t *WagerTransaction) Money() Money { return t.money }

// State returns the current state of the transaction.
func (t *WagerTransaction) State() TransactionState { return t.state }

// FailureCode returns the code of a rejection or failure. The second result is
// false while the transaction has not been refused.
func (t *WagerTransaction) FailureCode() (FailureCode, bool) {
	return t.failureCode, t.failureCode != ""
}

// ResultBalance returns the balance observed when the operation was processed,
// which a replay answers with. The second result is false while the transaction
// has not been processed.
func (t *WagerTransaction) ResultBalance() (Money, bool) {
	return t.resultBalance, t.resultBalance.validate() == nil
}

// CreatedAt returns the creation instant in UTC.
func (t *WagerTransaction) CreatedAt() time.Time { return t.createdAt }

// UpdatedAt returns the instant of the last transition in UTC.
func (t *WagerTransaction) UpdatedAt() time.Time { return t.updatedAt }
