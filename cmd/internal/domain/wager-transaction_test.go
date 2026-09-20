/*
@Author: Franco Ribeiro Borba
@Description: Unit tests for the wager transaction entity
*/
package domain

import (
	"errors"
	"testing"
	"time"

	"github.com/google/uuid"
)

var externalKinds = []TransactionKind{KindBet, KindWin, KindLoss, KindRefund, KindRollback}

// --------------------------------------------------------------------------
// TransactionKind
// --------------------------------------------------------------------------

func TestTransactionKind_IsValid(t *testing.T) {
	tests := []struct {
		kind TransactionKind
		want bool
	}{
		{KindOpening, true},
		{KindBet, true},
		{KindWin, true},
		{KindLoss, true},
		{KindRefund, true},
		{KindRollback, true},
		{"", false},
		{"bet", false},
		{"BET ", false},
		{"DEPOSIT", false},
	}

	for _, tc := range tests {
		t.Run(string(tc.kind), func(t *testing.T) {
			if got := tc.kind.IsValid(); got != tc.want {
				t.Errorf("IsValid() = %v, quero %v", got, tc.want)
			}
		})
	}
}

func TestTransactionKind_IsExternal(t *testing.T) {
	if KindOpening.IsExternal() {
		t.Error("OPENING nao pode ser considerado externo")
	}
	if TransactionKind("").IsExternal() {
		t.Error("kind desconhecido nao pode ser considerado externo")
	}
	for _, kind := range externalKinds {
		if !kind.IsExternal() {
			t.Errorf("%s deveria ser externo", kind)
		}
	}
}

func TestTransactionKind_Direction(t *testing.T) {
	tests := []struct {
		kind  TransactionKind
		want  Direction
		moves bool
	}{
		{KindOpening, DirectionCredit, true},
		{KindBet, DirectionDebit, true},
		{KindWin, DirectionCredit, true},
		{KindRefund, DirectionCredit, true},
		// LOSS nao movimenta saldo nem gera lancamento no ledger.
		{KindLoss, "", false},
		// ROLLBACK depende da direcao da transacao referenciada.
		{KindRollback, "", false},
		{"DEPOSIT", "", false},
	}

	for _, tc := range tests {
		t.Run(string(tc.kind), func(t *testing.T) {
			got, moves := tc.kind.Direction()
			if moves != tc.moves {
				t.Fatalf("moves = %v, quero %v", moves, tc.moves)
			}
			if got != tc.want {
				t.Errorf("Direction() = %q, quero %q", got, tc.want)
			}
		})
	}
}

func TestTransactionKind_ReferenceRules(t *testing.T) {
	tests := []struct {
		kind     TransactionKind
		requires bool
		accepts  bool
	}{
		{KindOpening, false, false},
		{KindBet, false, false},
		{KindLoss, false, false},
		{KindWin, false, true},
		{KindRefund, true, true},
		{KindRollback, true, true},
	}

	for _, tc := range tests {
		t.Run(string(tc.kind), func(t *testing.T) {
			if got := tc.kind.RequiresReference(); got != tc.requires {
				t.Errorf("RequiresReference() = %v, quero %v", got, tc.requires)
			}
			if got := tc.kind.AcceptsReference(); got != tc.accepts {
				t.Errorf("AcceptsReference() = %v, quero %v", got, tc.accepts)
			}
		})
	}
}

// --------------------------------------------------------------------------
// TransactionState
// --------------------------------------------------------------------------

func TestTransactionState_IsValid(t *testing.T) {
	valid := []TransactionState{StatePending, StatePendingRef, StateProcessed, StateRejected, StateFailed}
	for _, state := range valid {
		if !state.IsValid() {
			t.Errorf("%s deveria ser valido", state)
		}
	}

	invalid := []TransactionState{"", "pending", "PENDING_REF", "DONE"}
	for _, state := range invalid {
		if state.IsValid() {
			t.Errorf("%q nao deveria ser valido", state)
		}
	}
}

func TestTransactionState_IsTerminal(t *testing.T) {
	tests := []struct {
		state TransactionState
		want  bool
	}{
		{StatePending, false},
		{StatePendingRef, false},
		{StateProcessed, true},
		{StateRejected, true},
		{StateFailed, true},
		{"", false},
	}

	for _, tc := range tests {
		t.Run(string(tc.state), func(t *testing.T) {
			if got := tc.state.IsTerminal(); got != tc.want {
				t.Errorf("IsTerminal() = %v, quero %v", got, tc.want)
			}
		})
	}
}

// --------------------------------------------------------------------------
// NewOpeningTransaction
// --------------------------------------------------------------------------

func TestNewOpeningTransaction(t *testing.T) {
	id, walletID, playerID := uuid.New(), uuid.New(), uuid.New()
	amount := mustParse(t, "1000.00", "BRL")

	tx, err := NewOpeningTransaction(id, walletID, playerID, amount, testNow)
	if err != nil {
		t.Fatalf("erro inesperado: %v", err)
	}

	if tx.ID() != id || tx.WalletID() != walletID || tx.PlayerID() != playerID {
		t.Errorf("identificadores = %s/%s/%s, quero %s/%s/%s",
			tx.ID(), tx.WalletID(), tx.PlayerID(), id, walletID, playerID)
	}
	if tx.Kind() != KindOpening {
		t.Errorf("Kind() = %s, quero %s", tx.Kind(), KindOpening)
	}
	// A abertura e aplicada no mesmo commit da carteira, entao ja nasce final.
	if tx.State() != StateProcessed {
		t.Errorf("State() = %s, quero %s", tx.State(), StateProcessed)
	}
	if !tx.IsTerminal() {
		t.Error("OPENING deveria nascer em estado terminal")
	}
	if tx.IsExternal() {
		t.Error("OPENING nao pode ser marcada como externa")
	}
	if tx.Money() != amount {
		t.Errorf("Money() = %s, quero %s", tx.Money(), amount)
	}

	balance, ok := tx.ResultBalance()
	if !ok {
		t.Fatal("OPENING processada deveria carregar o saldo resultante")
	}
	if balance != amount {
		t.Errorf("ResultBalance() = %s, quero %s", balance, amount)
	}
	if !tx.CreatedAt().Equal(testNow) || !tx.UpdatedAt().Equal(testNow) {
		t.Errorf("timestamps = %s/%s, quero %s", tx.CreatedAt(), tx.UpdatedAt(), testNow)
	}
}

// TestNewOpeningTransaction_HasNoExternalMetadata cobre a regra de que provedor,
// ID externo, chave, hash, rodada, jogo e referencia nao se aplicam a origem
// interna.
func TestNewOpeningTransaction_HasNoExternalMetadata(t *testing.T) {
	tx, err := NewOpeningTransaction(uuid.New(), uuid.New(), uuid.New(), mustParse(t, "10.00", "BRL"), testNow)
	if err != nil {
		t.Fatalf("erro inesperado: %v", err)
	}

	empties := map[string]string{
		"ProviderID":            tx.ProviderID(),
		"ExternalTransactionID": tx.ExternalTransactionID(),
		"IdempotencyKey":        tx.IdempotencyKey(),
		"PayloadHash":           tx.PayloadHash(),
		"RoundID":               tx.RoundID(),
		"GameID":                tx.GameID(),
	}
	for name, value := range empties {
		if value != "" {
			t.Errorf("%s() = %q, quero vazio", name, value)
		}
	}
	if _, ok := tx.ReferenceExternalID(); ok {
		t.Error("OPENING nao pode carregar referencia externa")
	}
	if _, ok := tx.ReferenceTransactionID(); ok {
		t.Error("OPENING nao pode carregar referencia interna")
	}
	if _, ok := tx.FailureCode(); ok {
		t.Error("OPENING nao pode nascer com failure code")
	}
}

func TestNewOpeningTransaction_Rejections(t *testing.T) {
	valid := mustMoneyValue(100000, "BRL")

	tests := []struct {
		name     string
		id       uuid.UUID
		walletID uuid.UUID
		playerID uuid.UUID
		amount   Money
		now      time.Time
		wantErr  error
	}{
		{"id nulo", uuid.Nil, uuid.New(), uuid.New(), valid, testNow, ErrInvalidTransactionID},
		{"wallet nula", uuid.New(), uuid.Nil, uuid.New(), valid, testNow, ErrInvalidWalletID},
		{"player nulo", uuid.New(), uuid.New(), uuid.Nil, valid, testNow, ErrInvalidPlayerID},
		{"money nao inicializada", uuid.New(), uuid.New(), uuid.New(), Money{}, testNow, ErrUninitializedMoney},
		// Abertura com saldo zero nao cria OPENING, ledger nem eventos.
		{"valor zero", uuid.New(), uuid.New(), uuid.New(), mustMoneyValue(0, "BRL"), testNow, ErrInvalidTransactionAmount},
		{"valor negativo", uuid.New(), uuid.New(), uuid.New(), mustMoneyValue(-1, "BRL"), testNow, ErrInvalidTransactionAmount},
		{"timestamp zerado", uuid.New(), uuid.New(), uuid.New(), valid, time.Time{}, ErrInvalidTimestamp},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			tx, err := NewOpeningTransaction(tc.id, tc.walletID, tc.playerID, tc.amount, tc.now)
			if !errors.Is(err, tc.wantErr) {
				t.Fatalf("erro = %v, quero %v", err, tc.wantErr)
			}
			if tx != nil {
				t.Errorf("em caso de erro deve retornar nil, obtive %+v", tx)
			}
		})
	}
}

func TestNewOpeningTransaction_StoresTimestampsInUTC(t *testing.T) {
	zone := time.FixedZone("BRT", -3*60*60)
	local := testNow.In(zone)

	tx, err := NewOpeningTransaction(uuid.New(), uuid.New(), uuid.New(), mustParse(t, "10.00", "BRL"), local)
	if err != nil {
		t.Fatalf("erro inesperado: %v", err)
	}
	if tx.CreatedAt().Location() != time.UTC || tx.UpdatedAt().Location() != time.UTC {
		t.Errorf("timestamps devem ser armazenados em UTC, obtive %s", tx.CreatedAt().Location())
	}
	if !tx.CreatedAt().Equal(testNow) {
		t.Errorf("CreatedAt() = %s, quero o mesmo instante de %s", tx.CreatedAt(), testNow)
	}
}

// --------------------------------------------------------------------------
// NewExternalTransaction
// --------------------------------------------------------------------------

func TestNewExternalTransaction(t *testing.T) {
	params := externalParams(KindBet)

	tx, err := NewExternalTransaction(params)
	if err != nil {
		t.Fatalf("erro inesperado: %v", err)
	}

	if tx.State() != StatePending {
		t.Errorf("State() = %s, quero %s", tx.State(), StatePending)
	}
	if tx.IsTerminal() {
		t.Error("uma transacao externa nao pode nascer terminal")
	}
	if !tx.IsExternal() {
		t.Error("BET deveria ser marcada como externa")
	}
	if tx.ProviderID() != params.ProviderID {
		t.Errorf("ProviderID() = %q, quero %q", tx.ProviderID(), params.ProviderID)
	}
	if tx.ExternalTransactionID() != params.ExternalTransactionID {
		t.Errorf("ExternalTransactionID() = %q, quero %q", tx.ExternalTransactionID(), params.ExternalTransactionID)
	}
	if tx.IdempotencyKey() != params.IdempotencyKey {
		t.Errorf("IdempotencyKey() = %q, quero %q", tx.IdempotencyKey(), params.IdempotencyKey)
	}
	if tx.PayloadHash() != params.PayloadHash {
		t.Errorf("PayloadHash() = %q, quero %q", tx.PayloadHash(), params.PayloadHash)
	}
	if tx.RoundID() != params.RoundID || tx.GameID() != params.GameID {
		t.Errorf("contexto de jogo = %q/%q, quero %q/%q", tx.RoundID(), tx.GameID(), params.RoundID, params.GameID)
	}
	if _, ok := tx.ResultBalance(); ok {
		t.Error("uma transacao PENDING nao pode ter saldo resultante")
	}
	if _, ok := tx.FailureCode(); ok {
		t.Error("uma transacao PENDING nao pode ter failure code")
	}
}

// TestNewExternalTransaction_RejectsOpening cobre a regra de que OPENING e
// reservado a abertura interna e deve ser recusado por HTTP e SQS.
func TestNewExternalTransaction_RejectsOpening(t *testing.T) {
	params := externalParams(KindBet)
	params.Kind = KindOpening

	tx, err := NewExternalTransaction(params)
	if !errors.Is(err, ErrInvalidExternalKind) {
		t.Fatalf("erro = %v, quero %v", err, ErrInvalidExternalKind)
	}
	if tx != nil {
		t.Errorf("em caso de erro deve retornar nil, obtive %+v", tx)
	}
}

func TestNewExternalTransaction_UnknownKind(t *testing.T) {
	for _, kind := range []TransactionKind{"", "bet", "DEPOSIT"} {
		t.Run(string(kind), func(t *testing.T) {
			params := externalParams(KindBet)
			params.Kind = kind

			if _, err := NewExternalTransaction(params); !errors.Is(err, ErrUnknownTransactionKind) {
				t.Errorf("erro = %v, quero %v", err, ErrUnknownTransactionKind)
			}
		})
	}
}

func TestNewExternalTransaction_Rejections(t *testing.T) {
	tests := []struct {
		name    string
		mutate  func(*ExternalTransactionParams)
		wantErr error
	}{
		{"id nulo", func(p *ExternalTransactionParams) { p.ID = uuid.Nil }, ErrInvalidTransactionID},
		{"wallet nula", func(p *ExternalTransactionParams) { p.WalletID = uuid.Nil }, ErrInvalidWalletID},
		{"player nulo", func(p *ExternalTransactionParams) { p.PlayerID = uuid.Nil }, ErrInvalidPlayerID},
		{"provider vazio", func(p *ExternalTransactionParams) { p.ProviderID = "" }, ErrMissingExternalMetadata},
		{"provider em branco", func(p *ExternalTransactionParams) { p.ProviderID = "   " }, ErrMissingExternalMetadata},
		{"id externo vazio", func(p *ExternalTransactionParams) { p.ExternalTransactionID = "" }, ErrMissingExternalMetadata},
		{"chave de idempotencia vazia", func(p *ExternalTransactionParams) { p.IdempotencyKey = "" }, ErrMissingExternalMetadata},
		{"hash de payload vazio", func(p *ExternalTransactionParams) { p.PayloadHash = "" }, ErrMissingExternalMetadata},
		{"rodada vazia", func(p *ExternalTransactionParams) { p.RoundID = "" }, ErrMissingExternalMetadata},
		{"jogo vazio", func(p *ExternalTransactionParams) { p.GameID = "" }, ErrMissingExternalMetadata},
		{"money nao inicializada", func(p *ExternalTransactionParams) { p.Money = Money{} }, ErrUninitializedMoney},
		{"timestamp zerado", func(p *ExternalTransactionParams) { p.Now = time.Time{} }, ErrInvalidTimestamp},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			params := externalParams(KindBet)
			tc.mutate(&params)

			tx, err := NewExternalTransaction(params)
			if !errors.Is(err, tc.wantErr) {
				t.Fatalf("erro = %v, quero %v", err, tc.wantErr)
			}
			if tx != nil {
				t.Errorf("em caso de erro deve retornar nil, obtive %+v", tx)
			}
		})
	}
}

// TestNewExternalTransaction_AmountPolicyByKind cobre a politica de valores zero
// de cada tipo: LOSS exige "0.00" e os demais exigem valor maior que zero.
func TestNewExternalTransaction_AmountPolicyByKind(t *testing.T) {
	tests := []struct {
		name    string
		kind    TransactionKind
		amount  Money
		wantErr error
	}{
		{"BET positiva", KindBet, mustMoneyValue(2500, "BRL"), nil},
		{"BET zerada", KindBet, mustMoneyValue(0, "BRL"), ErrInvalidTransactionAmount},
		{"BET negativa", KindBet, mustMoneyValue(-2500, "BRL"), ErrInvalidTransactionAmount},
		{"WIN positiva", KindWin, mustMoneyValue(2500, "BRL"), nil},
		{"WIN zerada", KindWin, mustMoneyValue(0, "BRL"), ErrInvalidTransactionAmount},
		{"LOSS zerada", KindLoss, mustMoneyValue(0, "BRL"), nil},
		{"LOSS positiva", KindLoss, mustMoneyValue(1, "BRL"), ErrInvalidTransactionAmount},
		{"LOSS negativa", KindLoss, mustMoneyValue(-1, "BRL"), ErrInvalidTransactionAmount},
		{"REFUND positiva", KindRefund, mustMoneyValue(2500, "BRL"), nil},
		{"REFUND zerada", KindRefund, mustMoneyValue(0, "BRL"), ErrInvalidTransactionAmount},
		{"ROLLBACK positiva", KindRollback, mustMoneyValue(2500, "BRL"), nil},
		{"ROLLBACK zerada", KindRollback, mustMoneyValue(0, "BRL"), ErrInvalidTransactionAmount},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			params := externalParams(tc.kind)
			params.Money = tc.amount

			tx, err := NewExternalTransaction(params)
			if tc.wantErr == nil {
				if err != nil {
					t.Fatalf("erro inesperado: %v", err)
				}
				if tx.Money() != tc.amount {
					t.Errorf("Money() = %s, quero %s", tx.Money(), tc.amount)
				}
				return
			}
			if !errors.Is(err, tc.wantErr) {
				t.Fatalf("erro = %v, quero %v", err, tc.wantErr)
			}
		})
	}
}

// TestNewExternalTransaction_ReferencePolicyByKind cobre a obrigatoriedade de
// referenceExternalTransactionId em REFUND e ROLLBACK e sua recusa nos tipos
// que nao revertem nada.
func TestNewExternalTransaction_ReferencePolicyByKind(t *testing.T) {
	tests := []struct {
		name      string
		kind      TransactionKind
		reference string
		wantErr   error
	}{
		{"BET sem referencia", KindBet, "", nil},
		{"BET com referencia", KindBet, "transaction-000", ErrReferenceNotApplicable},
		{"LOSS com referencia", KindLoss, "transaction-000", ErrReferenceNotApplicable},
		{"WIN sem referencia", KindWin, "", nil},
		{"WIN com referencia da rodada", KindWin, "transaction-000", nil},
		{"REFUND com referencia", KindRefund, "transaction-000", nil},
		{"REFUND sem referencia", KindRefund, "", ErrMissingReference},
		{"REFUND com referencia em branco", KindRefund, "   ", ErrMissingReference},
		{"ROLLBACK com referencia", KindRollback, "transaction-000", nil},
		{"ROLLBACK sem referencia", KindRollback, "", ErrMissingReference},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			params := externalParams(tc.kind)
			params.ReferenceExternalID = tc.reference

			tx, err := NewExternalTransaction(params)
			if tc.wantErr != nil {
				if !errors.Is(err, tc.wantErr) {
					t.Fatalf("erro = %v, quero %v", err, tc.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatalf("erro inesperado: %v", err)
			}

			reference, ok := tx.ReferenceExternalID()
			if ok != (tc.reference != "") {
				t.Fatalf("presenca da referencia = %v, quero %v", ok, tc.reference != "")
			}
			if ok && reference != tc.reference {
				t.Errorf("ReferenceExternalID() = %q, quero %q", reference, tc.reference)
			}
			// A referencia interna so existe depois da resolucao.
			if _, resolved := tx.ReferenceTransactionID(); resolved {
				t.Error("a referencia interna nao pode vir resolvida da criacao")
			}
		})
	}
}

func TestNewExternalTransaction_StoresTimestampsInUTC(t *testing.T) {
	zone := time.FixedZone("BRT", -3*60*60)
	params := externalParams(KindBet)
	params.Now = testNow.In(zone)

	tx, err := NewExternalTransaction(params)
	if err != nil {
		t.Fatalf("erro inesperado: %v", err)
	}
	if tx.CreatedAt().Location() != time.UTC || tx.UpdatedAt().Location() != time.UTC {
		t.Errorf("timestamps devem ser armazenados em UTC, obtive %s", tx.CreatedAt().Location())
	}
	if !tx.CreatedAt().Equal(testNow) {
		t.Errorf("CreatedAt() = %s, quero o mesmo instante de %s", tx.CreatedAt(), testNow)
	}
}

// --------------------------------------------------------------------------
// RestoreTransaction
// --------------------------------------------------------------------------

func TestRestoreTransaction_PreservesPersistedState(t *testing.T) {
	params := restoredParams(KindRefund, StateProcessed)

	tx, err := RestoreTransaction(params)
	if err != nil {
		t.Fatalf("erro inesperado: %v", err)
	}

	if tx.State() != StateProcessed {
		t.Errorf("State() = %s, quero %s", tx.State(), StateProcessed)
	}
	if tx.Kind() != KindRefund {
		t.Errorf("Kind() = %s, quero %s", tx.Kind(), KindRefund)
	}
	if tx.Money() != params.Money {
		t.Errorf("Money() = %s, quero %s", tx.Money(), params.Money)
	}

	balance, ok := tx.ResultBalance()
	if !ok || balance != params.ResultBalance {
		t.Errorf("ResultBalance() = %s/%v, quero %s", balance, ok, params.ResultBalance)
	}
	reference, resolved := tx.ReferenceTransactionID()
	if !resolved || reference != params.ReferenceTransactionID {
		t.Errorf("ReferenceTransactionID() = %s/%v, quero %s", reference, resolved, params.ReferenceTransactionID)
	}
	if !tx.CreatedAt().Equal(params.CreatedAt) || !tx.UpdatedAt().Equal(params.UpdatedAt) {
		t.Errorf("timestamps = %s/%s, quero %s/%s", tx.CreatedAt(), tx.UpdatedAt(), params.CreatedAt, params.UpdatedAt)
	}
}

// TestRestoreTransaction_DoesNotReapplyTransitions garante que a reidratacao nao
// reaplica a operacao: um registro PENDING volta PENDING, sem resultado.
func TestRestoreTransaction_DoesNotReapplyTransitions(t *testing.T) {
	tx, err := RestoreTransaction(restoredParams(KindBet, StatePending))
	if err != nil {
		t.Fatalf("erro inesperado: %v", err)
	}

	if tx.State() != StatePending {
		t.Errorf("State() = %s, quero %s", tx.State(), StatePending)
	}
	if tx.IsTerminal() {
		t.Error("um registro PENDING nao pode voltar terminal")
	}
	if _, ok := tx.ResultBalance(); ok {
		t.Error("um registro PENDING nao pode carregar saldo resultante")
	}
	if _, ok := tx.FailureCode(); ok {
		t.Error("um registro PENDING nao pode carregar failure code")
	}
}

func TestRestoreTransaction_AcceptsEveryConsistentCombination(t *testing.T) {
	states := []TransactionState{StatePending, StatePendingRef, StateProcessed, StateRejected, StateFailed}

	for _, kind := range externalKinds {
		for _, state := range states {
			// PENDING_REFERENCE so existe para quem aceita referencia.
			if state == StatePendingRef && !kind.AcceptsReference() {
				continue
			}
			t.Run(string(kind)+"/"+string(state), func(t *testing.T) {
				if _, err := RestoreTransaction(restoredParams(kind, state)); err != nil {
					t.Errorf("erro inesperado: %v", err)
				}
			})
		}
	}

	t.Run("OPENING/PROCESSED", func(t *testing.T) {
		if _, err := RestoreTransaction(restoredParams(KindOpening, StateProcessed)); err != nil {
			t.Errorf("erro inesperado: %v", err)
		}
	})
}

func TestRestoreTransaction_Rejections(t *testing.T) {
	tests := []struct {
		name    string
		kind    TransactionKind
		state   TransactionState
		mutate  func(*RestoredTransactionParams)
		wantErr error
	}{
		{"kind desconhecido", KindBet, StatePending,
			func(p *RestoredTransactionParams) { p.Kind = "DEPOSIT" }, ErrUnknownTransactionKind},
		{"state desconhecido", KindBet, StatePending,
			func(p *RestoredTransactionParams) { p.State = "DONE" }, ErrUnknownTransactionState},
		{"state vazio", KindBet, StatePending,
			func(p *RestoredTransactionParams) { p.State = "" }, ErrUnknownTransactionState},
		{"id nulo", KindBet, StatePending,
			func(p *RestoredTransactionParams) { p.ID = uuid.Nil }, ErrInvalidTransactionID},
		{"wallet nula", KindBet, StatePending,
			func(p *RestoredTransactionParams) { p.WalletID = uuid.Nil }, ErrInvalidWalletID},
		{"player nulo", KindBet, StatePending,
			func(p *RestoredTransactionParams) { p.PlayerID = uuid.Nil }, ErrInvalidPlayerID},
		{"externa sem provedor", KindBet, StatePending,
			func(p *RestoredTransactionParams) { p.ProviderID = "" }, ErrMissingExternalMetadata},
		{"externa sem hash", KindBet, StatePending,
			func(p *RestoredTransactionParams) { p.PayloadHash = "" }, ErrMissingExternalMetadata},
		{"OPENING com provedor", KindOpening, StateProcessed,
			func(p *RestoredTransactionParams) { p.ProviderID = "provider-a" }, ErrInvalidTransactionRecord},
		{"OPENING com rodada", KindOpening, StateProcessed,
			func(p *RestoredTransactionParams) { p.RoundID = "round-987" }, ErrInvalidTransactionRecord},
		{"valor invalido para o tipo", KindLoss, StatePending,
			func(p *RestoredTransactionParams) { p.Money = mustMoneyValue(1, "BRL") }, ErrInvalidTransactionAmount},
		{"money nao inicializada", KindBet, StatePending,
			func(p *RestoredTransactionParams) { p.Money = Money{} }, ErrUninitializedMoney},
		{"reversao sem referencia externa", KindRollback, StatePending,
			func(p *RestoredTransactionParams) { p.ReferenceExternalID = "" }, ErrMissingReference},
		{"BET com referencia externa", KindBet, StatePending,
			func(p *RestoredTransactionParams) { p.ReferenceExternalID = "transaction-000" }, ErrReferenceNotApplicable},
		{"rejeitada sem failure code", KindBet, StateRejected,
			func(p *RestoredTransactionParams) { p.FailureCode = "" }, ErrInvalidTransactionRecord},
		{"falha sem failure code", KindBet, StateFailed,
			func(p *RestoredTransactionParams) { p.FailureCode = "" }, ErrInvalidTransactionRecord},
		{"pendente com failure code", KindBet, StatePending,
			func(p *RestoredTransactionParams) { p.FailureCode = FailureInsufficientFunds }, ErrInvalidTransactionRecord},
		{"processada com failure code", KindBet, StateProcessed,
			func(p *RestoredTransactionParams) { p.FailureCode = FailureInsufficientFunds }, ErrInvalidTransactionRecord},
		{"processada sem saldo resultante", KindBet, StateProcessed,
			func(p *RestoredTransactionParams) { p.ResultBalance = Money{} }, ErrInvalidTransactionRecord},
		{"pendente com saldo resultante", KindBet, StatePending,
			func(p *RestoredTransactionParams) { p.ResultBalance = mustMoneyValue(100, "BRL") }, ErrInvalidTransactionRecord},
		{"saldo resultante em outra moeda", KindBet, StateProcessed,
			func(p *RestoredTransactionParams) { p.ResultBalance = mustMoneyValue(100, "USD") }, ErrCurrencyMismatch},
		{"saldo resultante negativo", KindBet, StateProcessed,
			func(p *RestoredTransactionParams) { p.ResultBalance = mustMoneyValue(-1, "BRL") }, ErrInvalidTransactionRecord},
		{"reversao processada sem referencia interna", KindRefund, StateProcessed,
			func(p *RestoredTransactionParams) { p.ReferenceTransactionID = uuid.Nil }, ErrInvalidTransactionRecord},
		{"BET com referencia interna", KindBet, StatePending,
			func(p *RestoredTransactionParams) { p.ReferenceTransactionID = uuid.New() }, ErrInvalidTransactionRecord},
		{"referencia interna sem referencia externa", KindWin, StatePending,
			func(p *RestoredTransactionParams) { p.ReferenceTransactionID = uuid.New() }, ErrInvalidTransactionRecord},
		{"transacao referenciando a si mesma", KindRefund, StateProcessed,
			func(p *RestoredTransactionParams) { p.ReferenceTransactionID = p.ID }, ErrInvalidTransactionRecord},
		{"createdAt zerado", KindBet, StatePending,
			func(p *RestoredTransactionParams) { p.CreatedAt = time.Time{} }, ErrInvalidTimestamp},
		{"updatedAt zerado", KindBet, StatePending,
			func(p *RestoredTransactionParams) { p.UpdatedAt = time.Time{} }, ErrInvalidTimestamp},
		{"updatedAt antes de createdAt", KindBet, StatePending,
			func(p *RestoredTransactionParams) { p.UpdatedAt = p.CreatedAt.Add(-time.Second) }, ErrInvalidTransactionRecord},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			params := restoredParams(tc.kind, tc.state)
			tc.mutate(&params)

			tx, err := RestoreTransaction(params)
			if !errors.Is(err, tc.wantErr) {
				t.Fatalf("erro = %v, quero %v", err, tc.wantErr)
			}
			if tx != nil {
				t.Errorf("em caso de erro deve retornar nil, obtive %+v", tx)
			}
		})
	}
}

func TestRestoreTransaction_StoresTimestampsInUTC(t *testing.T) {
	zone := time.FixedZone("BRT", -3*60*60)
	params := restoredParams(KindBet, StatePending)
	params.CreatedAt = testNow.In(zone)
	params.UpdatedAt = testNow.In(zone)

	tx, err := RestoreTransaction(params)
	if err != nil {
		t.Fatalf("erro inesperado: %v", err)
	}
	if tx.CreatedAt().Location() != time.UTC || tx.UpdatedAt().Location() != time.UTC {
		t.Errorf("timestamps devem ser armazenados em UTC, obtive %s", tx.CreatedAt().Location())
	}
}

// --------------------------------------------------------------------------
// MarkAsProcessed
// --------------------------------------------------------------------------

func TestMarkAsProcessed(t *testing.T) {
	tx := mustExternal(t, KindBet)
	balance := mustParse(t, "975.00", "BRL")

	if err := tx.MarkAsProcessed(balance, testLater); err != nil {
		t.Fatalf("erro inesperado: %v", err)
	}

	if tx.State() != StateProcessed {
		t.Errorf("State() = %s, quero %s", tx.State(), StateProcessed)
	}
	if !tx.IsTerminal() {
		t.Error("PROCESSED deveria ser terminal")
	}
	got, ok := tx.ResultBalance()
	if !ok || got != balance {
		t.Errorf("ResultBalance() = %s/%v, quero %s", got, ok, balance)
	}
	if _, ok := tx.FailureCode(); ok {
		t.Error("uma transacao processada nao pode ter failure code")
	}
	if !tx.UpdatedAt().Equal(testLater) {
		t.Errorf("UpdatedAt() = %s, quero %s", tx.UpdatedAt(), testLater)
	}
	if !tx.CreatedAt().Equal(testNow) {
		t.Errorf("CreatedAt() = %s nao deveria mudar", tx.CreatedAt())
	}
}

// TestMarkAsProcessed_LossKeepsBalance cobre LOSS: valor zero, sem movimentacao,
// mas com resultado registrado para o replay.
func TestMarkAsProcessed_LossKeepsBalance(t *testing.T) {
	tx := mustExternal(t, KindLoss)
	balance := mustParse(t, "1000.00", "BRL")

	if err := tx.MarkAsProcessed(balance, testLater); err != nil {
		t.Fatalf("erro inesperado: %v", err)
	}
	if !tx.Money().IsZero() {
		t.Errorf("Money() = %s, quero 0.00", tx.Money())
	}
	if _, moves := tx.Kind().Direction(); moves {
		t.Error("LOSS nao pode gerar movimentacao no ledger")
	}
	got, _ := tx.ResultBalance()
	if got != balance {
		t.Errorf("ResultBalance() = %s, quero %s", got, balance)
	}
}

func TestMarkAsProcessed_Rejections(t *testing.T) {
	tests := []struct {
		name    string
		balance Money
		wantErr error
	}{
		{"saldo nao inicializado", Money{}, ErrUninitializedMoney},
		{"moeda divergente", mustMoneyValue(97500, "USD"), ErrCurrencyMismatch},
		{"saldo negativo", mustMoneyValue(-1, "BRL"), ErrInvalidTransactionAmount},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			tx := mustExternal(t, KindBet)

			if err := tx.MarkAsProcessed(tc.balance, testLater); !errors.Is(err, tc.wantErr) {
				t.Fatalf("erro = %v, quero %v", err, tc.wantErr)
			}
			if tx.State() != StatePending {
				t.Errorf("State() = %s, a transacao nao pode mudar em caso de erro", tx.State())
			}
			if _, ok := tx.ResultBalance(); ok {
				t.Error("nenhum saldo deveria ter sido registrado")
			}
		})
	}
}

// TestMarkAsProcessed_ReversalRequiresResolvedReference garante que uma reversao
// nunca e aplicada sem saber qual transacao interna ela desfaz.
func TestMarkAsProcessed_ReversalRequiresResolvedReference(t *testing.T) {
	for _, kind := range []TransactionKind{KindRefund, KindRollback} {
		t.Run(string(kind), func(t *testing.T) {
			tx := mustExternal(t, kind)
			balance := mustParse(t, "100.00", "BRL")

			if err := tx.MarkAsProcessed(balance, testLater); !errors.Is(err, ErrUnresolvedReference) {
				t.Fatalf("erro = %v, quero %v", err, ErrUnresolvedReference)
			}
			if tx.State() != StatePending {
				t.Errorf("State() = %s, quero %s", tx.State(), StatePending)
			}

			if err := tx.ResolveReference(uuid.New(), testLater); err != nil {
				t.Fatalf("ResolveReference: %v", err)
			}
			if err := tx.MarkAsProcessed(balance, testLater); err != nil {
				t.Fatalf("apos resolver a referencia deveria processar: %v", err)
			}
		})
	}
}

// TestProcessedReplayKeepsOriginalBalance cobre a regra de que o replay devolve o
// saldo observado no processamento original, mesmo que a carteira tenha se
// movimentado depois.
func TestProcessedReplayKeepsOriginalBalance(t *testing.T) {
	tx := mustExternal(t, KindBet)
	observed := mustParse(t, "975.00", "BRL")

	if err := tx.MarkAsProcessed(observed, testLater); err != nil {
		t.Fatalf("erro inesperado: %v", err)
	}

	// Uma nova tentativa sobre a mesma transacao nao reaplica nada.
	if err := tx.MarkAsProcessed(mustParse(t, "500.00", "BRL"), testLater.Add(time.Hour)); !errors.Is(err, ErrTransactionTerminal) {
		t.Fatalf("erro = %v, quero %v", err, ErrTransactionTerminal)
	}

	got, _ := tx.ResultBalance()
	if got != observed {
		t.Errorf("ResultBalance() = %s, quero %s", got, observed)
	}
	if !tx.UpdatedAt().Equal(testLater) {
		t.Errorf("UpdatedAt() = %s, quero %s", tx.UpdatedAt(), testLater)
	}
}

// --------------------------------------------------------------------------
// Reject e Fail
// --------------------------------------------------------------------------

func TestReject(t *testing.T) {
	tx := mustExternal(t, KindBet)

	if err := tx.Reject(FailureInsufficientFunds, testLater); err != nil {
		t.Fatalf("erro inesperado: %v", err)
	}
	if tx.State() != StateRejected {
		t.Errorf("State() = %s, quero %s", tx.State(), StateRejected)
	}
	if !tx.IsTerminal() {
		t.Error("REJECTED deveria ser terminal")
	}
	code, ok := tx.FailureCode()
	if !ok || code != FailureInsufficientFunds {
		t.Errorf("FailureCode() = %q/%v, quero %q", code, ok, FailureInsufficientFunds)
	}
	if _, ok := tx.ResultBalance(); ok {
		t.Error("uma transacao rejeitada nao pode ter saldo resultante")
	}
}

func TestFail(t *testing.T) {
	tx := mustExternal(t, KindBet)

	if err := tx.Fail(FailureInternalError, testLater); err != nil {
		t.Fatalf("erro inesperado: %v", err)
	}
	if tx.State() != StateFailed {
		t.Errorf("State() = %s, quero %s", tx.State(), StateFailed)
	}
	code, ok := tx.FailureCode()
	if !ok || code != FailureInternalError {
		t.Errorf("FailureCode() = %q/%v, quero %q", code, ok, FailureInternalError)
	}
}

func TestRejectAndFail_RequireFailureCode(t *testing.T) {
	for _, code := range []FailureCode{"", "   "} {
		t.Run("codigo "+string(code), func(t *testing.T) {
			tx := mustExternal(t, KindBet)
			if err := tx.Reject(code, testLater); !errors.Is(err, ErrMissingFailureCode) {
				t.Errorf("Reject erro = %v, quero %v", err, ErrMissingFailureCode)
			}
			if err := tx.Fail(code, testLater); !errors.Is(err, ErrMissingFailureCode) {
				t.Errorf("Fail erro = %v, quero %v", err, ErrMissingFailureCode)
			}
			if tx.State() != StatePending {
				t.Errorf("State() = %s, a transacao nao pode mudar em caso de erro", tx.State())
			}
		})
	}
}

// TestFailureCodesAreDistinct cobre a exigencia de que a rejeicao de uma reversao
// sem saldo use um codigo diferente do de uma aposta sem saldo.
func TestFailureCodesAreDistinct(t *testing.T) {
	if FailureReversalInsufficientFunds == FailureInsufficientFunds {
		t.Error("a reversao sem saldo precisa de um failure code proprio")
	}

	seen := map[FailureCode]bool{}
	codes := []FailureCode{
		FailureInsufficientFunds,
		FailureReversalInsufficientFunds,
		FailureReferenceNotFound,
		FailureReferenceNotProcessed,
		FailureReferenceMismatch,
		FailureAlreadyReversed,
		FailureWalletNotFound,
		FailureCurrencyMismatch,
		FailureInternalError,
	}
	for _, code := range codes {
		if code == "" {
			t.Error("nenhum failure code pode ser vazio")
		}
		if seen[code] {
			t.Errorf("failure code duplicado: %q", code)
		}
		seen[code] = true
	}
}

// --------------------------------------------------------------------------
// PENDING_REFERENCE e resolucao
// --------------------------------------------------------------------------

func TestMarkAsPendingReference(t *testing.T) {
	tx := mustExternal(t, KindRefund)

	if err := tx.MarkAsPendingReference(testLater); err != nil {
		t.Fatalf("erro inesperado: %v", err)
	}
	if tx.State() != StatePendingRef {
		t.Errorf("State() = %s, quero %s", tx.State(), StatePendingRef)
	}
	if tx.IsTerminal() {
		t.Error("PENDING_REFERENCE nao pode ser terminal")
	}
	if !tx.UpdatedAt().Equal(testLater) {
		t.Errorf("UpdatedAt() = %s, quero %s", tx.UpdatedAt(), testLater)
	}
}

// TestPendingReferenceFullFlow percorre o caminho da reversao que chega antes da
// referencia: espera, resolucao pelo worker e conclusao.
func TestPendingReferenceFullFlow(t *testing.T) {
	tx := mustExternal(t, KindRollback)
	referenceID := uuid.New()
	balance := mustParse(t, "100.00", "BRL")

	if err := tx.MarkAsPendingReference(testLater); err != nil {
		t.Fatalf("MarkAsPendingReference: %v", err)
	}
	if err := tx.ResolveReference(referenceID, testLater.Add(time.Minute)); err != nil {
		t.Fatalf("ResolveReference: %v", err)
	}

	// Resolver nao conclui a operacao, apenas registra a referencia interna.
	if tx.State() != StatePendingRef {
		t.Errorf("State() = %s, quero %s", tx.State(), StatePendingRef)
	}
	got, resolved := tx.ReferenceTransactionID()
	if !resolved || got != referenceID {
		t.Errorf("ReferenceTransactionID() = %s/%v, quero %s", got, resolved, referenceID)
	}

	if err := tx.MarkAsProcessed(balance, testLater.Add(2*time.Minute)); err != nil {
		t.Fatalf("MarkAsProcessed: %v", err)
	}
	if tx.State() != StateProcessed {
		t.Errorf("State() = %s, quero %s", tx.State(), StateProcessed)
	}
}

// TestPendingReferenceExpiresAsRejected cobre o esgotamento das tentativas do
// worker de referencias.
func TestPendingReferenceExpiresAsRejected(t *testing.T) {
	tx := mustExternal(t, KindRefund)

	if err := tx.MarkAsPendingReference(testLater); err != nil {
		t.Fatalf("MarkAsPendingReference: %v", err)
	}
	if err := tx.Reject(FailureReferenceNotFound, testLater.Add(time.Hour)); err != nil {
		t.Fatalf("Reject: %v", err)
	}

	if tx.State() != StateRejected {
		t.Errorf("State() = %s, quero %s", tx.State(), StateRejected)
	}
	code, _ := tx.FailureCode()
	if code != FailureReferenceNotFound {
		t.Errorf("FailureCode() = %q, quero %q", code, FailureReferenceNotFound)
	}
}

func TestMarkAsPendingReference_NotApplicable(t *testing.T) {
	for _, kind := range []TransactionKind{KindBet, KindLoss} {
		t.Run(string(kind), func(t *testing.T) {
			tx := mustExternal(t, kind)

			if err := tx.MarkAsPendingReference(testLater); !errors.Is(err, ErrReferenceNotApplicable) {
				t.Fatalf("erro = %v, quero %v", err, ErrReferenceNotApplicable)
			}
			if tx.State() != StatePending {
				t.Errorf("State() = %s, quero %s", tx.State(), StatePending)
			}
		})
	}
}

// TestMarkAsPendingReference_WinWithoutReference garante que um WIN sem
// referencia nao fica esperando algo que nunca foi pedido.
func TestMarkAsPendingReference_WinWithoutReference(t *testing.T) {
	tx := mustExternal(t, KindWin)

	if err := tx.MarkAsPendingReference(testLater); !errors.Is(err, ErrMissingReference) {
		t.Fatalf("erro = %v, quero %v", err, ErrMissingReference)
	}
}

func TestResolveReference_Rejections(t *testing.T) {
	t.Run("id interno nulo", func(t *testing.T) {
		tx := mustExternal(t, KindRefund)
		if err := tx.ResolveReference(uuid.Nil, testLater); !errors.Is(err, ErrInvalidTransactionID) {
			t.Errorf("erro = %v, quero %v", err, ErrInvalidTransactionID)
		}
	})

	t.Run("referencia a si mesma", func(t *testing.T) {
		tx := mustExternal(t, KindRefund)
		if err := tx.ResolveReference(tx.ID(), testLater); !errors.Is(err, ErrInvalidTransactionID) {
			t.Errorf("erro = %v, quero %v", err, ErrInvalidTransactionID)
		}
		if _, resolved := tx.ReferenceTransactionID(); resolved {
			t.Error("nenhuma referencia deveria ter sido registrada")
		}
	})

	t.Run("tipo sem referencia", func(t *testing.T) {
		tx := mustExternal(t, KindBet)
		if err := tx.ResolveReference(uuid.New(), testLater); !errors.Is(err, ErrReferenceNotApplicable) {
			t.Errorf("erro = %v, quero %v", err, ErrReferenceNotApplicable)
		}
	})

	t.Run("WIN sem referencia externa", func(t *testing.T) {
		tx := mustExternal(t, KindWin)
		if err := tx.ResolveReference(uuid.New(), testLater); !errors.Is(err, ErrMissingReference) {
			t.Errorf("erro = %v, quero %v", err, ErrMissingReference)
		}
	})
}

// TestResolveReference_IsIdempotentButStable aceita a repeticao do worker e
// recusa a troca silenciosa da transacao referenciada.
func TestResolveReference_IsIdempotentButStable(t *testing.T) {
	tx := mustExternal(t, KindRefund)
	referenceID := uuid.New()

	if err := tx.ResolveReference(referenceID, testLater); err != nil {
		t.Fatalf("primeira resolucao: %v", err)
	}
	if err := tx.ResolveReference(referenceID, testLater); err != nil {
		t.Fatalf("repetir a mesma resolucao deveria ser aceito: %v", err)
	}
	if err := tx.ResolveReference(uuid.New(), testLater); !errors.Is(err, ErrReferenceAlreadyResolved) {
		t.Fatalf("erro = %v, quero %v", err, ErrReferenceAlreadyResolved)
	}

	got, _ := tx.ReferenceTransactionID()
	if got != referenceID {
		t.Errorf("ReferenceTransactionID() = %s, quero %s", got, referenceID)
	}
}

// --------------------------------------------------------------------------
// Invariantes da maquina de estados
// --------------------------------------------------------------------------

// TestTerminalStateRejectsEveryTransition cobre a regra de que uma transacao
// terminal nao sofre novas transicoes, para qualquer combinacao.
func TestTerminalStateRejectsEveryTransition(t *testing.T) {
	terminals := []TransactionState{StateProcessed, StateRejected, StateFailed}
	transitions := map[string]func(*WagerTransaction) error{
		"MarkAsProcessed": func(tx *WagerTransaction) error {
			return tx.MarkAsProcessed(mustMoneyValue(100, "BRL"), testLater)
		},
		"Reject": func(tx *WagerTransaction) error {
			return tx.Reject(FailureInsufficientFunds, testLater)
		},
		"Fail": func(tx *WagerTransaction) error {
			return tx.Fail(FailureInternalError, testLater)
		},
		"MarkAsPendingReference": func(tx *WagerTransaction) error {
			return tx.MarkAsPendingReference(testLater)
		},
		"ResolveReference": func(tx *WagerTransaction) error {
			return tx.ResolveReference(uuid.New(), testLater)
		},
	}

	for _, state := range terminals {
		for name, transition := range transitions {
			t.Run(string(state)+"/"+name, func(t *testing.T) {
				tx, err := RestoreTransaction(restoredParams(KindRefund, state))
				if err != nil {
					t.Fatalf("RestoreTransaction: %v", err)
				}
				before := *tx

				if err := transition(tx); !errors.Is(err, ErrTransactionTerminal) {
					t.Fatalf("erro = %v, quero %v", err, ErrTransactionTerminal)
				}
				if *tx != before {
					t.Error("a transacao nao pode ser alterada por uma transicao recusada")
				}
			})
		}
	}
}

// TestOpeningIsBornTerminal garante que a abertura interna nao pode ser
// reprocessada depois de criada.
func TestOpeningIsBornTerminal(t *testing.T) {
	tx, err := NewOpeningTransaction(uuid.New(), uuid.New(), uuid.New(), mustParse(t, "1000.00", "BRL"), testNow)
	if err != nil {
		t.Fatalf("erro inesperado: %v", err)
	}

	if err := tx.MarkAsProcessed(mustParse(t, "1000.00", "BRL"), testLater); !errors.Is(err, ErrTransactionTerminal) {
		t.Errorf("erro = %v, quero %v", err, ErrTransactionTerminal)
	}
	if err := tx.Reject(FailureInsufficientFunds, testLater); !errors.Is(err, ErrTransactionTerminal) {
		t.Errorf("erro = %v, quero %v", err, ErrTransactionTerminal)
	}
}

// TestUninitializedTransactionRejectsTransitions garante que um valor zero nao se
// comporte como uma transacao PENDING valida.
func TestUninitializedTransactionRejectsTransitions(t *testing.T) {
	tests := map[string]func(*WagerTransaction) error{
		"MarkAsProcessed": func(tx *WagerTransaction) error {
			return tx.MarkAsProcessed(mustMoneyValue(100, "BRL"), testNow)
		},
		"Reject": func(tx *WagerTransaction) error { return tx.Reject(FailureInsufficientFunds, testNow) },
		"Fail":   func(tx *WagerTransaction) error { return tx.Fail(FailureInternalError, testNow) },
		"MarkAsPendingReference": func(tx *WagerTransaction) error {
			return tx.MarkAsPendingReference(testNow)
		},
		"ResolveReference": func(tx *WagerTransaction) error { return tx.ResolveReference(uuid.New(), testNow) },
	}

	for name, transition := range tests {
		t.Run(name, func(t *testing.T) {
			var tx WagerTransaction

			if err := transition(&tx); !errors.Is(err, ErrUnknownTransactionState) {
				t.Errorf("erro = %v, quero %v", err, ErrUnknownTransactionState)
			}
		})
	}
}

func TestTransitionsRequireTimestamp(t *testing.T) {
	tests := map[string]func(*WagerTransaction) error{
		"MarkAsProcessed": func(tx *WagerTransaction) error {
			return tx.MarkAsProcessed(mustMoneyValue(100, "BRL"), time.Time{})
		},
		"Reject": func(tx *WagerTransaction) error { return tx.Reject(FailureInsufficientFunds, time.Time{}) },
		"Fail":   func(tx *WagerTransaction) error { return tx.Fail(FailureInternalError, time.Time{}) },
		"MarkAsPendingReference": func(tx *WagerTransaction) error {
			return tx.MarkAsPendingReference(time.Time{})
		},
		"ResolveReference": func(tx *WagerTransaction) error { return tx.ResolveReference(uuid.New(), time.Time{}) },
	}

	for name, transition := range tests {
		t.Run(name, func(t *testing.T) {
			tx := mustExternal(t, KindRefund)

			if err := transition(tx); !errors.Is(err, ErrInvalidTimestamp) {
				t.Fatalf("erro = %v, quero %v", err, ErrInvalidTimestamp)
			}
			if tx.State() != StatePending {
				t.Errorf("State() = %s, a transacao nao pode mudar em caso de erro", tx.State())
			}
		})
	}
}

// TestUpdatedAtNeverMovesBackwards protege contra o relogio atrasado de outra
// instancia, que deixaria updatedAt antes de createdAt.
func TestTransactionUpdatedAtNeverMovesBackwards(t *testing.T) {
	tx := mustExternal(t, KindRefund)
	past := testNow.Add(-time.Hour)

	if err := tx.ResolveReference(uuid.New(), past); err != nil {
		t.Fatalf("ResolveReference: %v", err)
	}
	if tx.UpdatedAt().Before(tx.CreatedAt()) {
		t.Errorf("UpdatedAt() = %s esta antes de CreatedAt() = %s", tx.UpdatedAt(), tx.CreatedAt())
	}
	if !tx.UpdatedAt().Equal(testNow) {
		t.Errorf("UpdatedAt() = %s, quero %s", tx.UpdatedAt(), testNow)
	}

	if err := tx.MarkAsProcessed(mustParse(t, "100.00", "BRL"), past); err != nil {
		t.Fatalf("MarkAsProcessed: %v", err)
	}
	if !tx.UpdatedAt().Equal(testNow) {
		t.Errorf("UpdatedAt() = %s, quero %s", tx.UpdatedAt(), testNow)
	}
}

func TestTransitionsStoreTimestampsInUTC(t *testing.T) {
	zone := time.FixedZone("BRT", -3*60*60)
	tx := mustExternal(t, KindBet)

	if err := tx.MarkAsProcessed(mustParse(t, "100.00", "BRL"), testLater.In(zone)); err != nil {
		t.Fatalf("erro inesperado: %v", err)
	}
	if tx.UpdatedAt().Location() != time.UTC {
		t.Errorf("UpdatedAt().Location() = %s, quero UTC", tx.UpdatedAt().Location())
	}
	if !tx.UpdatedAt().Equal(testLater) {
		t.Errorf("UpdatedAt() = %s, quero %s", tx.UpdatedAt(), testLater)
	}
}

// --------------------------------------------------------------------------
// Encapsulamento
// --------------------------------------------------------------------------

// TestAccessorsDoNotLeakMutableState garante que nenhum getter devolva algo que
// permita alterar a transacao por fora.
func TestAccessorsDoNotLeakMutableState(t *testing.T) {
	params := externalParams(KindRefund)
	reference := params.ReferenceExternalID

	tx, err := NewExternalTransaction(params)
	if err != nil {
		t.Fatalf("erro inesperado: %v", err)
	}

	// Alterar a struct de entrada depois da construcao nao pode afetar a
	// entidade, ja que todos os campos sao copiados por valor.
	params.ReferenceExternalID = "outra-transacao"
	params.ProviderID = "outro-provider"
	params.Money = mustMoneyValue(1, "USD")

	got, _ := tx.ReferenceExternalID()
	if got != reference {
		t.Errorf("ReferenceExternalID() = %q, quero %q", got, reference)
	}
	if tx.ProviderID() != "provider-a" {
		t.Errorf("ProviderID() = %q, quero provider-a", tx.ProviderID())
	}
	if tx.Money().Currency() != "BRL" {
		t.Errorf("Money().Currency() = %q, quero BRL", tx.Money().Currency())
	}
}

// --------------------------------------------------------------------------
// helpers
// --------------------------------------------------------------------------

// amountFor devolve um valor valido para o tipo, respeitando a politica de zero.
func amountFor(kind TransactionKind) Money {
	if kind == KindLoss {
		return mustMoneyValue(0, "BRL")
	}
	return mustMoneyValue(2500, "BRL")
}

// externalParams monta um comando externo valido para o tipo informado.
func externalParams(kind TransactionKind) ExternalTransactionParams {
	params := ExternalTransactionParams{
		ID:                    uuid.New(),
		WalletID:              uuid.New(),
		PlayerID:              uuid.New(),
		ProviderID:            "provider-a",
		ExternalTransactionID: "transaction-123",
		IdempotencyKey:        "provider-a:transaction-123",
		PayloadHash:           "9f2c7a1b4e6d8c0a",
		RoundID:               "round-987",
		GameID:                "fortune-chimp",
		Kind:                  kind,
		Money:                 amountFor(kind),
		Now:                   testNow,
	}
	if kind.RequiresReference() {
		params.ReferenceExternalID = "transaction-000"
	}
	return params
}

func mustExternal(t *testing.T, kind TransactionKind) *WagerTransaction {
	t.Helper()

	tx, err := NewExternalTransaction(externalParams(kind))
	if err != nil {
		t.Fatalf("NewExternalTransaction(%s): %v", kind, err)
	}
	return tx
}

// restoredParams monta uma linha persistida coerente com o tipo e o estado, para
// que os testes de rejeicao quebrem um unico campo por vez.
func restoredParams(kind TransactionKind, state TransactionState) RestoredTransactionParams {
	params := RestoredTransactionParams{
		ID:        uuid.New(),
		WalletID:  uuid.New(),
		PlayerID:  uuid.New(),
		Kind:      kind,
		Money:     amountFor(kind),
		State:     state,
		CreatedAt: testNow,
		UpdatedAt: testLater,
	}

	if kind.IsExternal() {
		params.ProviderID = "provider-a"
		params.ExternalTransactionID = "transaction-123"
		params.IdempotencyKey = "provider-a:transaction-123"
		params.PayloadHash = "9f2c7a1b4e6d8c0a"
		params.RoundID = "round-987"
		params.GameID = "fortune-chimp"
	}
	if kind.RequiresReference() {
		params.ReferenceExternalID = "transaction-000"
	}

	switch state {
	case StateProcessed:
		params.ResultBalance = mustMoneyValue(97500, "BRL")
		if kind.RequiresReference() {
			params.ReferenceTransactionID = uuid.New()
		}
	case StateRejected:
		params.FailureCode = FailureInsufficientFunds
	case StateFailed:
		params.FailureCode = FailureInternalError
	}

	return params
}
