/*
@Author: Franco Ribeiro Borba
@Description: Integration event envelope. Every event this service publishes
travels inside the same envelope, which carries the identity of the event
(eventId), what it is (eventType and version), what it happened to
(aggregateType and aggregateId), how it relates to the work that produced it
(correlationId and the optional causationId), when it happened (occurredAt in
RFC 3339 UTC) and the typed payload (data). The envelope is immutable: its
fields are unexported and are only set by the constructor of each event, which
is what fixes the type and the version of the schema, so no caller can publish
an event that claims to be something other than what it carries. An envelope is
built inside the same SQL transaction as the state that justifies it, stored in
the outbox and only then published, which is why RestoreEnvelope rebuilds one
from its outbox row keeping the original eventId: a republication after a crash
must be recognised as the same event by the consumer.
@Date : 20/09/2026
@Update: -
*/
package events

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"
)

// timeFormat is the rendering of every instant in an event. RFC 3339 in UTC is
// what the contract requires, and the nanosecond variant keeps the precision
// PostgreSQL gives back, so an envelope restored from the outbox serializes to
// the same bytes that were published before a crash.
const timeFormat = time.RFC3339Nano

var (
	ErrUnknownEventType      = errors.New("unknown event type")
	ErrUnknownAggregateType  = errors.New("unknown aggregate type")
	ErrAggregateTypeMismatch = errors.New("aggregate type does not match the event type")
	ErrInvalidEventID        = errors.New("event id is required")
	ErrInvalidEventVersion   = errors.New("event version must be greater than zero")
	ErrInvalidAggregateID    = errors.New("aggregate id is required")
	ErrInvalidCorrelationID  = errors.New("correlation id is required")
	ErrInvalidOccurredAt     = errors.New("occurredAt is required")
	ErrMissingEventData      = errors.New("event data is required")
	ErrInvalidEventData      = errors.New("event data must be a JSON object")
)

// EventType is the name of an integration event. The four values below are the
// ones the outbox accepts, so a typo here is rejected by the database as well.
type EventType string

const (
	// TypeWagerTransactionProcessed reports the successful conclusion of an
	// operation, including LOSS, which moves no money.
	TypeWagerTransactionProcessed EventType = "WagerTransactionProcessed"
	// TypeWagerTransactionRejected reports a definitive refusal by a business
	// rule, always with a stable failure code.
	TypeWagerTransactionRejected EventType = "WagerTransactionRejected"
	// TypeWagerTransactionPendingReference reports that a reversal was stored
	// waiting for a reference that has not arrived yet.
	TypeWagerTransactionPendingReference EventType = "WagerTransactionPendingReference"
	// TypeWalletBalanceChanged reports an effective change of a balance.
	TypeWalletBalanceChanged EventType = "WalletBalanceChanged"
)

// IsValid reports whether the type is one of the published events, so that a
// string coming from an outbox row is rejected instead of being republished as
// an event nobody can consume.
func (t EventType) IsValid() bool {
	switch t {
	case TypeWagerTransactionProcessed,
		TypeWagerTransactionRejected,
		TypeWagerTransactionPendingReference,
		TypeWalletBalanceChanged:
		return true
	default:
		return false
	}
}

// AggregateType is the kind of entity an event happened to. It is what lets a
// consumer route messages and a human audit the outbox by subject.
type AggregateType string

const (
	AggregateWallet           AggregateType = "Wallet"
	AggregateWagerTransaction AggregateType = "WagerTransaction"
)

// IsValid reports whether the aggregate type is one of the supported ones.
func (a AggregateType) IsValid() bool {
	switch a {
	case AggregateWallet, AggregateWagerTransaction:
		return true
	default:
		return false
	}
}

// aggregateFor returns the aggregate an event type always belongs to. The pair
// is a property of the event and never a choice of the caller, so the publisher
// and the consumer cannot disagree about what a WalletBalanceChanged is about.
func aggregateFor(eventType EventType) (AggregateType, error) {
	switch eventType {
	case TypeWalletBalanceChanged:
		return AggregateWallet, nil

	case TypeWagerTransactionProcessed,
		TypeWagerTransactionRejected,
		TypeWagerTransactionPendingReference:
		return AggregateWagerTransaction, nil

	default:
		return "", fmt.Errorf("%w: %q", ErrUnknownEventType, eventType)
	}
}

// Metadata is the tracing context of an event. CorrelationID identifies the
// business operation as a whole, with the same value across every event, log
// line and message it produces. CausationID is optional and points at the
// message or event that directly caused this one, which is what turns a
// correlation group into a chain.
type Metadata struct {
	CorrelationID uuid.UUID
	CausationID   uuid.UUID
}

// NewMetadata builds the tracing context of an operation that starts here,
// caused by nothing this service knows about.
func NewMetadata(correlationID uuid.UUID) Metadata {
	return Metadata{CorrelationID: correlationID}
}

// CausedBy returns a copy of the metadata pointing at the message or event
// that caused the next one, keeping the same correlation.
func (m Metadata) CausedBy(causationID uuid.UUID) Metadata {
	m.CausationID = causationID

	return m
}

// validate rejects metadata without a correlation, since an event that cannot
// be tied back to its operation is useless for auditing a distributed flow.
func (m Metadata) validate() error {
	if m.CorrelationID == uuid.Nil {
		return ErrInvalidCorrelationID
	}

	return nil
}

// Envelope is an integration event ready to be stored in the outbox and
// published. Every field is unexported, so an envelope cannot be changed after
// it is built and the snapshot the outbox holds stays the one that was produced
// at commit time.
type Envelope struct {
	id            uuid.UUID
	eventType     EventType
	version       int
	aggregateType AggregateType
	aggregateID   uuid.UUID
	correlationID uuid.UUID
	causationID   uuid.UUID
	occurredAt    time.Time
	data          any
}

// envelopeJSON is the wire representation of an envelope. It exists so that the
// published contract is written down in one place and does not depend on the
// internal field names.
type envelopeJSON struct {
	ID            uuid.UUID     `json:"eventId"`
	Type          EventType     `json:"eventType"`
	Version       int           `json:"version"`
	AggregateType AggregateType `json:"aggregateType"`
	AggregateID   uuid.UUID     `json:"aggregateId"`
	CorrelationID uuid.UUID     `json:"correlationId"`
	CausationID   *uuid.UUID    `json:"causationId,omitempty"`
	OccurredAt    string        `json:"occurredAt"`
	Data          any           `json:"data"`
}

// newEnvelope builds and validates an envelope. It is unexported because the
// type and the version of an event are decided by its own constructor and never
// by the caller, and the aggregate type is derived from the event type instead
// of being informed.
func newEnvelope(eventType EventType, version int, aggregateID uuid.UUID, occurredAt time.Time, meta Metadata, data any) (Envelope, error) {
	aggregateType, err := aggregateFor(eventType)
	if err != nil {
		return Envelope{}, err
	}
	if version < 1 {
		return Envelope{}, fmt.Errorf("%w: %d", ErrInvalidEventVersion, version)
	}
	if aggregateID == uuid.Nil {
		return Envelope{}, ErrInvalidAggregateID
	}
	if occurredAt.IsZero() {
		return Envelope{}, ErrInvalidOccurredAt
	}
	if err := meta.validate(); err != nil {
		return Envelope{}, err
	}
	if data == nil {
		return Envelope{}, ErrMissingEventData
	}

	return Envelope{
		id:            uuid.New(),
		eventType:     eventType,
		version:       version,
		aggregateType: aggregateType,
		aggregateID:   aggregateID,
		correlationID: meta.CorrelationID,
		causationID:   meta.CausationID,
		occurredAt:    occurredAt.UTC(),
		data:          data,
	}, nil
}

// RestoredEnvelopeParams carries the persisted columns of an outbox row.
// Payload is the stored snapshot, still in its serialized form, because the
// publisher republishes exactly what was committed and never rebuilds it from
// the current state of the aggregate.
type RestoredEnvelopeParams struct {
	ID            uuid.UUID
	Type          EventType
	Version       int
	AggregateType AggregateType
	AggregateID   uuid.UUID
	CorrelationID uuid.UUID
	CausationID   uuid.UUID
	OccurredAt    time.Time
	Payload       json.RawMessage
}

// RestoreEnvelope rebuilds an envelope from its outbox row, keeping the stored
// eventId so that a republication after a crash is recognised as the same event.
// It validates the row instead of trusting it: an event whose aggregate type
// disagrees with its event type, or whose payload is not a JSON object, is a
// corrupted row and is refused rather than published.
func RestoreEnvelope(params RestoredEnvelopeParams) (Envelope, error) {
	expectedAggregate, err := aggregateFor(params.Type)
	if err != nil {
		return Envelope{}, err
	}
	if params.ID == uuid.Nil {
		return Envelope{}, ErrInvalidEventID
	}
	if !params.AggregateType.IsValid() {
		return Envelope{}, fmt.Errorf("%w: %q", ErrUnknownAggregateType, params.AggregateType)
	}
	if params.AggregateType != expectedAggregate {
		return Envelope{}, fmt.Errorf("%w: %q carries %q", ErrAggregateTypeMismatch, params.Type, params.AggregateType)
	}
	if params.Version < 1 {
		return Envelope{}, fmt.Errorf("%w: %d", ErrInvalidEventVersion, params.Version)
	}
	if params.AggregateID == uuid.Nil {
		return Envelope{}, ErrInvalidAggregateID
	}
	if params.CorrelationID == uuid.Nil {
		return Envelope{}, ErrInvalidCorrelationID
	}
	if params.OccurredAt.IsZero() {
		return Envelope{}, ErrInvalidOccurredAt
	}
	if err := validatePayload(params.Payload); err != nil {
		return Envelope{}, err
	}

	return Envelope{
		id:            params.ID,
		eventType:     params.Type,
		version:       params.Version,
		aggregateType: params.AggregateType,
		aggregateID:   params.AggregateID,
		correlationID: params.CorrelationID,
		causationID:   params.CausationID,
		occurredAt:    params.OccurredAt.UTC(),
		data:          params.Payload,
	}, nil
}

// validatePayload applies to a stored snapshot the same rule the outbox table
// enforces: the payload exists and is a JSON object.
func validatePayload(payload json.RawMessage) error {
	trimmed := bytes.TrimSpace(payload)

	if len(trimmed) == 0 {
		return ErrMissingEventData
	}
	if trimmed[0] != '{' || !json.Valid(trimmed) {
		return ErrInvalidEventData
	}

	return nil
}

// MarshalJSON serializes the envelope into the published contract. This is the
// body a consumer receives, and the same bytes are produced again when the
// event is republished from its outbox row.
func (e Envelope) MarshalJSON() ([]byte, error) {
	if e.data == nil {
		return nil, ErrMissingEventData
	}

	wire := envelopeJSON{
		ID:            e.id,
		Type:          e.eventType,
		Version:       e.version,
		AggregateType: e.aggregateType,
		AggregateID:   e.aggregateID,
		CorrelationID: e.correlationID,
		OccurredAt:    e.occurredAt.UTC().Format(timeFormat),
		Data:          e.data,
	}

	// causationId is optional and must be absent, not null, when the event was
	// not caused by another one.
	if e.causationID != uuid.Nil {
		causation := e.causationID
		wire.CausationID = &causation
	}

	return json.Marshal(wire)
}

// DataJSON returns the serialized payload that the outbox stores. It is the
// immutable snapshot of the event: the publisher never recomputes it from the
// aggregate, which may already have moved on.
func (e Envelope) DataJSON() ([]byte, error) {
	if e.data == nil {
		return nil, ErrMissingEventData
	}

	return json.Marshal(e.data)
}

// --- ACCESSORS ---

// ID returns the stable identity of the event, preserved across republications.
func (e Envelope) ID() uuid.UUID { return e.id }

// Type returns the name of the event.
func (e Envelope) Type() EventType { return e.eventType }

// Version returns the version of the event schema.
func (e Envelope) Version() int { return e.version }

// AggregateType returns the kind of entity the event happened to.
func (e Envelope) AggregateType() AggregateType { return e.aggregateType }

// AggregateID returns the identifier of the entity the event happened to.
func (e Envelope) AggregateID() uuid.UUID { return e.aggregateID }

// CorrelationID returns the identifier of the business operation.
func (e Envelope) CorrelationID() uuid.UUID { return e.correlationID }

// CausationID returns what directly caused the event. The second result is
// false when the event starts a chain.
func (e Envelope) CausationID() (uuid.UUID, bool) {
	return e.causationID, e.causationID != uuid.Nil
}

// OccurredAt returns the instant the fact happened, in UTC. It comes from the
// aggregate that produced the event, not from the moment of publication.
func (e Envelope) OccurredAt() time.Time { return e.occurredAt }

// Data returns the typed payload. It is a json.RawMessage when the envelope was
// restored from the outbox, since a republication ships the stored snapshot
// untouched.
func (e Envelope) Data() any { return e.data }
