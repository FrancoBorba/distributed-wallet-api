/*
@Author: Franco Ribeiro Borba
@Description: Outbox publisher worker. It is the second half of the pattern:
the use cases only ever write events to the database, and this loop is what
carries them to the broker afterwards. Each round claims a batch of due events,
sends them and records the outcome one by one, so a failure affects the event
it belongs to and not the batch. Several instances run this at the same time
without coordinating: the claim uses SKIP LOCKED, so they take disjoint work,
and the claim is a lease, so events held by an instance that died are taken
over once it expires. An event is never dropped, whatever happens: a delivery
that keeps failing keeps coming back with a longer delay each time, because
losing an event whose record was committed is the one outcome the outbox exists
to prevent. Publication is at-least-once by design, and the eventId, preserved
across every attempt, is what lets the consumer deduplicate.
@Date : 20/09/2026
@Update: -
*/
package worker

import (
	"context"
	"log/slog"
	"time"

	"github.com/FrancoBorba/distributed-wallet-api/cmd/internal/config"
	"github.com/FrancoBorba/distributed-wallet-api/cmd/internal/infra/metrics"
	"github.com/FrancoBorba/distributed-wallet-api/cmd/internal/infra/postgres"
	"github.com/FrancoBorba/distributed-wallet-api/cmd/internal/usecase"
	"github.com/google/uuid"
)

// OutboxPublisher delivers committed events to the broker.
type OutboxPublisher struct {
	store     *postgres.OutboxStore
	publisher usecase.Publisher
	metrics   *metrics.Metrics
	logger    *slog.Logger
	cfg       config.Outbox
}

// NewOutboxPublisher builds the worker.
func NewOutboxPublisher(store *postgres.OutboxStore, publisher usecase.Publisher, logger *slog.Logger, recorder *metrics.Metrics, cfg config.Outbox) *OutboxPublisher {
	return &OutboxPublisher{
		store:     store,
		publisher: publisher,
		metrics:   recorder,
		logger:    logger,
		cfg:       cfg,
	}
}

// Run publishes until the context is cancelled. It sleeps only when there was
// nothing to do, so a burst of events is drained as fast as the broker accepts
// them instead of one batch per tick.
func (w *OutboxPublisher) Run(ctx context.Context) {
	w.logger.Info("outbox publisher started", slog.String("publisherId", w.cfg.PublisherID))

	for {
		if ctx.Err() != nil {
			w.logger.Info("outbox publisher stopped")
			return
		}

		published, err := w.runOnce(ctx)
		if err != nil {
			if ctx.Err() != nil {
				return
			}

			// The claim itself failed, which means the database is
			// unreachable. There is nothing to release: the lease of whatever
			// was claimed expires on its own.
			w.logger.Error("outbox round failed", slog.String("error", err.Error()))
			sleep(ctx, w.cfg.PollInterval)

			continue
		}

		if published == 0 {
			sleep(ctx, w.cfg.PollInterval)
		}
	}
}

// runOnce claims a batch and delivers it, returning how many events it took.
func (w *OutboxPublisher) runOnce(ctx context.Context) (int, error) {
	// The backlog gauge is refreshed on every round, which is the cheapest
	// place to do it: the worker already runs at the rhythm the backlog moves.
	if lag, pending, err := w.store.PendingStats(ctx); err == nil {
		w.metrics.ObserveOutbox(lag, pending)
	}

	claimed, err := w.store.ClaimPending(ctx, w.cfg.PublisherID, w.cfg.BatchSize, w.cfg.ClaimLease)
	if err != nil {
		return 0, err
	}

	for _, event := range claimed {
		// Shutdown in the middle of a batch: what was not sent stays claimed
		// until the lease expires, and another instance takes it from there.
		if ctx.Err() != nil {
			break
		}

		w.deliver(ctx, event)
	}

	return len(claimed), nil
}

// deliver sends one event and records what happened to it.
func (w *OutboxPublisher) deliver(ctx context.Context, event postgres.ClaimedEvent) {
	envelope := event.Envelope

	logger := w.logger.With(
		slog.String("eventId", envelope.ID().String()),
		slog.String("eventType", string(envelope.Type())),
		slog.String("aggregateId", envelope.AggregateID().String()),
		slog.String("correlationId", envelope.CorrelationID().String()),
		slog.Int("attempts", event.Attempts),
	)

	if err := w.publisher.Publish(ctx, envelope); err != nil {
		delay := backoffDelay(event.Attempts, w.cfg.RetryBaseDelay, w.cfg.RetryMaxDelay)

		logger.Warn("publication failed, event rescheduled",
			slog.String("error", err.Error()),
			slog.Duration("retryIn", delay))

		w.metrics.ObserveRetry("outbox-publisher")
		w.reschedule(ctx, envelope.ID(), delay, err, logger)

		return
	}

	// The message is on the queue. A crash before the confirmation below
	// leaves the event claimed until its lease expires and it is sent again,
	// which the consumer recognises by the unchanged eventId.
	if err := w.store.MarkPublished(ctx, envelope.ID()); err != nil {
		logger.Error("event published but not confirmed, it will be published again",
			slog.String("error", err.Error()))

		return
	}

	logger.Debug("event published")
}

// reschedule records a failed attempt, falling back to the expiry of the lease
// when even that write fails.
func (w *OutboxPublisher) reschedule(ctx context.Context, eventID uuid.UUID, delay time.Duration, cause error, logger *slog.Logger) {
	// Detached from the shutdown signal: releasing the claim is the last
	// useful thing we can do for this event, and it takes one statement.
	writeCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
	defer cancel()

	if err := w.store.Reschedule(writeCtx, eventID, delay, cause); err != nil {
		logger.Error("reschedule failed, the claim will expire on its own",
			slog.String("error", err.Error()))
	}
}

// backoffDelay grows the wait exponentially with the attempts already made and
// stops at the cap. There is no jitter: publishers never compete for the same
// event, because the claim hands each row to exactly one of them, so there is
// no herd to spread out.
func backoffDelay(attempts int, base, max time.Duration) time.Duration {
	if attempts < 1 {
		attempts = 1
	}

	// Shifting past this would overflow the duration before the cap is ever
	// compared, turning a long delay into a negative one.
	const maxShift = 32

	shift := attempts - 1
	if shift > maxShift {
		return max
	}

	delay := base << shift
	if delay <= 0 || delay > max {
		return max
	}

	return delay
}

// sleep waits unless the context ends first.
func sleep(ctx context.Context, duration time.Duration) {
	timer := time.NewTimer(duration)
	defer timer.Stop()

	select {
	case <-ctx.Done():
	case <-timer.C:
	}
}
