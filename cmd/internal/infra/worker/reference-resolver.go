/*
@Author: Franco Ribeiro Borba
@Description: Worker that carries forward the reversals waiting for a
reference. A REFUND or a ROLLBACK that arrived before the transaction it
reverses is parked in PENDING_REFERENCE, and without this loop nothing would
ever pick it up again: the message that brought it was already deleted from the
queue, exactly as the contract requires, precisely because this worker takes
over the continuity. Each round claims a batch of due reversals, tries them
again through the ordinary flow and either concludes them or pushes the next
attempt further away. The budget is deliberately double, a maximum number of
attempts and a wall clock deadline, because the two protect against different
failures: the attempt count bounds a reference that never comes, and the
deadline bounds a worker that restarted so often that its attempt count never
grew.
@Date : 21/09/2026
@Update: -
*/
package worker

import (
	"context"
	"log/slog"
	"time"

	"github.com/FrancoBorba/distributed-wallet-api/cmd/internal/config"
	"github.com/FrancoBorba/distributed-wallet-api/cmd/internal/domain"
	"github.com/FrancoBorba/distributed-wallet-api/cmd/internal/events"
	"github.com/FrancoBorba/distributed-wallet-api/cmd/internal/infra/metrics"
	"github.com/FrancoBorba/distributed-wallet-api/cmd/internal/usecase"
	"github.com/google/uuid"
)

// ReferenceResolver resolves reversals that are waiting for their reference.
type ReferenceResolver struct {
	store   usecase.PendingReferenceStore
	metrics *metrics.Metrics
	resume  *usecase.ResumePendingReference
	logger  *slog.Logger
	clock   usecase.Clock
	cfg     config.Reference
}

// NewReferenceResolver builds the worker.
func NewReferenceResolver(store usecase.PendingReferenceStore, resume *usecase.ResumePendingReference, logger *slog.Logger, recorder *metrics.Metrics, clock usecase.Clock, cfg config.Reference) *ReferenceResolver {
	return &ReferenceResolver{store: store, resume: resume, logger: logger, metrics: recorder, clock: clock, cfg: cfg}
}

// Run resolves until the context is cancelled. It sleeps only when a round
// found nothing, so a burst of pending reversals is drained as fast as the
// database answers instead of one batch per tick.
func (w *ReferenceResolver) Run(ctx context.Context) {
	w.logger.Info("reference resolver started",
		slog.String("workerId", w.cfg.WorkerID),
		slog.Int("maxAttempts", w.cfg.MaxAttempts),
		slog.Duration("ttl", w.cfg.TTL))

	for {
		if ctx.Err() != nil {
			w.logger.Info("reference resolver stopped")

			return
		}

		handled, err := w.runOnce(ctx)
		if err != nil {
			if ctx.Err() != nil {
				return
			}

			w.logger.Error("reference round failed", slog.String("error", err.Error()))
			sleep(ctx, w.cfg.PollInterval)

			continue
		}

		if handled == 0 {
			sleep(ctx, w.cfg.PollInterval)
		}
	}
}

// runOnce claims a batch and works through it.
func (w *ReferenceResolver) runOnce(ctx context.Context) (int, error) {
	claimed, err := w.store.ClaimDue(ctx, w.cfg.WorkerID, w.cfg.BatchSize, w.cfg.ClaimLease)
	if err != nil {
		return 0, err
	}

	for _, pending := range claimed {
		// Shutdown in the middle of a batch: what was not tried stays claimed
		// until the lease expires, and another instance takes it from there.
		if ctx.Err() != nil {
			break
		}

		w.resolve(ctx, pending)
	}

	return len(claimed), nil
}

// resolve decides what happens to one waiting reversal.
func (w *ReferenceResolver) resolve(ctx context.Context, pending usecase.PendingReference) {
	logger := w.logger.With(
		slog.String("transactionId", pending.TransactionID.String()),
		slog.String("providerId", pending.Command.ProviderID),
		slog.String("externalTransactionId", pending.Command.ExternalTransactionID),
		slog.String("referenceExternalTransactionId", pending.Command.ReferenceExternalID),
		slog.Int("attempts", pending.Attempts),
	)

	if w.exhausted(pending) {
		w.expire(ctx, pending, logger)

		return
	}

	// The retry carries its own correlation, because the operation that
	// originally brought this reversal in ended long ago. The events it
	// produces stay linked to the operation through the aggregate identifier.
	meta := events.NewMetadata(uuid.New())

	command := pending.Command
	command.Metadata = meta

	result, err := w.resume.Resume(ctx, usecase.PendingReference{
		TransactionID: pending.TransactionID,
		Attempts:      pending.Attempts,
		CreatedAt:     pending.CreatedAt,
		Command:       command,
	})
	if err != nil {
		delay := backoffDelay(pending.Attempts, w.cfg.RetryBaseDelay, w.cfg.RetryMaxDelay)

		logger.Warn("resolution attempt failed",
			slog.String("error", err.Error()),
			slog.Duration("retryIn", delay))

		w.metrics.ObserveRetry("reference-resolver")
		w.reschedule(ctx, pending.TransactionID, delay, logger)

		return
	}

	// Still waiting: the reference has not arrived yet. The claim is released
	// and the next attempt is pushed further away.
	if result.State == domain.StatePendingRef {
		delay := backoffDelay(pending.Attempts, w.cfg.RetryBaseDelay, w.cfg.RetryMaxDelay)

		logger.Debug("reference still missing", slog.Duration("retryIn", delay))
		w.metrics.ObserveRetry("reference-resolver")
		w.reschedule(ctx, pending.TransactionID, delay, logger)

		return
	}

	logger.Info("pending reference resolved",
		slog.String("state", string(result.State)),
		slog.String("failureCode", string(result.FailureCode)))

	// The operation concluded, so the row is terminal and holds no claim that
	// needs releasing. Release is still called for the case where it moved to
	// another non terminal state, and is a no-op otherwise.
	w.release(ctx, pending.TransactionID, logger)
}

// exhausted reports whether the budget of a waiting reversal ran out. Either
// limit ends the wait on its own.
func (w *ReferenceResolver) exhausted(pending usecase.PendingReference) bool {
	if w.cfg.MaxAttempts > 0 && pending.Attempts > w.cfg.MaxAttempts {
		return true
	}

	return w.cfg.TTL > 0 && w.clock.Now().Sub(pending.CreatedAt) > w.cfg.TTL
}

// expire refuses a reversal whose reference never arrived.
func (w *ReferenceResolver) expire(ctx context.Context, pending usecase.PendingReference, logger *slog.Logger) {
	result, err := w.resume.Expire(ctx, pending.TransactionID, events.NewMetadata(uuid.New()))
	if err != nil {
		delay := backoffDelay(pending.Attempts, w.cfg.RetryBaseDelay, w.cfg.RetryMaxDelay)

		logger.Error("expiring the pending reference failed",
			slog.String("error", err.Error()),
			slog.Duration("retryIn", delay))

		w.metrics.ObserveRetry("reference-resolver")
		w.reschedule(ctx, pending.TransactionID, delay, logger)

		return
	}

	logger.Warn("pending reference expired",
		slog.String("state", string(result.State)),
		slog.String("failureCode", string(result.FailureCode)))
}

// reschedule releases the claim and moves the next attempt forward, falling
// back to the expiry of the lease when even that write fails.
func (w *ReferenceResolver) reschedule(ctx context.Context, transactionID uuid.UUID, delay time.Duration, logger *slog.Logger) {
	writeCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
	defer cancel()

	if err := w.store.Reschedule(writeCtx, transactionID, delay); err != nil {
		logger.Error("rescheduling failed, the claim will expire on its own",
			slog.String("error", err.Error()))
	}
}

// release drops a claim that is no longer needed.
func (w *ReferenceResolver) release(ctx context.Context, transactionID uuid.UUID, logger *slog.Logger) {
	writeCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
	defer cancel()

	if err := w.store.Release(writeCtx, transactionID); err != nil {
		logger.Warn("releasing the claim failed, it will expire on its own",
			slog.String("error", err.Error()))
	}
}
