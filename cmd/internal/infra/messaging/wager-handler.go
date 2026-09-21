/*
@Author: Franco Ribeiro Borba
@Description: Handler of the inbound queue. It is a translation layer and
nothing else: it parses the message, turns it into the same command the HTTP
entry point builds and calls the same use case, so an operation gets identical
rules and identical idempotency whichever way it arrived. What it does own is
the classification of failures, which decides what the consumer does next: a
message that breaks the contract or reuses an identifier with a different body
can never succeed and is marked permanent, so it travels to the dead letter
queue instead of being retried forever, while an unavailable database is
transient and comes back. A business rejection is neither: it was decided and
committed, so the message is done. The logs carry identifiers only, never
amounts or balances.
@Date : 20/09/2026
@Update: -
*/
package messaging

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"github.com/FrancoBorba/distributed-wallet-api/cmd/internal/config"
	"github.com/FrancoBorba/distributed-wallet-api/cmd/internal/events"
	"github.com/FrancoBorba/distributed-wallet-api/cmd/internal/infra/metrics"
	"github.com/FrancoBorba/distributed-wallet-api/cmd/internal/usecase"
	"github.com/google/uuid"
)

// messageNamespace turns the messageId a provider chose, which is an arbitrary
// string, into a stable identifier for the causation chain. The same message
// always produces the same value, so a redelivery does not look like a new
// cause for the events it produces.
var messageNamespace = uuid.MustParse("6f1e6a5c-0c4a-4f9a-9a1e-4f2b0d3c7a10")

// WagerHandler applies provider operations received over SQS.
type WagerHandler struct {
	processor *usecase.ProcessWagerTransaction
	logger    *slog.Logger
	metrics   *metrics.Metrics
	consumer  string
}

// NewWagerHandler builds the handler of the inbound queue.
func NewWagerHandler(processor *usecase.ProcessWagerTransaction, logger *slog.Logger, recorder *metrics.Metrics, cfg config.Consumer) *WagerHandler {
	return &WagerHandler{
		processor: processor,
		logger:    logger,
		metrics:   recorder,
		consumer:  cfg.Name,
	}
}

// Handle parses one message and applies the operation it asks for.
func (h *WagerHandler) Handle(ctx context.Context, body []byte) error {
	started := time.Now()

	message, err := events.ParseInbound(body)
	if err != nil {
		// A body that is not a valid operation will not become one on a
		// redelivery, so it is permanent by definition.
		return fmt.Errorf("%w: %v", ErrPermanentMessage, err)
	}

	data := message.Data()
	causationID := uuid.NewSHA1(messageNamespace, []byte(message.MessageID()))

	command := usecase.WagerCommand{
		ProviderID:            data.ProviderID,
		ExternalTransactionID: data.ExternalTransactionID,
		IdempotencyKey:        data.IdempotencyKey,
		PayloadHash:           message.PayloadHash(),
		PlayerID:              data.PlayerID,
		WalletID:              data.WalletID,
		RoundID:               data.RoundID,
		GameID:                data.GameID,
		Kind:                  data.Kind,
		Money:                 data.Money,
		ReferenceExternalID:   data.ReferenceExternalTransactionID,
		Metadata:              message.Metadata(causationID),
	}

	logger := h.logger.With(
		slog.String("messageId", message.MessageID()),
		slog.String("correlationId", command.Metadata.CorrelationID.String()),
		slog.String("providerId", data.ProviderID),
		slog.String("externalTransactionId", data.ExternalTransactionID),
		slog.String("walletId", data.WalletID.String()),
		slog.String("kind", string(data.Kind)),
	)

	result, err := h.processor.ExecuteFromMessage(ctx, h.consumer, command, message.MessageID(), message.PayloadHash())
	if err != nil {
		return h.classify(err, logger)
	}

	h.record(result, string(data.Kind), time.Since(started))

	logger.Info("wager operation handled",
		slog.String("transactionId", result.TransactionID.String()),
		slog.String("state", string(result.State)),
		slog.String("failureCode", string(result.FailureCode)),
		slog.Bool("replayed", result.Replayed),
	)

	return nil
}

// record reports what happened to the metrics.
//
// A redelivery caught by the inbox carries no state, because nothing was
// applied: it is counted as a duplicate and nothing else, which keeps the
// operation counters describing movements rather than deliveries.
func (h *WagerHandler) record(result usecase.WagerResult, kind string, elapsed time.Duration) {
	if result.Replayed {
		reason := "business-identity"
		if result.State == "" {
			reason = "inbox"
		}

		h.metrics.ObserveDuplicate(metrics.TransportSQS, reason)
	}

	state := string(result.State)
	if state == "" {
		state = "REPLAYED"
	}

	h.metrics.ObserveOperation(metrics.TransportSQS, kind, state, elapsed)
}

// classify decides whether the consumer should retry the message or let it
// reach the dead letter queue.
func (h *WagerHandler) classify(err error, logger *slog.Logger) error {
	switch {
	// The caller made a mistake that repeating cannot fix: the same key or the
	// same message id carrying a different operation, or a command that does
	// not describe one at all.
	case errors.Is(err, usecase.ErrPayloadConflict),
		errors.Is(err, usecase.ErrMessageConflict),
		errors.Is(err, usecase.ErrInvalidCommand):
		logger.Error("message refused by contract", slog.String("error", err.Error()))
		h.metrics.ObserveConflict("contract")

		return fmt.Errorf("%w: %v", ErrPermanentMessage, err)

	default:
		// Everything else, including a wallet moved by a concurrent writer
		// after the retries were exhausted, is worth another delivery.
		return err
	}
}
