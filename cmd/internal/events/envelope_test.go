/*
@Author: Franco Ribeiro Borba
@Description: Unit tests for the integration events
*/
package events

import (
	"encoding/json"
	"errors"
	"testing"
	"time"

	"github.com/FrancoBorba/distributed-wallet-api/cmd/internal/domain"
	"github.com/google/uuid"
)

var (
	walletID      = uuid.MustParse("0192f291-27dd-7d3f-8071-5f8685deef37")
	playerID      = uuid.MustParse("0192f28f-5dc0-7d58-bdb2-814ad6a0f4a1")
	transactionID = uuid.MustParse("0192f2a0-0000-7000-8000-000000000001")
	correlationID = uuid.MustParse("0192f2a0-0000-7000-8000-000000000002")
	causationID   = uuid.MustParse("0192f2a0-0000-7000-8000-000000000003")
)

// occurredAt is a fixed instant, so the tests never depend on the clock.
var occurredAt = time.Date(2026, 9, 20, 12, 0, 0, 0, time.UTC)

// money builds a Money or fails the test, since an invalid fixture is a bug in
// the test and not a case under verification.
func money(t *testing.T, amount string) domain.Money {
	t.Helper()

	value, err := domain.ParseMoney(amount, "BRL")
	if err != nil {
		t.Fatalf("ParseMoney(%q) erro inesperado: %v", amount, err)
	}

	return value
}

// debitChange is the BalanceChange of a bet of 25.00 over a balance of 100.00.
func debitChange(t *testing.T) domain.BalanceChange {
	t.Helper()

	return domain.BalanceChange{
		WalletID:      walletID,
		Direction:     domain.DirectionDebit,
		Amount:        money(t, "25.00"),
		BalanceBefore: money(t, "100.00"),
		BalanceAfter:  money(t, "75.00"),
		WalletVersion: 2,
		OccurredAt:    occurredAt,
	}
}

// externalBet builds a PENDING bet sent by a provider.
func externalBet(t *testing.T) *domain.WagerTransaction {
	t.Helper()

	transaction, err := domain.NewExternalTransaction(domain.ExternalTransactionParams{
		ID:                    transactionID,
		WalletID:              walletID,
		PlayerID:              playerID,
		ProviderID:            "provider-a",
		ExternalTransactionID: "transaction-123",
		IdempotencyKey:        "provider-a:transaction-123",
		PayloadHash:           "0000000000000000000000000000000000000000000000000000000000000000",
		RoundID:               "round-987",
		GameID:                "fortune-chimp",
		Kind:                  domain.KindBet,
		Money:                 money(t, "25.00"),
		Now:                   occurredAt,
	})
	if err != nil {
		t.Fatalf("NewExternalTransaction erro inesperado: %v", err)
	}

	return transaction
}

// decode serializes an envelope and reads it back as a generic document, which
// is what a consumer sees on the wire.
func decode(t *testing.T, envelope Envelope) map[string]any {
	t.Helper()

	encoded, err := json.Marshal(envelope)
	if err != nil {
		t.Fatalf("Marshal erro inesperado: %v", err)
	}

	var document map[string]any
	if err := json.Unmarshal(encoded, &document); err != nil {
		t.Fatalf("Unmarshal erro inesperado: %v", err)
	}

	return document
}

// --------------------------------------------------------------------------
// WalletBalanceChanged
// --------------------------------------------------------------------------

func TestNewWalletBalanceChanged_Valid(t *testing.T) {
	envelope, err := NewWalletBalanceChanged(debitChange(t), transactionID, NewMetadata(correlationID))
	if err != nil {
		t.Fatalf("NewWalletBalanceChanged erro inesperado: %v", err)
	}

	if envelope.Type() != TypeWalletBalanceChanged {
		t.Errorf("eventType = %q, quero %q", envelope.Type(), TypeWalletBalanceChanged)
	}
	if envelope.Version() != versionWalletBalanceChanged {
		t.Errorf("version = %d, quero %d", envelope.Version(), versionWalletBalanceChanged)
	}
	if envelope.AggregateType() != AggregateWallet {
		t.Errorf("aggregateType = %q, quero %q", envelope.AggregateType(), AggregateWallet)
	}
	if envelope.AggregateID() != walletID {
		t.Errorf("aggregateId = %s, quero %s", envelope.AggregateID(), walletID)
	}
	if envelope.ID() == uuid.Nil {
		t.Error("eventId não pode ser nulo")
	}
	// O instante vem da carteira, não do momento da publicação.
	if !envelope.OccurredAt().Equal(occurredAt) {
		t.Errorf("occurredAt = %s, quero %s", envelope.OccurredAt(), occurredAt)
	}
	if _, caused := envelope.CausationID(); caused {
		t.Error("causationId deveria estar ausente")
	}
}

func TestNewWalletBalanceChanged_JSONContract(t *testing.T) {
	envelope, err := NewWalletBalanceChanged(debitChange(t), transactionID, NewMetadata(correlationID).CausedBy(causationID))
	if err != nil {
		t.Fatalf("NewWalletBalanceChanged erro inesperado: %v", err)
	}

	document := decode(t, envelope)

	for _, field := range []string{"eventId", "eventType", "version", "aggregateType", "aggregateId", "correlationId", "causationId", "occurredAt", "data"} {
		if _, found := document[field]; !found {
			t.Errorf("campo %q ausente no envelope", field)
		}
	}

	if document["occurredAt"] != "2026-09-20T12:00:00Z" {
		t.Errorf("occurredAt = %v, quero RFC 3339 em UTC", document["occurredAt"])
	}

	data, ok := document["data"].(map[string]any)
	if !ok {
		t.Fatalf("data = %T, quero um objeto", document["data"])
	}

	for _, field := range []string{"walletId", "transactionId", "direction", "money", "balanceBefore", "balanceAfter", "walletVersion"} {
		if _, found := data[field]; !found {
			t.Errorf("campo %q ausente no payload exigido pela Seção 11", field)
		}
	}

	if data["direction"] != string(domain.DirectionDebit) {
		t.Errorf("direction = %v, quero %q", data["direction"], domain.DirectionDebit)
	}

	// Valores monetários viajam como string decimal com a moeda.
	amount, ok := data["money"].(map[string]any)
	if !ok {
		t.Fatalf("money = %T, quero um objeto", data["money"])
	}
	if amount["amount"] != "25.00" || amount["currency"] != "BRL" {
		t.Errorf("money = %v, quero {25.00 BRL}", amount)
	}
}

func TestNewWalletBalanceChanged_Invalido(t *testing.T) {
	valid := debitChange(t)

	tests := []struct {
		name          string
		change        func(domain.BalanceChange) domain.BalanceChange
		transactionID uuid.UUID
		meta          Metadata
		wantErr       error
	}{
		{
			name:          "sem carteira",
			change:        func(c domain.BalanceChange) domain.BalanceChange { c.WalletID = uuid.Nil; return c },
			transactionID: transactionID,
			meta:          NewMetadata(correlationID),
			wantErr:       domain.ErrInvalidWalletID,
		},
		{
			name:          "sem transação",
			change:        func(c domain.BalanceChange) domain.BalanceChange { return c },
			transactionID: uuid.Nil,
			meta:          NewMetadata(correlationID),
			wantErr:       domain.ErrInvalidTransactionID,
		},
		{
			name:          "direção desconhecida",
			change:        func(c domain.BalanceChange) domain.BalanceChange { c.Direction = "SIDEWAYS"; return c },
			transactionID: transactionID,
			meta:          NewMetadata(correlationID),
			wantErr:       domain.ErrInvalidDirection,
		},
		{
			name:          "money não inicializado",
			change:        func(c domain.BalanceChange) domain.BalanceChange { c.Amount = domain.Money{}; return c },
			transactionID: transactionID,
			meta:          NewMetadata(correlationID),
			wantErr:       domain.ErrUninitializedMoney,
		},
		{
			name:          "valor zero não muda saldo",
			change:        func(c domain.BalanceChange) domain.BalanceChange { c.Amount = money(t, "0.00"); return c },
			transactionID: transactionID,
			meta:          NewMetadata(correlationID),
			wantErr:       ErrInvalidChangeAmount,
		},
		{
			name:          "versão da carteira inválida",
			change:        func(c domain.BalanceChange) domain.BalanceChange { c.WalletVersion = 0; return c },
			transactionID: transactionID,
			meta:          NewMetadata(correlationID),
			wantErr:       ErrInvalidWalletVersion,
		},
		{
			name:          "sem instante de ocorrência",
			change:        func(c domain.BalanceChange) domain.BalanceChange { c.OccurredAt = time.Time{}; return c },
			transactionID: transactionID,
			meta:          NewMetadata(correlationID),
			wantErr:       ErrInvalidOccurredAt,
		},
		{
			name:          "sem correlação",
			change:        func(c domain.BalanceChange) domain.BalanceChange { return c },
			transactionID: transactionID,
			meta:          Metadata{},
			wantErr:       ErrInvalidCorrelationID,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			_, err := NewWalletBalanceChanged(tc.change(valid), tc.transactionID, tc.meta)
			if !errors.Is(err, tc.wantErr) {
				t.Errorf("erro = %v, quero %v", err, tc.wantErr)
			}
		})
	}
}

// --------------------------------------------------------------------------
// WagerTransactionProcessed
// --------------------------------------------------------------------------

func TestNewWagerTransactionProcessed_Valid(t *testing.T) {
	transaction := externalBet(t)
	if err := transaction.MarkAsProcessed(money(t, "75.00"), occurredAt); err != nil {
		t.Fatalf("MarkAsProcessed erro inesperado: %v", err)
	}

	envelope, err := NewWagerTransactionProcessed(transaction, NewMetadata(correlationID))
	if err != nil {
		t.Fatalf("NewWagerTransactionProcessed erro inesperado: %v", err)
	}

	if envelope.Type() != TypeWagerTransactionProcessed {
		t.Errorf("eventType = %q, quero %q", envelope.Type(), TypeWagerTransactionProcessed)
	}
	if envelope.AggregateType() != AggregateWagerTransaction {
		t.Errorf("aggregateType = %q, quero %q", envelope.AggregateType(), AggregateWagerTransaction)
	}
	if envelope.AggregateID() != transactionID {
		t.Errorf("aggregateId = %s, quero %s", envelope.AggregateID(), transactionID)
	}

	data := decode(t, envelope)["data"].(map[string]any)

	if data["providerId"] != "provider-a" || data["externalTransactionId"] != "transaction-123" {
		t.Errorf("metadados do provedor ausentes: %v", data)
	}
	if data["kind"] != string(domain.KindBet) {
		t.Errorf("kind = %v, quero %q", data["kind"], domain.KindBet)
	}
	if _, found := data["resultBalance"]; !found {
		t.Error("resultBalance ausente, é o saldo que o replay idempotente responde")
	}
	if _, found := data["referenceTransactionId"]; found {
		t.Error("referenceTransactionId não deveria aparecer em uma aposta")
	}
}

func TestNewWagerTransactionProcessed_AberturaOmiteMetadadosExternos(t *testing.T) {
	opening, err := domain.NewOpeningTransaction(transactionID, walletID, playerID, money(t, "1000.00"), occurredAt)
	if err != nil {
		t.Fatalf("NewOpeningTransaction erro inesperado: %v", err)
	}

	envelope, err := NewWagerTransactionProcessed(opening, NewMetadata(correlationID))
	if err != nil {
		t.Fatalf("NewWagerTransactionProcessed erro inesperado: %v", err)
	}

	data := decode(t, envelope)["data"].(map[string]any)

	// A abertura é de origem interna e não tem provedor, rodada nem jogo.
	for _, field := range []string{"providerId", "externalTransactionId", "roundId", "gameId"} {
		if _, found := data[field]; found {
			t.Errorf("campo %q não se aplica a uma abertura interna", field)
		}
	}
	if data["kind"] != string(domain.KindOpening) {
		t.Errorf("kind = %v, quero %q", data["kind"], domain.KindOpening)
	}
}

func TestNewWagerTransactionProcessed_EstadoErrado(t *testing.T) {
	// Uma transação ainda PENDING não pode ser anunciada como concluída.
	_, err := NewWagerTransactionProcessed(externalBet(t), NewMetadata(correlationID))
	if !errors.Is(err, ErrUnexpectedState) {
		t.Errorf("erro = %v, quero %v", err, ErrUnexpectedState)
	}
}

func TestNewWagerTransactionProcessed_Nil(t *testing.T) {
	_, err := NewWagerTransactionProcessed(nil, NewMetadata(correlationID))
	if !errors.Is(err, ErrNilTransaction) {
		t.Errorf("erro = %v, quero %v", err, ErrNilTransaction)
	}
}

// --------------------------------------------------------------------------
// WagerTransactionRejected
// --------------------------------------------------------------------------

func TestNewWagerTransactionRejected_Valid(t *testing.T) {
	transaction := externalBet(t)
	if err := transaction.Reject(domain.FailureInsufficientFunds, occurredAt); err != nil {
		t.Fatalf("Reject erro inesperado: %v", err)
	}

	envelope, err := NewWagerTransactionRejected(transaction, NewMetadata(correlationID))
	if err != nil {
		t.Fatalf("NewWagerTransactionRejected erro inesperado: %v", err)
	}

	if envelope.Type() != TypeWagerTransactionRejected {
		t.Errorf("eventType = %q, quero %q", envelope.Type(), TypeWagerTransactionRejected)
	}

	data := decode(t, envelope)["data"].(map[string]any)
	if data["failureCode"] != string(domain.FailureInsufficientFunds) {
		t.Errorf("failureCode = %v, quero %q", data["failureCode"], domain.FailureInsufficientFunds)
	}
}

func TestNewWagerTransactionRejected_EstadoErrado(t *testing.T) {
	transaction := externalBet(t)
	if err := transaction.MarkAsProcessed(money(t, "75.00"), occurredAt); err != nil {
		t.Fatalf("MarkAsProcessed erro inesperado: %v", err)
	}

	_, err := NewWagerTransactionRejected(transaction, NewMetadata(correlationID))
	if !errors.Is(err, ErrUnexpectedState) {
		t.Errorf("erro = %v, quero %v", err, ErrUnexpectedState)
	}
}

// --------------------------------------------------------------------------
// WagerTransactionPendingReference
// --------------------------------------------------------------------------

func TestNewWagerTransactionPendingReference_Valid(t *testing.T) {
	transaction, err := domain.NewExternalTransaction(domain.ExternalTransactionParams{
		ID:                    transactionID,
		WalletID:              walletID,
		PlayerID:              playerID,
		ProviderID:            "provider-a",
		ExternalTransactionID: "transaction-456",
		IdempotencyKey:        "provider-a:transaction-456",
		PayloadHash:           "0000000000000000000000000000000000000000000000000000000000000000",
		RoundID:               "round-987",
		GameID:                "fortune-chimp",
		ReferenceExternalID:   "transaction-123",
		Kind:                  domain.KindRefund,
		Money:                 money(t, "25.00"),
		Now:                   occurredAt,
	})
	if err != nil {
		t.Fatalf("NewExternalTransaction erro inesperado: %v", err)
	}
	if err := transaction.MarkAsPendingReference(occurredAt); err != nil {
		t.Fatalf("MarkAsPendingReference erro inesperado: %v", err)
	}

	envelope, err := NewWagerTransactionPendingReference(transaction, NewMetadata(correlationID))
	if err != nil {
		t.Fatalf("NewWagerTransactionPendingReference erro inesperado: %v", err)
	}

	if envelope.Type() != TypeWagerTransactionPendingReference {
		t.Errorf("eventType = %q, quero %q", envelope.Type(), TypeWagerTransactionPendingReference)
	}

	data := decode(t, envelope)["data"].(map[string]any)
	if data["referenceExternalTransactionId"] != "transaction-123" {
		t.Errorf("referenceExternalTransactionId = %v, quero %q", data["referenceExternalTransactionId"], "transaction-123")
	}
}

// --------------------------------------------------------------------------
// Outbox
// --------------------------------------------------------------------------

func TestRestoreEnvelope_PreservaIdentidadeERepublica(t *testing.T) {
	original, err := NewWalletBalanceChanged(debitChange(t), transactionID, NewMetadata(correlationID).CausedBy(causationID))
	if err != nil {
		t.Fatalf("NewWalletBalanceChanged erro inesperado: %v", err)
	}

	// O que a linha da outbox guarda: as colunas e o snapshot serializado.
	payload, err := original.DataJSON()
	if err != nil {
		t.Fatalf("DataJSON erro inesperado: %v", err)
	}

	causation, _ := original.CausationID()

	restored, err := RestoreEnvelope(RestoredEnvelopeParams{
		ID:            original.ID(),
		Type:          original.Type(),
		Version:       original.Version(),
		AggregateType: original.AggregateType(),
		AggregateID:   original.AggregateID(),
		CorrelationID: original.CorrelationID(),
		CausationID:   causation,
		OccurredAt:    original.OccurredAt(),
		Payload:       payload,
	})
	if err != nil {
		t.Fatalf("RestoreEnvelope erro inesperado: %v", err)
	}

	// Uma republicação depois de uma queda mantém o mesmo eventId, que é como o
	// consumidor a reconhece como duplicata.
	if restored.ID() != original.ID() {
		t.Errorf("eventId = %s, quero %s", restored.ID(), original.ID())
	}

	before, err := json.Marshal(original)
	if err != nil {
		t.Fatalf("Marshal erro inesperado: %v", err)
	}
	after, err := json.Marshal(restored)
	if err != nil {
		t.Fatalf("Marshal erro inesperado: %v", err)
	}
	if string(before) != string(after) {
		t.Errorf("republicação mudou o corpo:\nantes: %s\ndepois: %s", before, after)
	}
}

func TestRestoreEnvelope_Invalido(t *testing.T) {
	valid := RestoredEnvelopeParams{
		ID:            uuid.New(),
		Type:          TypeWalletBalanceChanged,
		Version:       1,
		AggregateType: AggregateWallet,
		AggregateID:   walletID,
		CorrelationID: correlationID,
		OccurredAt:    occurredAt,
		Payload:       json.RawMessage(`{"walletId":"0192f291-27dd-7d3f-8071-5f8685deef37"}`),
	}

	tests := []struct {
		name    string
		mutate  func(RestoredEnvelopeParams) RestoredEnvelopeParams
		wantErr error
	}{
		{
			name:    "tipo desconhecido",
			mutate:  func(p RestoredEnvelopeParams) RestoredEnvelopeParams { p.Type = "WalletExploded"; return p },
			wantErr: ErrUnknownEventType,
		},
		{
			name: "agregado incompatível com o evento",
			mutate: func(p RestoredEnvelopeParams) RestoredEnvelopeParams {
				p.AggregateType = AggregateWagerTransaction
				return p
			},
			wantErr: ErrAggregateTypeMismatch,
		},
		{
			name:    "sem eventId",
			mutate:  func(p RestoredEnvelopeParams) RestoredEnvelopeParams { p.ID = uuid.Nil; return p },
			wantErr: ErrInvalidEventID,
		},
		{
			name:    "versão inválida",
			mutate:  func(p RestoredEnvelopeParams) RestoredEnvelopeParams { p.Version = 0; return p },
			wantErr: ErrInvalidEventVersion,
		},
		{
			name:    "payload vazio",
			mutate:  func(p RestoredEnvelopeParams) RestoredEnvelopeParams { p.Payload = nil; return p },
			wantErr: ErrMissingEventData,
		},
		{
			name: "payload não é objeto",
			mutate: func(p RestoredEnvelopeParams) RestoredEnvelopeParams {
				p.Payload = json.RawMessage(`["walletId"]`)
				return p
			},
			wantErr: ErrInvalidEventData,
		},
		{
			name: "payload truncado",
			mutate: func(p RestoredEnvelopeParams) RestoredEnvelopeParams {
				p.Payload = json.RawMessage(`{"walletId":`)
				return p
			},
			wantErr: ErrInvalidEventData,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			_, err := RestoreEnvelope(tc.mutate(valid))
			if !errors.Is(err, tc.wantErr) {
				t.Errorf("erro = %v, quero %v", err, tc.wantErr)
			}
		})
	}
}

func TestAggregateFor_CobreTodosOsTipos(t *testing.T) {
	types := []EventType{
		TypeWagerTransactionProcessed,
		TypeWagerTransactionRejected,
		TypeWagerTransactionPendingReference,
		TypeWalletBalanceChanged,
	}

	for _, eventType := range types {
		t.Run(string(eventType), func(t *testing.T) {
			if !eventType.IsValid() {
				t.Fatalf("%q deveria ser um tipo válido", eventType)
			}

			aggregate, err := aggregateFor(eventType)
			if err != nil {
				t.Fatalf("aggregateFor(%q) erro inesperado: %v", eventType, err)
			}
			if !aggregate.IsValid() {
				t.Errorf("aggregateType = %q, não é um agregado conhecido", aggregate)
			}
		})
	}
}
