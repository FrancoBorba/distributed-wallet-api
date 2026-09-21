/*
@Author: Franco Ribeiro Borba
@Description: Unit tests for the inbound message contract
*/
package events

import (
	"errors"
	"testing"

	"github.com/FrancoBorba/distributed-wallet-api/cmd/internal/domain"
	"github.com/FrancoBorba/distributed-wallet-api/cmd/internal/pkg/idempotency"
	"github.com/google/uuid"
)

// sqsBody is the example message of the contract.
const sqsBody = `{
  "messageId": "msg-123",
  "type": "WagerTransactionRequested",
  "occurredAt": "2026-09-08T12:00:00.000Z",
  "data": {
    "providerId": "provider-a",
    "externalTransactionId": "transaction-123",
    "idempotencyKey": "provider-a:transaction-123",
    "playerId": "0192f28f-5dc0-7d58-bdb2-814ad6a0f4a1",
    "walletId": "0192f291-27dd-7d3f-8071-5f8685deef37",
    "roundId": "round-987",
    "gameId": "fortune-chimp",
    "kind": "BET",
    "money": { "amount": "25.00", "currency": "BRL" }
  }
}`

func TestParseInbound_Valid(t *testing.T) {
	message, err := ParseInbound([]byte(sqsBody))
	if err != nil {
		t.Fatalf("ParseInbound erro inesperado: %v", err)
	}

	if message.MessageID() != "msg-123" {
		t.Errorf("messageId = %q, quero %q", message.MessageID(), "msg-123")
	}
	if message.Type() != TypeWagerTransactionRequested {
		t.Errorf("type = %q, quero %q", message.Type(), TypeWagerTransactionRequested)
	}

	data := message.Data()
	if data.Kind != domain.KindBet {
		t.Errorf("kind = %q, quero BET", data.Kind)
	}
	if got := data.Money.AmountString(); got != "25.00" {
		t.Errorf("money = %s, quero 25.00", got)
	}
	if len(message.PayloadHash()) != 64 {
		t.Errorf("payloadHash = %q, quero 64 caracteres hexadecimais", message.PayloadHash())
	}
}

// Esta é a garantia que o desafio exige: a mesma operação enviada por HTTP e
// por SQS precisa produzir o mesmo hash, senão o reenvio por um transporte
// diferente moveria dinheiro duas vezes.
func TestParseInbound_HashIgualAoDoCorpoHTTP(t *testing.T) {
	// O corpo HTTP carrega os campos de negócio direto, com outra ordem de
	// chaves e outra indentação.
	httpBody := []byte(`{"money":{"currency":"BRL","amount":"25.00"},"kind":"BET",` +
		`"gameId":"fortune-chimp","roundId":"round-987",` +
		`"walletId":"0192f291-27dd-7d3f-8071-5f8685deef37",` +
		`"playerId":"0192f28f-5dc0-7d58-bdb2-814ad6a0f4a1",` +
		`"idempotencyKey":"provider-a:transaction-123",` +
		`"externalTransactionId":"transaction-123","providerId":"provider-a"}`)

	httpHash, err := idempotency.CanonicalHash(httpBody)
	if err != nil {
		t.Fatalf("CanonicalHash erro inesperado: %v", err)
	}

	message, err := ParseInbound([]byte(sqsBody))
	if err != nil {
		t.Fatalf("ParseInbound erro inesperado: %v", err)
	}

	if message.PayloadHash() != httpHash {
		t.Errorf("hash SQS = %s\nhash HTTP = %s\nos dois transportes precisam concordar",
			message.PayloadHash(), httpHash)
	}
}

func TestParseInbound_Invalido(t *testing.T) {
	tests := []struct {
		name    string
		body    string
		wantErr error
	}{
		{
			name:    "json quebrado",
			body:    `{"messageId":`,
			wantErr: ErrInvalidMessage,
		},
		{
			name:    "sem messageId",
			body:    `{"type":"WagerTransactionRequested","occurredAt":"2026-09-08T12:00:00Z","data":{}}`,
			wantErr: ErrMissingMessageID,
		},
		{
			name:    "tipo desconhecido",
			body:    `{"messageId":"m1","type":"SomethingElse","occurredAt":"2026-09-08T12:00:00Z","data":{}}`,
			wantErr: ErrUnknownMessageType,
		},
		{
			name:    "sem data",
			body:    `{"messageId":"m1","type":"WagerTransactionRequested","occurredAt":"2026-09-08T12:00:00Z"}`,
			wantErr: ErrMissingMessageData,
		},
		{
			name: "OPENING é interno e não pode vir de um provedor",
			body: `{"messageId":"m1","type":"WagerTransactionRequested","occurredAt":"2026-09-08T12:00:00Z",
				"data":{"providerId":"p","externalTransactionId":"t","idempotencyKey":"k",
				"playerId":"0192f28f-5dc0-7d58-bdb2-814ad6a0f4a1","walletId":"0192f291-27dd-7d3f-8071-5f8685deef37",
				"roundId":"r","gameId":"g","kind":"OPENING","money":{"amount":"10.00","currency":"BRL"}}}`,
			wantErr: domain.ErrInvalidExternalKind,
		},
		{
			name: "REFUND sem referência",
			body: `{"messageId":"m1","type":"WagerTransactionRequested","occurredAt":"2026-09-08T12:00:00Z",
				"data":{"providerId":"p","externalTransactionId":"t","idempotencyKey":"k",
				"playerId":"0192f28f-5dc0-7d58-bdb2-814ad6a0f4a1","walletId":"0192f291-27dd-7d3f-8071-5f8685deef37",
				"roundId":"r","gameId":"g","kind":"REFUND","money":{"amount":"10.00","currency":"BRL"}}}`,
			wantErr: domain.ErrMissingReference,
		},
		{
			name: "BET com referência de reversão",
			body: `{"messageId":"m1","type":"WagerTransactionRequested","occurredAt":"2026-09-08T12:00:00Z",
				"data":{"providerId":"p","externalTransactionId":"t","idempotencyKey":"k",
				"playerId":"0192f28f-5dc0-7d58-bdb2-814ad6a0f4a1","walletId":"0192f291-27dd-7d3f-8071-5f8685deef37",
				"roundId":"r","gameId":"g","kind":"BET","money":{"amount":"10.00","currency":"BRL"},
				"referenceExternalTransactionId":"outra"}}`,
			wantErr: domain.ErrReferenceNotApplicable,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			_, err := ParseInbound([]byte(tc.body))
			if !errors.Is(err, tc.wantErr) {
				t.Errorf("erro = %v, quero %v", err, tc.wantErr)
			}
		})
	}
}

func TestInboundMessage_MetadataMantemACorrelacaoDoProvedor(t *testing.T) {
	message, err := ParseInbound([]byte(sqsBody))
	if err != nil {
		t.Fatalf("ParseInbound erro inesperado: %v", err)
	}

	causation := uuidFromString(t, "0192f2a0-0000-7000-8000-000000000003")

	meta := message.Metadata(causation)
	if meta.CorrelationID == causation {
		t.Error("sem correlationId do provedor, uma nova deve ser criada, não a causation")
	}
	if meta.CausationID != causation {
		t.Errorf("causationId = %s, quero %s", meta.CausationID, causation)
	}
}

// uuidFromString parses a fixture identifier.
func uuidFromString(t *testing.T, value string) uuid.UUID {
	t.Helper()

	parsed, err := uuid.Parse(value)
	if err != nil {
		t.Fatalf("uuid.Parse(%q) erro inesperado: %v", value, err)
	}

	return parsed
}
