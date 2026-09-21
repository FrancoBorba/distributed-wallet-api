/*
@Author: Franco Ribeiro Borba
@Description: Claim side of the reversals waiting for a reference. It mirrors
the outbox claim, and for the same reasons: FOR UPDATE SKIP LOCKED lets several
instances take disjoint work without coordinating, and the lease means a worker
that dies blocks nothing, because its claim expires and another instance takes
the work over. The claim happens outside any business transaction, since
resolving a reversal opens a transaction of its own and holding one open across
that work would keep a connection busy for no reason. Every instant compared
here comes from now() in the database rather than from the clock of an
instance, so a machine whose clock drifts cannot claim work early.
@Date : 21/09/2026
@Update: -
*/
package postgres

import (
	"context"
	"fmt"
	"time"

	"github.com/FrancoBorba/distributed-wallet-api/cmd/internal/domain"
	"github.com/FrancoBorba/distributed-wallet-api/cmd/internal/usecase"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
)

// PendingReferenceStore claims reversals that are waiting for a reference.
type PendingReferenceStore struct {
	pool *pgxpool.Pool
}

// NewPendingReferenceStore builds the claim side over the connection pool.
func NewPendingReferenceStore(pool *pgxpool.Pool) *PendingReferenceStore {
	return &PendingReferenceStore{pool: pool}
}

// ClaimDue takes up to limit reversals that are due and free, and returns them
// rebuilt as the commands that originally created them.
//
// It runs in two steps on purpose. The UPDATE claims the rows and reports which
// ones it took, and a second read loads them in full through the same scan the
// rest of the repository uses. Holding the claim makes the second read safe:
// those rows belong to this worker until the lease expires.
func (s *PendingReferenceStore) ClaimDue(ctx context.Context, workerID string, limit int, lease time.Duration) ([]usecase.PendingReference, error) {
	rows, err := s.pool.Query(ctx,
		`UPDATE wager_transactions
		    SET reference_locked_by = $1,
		        reference_locked_until = now() + make_interval(secs => $2),
		        reference_attempts = reference_attempts + 1
		  WHERE id IN (
		        SELECT id
		          FROM wager_transactions
		         WHERE state = 'PENDING_REFERENCE'
		           AND (reference_next_attempt_at IS NULL OR reference_next_attempt_at <= now())
		           AND (reference_locked_until IS NULL OR reference_locked_until <= now())
		         ORDER BY reference_next_attempt_at NULLS FIRST, id
		           FOR UPDATE SKIP LOCKED
		         LIMIT $3
		  )
		RETURNING id, reference_attempts`,
		workerID, lease.Seconds(), limit,
	)
	if err != nil {
		return nil, fmt.Errorf("claim pending references: %w", err)
	}

	claimed := map[uuid.UUID]int{}
	order := make([]uuid.UUID, 0, limit)

	for rows.Next() {
		var (
			id       uuid.UUID
			attempts int
		)

		if err := rows.Scan(&id, &attempts); err != nil {
			rows.Close()

			return nil, fmt.Errorf("scan claimed reference: %w", err)
		}

		claimed[id] = attempts
		order = append(order, id)
	}

	rows.Close()

	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("read claimed references: %w", err)
	}
	if len(order) == 0 {
		return nil, nil
	}

	return s.load(ctx, order, claimed)
}

// load reads the claimed rows in full and rebuilds the original commands.
func (s *PendingReferenceStore) load(ctx context.Context, order []uuid.UUID, attempts map[uuid.UUID]int) ([]usecase.PendingReference, error) {
	rows, err := s.pool.Query(ctx,
		`SELECT `+transactionColumns+` FROM wager_transactions WHERE id = ANY($1)`,
		order,
	)
	if err != nil {
		return nil, fmt.Errorf("load claimed references: %w", err)
	}
	defer rows.Close()

	pending := make([]usecase.PendingReference, 0, len(order))

	for rows.Next() {
		transaction, err := scanTransaction(rows)
		if err != nil {
			return nil, fmt.Errorf("scan pending reference: %w", err)
		}

		pending = append(pending, usecase.PendingReference{
			TransactionID: transaction.ID(),
			Attempts:      attempts[transaction.ID()],
			CreatedAt:     transaction.CreatedAt(),
			Command:       commandOf(transaction),
		})
	}

	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("read pending references: %w", err)
	}

	return pending, nil
}

// Reschedule releases the claim and pushes the next attempt forward.
func (s *PendingReferenceStore) Reschedule(ctx context.Context, transactionID uuid.UUID, delay time.Duration) error {
	_, err := s.pool.Exec(ctx,
		`UPDATE wager_transactions
		    SET reference_next_attempt_at = now() + make_interval(secs => $2),
		        reference_locked_by = NULL,
		        reference_locked_until = NULL
		  WHERE id = $1 AND state = 'PENDING_REFERENCE'`,
		transactionID, delay.Seconds(),
	)
	if err != nil {
		return fmt.Errorf("reschedule pending reference %s: %w", transactionID, err)
	}

	return nil
}

// Release drops the claim of an operation that has left PENDING_REFERENCE. The
// state condition is what makes it a no-op for a row that was concluded in the
// meantime, since a terminal row must not be touched at all.
func (s *PendingReferenceStore) Release(ctx context.Context, transactionID uuid.UUID) error {
	_, err := s.pool.Exec(ctx,
		`UPDATE wager_transactions
		    SET reference_locked_by = NULL, reference_locked_until = NULL
		  WHERE id = $1 AND state = 'PENDING_REFERENCE'`,
		transactionID,
	)
	if err != nil {
		return fmt.Errorf("release pending reference %s: %w", transactionID, err)
	}

	return nil
}

// commandOf rebuilds the request that created a transaction. Every field of the
// original operation is stored, which is what makes a resumption identical to a
// redelivery instead of a second, weaker code path.
func commandOf(transaction *domain.WagerTransaction) usecase.WagerCommand {
	referenceExternalID, _ := transaction.ReferenceExternalID()

	return usecase.WagerCommand{
		ProviderID:            transaction.ProviderID(),
		ExternalTransactionID: transaction.ExternalTransactionID(),
		IdempotencyKey:        transaction.IdempotencyKey(),
		PayloadHash:           transaction.PayloadHash(),
		PlayerID:              transaction.PlayerID(),
		WalletID:              transaction.WalletID(),
		RoundID:               transaction.RoundID(),
		GameID:                transaction.GameID(),
		Kind:                  transaction.Kind(),
		Money:                 transaction.Money(),
		ReferenceExternalID:   referenceExternalID,
	}
}
