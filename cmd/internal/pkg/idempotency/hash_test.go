/*
@Author: Franco Ribeiro Borba
@Description: Unit tests for the canonical idempotency hash
*/
package idempotency

import (
	"encoding/hex"
	"errors"
	"testing"
)

// httpBody e o corpo do POST /wagering/transactions exatamente como o desafio
// mostra. A chave de idempotencia viaja no header, entao nao aparece aqui.
const httpBody = `{
	"providerId": "provider-a",
	"externalTransactionId": "transaction-123",
	"playerId": "0192f28f-5dc0-7d58-bdb2-814ad6a0f4a1",
	"walletId": "0192f291-27dd-7d3f-8071-5f8685deef37",
	"roundId": "round-987",
	"gameId": "fortune-chimp",
	"kind": "BET",
	"money": { "amount": "25.00", "currency": "BRL" }
}`

// sqsData e o campo data da mensagem WagerTransactionRequested. E a mesma
// operacao do httpBody, mas com a chave de idempotencia dentro do corpo e com
// as chaves em outra ordem.
const sqsData = `{
	"money": { "currency": "BRL", "amount": "25.00" },
	"kind": "BET",
	"idempotencyKey": "provider-a:transaction-123",
	"gameId": "fortune-chimp",
	"roundId": "round-987",
	"walletId": "0192f291-27dd-7d3f-8071-5f8685deef37",
	"playerId": "0192f28f-5dc0-7d58-bdb2-814ad6a0f4a1",
	"externalTransactionId": "transaction-123",
	"providerId": "provider-a"
}`

// baseTransaction monta o mesmo corpo do httpBody com um campo trocado, para
// os casos que precisam provar que uma diferenca de negocio muda o hash.
func baseTransaction(amount, currency, kind, gameID, walletID, extra string) string {
	return `{"providerId":"provider-a","externalTransactionId":"transaction-123",` +
		`"playerId":"0192f28f-5dc0-7d58-bdb2-814ad6a0f4a1","walletId":"` + walletID + `",` +
		`"roundId":"round-987","gameId":"` + gameID + `","kind":"` + kind + `",` +
		`"money":{"amount":"` + amount + `","currency":"` + currency + `"}` + extra + `}`
}

func mustHash(t *testing.T, payload string) string {
	t.Helper()

	hash, err := CanonicalHash([]byte(payload))
	if err != nil {
		t.Fatalf("CanonicalHash: %v", err)
	}

	return hash
}

// --------------------------------------------------------------------------
// Determinismo
// --------------------------------------------------------------------------

// TestCanonicalHashIsHexSHA256 garante o formato do que sera persistido.
func TestCanonicalHashIsHexSHA256(t *testing.T) {
	hash := mustHash(t, httpBody)

	if len(hash) != 64 {
		t.Errorf("len(hash) = %d, quero 64", len(hash))
	}
	if _, err := hex.DecodeString(hash); err != nil {
		t.Errorf("hash nao e hexadecimal: %v", err)
	}
}

// TestCanonicalHashIsStable roda a mesma entrada varias vezes porque a ordem de
// iteracao de um map em Go e aleatoria: se a canonicalizacao dependesse dela, o
// hash mudaria entre chamadas e a idempotencia persistente quebraria.
func TestCanonicalHashIsStable(t *testing.T) {
	first := mustHash(t, httpBody)

	for i := 0; i < 50; i++ {
		if got := mustHash(t, httpBody); got != first {
			t.Fatalf("hash mudou na tentativa %d: %s, quero %s", i, got, first)
		}
	}
}

// TestCanonicalHashIgnoresKeyOrderAndWhitespace cobre a exigencia de JSON
// canonico com ordenacao de chaves.
func TestCanonicalHashIgnoresKeyOrderAndWhitespace(t *testing.T) {
	compact := `{"kind":"BET","money":{"currency":"BRL","amount":"25.00"},"providerId":"provider-a"}`
	spaced := `{
		"providerId" : "provider-a",
		"money"      : { "amount" : "25.00", "currency" : "BRL" },
		"kind"       : "BET"
	}`

	if mustHash(t, compact) != mustHash(t, spaced) {
		t.Error("ordem das chaves ou espacos mudaram o hash")
	}
}

// --------------------------------------------------------------------------
// Equivalencia entre HTTP e SQS
// --------------------------------------------------------------------------

// TestCanonicalHashMatchesBetweenHTTPAndSQS e a garantia central do contrato:
// a mesma operacao financeira entrando por HTTP ou por SQS produz o mesmo hash.
func TestCanonicalHashMatchesBetweenHTTPAndSQS(t *testing.T) {
	if mustHash(t, httpBody) != mustHash(t, sqsData) {
		t.Error("HTTP e SQS produziram hashes diferentes para a mesma operacao")
	}
}

// TestCanonicalHashExcludesIdempotencyKey isola o efeito da chave: trocar a
// chave sem trocar o negocio nao pode mudar o hash, senao um replay legitimo
// seria lido como conflito.
func TestCanonicalHashExcludesIdempotencyKey(t *testing.T) {
	withKey := `{"idempotencyKey":"provider-a:transaction-123","kind":"BET"}`
	withOtherKey := `{"idempotencyKey":"provider-b:transaction-999","kind":"BET"}`
	withoutKey := `{"kind":"BET"}`

	base := mustHash(t, withoutKey)

	if mustHash(t, withKey) != base {
		t.Error("idempotencyKey entrou no hash")
	}
	if mustHash(t, withOtherKey) != base {
		t.Error("trocar a idempotencyKey mudou o hash")
	}
}

// TestCanonicalHashExcludesTransportMetadata cobre o envelope do SQS e o
// tracing: uma reentrega muda esses campos sem mudar a operacao.
func TestCanonicalHashExcludesTransportMetadata(t *testing.T) {
	first := `{
		"messageId": "msg-123",
		"type": "WagerTransactionRequested",
		"occurredAt": "2026-09-08T12:00:00.000Z",
		"messageGroupId": "wallet-1",
		"messageDeduplicationId": "dedup-1",
		"sequenceNumber": "10",
		"receiptHandle": "handle-aaa",
		"requestId": "req-1",
		"correlationId": "corr-1",
		"traceId": "trace-1",
		"sentAt": "2026-09-08T12:00:00.000Z",
		"timestamp": "2026-09-08T12:00:00.000Z",
		"kind": "BET"
	}`
	redelivery := `{
		"messageId": "msg-123",
		"type": "WagerTransactionRequested",
		"occurredAt": "2026-09-08T12:05:00.000Z",
		"messageGroupId": "wallet-1",
		"messageDeduplicationId": "dedup-2",
		"sequenceNumber": "11",
		"receiptHandle": "handle-bbb",
		"requestId": "req-2",
		"correlationId": "corr-2",
		"traceId": "trace-2",
		"sentAt": "2026-09-08T12:05:00.000Z",
		"timestamp": "2026-09-08T12:05:00.000Z",
		"kind": "BET"
	}`

	if mustHash(t, first) != mustHash(t, redelivery) {
		t.Error("metadados de transporte mudaram o hash da reentrega")
	}
	if mustHash(t, first) != mustHash(t, `{"kind":"BET"}`) {
		t.Error("algum metadado de transporte sobreviveu ate o hash")
	}
}

// TestTransportFieldsAreCaseInsensitive: encoding/json casa nomes de campo sem
// diferenciar maiusculas ao preencher uma struct, entao a exclusao precisa se
// comportar do mesmo jeito.
func TestTransportFieldsAreCaseInsensitive(t *testing.T) {
	variants := []string{
		`{"IdempotencyKey":"provider-a:transaction-123","kind":"BET"}`,
		`{"IDEMPOTENCYKEY":"provider-a:transaction-123","kind":"BET"}`,
		`{"MessageId":"msg-123","kind":"BET"}`,
	}

	base := mustHash(t, `{"kind":"BET"}`)

	for _, payload := range variants {
		if got := mustHash(t, payload); got != base {
			t.Errorf("%s: hash = %s, quero %s", payload, got, base)
		}
	}
}

// TestTransportNamesInsideNestedObjectsAreKept: a exclusao vale so para o topo.
// Um campo chamado type dentro de um objeto de negocio e conteudo, nao envelope.
func TestTransportNamesInsideNestedObjectsAreKept(t *testing.T) {
	first := `{"kind":"BET","bonus":{"type":"FREE_ROUND"}}`
	second := `{"kind":"BET","bonus":{"type":"CASHBACK"}}`

	if mustHash(t, first) == mustHash(t, second) {
		t.Error("um campo de negocio aninhado foi descartado como transporte")
	}
}

// --------------------------------------------------------------------------
// Ausente e null
// --------------------------------------------------------------------------

// TestNullFieldEqualsAbsentField: o produtor do SQS costuma serializar o campo
// opcional como null enquanto o cliente HTTP simplesmente o omite. Sao a mesma
// operacao.
func TestNullFieldEqualsAbsentField(t *testing.T) {
	absent := `{"kind":"BET","money":{"amount":"25.00","currency":"BRL"}}`
	null := `{"kind":"BET","referenceExternalTransactionId":null,"money":{"amount":"25.00","currency":"BRL"}}`

	if mustHash(t, absent) != mustHash(t, null) {
		t.Error("campo null deveria ser equivalente a campo ausente")
	}
}

// TestNullInsideNestedObjectIsDropped garante que a regra desce a arvore toda.
func TestNullInsideNestedObjectIsDropped(t *testing.T) {
	absent := `{"reference":{"externalTransactionId":"transaction-1"}}`
	null := `{"reference":{"externalTransactionId":"transaction-1","resolvedAt":null}}`

	if mustHash(t, absent) != mustHash(t, null) {
		t.Error("null aninhado nao foi descartado")
	}
}

// TestNullInsideArrayIsKept: dentro de um array a posicao faz parte do
// conteudo, entao descartar o null mudaria o significado da lista.
func TestNullInsideArrayIsKept(t *testing.T) {
	withNull := `{"items":[1,null,2]}`
	withoutNull := `{"items":[1,2]}`

	if mustHash(t, withNull) == mustHash(t, withoutNull) {
		t.Error("null dentro de array nao pode ser descartado")
	}
}

// TestNullIsNotEqualToEmptyString: um campo ausente e um campo vazio sao coisas
// diferentes para o negocio, e o hash precisa separa-las.
func TestNullIsNotEqualToEmptyString(t *testing.T) {
	null := `{"kind":"BET","roundId":null}`
	empty := `{"kind":"BET","roundId":""}`

	if mustHash(t, null) == mustHash(t, empty) {
		t.Error("null e string vazia produziram o mesmo hash")
	}
}

// --------------------------------------------------------------------------
// Deteccao de conteudo diferente
// --------------------------------------------------------------------------

// TestDifferentBusinessContentChangesHash cobre a outra metade do contrato:
// mesma chave com conteudo diferente precisa ser detectavel como conflito.
func TestDifferentBusinessContentChangesHash(t *testing.T) {
	const (
		wallet      = "0192f291-27dd-7d3f-8071-5f8685deef37"
		otherWallet = "0192f291-27dd-7d3f-8071-5f8685deef38"
	)

	base := mustHash(t, httpBody)

	cases := map[string]string{
		"valor diferente":      baseTransaction("25.01", "BRL", "BET", "fortune-chimp", wallet, ""),
		"moeda diferente":      baseTransaction("25.00", "USD", "BET", "fortune-chimp", wallet, ""),
		"tipo diferente":       baseTransaction("25.00", "BRL", "WIN", "fortune-chimp", wallet, ""),
		"carteira diferente":   baseTransaction("25.00", "BRL", "BET", "fortune-chimp", otherWallet, ""),
		"caixa alta no gameId": baseTransaction("25.00", "BRL", "BET", "FORTUNE-CHIMP", wallet, ""),
		"campo extra":          baseTransaction("25.00", "BRL", "BET", "fortune-chimp", wallet, `,"referenceExternalTransactionId":"transaction-000"`),
	}

	for name, payload := range cases {
		t.Run(name, func(t *testing.T) {
			if mustHash(t, payload) == base {
				t.Error("conteudo diferente produziu o mesmo hash")
			}
		})
	}
}

// TestSameContentInOtherOrderMatchesBase protege o helper: sem este caso, um
// erro em baseTransaction faria todos os testes acima passarem por engano.
func TestSameContentInOtherOrderMatchesBase(t *testing.T) {
	same := baseTransaction("25.00", "BRL", "BET", "fortune-chimp", "0192f291-27dd-7d3f-8071-5f8685deef37", "")

	if mustHash(t, same) != mustHash(t, httpBody) {
		t.Error("o mesmo conteudo deveria produzir o hash do corpo original")
	}
}

// TestStringValuesAreNotTrimmed: espaco em branco dentro de um valor nao e
// normalizado, porque isso seria aceitar em silencio uma entrada diferente.
func TestStringValuesAreNotTrimmed(t *testing.T) {
	if mustHash(t, `{"roundId":"round-987"}`) == mustHash(t, `{"roundId":" round-987 "}`) {
		t.Error("o valor foi normalizado em silencio")
	}
}

// TestArrayOrderChangesHash: um array e uma sequencia, nao um conjunto.
func TestArrayOrderChangesHash(t *testing.T) {
	if mustHash(t, `{"items":["a","b"]}`) == mustHash(t, `{"items":["b","a"]}`) {
		t.Error("a ordem do array deveria mudar o hash")
	}
}

// --------------------------------------------------------------------------
// Numeros
// --------------------------------------------------------------------------

// TestLargeNumbersKeepPrecision: passar por float64 arredondaria esses dois
// identificadores para o mesmo valor e faria operacoes distintas colidirem.
func TestLargeNumbersKeepPrecision(t *testing.T) {
	first := `{"externalSequence":9007199254740993}`
	second := `{"externalSequence":9007199254740992}`

	if mustHash(t, first) == mustHash(t, second) {
		t.Error("inteiro grande foi arredondado antes do hash")
	}
}

// TestNumberLiteralIsPreserved: 25.0 e 25 nao sao reescritos um no outro. Nao
// ha perda para o dinheiro, que chega como a string "25.00", mas evita que o
// hash concorde com duas entradas escritas de formas diferentes.
func TestNumberLiteralIsPreserved(t *testing.T) {
	if mustHash(t, `{"factor":25.0}`) == mustHash(t, `{"factor":25}`) {
		t.Error("o literal numerico foi reescrito antes do hash")
	}
}

// --------------------------------------------------------------------------
// Entrada invalida
// --------------------------------------------------------------------------

func TestCanonicalHashRejectsInvalidPayloads(t *testing.T) {
	cases := map[string]struct {
		payload string
		want    error
	}{
		"payload vazio":         {"", ErrEmptyPayload},
		"so espacos":            {"   \n\t ", ErrEmptyPayload},
		"json quebrado":         {`{"kind":`, ErrInvalidPayload},
		"chave sem aspas":       {`{kind:"BET"}`, ErrInvalidPayload},
		"lixo depois do objeto": {`{"kind":"BET"} {"kind":"WIN"}`, ErrInvalidPayload},
		"array no topo":         {`[{"kind":"BET"}]`, ErrPayloadNotObject},
		"string no topo":        {`"BET"`, ErrPayloadNotObject},
		"numero no topo":        {`25`, ErrPayloadNotObject},
		"null no topo":          {`null`, ErrPayloadNotObject},
		"objeto vazio":          {`{}`, ErrNoBusinessFields},
		"so transporte":         {`{"idempotencyKey":"provider-a:transaction-123","messageId":"msg-123"}`, ErrNoBusinessFields},
		"so nulls":              {`{"roundId":null,"gameId":null}`, ErrNoBusinessFields},
	}

	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			hash, err := CanonicalHash([]byte(tc.payload))
			if !errors.Is(err, tc.want) {
				t.Fatalf("err = %v, quero %v", err, tc.want)
			}
			if hash != "" {
				t.Errorf("hash = %q, quero string vazia em caso de erro", hash)
			}
		})
	}
}

// --------------------------------------------------------------------------
// CanonicalJSON
// --------------------------------------------------------------------------

// TestCanonicalJSONOutput fixa a forma canonica exata, que e o que a resposta
// de conflito pode mostrar quando dois payloads divergem.
func TestCanonicalJSONOutput(t *testing.T) {
	payload := `{
		"money": { "currency": "BRL", "amount": "25.00" },
		"idempotencyKey": "provider-a:transaction-123",
		"kind": "BET",
		"roundId": null,
		"providerId": "provider-a"
	}`
	want := `{"kind":"BET","money":{"amount":"25.00","currency":"BRL"},"providerId":"provider-a"}`

	canonical, err := CanonicalJSON([]byte(payload))
	if err != nil {
		t.Fatalf("erro inesperado: %v", err)
	}

	if got := string(canonical); got != want {
		t.Errorf("CanonicalJSON() = %s, quero %s", got, want)
	}
}

// TestCanonicalJSONDoesNotMutateInput: o handler ainda precisa do corpo
// original para desserializar o comando depois de calcular o hash.
func TestCanonicalJSONDoesNotMutateInput(t *testing.T) {
	payload := []byte(httpBody)
	original := string(payload)

	if _, err := CanonicalJSON(payload); err != nil {
		t.Fatalf("erro inesperado: %v", err)
	}

	if string(payload) != original {
		t.Error("CanonicalJSON alterou o slice recebido")
	}
}

// TestAlgorithmIsDeclared: o identificador do algoritmo e persistido junto do
// hash para permitir migrar a canonicalizacao no futuro.
func TestAlgorithmIsDeclared(t *testing.T) {
	if Algorithm == "" {
		t.Error("Algorithm nao pode ser vazio")
	}
}
