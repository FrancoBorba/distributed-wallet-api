/*
@Author: Franco Ribeiro Borba
@Description: Unit tests for the HTTP contract and the isolation rules
*/
package http

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/FrancoBorba/distributed-wallet-api/cmd/internal/domain"
	"github.com/FrancoBorba/distributed-wallet-api/cmd/internal/infra/metrics"
	"github.com/FrancoBorba/distributed-wallet-api/cmd/internal/usecase"
	"github.com/google/uuid"
	"github.com/prometheus/client_golang/prometheus"
)

// discardLogger keeps the test output readable.
func discardLogger() *slog.Logger {
	return slog.New(slog.NewJSONHandler(io.Discard, nil))
}

// --------------------------------------------------------------------------
// Contrato de erros
// --------------------------------------------------------------------------

// As cinco situações que o desafio exige que sejam distinguíveis pelo contrato
// precisam cair em códigos diferentes, senão o cliente não consegue decidir o
// que fazer em seguida.
func TestClassify(t *testing.T) {
	tests := []struct {
		name       string
		err        error
		wantStatus int
		wantCode   string
	}{
		{"entrada inválida", usecase.ErrInvalidCommand, http.StatusBadRequest, CodeInvalidRequest},
		{"dinheiro malformado", domain.ErrInvalidAmount, http.StatusBadRequest, CodeInvalidRequest},
		{"moeda inválida", domain.ErrInvalidCurrencyFormat, http.StatusBadRequest, CodeInvalidRequest},
		{"conflito de idempotência", usecase.ErrPayloadConflict, http.StatusConflict, CodeIdempotencyClash},
		{"carteira duplicada", usecase.ErrWalletAlreadyExists, http.StatusConflict, CodeWalletExists},
		{"carteira inexistente", usecase.ErrWalletNotFound, http.StatusNotFound, CodeNotFound},
		{"transação inexistente", usecase.ErrTransactionNotFound, http.StatusNotFound, CodeNotFound},
		{"disputa pela carteira", usecase.ErrConcurrentUpdate, http.StatusServiceUnavailable, CodeUnavailable},
		{"prazo estourado", context.DeadlineExceeded, http.StatusServiceUnavailable, CodeUnavailable},
		{"desconhecido", errors.New("boom"), http.StatusInternalServerError, CodeInternal},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			status, code := classify(tc.err)
			if status != tc.wantStatus || code != tc.wantCode {
				t.Errorf("classify = %d/%s, quero %d/%s", status, code, tc.wantStatus, tc.wantCode)
			}
		})
	}
}

// Uma falha interna nunca pode ecoar a mensagem: ela carrega query, nome de
// constraint ou string de conexão.
func TestFail_NaoVazaDetalheInterno(t *testing.T) {
	recorder := httptest.NewRecorder()

	fail(recorder, discardLogger(), errors.New("pq: password authentication failed for user wager_user"))

	if recorder.Code != http.StatusInternalServerError {
		t.Fatalf("status = %d, quero 500", recorder.Code)
	}

	var body ErrorResponse
	if err := json.Unmarshal(recorder.Body.Bytes(), &body); err != nil {
		t.Fatalf("resposta não é JSON: %v", err)
	}
	if body.Message == "" || body.Message == "pq: password authentication failed for user wager_user" {
		t.Errorf("message = %q, não pode repetir o erro interno", body.Message)
	}
}

func TestStatusForResult(t *testing.T) {
	tests := []struct {
		state domain.TransactionState
		want  int
	}{
		{domain.StateProcessed, http.StatusOK},
		{domain.StateRejected, http.StatusUnprocessableEntity},
		{domain.StateFailed, http.StatusUnprocessableEntity},
		{domain.StatePendingRef, http.StatusAccepted},
		{domain.StatePending, http.StatusAccepted},
	}

	for _, tc := range tests {
		t.Run(string(tc.state), func(t *testing.T) {
			got := statusForResult(usecase.WagerResult{State: tc.state})
			if got != tc.want {
				t.Errorf("status de %s = %d, quero %d", tc.state, got, tc.want)
			}
		})
	}
}

// Uma rejeição não guarda saldo. Devolver "0.00" leria como carteira vazia.
func TestWagerResponseOf_RejeicaoNaoInventaSaldo(t *testing.T) {
	response := wagerResponseOf(usecase.WagerResult{
		TransactionID: uuid.New(),
		State:         domain.StateRejected,
		FailureCode:   domain.FailureInsufficientFunds,
	})

	if response.Balance != nil {
		t.Errorf("balance = %v, quero ausente", response.Balance)
	}
	if response.FailureCode != string(domain.FailureInsufficientFunds) {
		t.Errorf("failureCode = %q, quero %q", response.FailureCode, domain.FailureInsufficientFunds)
	}
}

// --------------------------------------------------------------------------
// Cursor opaco
// --------------------------------------------------------------------------

func TestCursor(t *testing.T) {
	for _, sequence := range []int64{1, 42, 999999} {
		encoded := encodeCursor(sequence)

		// Opaco: o cliente não pode ler a chave de ordenação nele.
		if encoded == "42" || encoded == "1" {
			t.Errorf("cursor %q expõe a sequência", encoded)
		}

		decoded, err := decodeCursor(encoded)
		if err != nil {
			t.Fatalf("decodeCursor(%q) erro inesperado: %v", encoded, err)
		}
		if decoded != sequence {
			t.Errorf("decodeCursor = %d, quero %d", decoded, sequence)
		}
	}
}

func TestDecodeCursor_VazioEPrimeiraPagina(t *testing.T) {
	decoded, err := decodeCursor("")
	if err != nil || decoded != 0 {
		t.Errorf("cursor vazio = %d/%v, quero 0/nil", decoded, err)
	}

	if _, err := decodeCursor("não-é-um-cursor"); err == nil {
		t.Error("um cursor inválido deveria ser recusado")
	}
}

func TestParseLimit(t *testing.T) {
	tests := []struct {
		raw  string
		want int
	}{
		{"", defaultLedgerLimit},
		{"abc", defaultLedgerLimit},
		{"0", defaultLedgerLimit},
		{"-5", defaultLedgerLimit},
		{"10", 10},
		{"5000", maxLedgerLimit},
	}

	for _, tc := range tests {
		if got := parseLimit(tc.raw); got != tc.want {
			t.Errorf("parseLimit(%q) = %d, quero %d", tc.raw, got, tc.want)
		}
	}
}

// --------------------------------------------------------------------------
// Identidade e isolamento
// --------------------------------------------------------------------------

func TestIdentity_MayActAs(t *testing.T) {
	tests := []struct {
		name     string
		identity Identity
		provider string
		want     bool
	}{
		{"provedor age por si", Identity{ProviderID: "provider-a"}, "provider-a", true},
		{"provedor não age por outro", Identity{ProviderID: "provider-a"}, "provider-b", false},
		{"serviço interno age por qualquer um", Identity{Internal: true}, "provider-b", true},
		{"identidade vazia não age por ninguém", Identity{}, "", false},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := tc.identity.mayActAs(tc.provider); got != tc.want {
				t.Errorf("mayActAs(%q) = %v, quero %v", tc.provider, got, tc.want)
			}
		})
	}
}

func TestAuthenticated_SemCredencial(t *testing.T) {
	handler := authenticated(TrustedHeaderAuthenticator{}, func(http.ResponseWriter, *http.Request) {
		t.Error("o handler não deveria ser alcançado sem credencial")
	})

	recorder := httptest.NewRecorder()
	handler(recorder, httptest.NewRequest(http.MethodGet, "/wallets", nil))

	if recorder.Code != http.StatusUnauthorized {
		t.Errorf("status = %d, quero 401", recorder.Code)
	}
}

func TestInternalOnly_RecusaProvedor(t *testing.T) {
	handler := authenticated(TrustedHeaderAuthenticator{}, internalOnly(func(http.ResponseWriter, *http.Request) {
		t.Error("um provedor não pode alcançar uma operação de carteira")
	}))

	request := httptest.NewRequest(http.MethodPost, "/wallets", nil)
	request.Header.Set("X-Provider-Id", "provider-a")

	recorder := httptest.NewRecorder()
	handler(recorder, request)

	if recorder.Code != http.StatusForbidden {
		t.Errorf("status = %d, quero 403", recorder.Code)
	}
}

// --------------------------------------------------------------------------
// Leitura de transações
// --------------------------------------------------------------------------

// stubTransactions serves one fixed transaction.
type stubTransactions struct {
	transaction *domain.WagerTransaction
}

func (s stubTransactions) FindTransaction(context.Context, uuid.UUID) (*domain.WagerTransaction, error) {
	if s.transaction == nil {
		return nil, usecase.ErrTransactionNotFound
	}

	return s.transaction, nil
}

func (s stubTransactions) FindTransactionByProvider(context.Context, string, string) (*domain.WagerTransaction, error) {
	if s.transaction == nil {
		return nil, usecase.ErrTransactionNotFound
	}

	return s.transaction, nil
}

// transactionOf builds a PENDING bet of a provider.
func transactionOf(t *testing.T, providerID string) *domain.WagerTransaction {
	t.Helper()

	money, err := domain.ParseMoney("25.00", "BRL")
	if err != nil {
		t.Fatalf("ParseMoney erro inesperado: %v", err)
	}

	transaction, err := domain.NewExternalTransaction(domain.ExternalTransactionParams{
		ID:                    uuid.New(),
		WalletID:              uuid.New(),
		PlayerID:              uuid.New(),
		ProviderID:            providerID,
		ExternalTransactionID: "transaction-123",
		IdempotencyKey:        providerID + ":transaction-123",
		PayloadHash:           "1111111111111111111111111111111111111111111111111111111111111111",
		RoundID:               "round-987",
		GameID:                "fortune-chimp",
		Kind:                  domain.KindBet,
		Money:                 money,
		Now:                   time.Date(2026, 9, 20, 12, 0, 0, 0, time.UTC),
	})
	if err != nil {
		t.Fatalf("NewExternalTransaction erro inesperado: %v", err)
	}

	return transaction
}

// Este é o isolamento entre provedores, que é critério eliminatório. A resposta
// é 404 e não 403 de propósito: um provedor não pode descobrir que uma
// transação existe testando identificadores.
func TestGetByID_ProvedorNaoLeTransacaoDeOutro(t *testing.T) {
	transaction := transactionOf(t, "provider-a")
	handler := NewWageringHandler(nil, stubTransactions{transaction: transaction}, discardLogger(), testMetrics())

	tests := []struct {
		name       string
		provider   string
		wantStatus int
	}{
		{"o dono lê", "provider-a", http.StatusOK},
		{"outro provedor não lê", "provider-b", http.StatusNotFound},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			request := httptest.NewRequest(http.MethodGet, "/wagering/transactions/"+transaction.ID().String(), nil)
			request.Header.Set("X-Provider-Id", tc.provider)
			request.SetPathValue("transactionId", transaction.ID().String())

			recorder := httptest.NewRecorder()
			authenticated(TrustedHeaderAuthenticator{}, handler.GetByID)(recorder, request)

			if recorder.Code != tc.wantStatus {
				t.Errorf("status = %d, quero %d", recorder.Code, tc.wantStatus)
			}
		})
	}
}

func TestGetByProvider_RecusaOutroProvedor(t *testing.T) {
	handler := NewWageringHandler(nil, stubTransactions{transaction: transactionOf(t, "provider-a")}, discardLogger(), testMetrics())

	request := httptest.NewRequest(http.MethodGet, "/providers/provider-a/wagering/transactions/transaction-123", nil)
	request.Header.Set("X-Provider-Id", "provider-b")
	request.SetPathValue("providerId", "provider-a")
	request.SetPathValue("externalTransactionId", "transaction-123")

	recorder := httptest.NewRecorder()
	authenticated(TrustedHeaderAuthenticator{}, handler.GetByProvider)(recorder, request)

	if recorder.Code != http.StatusForbidden {
		t.Errorf("status = %d, quero 403", recorder.Code)
	}
}

// --------------------------------------------------------------------------
// Submissão
// --------------------------------------------------------------------------

// O header é obrigatório e o servidor nunca calcula uma chave no lugar dele.
func TestSubmit_SemIdempotencyKey(t *testing.T) {
	handler := NewWageringHandler(nil, stubTransactions{}, discardLogger(), testMetrics())

	request := httptest.NewRequest(http.MethodPost, "/wagering/transactions", nil)
	request.Header.Set("X-Provider-Id", "provider-a")

	recorder := httptest.NewRecorder()
	authenticated(TrustedHeaderAuthenticator{}, handler.Submit)(recorder, request)

	if recorder.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, quero 400", recorder.Code)
	}

	var body ErrorResponse
	if err := json.Unmarshal(recorder.Body.Bytes(), &body); err != nil {
		t.Fatalf("resposta não é JSON: %v", err)
	}
	if body.Code != CodeMissingKey {
		t.Errorf("code = %q, quero %q", body.Code, CodeMissingKey)
	}
}

// --------------------------------------------------------------------------
// Probes
// --------------------------------------------------------------------------

func TestHealth(t *testing.T) {
	failing := []Check{
		{Name: "postgres", Probe: func(context.Context) error { return nil }},
		{Name: "sqs", Probe: func(context.Context) error { return errors.New("connection refused") }},
	}

	handler := NewHealthHandler(failing, discardLogger())

	// Liveness não toca em dependência: reiniciar o processo não conserta um
	// banco fora do ar.
	live := httptest.NewRecorder()
	handler.Live(live, httptest.NewRequest(http.MethodGet, "/health/live", nil))
	if live.Code != http.StatusOK {
		t.Errorf("liveness = %d, quero 200 mesmo com dependência fora", live.Code)
	}

	ready := httptest.NewRecorder()
	handler.Ready(ready, httptest.NewRequest(http.MethodGet, "/health/ready", nil))
	if ready.Code != http.StatusServiceUnavailable {
		t.Errorf("readiness = %d, quero 503", ready.Code)
	}

	var body HealthResponse
	if err := json.Unmarshal(ready.Body.Bytes(), &body); err != nil {
		t.Fatalf("resposta não é JSON: %v", err)
	}
	if body.Checks["postgres"] != "ok" || body.Checks["sqs"] != "unavailable" {
		t.Errorf("checks = %v, quero postgres ok e sqs unavailable", body.Checks)
	}
}

// testMetrics builds a fresh set of collectors, so one test never sees the
// counters another one moved.
func testMetrics() *metrics.Metrics {
	return metrics.New(prometheus.NewRegistry())
}
