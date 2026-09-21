/*
@Author: Franco Ribeiro Borba
@Description: Resumption of reversals that are waiting for a reference. A
REFUND or a ROLLBACK can arrive before the transaction it reverses, because
nothing orders two independent producers, so the operation is stored in
PENDING_REFERENCE rather than refused and this use case is what carries it
forward afterwards. Resuming is deliberately not a separate code path: the
stored row holds every field of the original request, so the command is rebuilt
from it and goes through the same flow a new delivery would, which means the
resolution, the movement and the events are decided by exactly the same rules.
When the retry budget runs out the operation is refused for good with
REFERENCE_NOT_FOUND, because a reversal that waits forever is worse for a
provider than a refusal it can act upon.
@Date : 21/09/2026
@Update: -
*/
package usecase

import (
	"context"
	"fmt"
	"time"

	"github.com/FrancoBorba/distributed-wallet-api/cmd/internal/domain"
	"github.com/FrancoBorba/distributed-wallet-api/cmd/internal/events"
	"github.com/google/uuid"
)

// PendingReference is one reversal the worker has taken responsibility for.
// Command is rebuilt from the stored row, so resuming needs nothing that was
// only ever held in the memory of the instance that first received it.
type PendingReference struct {
	TransactionID uuid.UUID
	Attempts      int
	CreatedAt     time.Time
	Command       WagerCommand
}

// PendingReferenceStore claims and reschedules reversals waiting for their
// reference. It works outside the business transaction, like the outbox claim,
// because claiming and resolving are separate steps.
type PendingReferenceStore interface {
	// ClaimDue takes up to limit reversals that are due and not held by
	// another worker, marking them as this worker's for the duration of the
	// lease.
	ClaimDue(ctx context.Context, workerID string, limit int, lease time.Duration) ([]PendingReference, error)
	// Reschedule releases the claim and pushes the next attempt forward.
	Reschedule(ctx context.Context, transactionID uuid.UUID, delay time.Duration) error
	// Release drops the claim without changing the schedule, used when the
	// operation left PENDING_REFERENCE and needs no further attention.
	Release(ctx context.Context, transactionID uuid.UUID) error
}

// ResumePendingReference carries waiting reversals forward.
type ResumePendingReference struct {
	processor *ProcessWagerTransaction
	uow       UnitOfWork
	clock     Clock
	ids       IDGenerator
}

// NewResumePendingReference builds the use case.
func NewResumePendingReference(processor *ProcessWagerTransaction, uow UnitOfWork, clock Clock, ids IDGenerator) *ResumePendingReference {
	return &ResumePendingReference{processor: processor, uow: uow, clock: clock, ids: ids}
}

// Resume tries the operation again. It goes through the ordinary flow, which
// finds the stored transaction by (providerId, externalTransactionId), sees it
// is not terminal and continues from where it stopped: if the reference has
// arrived the reversal is applied and concluded, and if it has not, the
// operation stays parked without announcing the wait a second time.
func (u *ResumePendingReference) Resume(ctx context.Context, pending PendingReference) (WagerResult, error) {
	return u.processor.Execute(ctx, pending.Command)
}

// Expire refuses a reversal whose reference never arrived within the budget.
// The refusal is definitive and carries REFERENCE_NOT_FOUND, which is a
// different code from a reference that exists but did not conclude, so the two
// cases stay distinguishable in an audit.
func (u *ResumePendingReference) Expire(ctx context.Context, transactionID uuid.UUID, meta events.Metadata) (WagerResult, error) {
	now := u.clock.Now()

	if meta.CorrelationID == uuid.Nil {
		meta.CorrelationID = u.ids.New()
	}

	var result WagerResult

	err := u.uow.Within(ctx, func(ctx context.Context, repos Repositories) error {
		transaction, err := repos.Transactions.FindByID(ctx, transactionID)
		if err != nil {
			return err
		}

		// Another instance may have resolved or refused it between the claim
		// and this transaction. A terminal record is already the final answer.
		if transaction.IsTerminal() {
			result = replayOf(transaction)

			return nil
		}

		if err := transaction.Reject(domain.FailureReferenceNotFound, now); err != nil {
			return err
		}
		if err := repos.Transactions.Update(ctx, transaction); err != nil {
			return err
		}

		err = saveEvents(ctx, repos.Outbox, func() (events.Envelope, error) {
			return events.NewWagerTransactionRejected(transaction, meta)
		})
		if err != nil {
			return err
		}

		result = WagerResult{
			TransactionID: transaction.ID(),
			State:         transaction.State(),
			FailureCode:   domain.FailureReferenceNotFound,
		}

		return nil
	})
	if err != nil {
		return WagerResult{}, fmt.Errorf("expire pending reference %s: %w", transactionID, err)
	}

	return result, nil
}
