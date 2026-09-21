/*
@Author: Franco Ribeiro Borba
@Description: Outbox repository. Save writes the event inside the transaction
of the state that justifies it, which is the whole point of the pattern: there
is no transaction spanning PostgreSQL and SQS, so the service only ever writes
to the database and lets a separate worker deliver what was committed. Delivery
lives in OutboxStore, which works on the pool instead of a business
transaction. Its claim is one statement using FOR UPDATE SKIP LOCKED, so
several publishers compete for rows without blocking each other and without
ever taking the same event twice, and the claim is a lease with an expiry: a
publisher that dies holding rows blocks nobody, because the lease runs out and
another instance takes the work over. Every instant compared here comes from
now() in the database, never from the clock of an instance, so a machine whose
clock drifts cannot claim work early or hold a lease longer than it should.
@Date : 20/09/2026
@Update: -
*/
package postgres

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	"github.com/FrancoBorba/distributed-wallet-api/cmd/internal/events"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
)

// OutboxRepository stores events in the transaction that produced them.
type OutboxRepository struct {
	querier Querier
}

// NewOutboxRepository binds the repository to a querier.
func NewOutboxRepository(querier Querier) *OutboxRepository {
	return &OutboxRepository{querier: querier}
}

// Save writes one event. The payload is the serialized snapshot of the event as
// it was produced, and a trigger keeps it immutable afterwards, so a
// republication after a crash ships exactly what was committed and never a
// recomputation over state that has moved on since.
func (r *OutboxRepository) Save(ctx context.Context, envelope events.Envelope) error {
	payload, err := envelope.DataJSON()
	if err != nil {
		return fmt.Errorf("serialize event %s: %w", envelope.ID(), err)
	}

	causationID, _ := envelope.CausationID()

	// created_at and next_attempt_at come from now(), which inside a
	// transaction is its start instant: the event becomes due the moment it is
	// committed, on the same clock the claim compares against.
	_, err = r.querier.Exec(ctx,
		`INSERT INTO outbox (
			event_id, aggregate_type, aggregate_id, event_type, event_version,
			payload, correlation_id, causation_id, occurred_at,
			attempts, next_attempt_at, created_at
		) VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,0,now(),now())`,
		envelope.ID(), string(envelope.AggregateType()), envelope.AggregateID(),
		string(envelope.Type()), envelope.Version(),
		payload, envelope.CorrelationID(), nullableUUID(causationID),
		envelope.OccurredAt(),
	)
	if err != nil {
		return fmt.Errorf("insert outbox event %s: %w", envelope.ID(), err)
	}

	return nil
}

// ClaimedEvent is an event a publisher has taken responsibility for. Attempts
// counts the deliveries tried so far, including this one, and is what the
// backoff and the dead letter decision are based on.
type ClaimedEvent struct {
	Envelope events.Envelope
	Attempts int
}

// OutboxStore is the delivery side of the outbox, used by the publisher worker.
// It works on the pool because claiming, publishing and confirming are separate
// steps: holding a business transaction open across a network call to SQS would
// keep a database connection blocked for as long as the broker takes to answer.
type OutboxStore struct {
	pool *pgxpool.Pool
}

// NewOutboxStore builds the delivery side over a connection pool.
func NewOutboxStore(pool *pgxpool.Pool) *OutboxStore {
	return &OutboxStore{pool: pool}
}

// ClaimPending takes up to limit events that are due and not held by anybody
// else, marking them as this publisher's work for the duration of the lease.
//
// It is a single statement on purpose. SKIP LOCKED makes concurrent publishers
// walk past the rows another one is already taking instead of queuing behind
// them, and the lease is what allows abandoned work to be recovered: a row held
// by a publisher that never came back becomes available again on its own.
func (s *OutboxStore) ClaimPending(ctx context.Context, publisherID string, limit int, lease time.Duration) ([]ClaimedEvent, error) {
	rows, err := s.pool.Query(ctx,
		`UPDATE outbox
		    SET locked_by = $1,
		        locked_until = now() + make_interval(secs => $2),
		        attempts = attempts + 1
		  WHERE event_id IN (
		        SELECT event_id
		          FROM outbox
		         WHERE published_at IS NULL
		           AND next_attempt_at <= now()
		           AND (locked_until IS NULL OR locked_until <= now())
		         ORDER BY next_attempt_at, event_id
		           FOR UPDATE SKIP LOCKED
		         LIMIT $3
		  )
		RETURNING event_id, event_type, event_version, aggregate_type, aggregate_id,
		          correlation_id, causation_id, occurred_at, payload, attempts`,
		publisherID, lease.Seconds(), limit,
	)
	if err != nil {
		return nil, fmt.Errorf("claim outbox events: %w", err)
	}
	defer rows.Close()

	var claimed []ClaimedEvent

	for rows.Next() {
		var (
			eventID, aggregateID, correlationID uuid.UUID
			causationID                         *uuid.UUID
			eventType, aggregateType            string
			version, attempts                   int
			occurredAt                          time.Time
			payload                             []byte
		)

		err := rows.Scan(
			&eventID, &eventType, &version, &aggregateType, &aggregateID,
			&correlationID, &causationID, &occurredAt, &payload, &attempts,
		)
		if err != nil {
			return nil, fmt.Errorf("scan outbox event: %w", err)
		}

		params := events.RestoredEnvelopeParams{
			ID:            eventID,
			Type:          events.EventType(eventType),
			Version:       version,
			AggregateType: events.AggregateType(aggregateType),
			AggregateID:   aggregateID,
			CorrelationID: correlationID,
			OccurredAt:    occurredAt,
			Payload:       json.RawMessage(payload),
		}
		if causationID != nil {
			params.CausationID = *causationID
		}

		// The stored event is validated on the way out: a corrupted row is
		// refused rather than published to consumers that cannot read it.
		envelope, err := events.RestoreEnvelope(params)
		if err != nil {
			return nil, fmt.Errorf("restore outbox event %s: %w", eventID, err)
		}

		claimed = append(claimed, ClaimedEvent{Envelope: envelope, Attempts: attempts})
	}

	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("read claimed outbox events: %w", err)
	}

	return claimed, nil
}

// MarkPublished confirms a delivery and releases the claim. The condition on
// published_at makes a second confirmation a no-op, which is what happens when
// a publisher crashes between sending the message and recording it: the event
// is delivered again, the consumer deduplicates it by eventId, and the first
// confirmation to arrive is the one that stands.
func (s *OutboxStore) MarkPublished(ctx context.Context, eventID uuid.UUID) error {
	_, err := s.pool.Exec(ctx,
		`UPDATE outbox
		    SET published_at = now(), locked_by = NULL, locked_until = NULL
		  WHERE event_id = $1 AND published_at IS NULL`,
		eventID,
	)
	if err != nil {
		return fmt.Errorf("mark outbox event %s as published: %w", eventID, err)
	}

	return nil
}

// Reschedule releases a claim and pushes the next attempt into the future. The
// failure is recorded so that an event stuck in retries can be diagnosed
// without reading the logs of whichever instance happened to try it.
func (s *OutboxStore) Reschedule(ctx context.Context, eventID uuid.UUID, delay time.Duration, cause error) error {
	message := ""
	if cause != nil {
		message = cause.Error()
	}

	_, err := s.pool.Exec(ctx,
		`UPDATE outbox
		    SET next_attempt_at = now() + make_interval(secs => $2),
		        locked_by = NULL,
		        locked_until = NULL,
		        last_error = $3
		  WHERE event_id = $1 AND published_at IS NULL`,
		eventID, delay.Seconds(), nullableText(message),
	)
	if err != nil {
		return fmt.Errorf("reschedule outbox event %s: %w", eventID, err)
	}

	return nil
}

// PendingStats returns how long the oldest unpublished event has been waiting
// and how many are waiting. Together they are the health of the whole
// publication path: both stay near zero while delivery keeps up and grow the
// moment it stops, which is the failure that is otherwise invisible because the
// API keeps answering normally.
func (s *OutboxStore) PendingStats(ctx context.Context) (time.Duration, int, error) {
	var (
		seconds *float64
		pending int
	)

	err := s.pool.QueryRow(ctx,
		`SELECT EXTRACT(EPOCH FROM (now() - MIN(occurred_at))), COUNT(*)
		   FROM outbox
		  WHERE published_at IS NULL`,
	).Scan(&seconds, &pending)
	if err != nil {
		return 0, 0, fmt.Errorf("read outbox stats: %w", err)
	}
	if seconds == nil {
		return 0, pending, nil
	}

	return time.Duration(*seconds * float64(time.Second)), pending, nil
}

// PendingLag returns how long the oldest unpublished event has been waiting. It
// is the health signal of the whole publication path: it stays near zero while
// delivery keeps up and grows the moment it stops.
func (s *OutboxStore) PendingLag(ctx context.Context) (time.Duration, error) {
	var seconds *float64

	err := s.pool.QueryRow(ctx,
		`SELECT EXTRACT(EPOCH FROM (now() - MIN(occurred_at)))
		   FROM outbox
		  WHERE published_at IS NULL`,
	).Scan(&seconds)
	if err != nil {
		return 0, fmt.Errorf("read outbox lag: %w", err)
	}
	if seconds == nil {
		return 0, nil
	}

	return time.Duration(*seconds * float64(time.Second)), nil
}
