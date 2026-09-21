/*
@Author: Franco Ribeiro Borba
@Description: Inbound message contract. A provider operation that arrives over
SQS is not one of the events this service publishes: it is a command asking for
something to happen, and it has its own shape, with messageId, type, occurredAt
and a data object carrying the operation. The two contracts are kept apart on
purpose, because an outbound Envelope states a fact that already happened while
an inbound message states an intention that may still be refused. ParseInbound
validates the shape at the edge and computes the payload hash over the raw data
object, using the same canonical JSON as the HTTP entry point, so the very same
operation sent through either transport produces the same hash and is recognised
as a replay instead of a second movement. The messageId is the durable identity
of the message for the inbox, and the hash is what tells a redelivery of the
same message from a different content reusing an id.
@Date : 20/09/2026
@Update: -
*/
package events

import (
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/FrancoBorba/distributed-wallet-api/cmd/internal/domain"
	"github.com/FrancoBorba/distributed-wallet-api/cmd/internal/pkg/idempotency"
	"github.com/google/uuid"
)

// TypeWagerTransactionRequested is the only inbound message type the consumer
// accepts. Anything else is a contract error and never a transient failure.
const TypeWagerTransactionRequested = "WagerTransactionRequested"

var (
	ErrInvalidMessage        = errors.New("message is not valid JSON")
	ErrMissingMessageID      = errors.New("messageId is required")
	ErrUnknownMessageType    = errors.New("unknown message type")
	ErrMissingMessageData    = errors.New("data is required")
	ErrMissingProviderID     = errors.New("providerId is required")
	ErrMissingExternalID     = errors.New("externalTransactionId is required")
	ErrMissingIdempotencyKey = errors.New("idempotencyKey is required")
	ErrMissingRoundID        = errors.New("roundId is required")
	ErrMissingGameID         = errors.New("gameId is required")
)

// WagerTransactionRequested is the operation a provider asks for. The field
// names mirror the published contract, and money arrives as the same decimal
// string object used everywhere else, so no scale is ever inferred.
type WagerTransactionRequested struct {
	ProviderID                     string                 `json:"providerId"`
	ExternalTransactionID          string                 `json:"externalTransactionId"`
	IdempotencyKey                 string                 `json:"idempotencyKey"`
	PlayerID                       uuid.UUID              `json:"playerId"`
	WalletID                       uuid.UUID              `json:"walletId"`
	RoundID                        string                 `json:"roundId"`
	GameID                         string                 `json:"gameId"`
	Kind                           domain.TransactionKind `json:"kind"`
	Money                          domain.Money           `json:"money"`
	ReferenceExternalTransactionID string                 `json:"referenceExternalTransactionId,omitempty"`
	// CorrelationID is optional. A provider that already traces its own calls
	// may send it, and the whole flow is then tied to the identifier it knows.
	CorrelationID *uuid.UUID `json:"correlationId,omitempty"`
}

// inboundJSON is the wire shape of a message, kept apart from the validated
// value so that the raw data object survives for the hash.
type inboundJSON struct {
	MessageID  string          `json:"messageId"`
	Type       string          `json:"type"`
	OccurredAt time.Time       `json:"occurredAt"`
	Data       json.RawMessage `json:"data"`
}

// InboundMessage is a validated provider command. Its fields are unexported
// because the payload hash is derived from the exact bytes that were received:
// letting a caller change the operation after parsing would leave the hash
// describing a message that no longer exists.
type InboundMessage struct {
	messageID   string
	messageType string
	occurredAt  time.Time
	data        WagerTransactionRequested
	payloadHash string
}

// ParseInbound validates a message body and computes its payload hash. It
// rejects a malformed or unknown message instead of retrying it, because a
// contract error will never succeed on a redelivery, and refuses OPENING, which
// is reserved for the internal opening of a wallet.
func ParseInbound(body []byte) (InboundMessage, error) {
	var wire inboundJSON
	if err := json.Unmarshal(body, &wire); err != nil {
		return InboundMessage{}, fmt.Errorf("%w: %v", ErrInvalidMessage, err)
	}

	if strings.TrimSpace(wire.MessageID) == "" {
		return InboundMessage{}, ErrMissingMessageID
	}
	if wire.Type != TypeWagerTransactionRequested {
		return InboundMessage{}, fmt.Errorf("%w: %q", ErrUnknownMessageType, wire.Type)
	}
	if wire.OccurredAt.IsZero() {
		return InboundMessage{}, ErrInvalidOccurredAt
	}
	if len(wire.Data) == 0 {
		return InboundMessage{}, ErrMissingMessageData
	}

	var data WagerTransactionRequested
	if err := json.Unmarshal(wire.Data, &data); err != nil {
		return InboundMessage{}, fmt.Errorf("%w: %v", ErrInvalidMessage, err)
	}
	if err := data.validate(); err != nil {
		return InboundMessage{}, err
	}

	// The hash covers the data object alone, which is exactly what the HTTP
	// body carries. The canonical form then drops idempotencyKey and the
	// transport metadata, so both transports agree on the same hash.
	payloadHash, err := idempotency.CanonicalHash(wire.Data)
	if err != nil {
		return InboundMessage{}, err
	}

	return InboundMessage{
		messageID:   wire.MessageID,
		messageType: wire.Type,
		occurredAt:  wire.OccurredAt.UTC(),
		data:        data,
		payloadHash: payloadHash,
	}, nil
}

// validate checks the shape of the operation: the fields the contract requires
// are present and the kind is one a provider may send. The business rules of
// each kind stay in the domain, which validates them again when the transaction
// is created.
func (d WagerTransactionRequested) validate() error {
	if strings.TrimSpace(d.ProviderID) == "" {
		return ErrMissingProviderID
	}
	if strings.TrimSpace(d.ExternalTransactionID) == "" {
		return ErrMissingExternalID
	}
	if strings.TrimSpace(d.IdempotencyKey) == "" {
		return ErrMissingIdempotencyKey
	}
	if d.PlayerID == uuid.Nil {
		return domain.ErrInvalidPlayerID
	}
	if d.WalletID == uuid.Nil {
		return domain.ErrInvalidWalletID
	}
	if strings.TrimSpace(d.RoundID) == "" {
		return ErrMissingRoundID
	}
	if strings.TrimSpace(d.GameID) == "" {
		return ErrMissingGameID
	}
	if !d.Kind.IsValid() {
		return fmt.Errorf("%w: %q", domain.ErrUnknownTransactionKind, d.Kind)
	}

	// OPENING is internal. A provider that sends it is refused by the contract
	// itself, not by a business rule deeper in the flow.
	if !d.Kind.IsExternal() {
		return domain.ErrInvalidExternalKind
	}
	if err := requireInitialized(d.Money); err != nil {
		return err
	}
	if d.Kind.RequiresReference() && strings.TrimSpace(d.ReferenceExternalTransactionID) == "" {
		return fmt.Errorf("%w: %s", domain.ErrMissingReference, d.Kind)
	}
	if !d.Kind.AcceptsReference() && d.ReferenceExternalTransactionID != "" {
		return fmt.Errorf("%w: %s", domain.ErrReferenceNotApplicable, d.Kind)
	}

	return nil
}

// Metadata returns the tracing context of the message. The message itself is
// what caused everything the service will publish about this operation, so its
// identity becomes the causation of those events. When the provider sent no
// correlation, a new one is started here and carried from end to end.
func (m InboundMessage) Metadata(causationID uuid.UUID) Metadata {
	correlationID := uuid.New()
	if m.data.CorrelationID != nil && *m.data.CorrelationID != uuid.Nil {
		correlationID = *m.data.CorrelationID
	}

	return Metadata{CorrelationID: correlationID, CausationID: causationID}
}

// --- ACCESSORS ---

// MessageID returns the durable identity of the message, which is what the
// inbox stores to recognise a redelivery.
func (m InboundMessage) MessageID() string { return m.messageID }

// Type returns the message type, always TypeWagerTransactionRequested.
func (m InboundMessage) Type() string { return m.messageType }

// OccurredAt returns the instant the provider stamped on the message, in UTC.
func (m InboundMessage) OccurredAt() time.Time { return m.occurredAt }

// Data returns the requested operation.
func (m InboundMessage) Data() WagerTransactionRequested { return m.data }

// PayloadHash returns the canonical hash of the business fields. The same
// operation over HTTP produces this same value.
func (m InboundMessage) PayloadHash() string { return m.payloadHash }
