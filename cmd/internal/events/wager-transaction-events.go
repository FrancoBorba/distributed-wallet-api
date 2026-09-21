/*
@Author: Franco Ribeiro Borba
@Description: Integration events of a WagerTransaction. Three facts about an
operation are published: WagerTransactionProcessed when it concludes
successfully, including LOSS, which moves no money; WagerTransactionRejected
when a business rule refuses it for good, always with a stable failure code; and
WagerTransactionPendingReference when a reversal is stored waiting for a
reference that has not arrived yet. Every constructor reads the aggregate
instead of taking loose fields, and refuses to build an event whose state does
not match the fact it announces, so a transaction that is still PENDING can
never be announced as processed. The provider metadata is omitted when it does
not apply, which is the case of the internal OPENING that justifies a wallet
created with a positive balance.
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

// Schema versions, set by the constructors. Each event evolves on its own, so
// a breaking change to one payload does not renumber the others.
const (
	versionWagerTransactionProcessed        = 1
	versionWagerTransactionRejected         = 1
	versionWagerTransactionPendingReference = 1
)

var (
	ErrNilTransaction       = errors.New("transaction is nil")
	ErrUnexpectedState      = errors.New("transaction state does not match the event")
	ErrMissingResultBalance = errors.New("processed transaction has no result balance")
)

// TransactionSubject is the identity block shared by the three events. It is
// embedded, so its fields are published at the top level of data instead of
// nested under a key that consumers would have to know about.
//
// The provider metadata and the game context are omitted when empty, which
// happens only on the internal OPENING, an origin that has no provider, no
// external identifier, no round and no game.
type TransactionSubject struct {
	TransactionID         uuid.UUID              `json:"transactionId"`
	WalletID              uuid.UUID              `json:"walletId"`
	PlayerID              uuid.UUID              `json:"playerId"`
	ProviderID            string                 `json:"providerId,omitempty"`
	ExternalTransactionID string                 `json:"externalTransactionId,omitempty"`
	RoundID               string                 `json:"roundId,omitempty"`
	GameID                string                 `json:"gameId,omitempty"`
	Kind                  domain.TransactionKind `json:"kind"`
	Money                 domain.Money           `json:"money"`
}

// WagerTransactionProcessed reports a successfully concluded operation. It
// carries the balance observed when the operation was applied, which is the
// same value an idempotent replay answers with, and the internal transaction a
// reversal resolved to, when there is one.
type WagerTransactionProcessed struct {
	TransactionSubject
	ResultBalance          domain.Money `json:"resultBalance"`
	ReferenceTransactionID *uuid.UUID   `json:"referenceTransactionId,omitempty"`
}

// WagerTransactionRejected reports a definitive refusal by a business rule. The
// failure code is stable and documented, so a provider can tell a correctable
// input from a final answer without parsing a message.
type WagerTransactionRejected struct {
	TransactionSubject
	FailureCode                    domain.FailureCode `json:"failureCode"`
	ReferenceExternalTransactionID string             `json:"referenceExternalTransactionId,omitempty"`
}

// WagerTransactionPendingReference reports that a reversal was accepted and
// stored, but cannot be applied until the transaction it reverses arrives. It
// is the durable trace of a wait that a worker retries with backoff, so the
// provider learns the operation was not lost nor refused.
type WagerTransactionPendingReference struct {
	TransactionSubject
	ReferenceExternalTransactionID string `json:"referenceExternalTransactionId"`
}

// NewWagerTransactionProcessed builds the event of a concluded operation. It
// requires the transaction to be in PROCESSED, because the event states a fact
// that is only true once the aggregate reached that state in the transaction
// that is being committed.
func NewWagerTransactionProcessed(transaction *domain.WagerTransaction, meta Metadata) (Envelope, error) {
	subject, err := newSubject(transaction, domain.StateProcessed)
	if err != nil {
		return Envelope{}, err
	}

	resultBalance, ok := transaction.ResultBalance()
	if !ok {
		return Envelope{}, fmt.Errorf("%w: %s", ErrMissingResultBalance, transaction.ID())
	}

	data := WagerTransactionProcessed{
		TransactionSubject: subject,
		ResultBalance:      resultBalance,
	}

	// Only a reversal resolves a reference, and it is published so that the
	// consumer can link the reversal to what it undid.
	if referenceID, resolved := transaction.ReferenceTransactionID(); resolved {
		data.ReferenceTransactionID = &referenceID
	}

	return newEnvelope(
		TypeWagerTransactionProcessed,
		versionWagerTransactionProcessed,
		transaction.ID(),
		transaction.UpdatedAt(),
		meta,
		data,
	)
}

// NewWagerTransactionRejected builds the event of a definitive refusal. It
// requires the transaction to be in REJECTED and to carry a failure code, since
// a rejection without a documented reason cannot be acted upon by the provider.
func NewWagerTransactionRejected(transaction *domain.WagerTransaction, meta Metadata) (Envelope, error) {
	subject, err := newSubject(transaction, domain.StateRejected)
	if err != nil {
		return Envelope{}, err
	}

	failureCode, ok := transaction.FailureCode()
	if !ok {
		return Envelope{}, domain.ErrMissingFailureCode
	}

	referenceExternalID, _ := transaction.ReferenceExternalID()

	data := WagerTransactionRejected{
		TransactionSubject:             subject,
		FailureCode:                    failureCode,
		ReferenceExternalTransactionID: referenceExternalID,
	}

	return newEnvelope(
		TypeWagerTransactionRejected,
		versionWagerTransactionRejected,
		transaction.ID(),
		transaction.UpdatedAt(),
		meta,
		data,
	)
}

// NewWagerTransactionPendingReference builds the event of a reversal waiting
// for its reference. It requires the transaction to be in PENDING_REFERENCE and
// to carry the external reference it is waiting for, which is what the worker
// will keep trying to resolve.
func NewWagerTransactionPendingReference(transaction *domain.WagerTransaction, meta Metadata) (Envelope, error) {
	subject, err := newSubject(transaction, domain.StatePendingRef)
	if err != nil {
		return Envelope{}, err
	}

	referenceExternalID, ok := transaction.ReferenceExternalID()
	if !ok {
		return Envelope{}, domain.ErrMissingReference
	}

	data := WagerTransactionPendingReference{
		TransactionSubject:             subject,
		ReferenceExternalTransactionID: referenceExternalID,
	}

	return newEnvelope(
		TypeWagerTransactionPendingReference,
		versionWagerTransactionPendingReference,
		transaction.ID(),
		transaction.UpdatedAt(),
		meta,
		data,
	)
}

// newSubject copies the identity of a transaction into the block the three
// events share, after checking that the aggregate is in the state the event
// announces. The state check is what keeps a caller from publishing a
// conclusion that was never committed.
func newSubject(transaction *domain.WagerTransaction, expected domain.TransactionState) (TransactionSubject, error) {
	if transaction == nil {
		return TransactionSubject{}, ErrNilTransaction
	}
	if transaction.State() != expected {
		return TransactionSubject{}, fmt.Errorf(
			"%w: %s is in %s, expected %s",
			ErrUnexpectedState, transaction.ID(), transaction.State(), expected,
		)
	}
	if transaction.ID() == uuid.Nil {
		return TransactionSubject{}, domain.ErrInvalidTransactionID
	}
	if transaction.WalletID() == uuid.Nil {
		return TransactionSubject{}, domain.ErrInvalidWalletID
	}
	if transaction.PlayerID() == uuid.Nil {
		return TransactionSubject{}, domain.ErrInvalidPlayerID
	}

	// LOSS carries "0.00", so the amount is not required to be positive here.
	// It must still be a value a domain constructor produced, otherwise it
	// would fail to serialize during the publication.
	if err := requireInitialized(transaction.Money()); err != nil {
		return TransactionSubject{}, err
	}

	return TransactionSubject{
		TransactionID:         transaction.ID(),
		WalletID:              transaction.WalletID(),
		PlayerID:              transaction.PlayerID(),
		ProviderID:            transaction.ProviderID(),
		ExternalTransactionID: transaction.ExternalTransactionID(),
		RoundID:               transaction.RoundID(),
		GameID:                transaction.GameID(),
		Kind:                  transaction.Kind(),
		Money:                 transaction.Money(),
	}, nil
}
