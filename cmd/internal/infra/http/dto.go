/*
@Author: Franco Ribeiro Borba
@Description: Wire types of the HTTP API. They exist as their own structs
instead of exposing the domain directly for two reasons. The published contract
becomes explicit and reviewable in one file, so a field is never renamed by
accident when an aggregate changes; and money crosses the boundary as the
decimal string pair the contract defines, parsed back through ParseMoney, which
applies exactly the same validation an SQS message goes through. Nothing here
carries a floating point number or an amount in minor units, and no response
ever exposes a field the caller has no business seeing.
@Date : 20/09/2026
@Update: -
*/
package http

import (
	"github.com/FrancoBorba/distributed-wallet-api/cmd/internal/domain"
	"github.com/FrancoBorba/distributed-wallet-api/cmd/internal/usecase"
)

// Money is a monetary value on the wire: a decimal string with a fixed scale
// of two and an ISO 4217 code. It is never a number, so no client can lose
// precision parsing it.
type Money struct {
	Amount   string `json:"amount" example:"25.00"`
	Currency string `json:"currency" example:"BRL"`
} // @name Money

// parse turns the wire value into the domain value, applying the canonical
// format rules of the contract.
func (m Money) parse() (domain.Money, error) {
	return domain.ParseMoney(m.Amount, m.Currency)
}

// moneyOf renders a domain value on the wire.
func moneyOf(value domain.Money) Money {
	return Money{Amount: value.AmountString(), Currency: value.Currency()}
}

// OpenWalletRequest opens a wallet for a player. The currency of the wallet is
// the currency of the initial balance, and "0.00" is accepted.
type OpenWalletRequest struct {
	PlayerID       string `json:"playerId" example:"0192f28f-5dc0-7d58-bdb2-814ad6a0f4a1"`
	InitialBalance Money  `json:"initialBalance"`
} // @name OpenWalletRequest

// WalletResponse is a wallet as the API reports it.
type WalletResponse struct {
	ID       string `json:"id" example:"0192f291-27dd-7d3f-8071-5f8685deef37"`
	PlayerID string `json:"playerId" example:"0192f28f-5dc0-7d58-bdb2-814ad6a0f4a1"`
	Balance  Money  `json:"balance"`
	Version  int64  `json:"version" example:"1"`
} // @name WalletResponse

// walletResponseOf renders a wallet.
func walletResponseOf(wallet *domain.Wallet) WalletResponse {
	return WalletResponse{
		ID:       wallet.ID().String(),
		PlayerID: wallet.PlayerID().String(),
		Balance:  moneyOf(wallet.Balance()),
		Version:  wallet.Version(),
	}
}

// WagerRequest is one provider operation. For REFUND and ROLLBACK,
// referenceExternalTransactionId is mandatory and names what is being undone.
type WagerRequest struct {
	ProviderID                     string `json:"providerId" example:"provider-a"`
	ExternalTransactionID          string `json:"externalTransactionId" example:"transaction-123"`
	PlayerID                       string `json:"playerId" example:"0192f28f-5dc0-7d58-bdb2-814ad6a0f4a1"`
	WalletID                       string `json:"walletId" example:"0192f291-27dd-7d3f-8071-5f8685deef37"`
	RoundID                        string `json:"roundId" example:"round-987"`
	GameID                         string `json:"gameId" example:"fortune-chimp"`
	Kind                           string `json:"kind" example:"BET" enums:"BET,WIN,LOSS,REFUND,ROLLBACK"`
	Money                          Money  `json:"money"`
	ReferenceExternalTransactionID string `json:"referenceExternalTransactionId,omitempty" example:""`
} // @name WagerRequest

// WagerResponse is the outcome of an operation. Balance is present when the
// operation concluded successfully, and on a replay it is the balance observed
// when the money actually moved, not the current one. FailureCode is present
// only on a refusal.
type WagerResponse struct {
	TransactionID    string `json:"transactionId" example:"0192f298-345e-7e38-af88-e43f851a819d"`
	Status           string `json:"status" example:"PROCESSED" enums:"PROCESSED,REJECTED,PENDING_REFERENCE,FAILED"`
	Balance          *Money `json:"balance,omitempty"`
	FailureCode      string `json:"failureCode,omitempty" example:""`
	IdempotentReplay bool   `json:"idempotentReplay" example:"false"`
} // @name WagerResponse

// wagerResponseOf renders the result of an operation.
func wagerResponseOf(result usecase.WagerResult) WagerResponse {
	response := WagerResponse{
		TransactionID:    result.TransactionID.String(),
		Status:           string(result.State),
		FailureCode:      string(result.FailureCode),
		IdempotentReplay: result.Replayed,
	}

	// A refusal stores no balance, so the field is absent rather than zero,
	// which would read as "the wallet is empty".
	if result.Balance.Currency() != "" {
		balance := moneyOf(result.Balance)
		response.Balance = &balance
	}

	return response
}

// TransactionResponse is the full record of an operation, used by the queries
// that let a provider follow a pending operation or read why one was refused.
type TransactionResponse struct {
	TransactionID                  string `json:"transactionId"`
	WalletID                       string `json:"walletId"`
	PlayerID                       string `json:"playerId"`
	ProviderID                     string `json:"providerId,omitempty"`
	ExternalTransactionID          string `json:"externalTransactionId,omitempty"`
	RoundID                        string `json:"roundId,omitempty"`
	GameID                         string `json:"gameId,omitempty"`
	Kind                           string `json:"kind" example:"BET"`
	Money                          Money  `json:"money"`
	Status                         string `json:"status" example:"PROCESSED"`
	FailureCode                    string `json:"failureCode,omitempty"`
	ResultBalance                  *Money `json:"resultBalance,omitempty"`
	ReferenceExternalTransactionID string `json:"referenceExternalTransactionId,omitempty"`
	ReferenceTransactionID         string `json:"referenceTransactionId,omitempty"`
	CreatedAt                      string `json:"createdAt" example:"2026-09-20T12:00:00Z"`
	UpdatedAt                      string `json:"updatedAt" example:"2026-09-20T12:00:00Z"`
} // @name TransactionResponse

// transactionResponseOf renders a transaction.
func transactionResponseOf(transaction *domain.WagerTransaction) TransactionResponse {
	response := TransactionResponse{
		TransactionID:         transaction.ID().String(),
		WalletID:              transaction.WalletID().String(),
		PlayerID:              transaction.PlayerID().String(),
		ProviderID:            transaction.ProviderID(),
		ExternalTransactionID: transaction.ExternalTransactionID(),
		RoundID:               transaction.RoundID(),
		GameID:                transaction.GameID(),
		Kind:                  string(transaction.Kind()),
		Money:                 moneyOf(transaction.Money()),
		Status:                string(transaction.State()),
		CreatedAt:             transaction.CreatedAt().Format(timeFormat),
		UpdatedAt:             transaction.UpdatedAt().Format(timeFormat),
	}

	if code, ok := transaction.FailureCode(); ok {
		response.FailureCode = string(code)
	}
	if balance, ok := transaction.ResultBalance(); ok {
		rendered := moneyOf(balance)
		response.ResultBalance = &rendered
	}
	if reference, ok := transaction.ReferenceExternalID(); ok {
		response.ReferenceExternalTransactionID = reference
	}
	if reference, ok := transaction.ReferenceTransactionID(); ok {
		response.ReferenceTransactionID = reference.String()
	}

	return response
}

// LedgerEntryResponse is one immutable line of the wallet history.
type LedgerEntryResponse struct {
	ID            string `json:"id"`
	TransactionID string `json:"transactionId"`
	Direction     string `json:"direction" example:"DEBIT" enums:"DEBIT,CREDIT"`
	Money         Money  `json:"money"`
	BalanceBefore Money  `json:"balanceBefore"`
	BalanceAfter  Money  `json:"balanceAfter"`
	CreatedAt     string `json:"createdAt" example:"2026-09-20T12:00:00Z"`
} // @name LedgerEntryResponse

// LedgerPageResponse is a page of the history. NextCursor is opaque: it is a
// token to send back, never a value to interpret, which keeps the ordering an
// implementation detail the API is free to change.
type LedgerPageResponse struct {
	WalletID   string                `json:"walletId"`
	Entries    []LedgerEntryResponse `json:"entries"`
	NextCursor string                `json:"nextCursor,omitempty"`
} // @name LedgerPageResponse

// ledgerEntryResponseOf renders one entry.
func ledgerEntryResponseOf(entry *domain.WalletLedgerEntry) LedgerEntryResponse {
	return LedgerEntryResponse{
		ID:            entry.ID().String(),
		TransactionID: entry.TransactionID().String(),
		Direction:     string(entry.Direction()),
		Money:         moneyOf(entry.Amount()),
		BalanceBefore: moneyOf(entry.BalanceBefore()),
		BalanceAfter:  moneyOf(entry.BalanceAfter()),
		CreatedAt:     entry.CreatedAt().Format(timeFormat),
	}
}

// ReconciliationResponse compares the stored balance against the one rebuilt
// from the ledger. Difference is the stored balance minus the calculated one,
// so a positive value means the wallet holds more than its history justifies.
type ReconciliationResponse struct {
	WalletID          string `json:"walletId"`
	StoredBalance     Money  `json:"storedBalance"`
	CalculatedBalance Money  `json:"calculatedBalance"`
	Difference        Money  `json:"difference"`
	Consistent        bool   `json:"consistent" example:"true"`
	CheckedEntries    int    `json:"checkedEntries" example:"2"`
} // @name ReconciliationResponse

// reconciliationResponseOf renders a reconciliation.
func reconciliationResponseOf(result usecase.Reconciliation) ReconciliationResponse {
	return ReconciliationResponse{
		WalletID:          result.WalletID.String(),
		StoredBalance:     moneyOf(result.StoredBalance),
		CalculatedBalance: moneyOf(result.CalculatedBalance),
		Difference:        moneyOf(result.Difference),
		Consistent:        result.Consistent,
		CheckedEntries:    result.CheckedEntries,
	}
}

// ErrorResponse is the single error shape of the API. Code is stable and meant
// to be branched on by a client; message is for a human reading a log.
type ErrorResponse struct {
	Code    string `json:"code" example:"IDEMPOTENCY_CONFLICT"`
	Message string `json:"message" example:"idempotency key was already used with a different payload"`
} // @name ErrorResponse

// HealthResponse is the answer of the probes. Checks is empty on liveness,
// which only reports that the process is running.
type HealthResponse struct {
	Status string            `json:"status" example:"ok" enums:"ok,degraded"`
	Checks map[string]string `json:"checks,omitempty"`
} // @name HealthResponse
