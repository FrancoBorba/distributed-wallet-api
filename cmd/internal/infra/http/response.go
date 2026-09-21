/*
@Author: Franco Ribeiro Borba
@Description: The error contract of the API. The challenge requires invalid
input, conflict, business rejection, pending processing and transient
unavailability to be distinguishable, and this file is where that distinction
is decided once for every endpoint. The rule behind the table is what the
caller should do next: 4xx means the request has to change before it is worth
sending again, 409 means the operation exists but not the way you described it,
422 means the request was understood and refused by a business rule with a
stable code to branch on, 202 means the answer is not ready yet, and 503 means
send the same thing again later. A rejection is never a 500: it is a decision
the service took and committed, and it comes back with the failure code that
explains it.
@Date : 20/09/2026
@Update: -
*/
package http

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"time"

	"github.com/FrancoBorba/distributed-wallet-api/cmd/internal/domain"
	"github.com/FrancoBorba/distributed-wallet-api/cmd/internal/usecase"
)

// timeFormat is how every instant is rendered: RFC 3339 in UTC.
const timeFormat = time.RFC3339Nano

// Error codes of the contract. They are stable, so a client may branch on
// them, and they never leak an internal message.
const (
	CodeInvalidRequest    = "INVALID_REQUEST"
	CodeMissingKey        = "MISSING_IDEMPOTENCY_KEY"
	CodeUnauthenticated   = "UNAUTHENTICATED"
	CodeForbidden         = "FORBIDDEN"
	CodeNotFound          = "NOT_FOUND"
	CodeIdempotencyClash  = "IDEMPOTENCY_CONFLICT"
	CodeWalletExists      = "WALLET_ALREADY_EXISTS"
	CodeUnavailable       = "TEMPORARILY_UNAVAILABLE"
	CodeInternal          = "INTERNAL_ERROR"
	CodeMessageConflict   = "MESSAGE_CONFLICT"
	CodeBusinessRejection = "BUSINESS_REJECTION"
)

// writeJSON sends a body with a status.
func writeJSON(w http.ResponseWriter, status int, body any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)

	if body == nil {
		return
	}

	// The status line is already sent, so a failure here can only be logged by
	// the caller's transport; there is no way to change the answer now.
	_ = json.NewEncoder(w).Encode(body)
}

// writeError sends an error in the single shape of the contract.
func writeError(w http.ResponseWriter, status int, code, message string) {
	writeJSON(w, status, ErrorResponse{Code: code, Message: message})
}

// fail maps an error to the contract and answers with it. A 5xx is logged with
// its cause, because it is the only class the caller cannot act upon and the
// only one that means the service itself misbehaved.
func fail(w http.ResponseWriter, logger *slog.Logger, err error) {
	status, code := classify(err)

	if status >= http.StatusInternalServerError {
		logger.Error("request failed", slog.String("error", err.Error()), slog.String("code", code))

		// An internal failure never echoes its message: it may carry a query,
		// a constraint name or a connection string.
		writeError(w, status, code, "the request could not be completed")

		return
	}

	writeError(w, status, code, err.Error())
}

// classify is the mapping itself.
//
//	400 the request cannot be understood or breaks the contract
//	404 the wallet or the transaction does not exist
//	409 the operation exists, but not as this request describes it
//	503 nothing is wrong with the request, the service cannot answer now
//	500 anything we did not foresee
func classify(err error) (int, string) {
	switch {
	case errors.Is(err, usecase.ErrPayloadConflict):
		return http.StatusConflict, CodeIdempotencyClash

	case errors.Is(err, usecase.ErrMessageConflict):
		return http.StatusConflict, CodeMessageConflict

	case errors.Is(err, usecase.ErrWalletAlreadyExists):
		return http.StatusConflict, CodeWalletExists

	case errors.Is(err, usecase.ErrWalletNotFound), errors.Is(err, usecase.ErrTransactionNotFound):
		return http.StatusNotFound, CodeNotFound

	// The wallet kept moving under every retry. The request is valid and the
	// caller should send it again, which is exactly what 503 means.
	case errors.Is(err, usecase.ErrConcurrentUpdate):
		return http.StatusServiceUnavailable, CodeUnavailable

	case errors.Is(err, context.DeadlineExceeded), errors.Is(err, context.Canceled):
		return http.StatusServiceUnavailable, CodeUnavailable

	case isBadRequest(err):
		return http.StatusBadRequest, CodeInvalidRequest

	default:
		return http.StatusInternalServerError, CodeInternal
	}
}

// isBadRequest reports whether the error is the caller describing an operation
// that cannot exist. These come from the command validation and from the domain
// constructors, which are the two places a malformed request is caught.
func isBadRequest(err error) bool {
	for _, sentinel := range []error{
		usecase.ErrInvalidCommand,
		domain.ErrInvalidAmount,
		domain.ErrNegativeAmount,
		domain.ErrInvalidCurrencyFormat,
		domain.ErrUninitializedMoney,
		domain.ErrUnknownTransactionKind,
		domain.ErrInvalidExternalKind,
		domain.ErrInvalidTransactionAmount,
		domain.ErrMissingReference,
		domain.ErrReferenceNotApplicable,
		domain.ErrMissingExternalMetadata,
		domain.ErrInvalidWalletID,
		domain.ErrInvalidPlayerID,
		domain.ErrNegativeInitialBalance,
	} {
		if errors.Is(err, sentinel) {
			return true
		}
	}

	return false
}

// statusForResult maps the outcome of an operation to a status code. The three
// outcomes are genuinely different answers and the contract keeps them apart:
// a conclusion, a refusal that will never change, and an operation that is
// waiting for something that has not arrived.
func statusForResult(result usecase.WagerResult) int {
	switch result.State {
	case domain.StateProcessed:
		return http.StatusOK

	// Understood, applied no money, and refused for a documented reason.
	case domain.StateRejected, domain.StateFailed:
		return http.StatusUnprocessableEntity

	// Accepted and durably stored, waiting for the transaction it reverses.
	// The provider polls the transaction endpoint to follow it.
	case domain.StatePendingRef, domain.StatePending:
		return http.StatusAccepted

	default:
		return http.StatusOK
	}
}

// decodeJSON parses a body that was already read. Unknown fields are accepted,
// so a provider that adds a field of its own is not refused over something the
// contract does not use anyway.
func decodeJSON(body []byte, target any) error {
	return json.Unmarshal(body, target)
}
