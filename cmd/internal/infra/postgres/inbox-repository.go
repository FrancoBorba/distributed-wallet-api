/*
@Author: Franco Ribeiro Borba
@Description: Inbox repository. SQS delivers at least once, so the same message
can arrive twice for reasons that have nothing to do with the sender: a network
timeout, a visibility timeout that expired while the handler was still working,
a redrive. This table is the durable side of the deduplication, and it is
written inside the same transaction as the domain changes, which is what makes
a message impossible to complete without the work it caused and impossible to
apply twice once completed. Register is a single upsert: the primary key
(consumerName, messageId) is what decides whether this is a first delivery, and
the stored hash is compared against the new one, because the same identifier
carrying a different body is a conflict and not a duplicate.
@Date : 20/09/2026
@Update: -
*/
package postgres

import (
	"context"
	"fmt"
	"time"

	"github.com/FrancoBorba/distributed-wallet-api/cmd/internal/usecase"
)

// InboxRepository records the identity of consumed messages.
type InboxRepository struct {
	querier Querier
}

// NewInboxRepository binds the repository to a querier.
func NewInboxRepository(querier Querier) *InboxRepository {
	return &InboxRepository{querier: querier}
}

// Register claims a message for a consumer and reports what was known about it
// before this delivery.
//
// The upsert only ever touches the attempt counter, because the identity of a
// message is immutable: a trigger refuses any statement that tries to rewrite
// the hash a redelivery is compared against. The xmax trick distinguishes an
// insert from an update in the same round trip, which is how a first delivery
// is told apart from a repeat without a second query racing against another
// consumer.
func (r *InboxRepository) Register(ctx context.Context, consumer, messageID, payloadHash string, now time.Time) (usecase.InboxStatus, error) {
	var (
		storedHash  string
		completedAt *time.Time
		inserted    bool
	)

	err := r.querier.QueryRow(ctx,
		`INSERT INTO inbox (consumer_name, message_id, payload_hash, received_at, attempts)
		      VALUES ($1, $2, $3, $4, 1)
		 ON CONFLICT (consumer_name, message_id)
		 DO UPDATE SET attempts = inbox.attempts + 1
		   RETURNING payload_hash, completed_at, (xmax = 0) AS inserted`,
		consumer, messageID, payloadHash, now.UTC(),
	).Scan(&storedHash, &completedAt, &inserted)
	if err != nil {
		return usecase.InboxNew, fmt.Errorf("register message %s for %s: %w", messageID, consumer, err)
	}

	if inserted {
		return usecase.InboxNew, nil
	}

	// The same identifier came back carrying something else. Treating it as a
	// duplicate would silently drop a real operation, so it is refused.
	if storedHash != payloadHash {
		return usecase.InboxNew, fmt.Errorf("%w: %s", usecase.ErrMessageConflict, messageID)
	}

	if completedAt != nil {
		return usecase.InboxCompleted, nil
	}

	// Accepted earlier but never concluded: the process handling it died before
	// committing, so nothing it did is visible and the work starts over.
	return usecase.InboxInProgress, nil
}

// Complete marks the handling as durably concluded. It runs in the transaction
// of the domain changes, so the message is only ever completed together with
// the work it caused.
//
// The instant is taken as the later of now and the moment the message was
// received: two instances with slightly different clocks must not be able to
// write a completion that appears to precede the reception.
func (r *InboxRepository) Complete(ctx context.Context, consumer, messageID string, now time.Time) error {
	_, err := r.querier.Exec(ctx,
		`UPDATE inbox
		    SET completed_at = GREATEST($3, received_at)
		  WHERE consumer_name = $1 AND message_id = $2 AND completed_at IS NULL`,
		consumer, messageID, now.UTC(),
	)
	if err != nil {
		return fmt.Errorf("complete message %s for %s: %w", messageID, consumer, err)
	}

	return nil
}

// RecordFailure keeps the reason a delivery failed, for auditing a message that
// ends up in the dead letter queue. It never changes the identity of the
// message, only the diagnosis attached to it.
func (r *InboxRepository) RecordFailure(ctx context.Context, consumer, messageID string, cause error) error {
	message := ""
	if cause != nil {
		message = cause.Error()
	}

	_, err := r.querier.Exec(ctx,
		`UPDATE inbox
		    SET last_error = $3
		  WHERE consumer_name = $1 AND message_id = $2 AND completed_at IS NULL`,
		consumer, messageID, nullableText(message),
	)
	if err != nil {
		return fmt.Errorf("record failure of message %s: %w", messageID, err)
	}

	return nil
}
