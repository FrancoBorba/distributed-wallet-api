/*
@Author: Franco Ribeiro Borba
@Description: Wager transaction repository. The table holds two origins in one
place, so the mapping has to be careful with absence: an internal OPENING
carries no provider metadata and every one of those columns must be written as
NULL, not as an empty string, because the schema checks the two origins apart
and because NULL is what keeps internal rows out of the uniqueness rules of
idempotency. The same care applies to the outcome columns, which only exist
once the operation is terminal. Reads go through RestoreTransaction, so a row
that breaks an invariant is refused instead of loaded, and the lookup by
provider and external identifier is the idempotency lookup of the whole
service: it is what recognises the same operation arriving again, over any
transport and with any key.
@Date : 20/09/2026
@Update: -
*/
package postgres

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/FrancoBorba/distributed-wallet-api/cmd/internal/domain"
	"github.com/FrancoBorba/distributed-wallet-api/cmd/internal/usecase"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
)

// transactionColumns is the projection every read uses, in the order
// scanTransaction expects.
const transactionColumns = `id, wallet_id, player_id, currency, amount_cents, kind, state,
	provider_id, external_transaction_id, idempotency_key, payload_hash, round_id, game_id,
	reference_external_transaction_id, reference_transaction_id,
	failure_code, result_balance_cents, created_at, updated_at`

// TransactionRepository reads and writes wager transactions.
type TransactionRepository struct {
	querier Querier
}

// NewTransactionRepository binds the repository to a querier.
func NewTransactionRepository(querier Querier) *TransactionRepository {
	return &TransactionRepository{querier: querier}
}

// FindByID loads a transaction by its internal identifier.
func (r *TransactionRepository) FindByID(ctx context.Context, id uuid.UUID) (*domain.WagerTransaction, error) {
	row := r.querier.QueryRow(ctx, `SELECT `+transactionColumns+` FROM wager_transactions WHERE id = $1`, id)

	transaction, err := scanTransaction(row)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, usecase.ErrTransactionNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("find transaction %s: %w", id, err)
	}

	return transaction, nil
}

// FindByProviderAndExternalID is the idempotency lookup. An operation is what
// the provider called it, so the same movement sent twice is found here even if
// the second delivery carried a different idempotency key or arrived over a
// different transport.
func (r *TransactionRepository) FindByProviderAndExternalID(ctx context.Context, providerID, externalID string) (*domain.WagerTransaction, error) {
	row := r.querier.QueryRow(ctx,
		`SELECT `+transactionColumns+`
		   FROM wager_transactions
		  WHERE provider_id = $1 AND external_transaction_id = $2`,
		providerID, externalID,
	)

	transaction, err := scanTransaction(row)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, usecase.ErrTransactionNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("find transaction %s of %s: %w", externalID, providerID, err)
	}

	return transaction, nil
}

// Insert writes a transaction. The record exists before the money moves, so an
// interrupted operation is resumable by any instance.
func (r *TransactionRepository) Insert(ctx context.Context, transaction *domain.WagerTransaction) error {
	referenceExternalID, _ := transaction.ReferenceExternalID()
	referenceID, _ := transaction.ReferenceTransactionID()
	failureCode, _ := transaction.FailureCode()
	resultBalance, hasResult := transaction.ResultBalance()

	_, err := r.querier.Exec(ctx,
		`INSERT INTO wager_transactions (
			id, wallet_id, player_id, currency, amount_cents, kind, state,
			provider_id, external_transaction_id, idempotency_key, payload_hash, round_id, game_id,
			reference_external_transaction_id, reference_transaction_id,
			failure_code, result_balance_cents, created_at, updated_at
		) VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13,$14,$15,$16,$17,$18,$19)`,
		transaction.ID(), transaction.WalletID(), transaction.PlayerID(),
		transaction.Money().Currency(), transaction.Money().Cents(),
		string(transaction.Kind()), string(transaction.State()),
		nullableText(transaction.ProviderID()),
		nullableText(transaction.ExternalTransactionID()),
		nullableText(transaction.IdempotencyKey()),
		nullableText(transaction.PayloadHash()),
		nullableText(transaction.RoundID()),
		nullableText(transaction.GameID()),
		nullableText(referenceExternalID),
		nullableUUID(referenceID),
		nullableText(string(failureCode)),
		nullableCents(resultBalance, hasResult),
		transaction.CreatedAt(), transaction.UpdatedAt(),
	)
	if isUniqueViolation(err, "") {
		// Another instance recorded the same operation first. The caller sees
		// the conflict it would have seen if it had read a moment later.
		return fmt.Errorf("%w: %s", usecase.ErrConcurrentUpdate, transaction.ExternalTransactionID())
	}
	if err != nil {
		return fmt.Errorf("insert transaction %s: %w", transaction.ID(), err)
	}

	return nil
}

// Update writes the outcome of a transition. Only the columns a transition may
// change are listed, and the database trigger refuses the statement outright if
// the row was already terminal, which is what stops a late worker from
// reopening a concluded operation.
func (r *TransactionRepository) Update(ctx context.Context, transaction *domain.WagerTransaction) error {
	referenceID, _ := transaction.ReferenceTransactionID()
	failureCode, _ := transaction.FailureCode()
	resultBalance, hasResult := transaction.ResultBalance()

	tag, err := r.querier.Exec(ctx,
		`UPDATE wager_transactions
		    SET state = $1,
		        reference_transaction_id = $2,
		        failure_code = $3,
		        result_balance_cents = $4,
		        updated_at = $5
		  WHERE id = $6`,
		string(transaction.State()),
		nullableUUID(referenceID),
		nullableText(string(failureCode)),
		nullableCents(resultBalance, hasResult),
		transaction.UpdatedAt(),
		transaction.ID(),
	)
	if isUniqueViolation(err, "wager_transactions_single_successful_reversal") {
		// Two reversals of the same reference raced to commit. The one that
		// lost is not a failure of this service: it is the second reversal,
		// and the caller turns it into ALREADY_REVERSED.
		return fmt.Errorf("%w: reference already reversed", usecase.ErrConcurrentUpdate)
	}
	if err != nil {
		return fmt.Errorf("update transaction %s: %w", transaction.ID(), err)
	}
	if tag.RowsAffected() == 0 {
		return usecase.ErrTransactionNotFound
	}

	return nil
}

// HasProcessedReversal reports whether a transaction was already reversed
// successfully. The partial unique index enforces the same rule at commit time;
// this read is what turns the race into a business answer instead of an error.
func (r *TransactionRepository) HasProcessedReversal(ctx context.Context, referenceID uuid.UUID) (bool, error) {
	var exists bool

	err := r.querier.QueryRow(ctx,
		`SELECT EXISTS (
			SELECT 1 FROM wager_transactions
			 WHERE reference_transaction_id = $1
			   AND state = 'PROCESSED'
			   AND kind IN ('REFUND', 'ROLLBACK')
		)`,
		referenceID,
	).Scan(&exists)
	if err != nil {
		return false, fmt.Errorf("check reversal of %s: %w", referenceID, err)
	}

	return exists, nil
}

// scanTransaction rebuilds a transaction from a row, turning every NULL back
// into the absent value the domain expects.
func scanTransaction(row pgx.Row) (*domain.WagerTransaction, error) {
	var (
		id, walletID, playerID uuid.UUID
		currency               string
		amountCents            int64
		kind, state            string

		providerID, externalID, idempotencyKey *string
		payloadHash, roundID, gameID           *string
		referenceExternalID                    *string
		referenceID                            *uuid.UUID
		failureCode                            *string
		resultBalanceCents                     *int64

		createdAt, updatedAt time.Time
	)

	err := row.Scan(
		&id, &walletID, &playerID, &currency, &amountCents, &kind, &state,
		&providerID, &externalID, &idempotencyKey, &payloadHash, &roundID, &gameID,
		&referenceExternalID, &referenceID,
		&failureCode, &resultBalanceCents, &createdAt, &updatedAt,
	)
	if err != nil {
		return nil, err
	}

	amount, err := domain.NewMoney(amountCents, currency)
	if err != nil {
		return nil, err
	}

	params := domain.RestoredTransactionParams{
		ID:                    id,
		WalletID:              walletID,
		PlayerID:              playerID,
		ProviderID:            textOf(providerID),
		ExternalTransactionID: textOf(externalID),
		IdempotencyKey:        textOf(idempotencyKey),
		PayloadHash:           textOf(payloadHash),
		RoundID:               textOf(roundID),
		GameID:                textOf(gameID),
		ReferenceExternalID:   textOf(referenceExternalID),
		Kind:                  domain.TransactionKind(kind),
		Money:                 amount,
		State:                 domain.TransactionState(state),
		FailureCode:           domain.FailureCode(textOf(failureCode)),
		CreatedAt:             createdAt,
		UpdatedAt:             updatedAt,
	}

	if referenceID != nil {
		params.ReferenceTransactionID = *referenceID
	}

	// An uninitialized Money is how the domain represents "no result balance
	// yet", which is exactly what a NULL column means here.
	if resultBalanceCents != nil {
		resultBalance, err := domain.NewMoney(*resultBalanceCents, currency)
		if err != nil {
			return nil, err
		}
		params.ResultBalance = resultBalance
	}

	return domain.RestoreTransaction(params)
}

// nullableText turns an empty string into NULL, which is what the schema
// requires for metadata that does not apply to an origin.
func nullableText(value string) *string {
	if value == "" {
		return nil
	}

	return &value
}

// nullableUUID turns the nil identifier into NULL.
func nullableUUID(value uuid.UUID) *uuid.UUID {
	if value == uuid.Nil {
		return nil
	}

	return &value
}

// nullableCents writes an amount only when it exists.
func nullableCents(value domain.Money, present bool) *int64 {
	if !present {
		return nil
	}

	cents := value.Cents()

	return &cents
}

// textOf reads a nullable column back as the empty string the domain uses for
// an absent value.
func textOf(value *string) string {
	if value == nil {
		return ""
	}

	return *value
}
