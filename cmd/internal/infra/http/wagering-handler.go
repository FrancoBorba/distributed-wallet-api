/*
@Author: Franco Ribeiro Borba
@Description: Wagering endpoints, the HTTP half of the financial entry point.
The handler reads the raw body once and hashes it before parsing anything,
because the hash has to cover exactly the bytes the caller sent: that is what
makes an operation submitted over HTTP and the same operation delivered over
SQS produce the same value and be recognised as one movement instead of two.
The Idempotency-Key header is mandatory and is used exactly as received, never
recomputed, so a caller is always answered about the key it actually presented.
Authorization is enforced on the way in and on the way out: a provider may only
submit operations under its own identity, and may only read transactions that
belong to it, including replays.
@Date : 20/09/2026
@Update: -
*/
package http

import (
	"errors"
	"io"
	"log/slog"
	"net/http"
	"strings"
	"time"

	"github.com/FrancoBorba/distributed-wallet-api/cmd/internal/domain"
	"github.com/FrancoBorba/distributed-wallet-api/cmd/internal/events"
	"github.com/FrancoBorba/distributed-wallet-api/cmd/internal/infra/metrics"
	"github.com/FrancoBorba/distributed-wallet-api/cmd/internal/pkg/idempotency"
	"github.com/FrancoBorba/distributed-wallet-api/cmd/internal/usecase"
	"github.com/google/uuid"
)

// maxBodySize bounds a request body. An operation is a small document, and
// refusing a large one early keeps a single caller from exhausting memory.
const maxBodySize = 64 << 10

// WageringHandler serves the provider operations.
type WageringHandler struct {
	processor *usecase.ProcessWagerTransaction
	queries   usecase.TransactionQueries
	logger    *slog.Logger
	metrics   *metrics.Metrics
}

// NewWageringHandler builds the handler.
func NewWageringHandler(processor *usecase.ProcessWagerTransaction, queries usecase.TransactionQueries, logger *slog.Logger, recorder *metrics.Metrics) *WageringHandler {
	return &WageringHandler{processor: processor, queries: queries, logger: logger, metrics: recorder}
}

// Submit godoc
//
//	@Summary		Submit a provider operation
//	@Description	Applies a BET, WIN, LOSS, REFUND or ROLLBACK to a wallet. The Idempotency-Key header is mandatory and is stored as received. Replaying the same key with an equivalent body returns the stored result with idempotentReplay true, and the balance it answers with is the one observed when the operation was originally processed, even if the wallet has moved since. Reusing the key with a different body is a conflict. A reversal whose reference has not arrived yet is accepted and answered with 202, and a worker resolves it later.
//	@Tags			wagering
//	@Security		BearerAuth
//	@Accept			json
//	@Produce		json
//	@Param			Idempotency-Key	header		string			true	"Idempotency key, e.g. {providerId}:{externalTransactionId}"	default(provider-a:transaction-123)
//	@Param			request			body		WagerRequest	true	"Operation"
//	@Success		200				{object}	WagerResponse	"Processed, or an idempotent replay of a processed operation"
//	@Success		202				{object}	WagerResponse	"Accepted and waiting for the transaction it reverses"
//	@Failure		400				{object}	ErrorResponse	"Invalid input or missing Idempotency-Key"
//	@Failure		401				{object}	ErrorResponse	"No valid credential"
//	@Failure		403				{object}	ErrorResponse	"Acting on behalf of another provider"
//	@Failure		409				{object}	ErrorResponse	"Key reused with a different payload"
//	@Failure		422				{object}	WagerResponse	"Refused by a business rule, with a stable failureCode"
//	@Failure		503				{object}	ErrorResponse	"Transient unavailability, send the same request again"
//	@Router			/wagering/transactions [post]
func (h *WageringHandler) Submit(w http.ResponseWriter, r *http.Request) {
	started := time.Now()

	idempotencyKey := strings.TrimSpace(r.Header.Get("Idempotency-Key"))
	if idempotencyKey == "" {
		writeError(w, http.StatusBadRequest, CodeMissingKey, "the Idempotency-Key header is required")

		return
	}

	// The raw bytes are read first: the hash must cover what the caller sent,
	// not a re-serialization of what we understood.
	body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, maxBodySize))
	if err != nil {
		writeError(w, http.StatusBadRequest, CodeInvalidRequest, "the body could not be read")

		return
	}

	payloadHash, err := idempotency.CanonicalHash(body)
	if err != nil {
		writeError(w, http.StatusBadRequest, CodeInvalidRequest, err.Error())

		return
	}

	// Parsing reuses the inbound message contract, so HTTP and SQS validate
	// the operation in exactly the same way and cannot drift apart.
	var request WagerRequest
	if err := decodeJSON(body, &request); err != nil {
		writeError(w, http.StatusBadRequest, CodeInvalidRequest, "the body is not valid JSON")

		return
	}

	command, err := request.toCommand(idempotencyKey, payloadHash, correlationOf(r.Context()))
	if err != nil {
		writeError(w, http.StatusBadRequest, CodeInvalidRequest, err.Error())

		return
	}

	// A provider cannot submit an operation under somebody else's name.
	if !identityOf(r.Context()).mayActAs(command.ProviderID) {
		writeError(w, http.StatusForbidden, CodeForbidden, "the credential does not allow acting for this provider")

		return
	}

	result, err := h.processor.Execute(r.Context(), command)
	if err != nil {
		if errors.Is(err, usecase.ErrPayloadConflict) {
			h.metrics.ObserveConflict("idempotency")
		}
		if errors.Is(err, usecase.ErrConcurrentUpdate) {
			h.metrics.ObserveConflict("wallet-contention")
		}

		fail(w, h.logger, err)

		return
	}

	h.metrics.ObserveOperation(metrics.TransportHTTP, string(command.Kind), string(result.State), time.Since(started))

	if result.Replayed {
		h.metrics.ObserveDuplicate(metrics.TransportHTTP, "business-identity")
	}

	h.logger.Info("wager operation submitted",
		slog.String("transactionId", result.TransactionID.String()),
		slog.String("state", string(result.State)),
		slog.String("providerId", command.ProviderID),
		slog.String("externalTransactionId", command.ExternalTransactionID),
		slog.String("walletId", command.WalletID.String()),
		slog.Bool("idempotentReplay", result.Replayed),
		slog.String("correlationId", command.Metadata.CorrelationID.String()))

	writeJSON(w, statusForResult(result), wagerResponseOf(result))
}

// GetByID godoc
//
//	@Summary		Read a transaction by its internal identifier
//	@Description	Lets a provider follow a pending operation and read the failure code of a refused one. A provider only ever sees its own transactions.
//	@Tags			wagering
//	@Security		BearerAuth
//	@Produce		json
//	@Param			transactionId	path		string	true	"Internal transaction identifier"
//	@Success		200				{object}	TransactionResponse
//	@Failure		400				{object}	ErrorResponse
//	@Failure		401				{object}	ErrorResponse
//	@Failure		403				{object}	ErrorResponse
//	@Failure		404				{object}	ErrorResponse
//	@Router			/wagering/transactions/{transactionId} [get]
func (h *WageringHandler) GetByID(w http.ResponseWriter, r *http.Request) {
	transactionID, err := uuid.Parse(r.PathValue("transactionId"))
	if err != nil {
		writeError(w, http.StatusBadRequest, CodeInvalidRequest, "transactionId must be a UUID")

		return
	}

	transaction, err := h.queries.FindTransaction(r.Context(), transactionID)
	if err != nil {
		fail(w, h.logger, err)

		return
	}

	// Answering 404 rather than 403 is deliberate: a provider must not be able
	// to learn that a transaction exists by probing identifiers.
	if !identityOf(r.Context()).mayActAs(transaction.ProviderID()) {
		writeError(w, http.StatusNotFound, CodeNotFound, "transaction not found")

		return
	}

	writeJSON(w, http.StatusOK, transactionResponseOf(transaction))
}

// GetByProvider godoc
//
//	@Summary		Read a transaction the way the provider knows it
//	@Description	Looks the operation up by the identifier the provider gave it, which is how a provider checks the outcome of something it sent without having stored our internal identifier.
//	@Tags			wagering
//	@Security		BearerAuth
//	@Produce		json
//	@Param			providerId				path		string	true	"Provider"			default(provider-a)
//	@Param			externalTransactionId	path		string	true	"Identifier given by the provider"
//	@Success		200						{object}	TransactionResponse
//	@Failure		401						{object}	ErrorResponse
//	@Failure		403						{object}	ErrorResponse
//	@Failure		404						{object}	ErrorResponse
//	@Router			/providers/{providerId}/wagering/transactions/{externalTransactionId} [get]
func (h *WageringHandler) GetByProvider(w http.ResponseWriter, r *http.Request) {
	providerID := r.PathValue("providerId")
	externalID := r.PathValue("externalTransactionId")

	if !identityOf(r.Context()).mayActAs(providerID) {
		writeError(w, http.StatusForbidden, CodeForbidden, "the credential does not allow reading this provider")

		return
	}

	transaction, err := h.queries.FindTransactionByProvider(r.Context(), providerID, externalID)
	if err != nil {
		fail(w, h.logger, err)

		return
	}

	writeJSON(w, http.StatusOK, transactionResponseOf(transaction))
}

// toCommand validates the request and turns it into the command the use case
// takes. The shape is checked here; the rules of each kind are checked by the
// domain, which both transports share.
func (q WagerRequest) toCommand(idempotencyKey, payloadHash string, correlationID uuid.UUID) (usecase.WagerCommand, error) {
	playerID, err := uuid.Parse(q.PlayerID)
	if err != nil {
		return usecase.WagerCommand{}, errors.New("playerId must be a UUID")
	}

	walletID, err := uuid.Parse(q.WalletID)
	if err != nil {
		return usecase.WagerCommand{}, errors.New("walletId must be a UUID")
	}

	money, err := q.Money.parse()
	if err != nil {
		return usecase.WagerCommand{}, err
	}

	kind := domain.TransactionKind(q.Kind)
	if !kind.IsValid() {
		return usecase.WagerCommand{}, errors.New("kind must be one of BET, WIN, LOSS, REFUND, ROLLBACK")
	}

	// OPENING belongs to the internal opening of a wallet and is refused by
	// the contract itself, exactly as it is on the SQS side.
	if !kind.IsExternal() {
		return usecase.WagerCommand{}, domain.ErrInvalidExternalKind
	}

	return usecase.WagerCommand{
		ProviderID:            strings.TrimSpace(q.ProviderID),
		ExternalTransactionID: strings.TrimSpace(q.ExternalTransactionID),
		IdempotencyKey:        idempotencyKey,
		PayloadHash:           payloadHash,
		PlayerID:              playerID,
		WalletID:              walletID,
		RoundID:               strings.TrimSpace(q.RoundID),
		GameID:                strings.TrimSpace(q.GameID),
		Kind:                  kind,
		Money:                 money,
		ReferenceExternalID:   strings.TrimSpace(q.ReferenceExternalTransactionID),
		Metadata:              events.NewMetadata(correlationID),
	}, nil
}
