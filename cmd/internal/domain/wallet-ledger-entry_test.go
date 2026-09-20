/*
@Author: Franco Ribeiro Borba
@Description: Unit tests for the wallet ledger entry
*/
package domain

import (
	"errors"
	"math"
	"testing"
	"time"

	"github.com/google/uuid"
)

// --------------------------------------------------------------------------
// NewLedgerEntry
// --------------------------------------------------------------------------

// TestNewLedgerEntryFromDebit cobre o caminho real: a carteira aceita o debito e
// o lancamento nasce do BalanceChange devolvido por ela.
func TestNewLedgerEntryFromDebit(t *testing.T) {
	w := mustWallet(t, "1000.00", testNow)
	change, err := w.Debit(mustParse(t, "25.00", "BRL"), testNow)
	if err != nil {
		t.Fatalf("Debit: %v", err)
	}

	id, transactionID := uuid.New(), uuid.New()

	entry, err := NewLedgerEntry(id, transactionID, change)
	if err != nil {
		t.Fatalf("erro inesperado: %v", err)
	}

	if entry.ID() != id {
		t.Errorf("ID() = %s, quero %s", entry.ID(), id)
	}
	if entry.WalletID() != w.ID() {
		t.Errorf("WalletID() = %s, quero %s", entry.WalletID(), w.ID())
	}
	if entry.TransactionID() != transactionID {
		t.Errorf("TransactionID() = %s, quero %s", entry.TransactionID(), transactionID)
	}
	if entry.Direction() != DirectionDebit {
		t.Errorf("Direction() = %s, quero %s", entry.Direction(), DirectionDebit)
	}
	if !entry.IsDebit() || entry.IsCredit() {
		t.Error("o lancamento deveria ser um debito")
	}
	if got := entry.Amount().AmountString(); got != "25.00" {
		t.Errorf("Amount() = %s, quero 25.00", got)
	}
	if got := entry.BalanceBefore().AmountString(); got != "1000.00" {
		t.Errorf("BalanceBefore() = %s, quero 1000.00", got)
	}
	if got := entry.BalanceAfter().AmountString(); got != "975.00" {
		t.Errorf("BalanceAfter() = %s, quero 975.00", got)
	}
	if entry.Currency() != "BRL" {
		t.Errorf("Currency() = %s, quero BRL", entry.Currency())
	}
	if !entry.CreatedAt().Equal(testNow) {
		t.Errorf("CreatedAt() = %s, quero %s", entry.CreatedAt(), testNow)
	}
}

func TestNewLedgerEntryFromCredit(t *testing.T) {
	w := mustWallet(t, "100.00", testNow)
	change, err := w.Credit(mustParse(t, "50.00", "BRL"), testLater)
	if err != nil {
		t.Fatalf("Credit: %v", err)
	}

	entry, err := NewLedgerEntry(uuid.New(), uuid.New(), change)
	if err != nil {
		t.Fatalf("erro inesperado: %v", err)
	}

	if !entry.IsCredit() || entry.IsDebit() {
		t.Error("o lancamento deveria ser um credito")
	}
	if got := entry.BalanceAfter().AmountString(); got != "150.00" {
		t.Errorf("BalanceAfter() = %s, quero 150.00", got)
	}
	if !entry.CreatedAt().Equal(testLater) {
		t.Errorf("CreatedAt() = %s, quero %s", entry.CreatedAt(), testLater)
	}
}

// TestNewLedgerEntryMatchesWallet garante que o lancamento descreve o estado em
// que a carteira ficou, que e o que sera gravado na mesma transacao SQL.
func TestNewLedgerEntryMatchesWallet(t *testing.T) {
	w := mustWallet(t, "1000.00", testNow)
	change, err := w.Debit(mustParse(t, "80.00", "BRL"), testNow)
	if err != nil {
		t.Fatalf("Debit: %v", err)
	}

	entry, err := NewLedgerEntry(uuid.New(), uuid.New(), change)
	if err != nil {
		t.Fatalf("erro inesperado: %v", err)
	}

	if entry.BalanceAfter() != w.Balance() {
		t.Errorf("BalanceAfter() = %s, mas a carteira esta com %s", entry.BalanceAfter(), w.Balance())
	}
	if !entry.CreatedAt().Equal(w.UpdatedAt()) {
		t.Errorf("CreatedAt() = %s, mas UpdatedAt() = %s", entry.CreatedAt(), w.UpdatedAt())
	}
}

// TestNewLedgerEntryValidatesForgedChange garante que um BalanceChange montado a
// mao nao consegue produzir um lancamento incoerente, ja que a struct e
// exportada e qualquer camada poderia preenche-la.
func TestNewLedgerEntryValidatesForgedChange(t *testing.T) {
	forged := BalanceChange{
		WalletID:      uuid.New(),
		Direction:     DirectionDebit,
		Amount:        mustMoneyValue(2500, "BRL"),
		BalanceBefore: mustMoneyValue(100000, "BRL"),
		BalanceAfter:  mustMoneyValue(100000, "BRL"), // nao desconta nada
		OccurredAt:    testNow,
	}

	entry, err := NewLedgerEntry(uuid.New(), uuid.New(), forged)
	if !errors.Is(err, ErrLedgerBalanceMismatch) {
		t.Fatalf("erro = %v, quero %v", err, ErrLedgerBalanceMismatch)
	}
	if entry != nil {
		t.Errorf("em caso de erro deve retornar nil, obtive %+v", entry)
	}
}

func TestNewLedgerEntry_StoresTimestampInUTC(t *testing.T) {
	zone := time.FixedZone("BRT", -3*60*60)
	change := debitChange(uuid.New(), "1000.00", "25.00", "975.00", testNow.In(zone))

	entry, err := NewLedgerEntry(uuid.New(), uuid.New(), change)
	if err != nil {
		t.Fatalf("erro inesperado: %v", err)
	}
	if entry.CreatedAt().Location() != time.UTC {
		t.Errorf("CreatedAt().Location() = %s, quero UTC", entry.CreatedAt().Location())
	}
	if !entry.CreatedAt().Equal(testNow) {
		t.Errorf("CreatedAt() = %s, quero o mesmo instante de %s", entry.CreatedAt(), testNow)
	}
}

// --------------------------------------------------------------------------
// Aritmetica do lancamento
// --------------------------------------------------------------------------

// TestLedgerArithmetic cobre a exigencia de que balanceAfter seja exatamente
// balanceBefore mais ou menos o valor, conforme a direcao.
func TestLedgerArithmetic(t *testing.T) {
	tests := []struct {
		name      string
		direction Direction
		before    string
		amount    string
		after     string
		wantErr   error
	}{
		{"debito coerente", DirectionDebit, "1000.00", "25.00", "975.00", nil},
		{"credito coerente", DirectionCredit, "1000.00", "25.00", "1025.00", nil},
		{"debito ate zerar", DirectionDebit, "25.00", "25.00", "0.00", nil},
		{"credito a partir de zero", DirectionCredit, "0.00", "25.00", "25.00", nil},
		{"centavo importa no debito", DirectionDebit, "1000.00", "25.00", "975.01", ErrLedgerBalanceMismatch},
		{"centavo importa no credito", DirectionCredit, "1000.00", "25.00", "1024.99", ErrLedgerBalanceMismatch},
		{"debito que nao desconta", DirectionDebit, "1000.00", "25.00", "1000.00", ErrLedgerBalanceMismatch},
		{"direcao invertida", DirectionDebit, "1000.00", "25.00", "1025.00", ErrLedgerBalanceMismatch},
		{"credito invertido", DirectionCredit, "1000.00", "25.00", "975.00", ErrLedgerBalanceMismatch},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			params := ledgerParams(tc.direction)
			params.BalanceBefore = mustParse(t, tc.before, "BRL")
			params.Amount = mustParse(t, tc.amount, "BRL")
			params.BalanceAfter = mustParse(t, tc.after, "BRL")

			entry, err := RestoreLedgerEntry(params)
			if tc.wantErr != nil {
				if !errors.Is(err, tc.wantErr) {
					t.Fatalf("erro = %v, quero %v", err, tc.wantErr)
				}
				if entry != nil {
					t.Errorf("em caso de erro deve retornar nil, obtive %+v", entry)
				}
				return
			}
			if err != nil {
				t.Fatalf("erro inesperado: %v", err)
			}
			if got := entry.BalanceAfter().AmountString(); got != tc.after {
				t.Errorf("BalanceAfter() = %s, quero %s", got, tc.after)
			}
		})
	}
}

// TestLedgerArithmeticOverflow garante que um valor extremo seja reportado como
// overflow e nao dobre para um saldo absurdo.
func TestLedgerArithmeticOverflow(t *testing.T) {
	params := ledgerParams(DirectionCredit)
	params.BalanceBefore = mustMoneyValue(math.MaxInt64, "BRL")
	params.Amount = mustMoneyValue(1, "BRL")
	params.BalanceAfter = mustMoneyValue(math.MaxInt64, "BRL")

	if _, err := RestoreLedgerEntry(params); !errors.Is(err, ErrArithmeticOverflow) {
		t.Fatalf("erro = %v, quero %v", err, ErrArithmeticOverflow)
	}
}

// --------------------------------------------------------------------------
// Rejeicoes
// --------------------------------------------------------------------------

func TestLedgerEntry_Rejections(t *testing.T) {
	tests := []struct {
		name    string
		mutate  func(*RestoredLedgerEntryParams)
		wantErr error
	}{
		{"id nulo", func(p *RestoredLedgerEntryParams) { p.ID = uuid.Nil }, ErrInvalidLedgerEntryID},
		{"wallet nula", func(p *RestoredLedgerEntryParams) { p.WalletID = uuid.Nil }, ErrInvalidWalletID},
		{"transacao nula", func(p *RestoredLedgerEntryParams) { p.TransactionID = uuid.Nil }, ErrInvalidTransactionID},
		{"direcao vazia", func(p *RestoredLedgerEntryParams) { p.Direction = "" }, ErrInvalidDirection},
		{"direcao desconhecida", func(p *RestoredLedgerEntryParams) { p.Direction = "TRANSFER" }, ErrInvalidDirection},
		{"direcao minuscula", func(p *RestoredLedgerEntryParams) { p.Direction = "debit" }, ErrInvalidDirection},
		{"valor nao inicializado", func(p *RestoredLedgerEntryParams) { p.Amount = Money{} }, ErrUninitializedMoney},
		{"saldo anterior nao inicializado", func(p *RestoredLedgerEntryParams) { p.BalanceBefore = Money{} }, ErrUninitializedMoney},
		{"saldo posterior nao inicializado", func(p *RestoredLedgerEntryParams) { p.BalanceAfter = Money{} }, ErrUninitializedMoney},
		{"timestamp zerado", func(p *RestoredLedgerEntryParams) { p.CreatedAt = time.Time{} }, ErrInvalidTimestamp},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			params := ledgerParams(DirectionDebit)
			tc.mutate(&params)

			entry, err := RestoreLedgerEntry(params)
			if !errors.Is(err, tc.wantErr) {
				t.Fatalf("erro = %v, quero %v", err, tc.wantErr)
			}
			if entry != nil {
				t.Errorf("em caso de erro deve retornar nil, obtive %+v", entry)
			}
		})
	}
}

// TestLedgerEntryRequiresPositiveAmount cobre a regra de que LOSS e operacoes
// rejeitadas nao produzem lancamento: um lancamento sempre movimenta dinheiro.
func TestLedgerEntryRequiresPositiveAmount(t *testing.T) {
	tests := []struct {
		name   string
		amount Money
		after  Money
	}{
		{"valor zero como LOSS", mustMoneyValue(0, "BRL"), mustMoneyValue(100000, "BRL")},
		{"valor negativo", mustMoneyValue(-2500, "BRL"), mustMoneyValue(102500, "BRL")},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			params := ledgerParams(DirectionDebit)
			params.Amount = tc.amount
			params.BalanceAfter = tc.after

			if _, err := RestoreLedgerEntry(params); !errors.Is(err, ErrInvalidLedgerAmount) {
				t.Fatalf("erro = %v, quero %v", err, ErrInvalidLedgerAmount)
			}
		})
	}
}

func TestLedgerEntryRejectsNegativeBalances(t *testing.T) {
	t.Run("saldo anterior negativo", func(t *testing.T) {
		params := ledgerParams(DirectionCredit)
		params.BalanceBefore = mustMoneyValue(-100, "BRL")
		params.Amount = mustMoneyValue(100, "BRL")
		params.BalanceAfter = mustMoneyValue(0, "BRL")

		if _, err := RestoreLedgerEntry(params); !errors.Is(err, ErrNegativeLedgerBalance) {
			t.Fatalf("erro = %v, quero %v", err, ErrNegativeLedgerBalance)
		}
	})

	t.Run("saldo posterior negativo", func(t *testing.T) {
		params := ledgerParams(DirectionDebit)
		params.BalanceBefore = mustMoneyValue(100, "BRL")
		params.Amount = mustMoneyValue(200, "BRL")
		params.BalanceAfter = mustMoneyValue(-100, "BRL")

		if _, err := RestoreLedgerEntry(params); !errors.Is(err, ErrNegativeLedgerBalance) {
			t.Fatalf("erro = %v, quero %v", err, ErrNegativeLedgerBalance)
		}
	})
}

// TestLedgerEntryRejectsCurrencyMismatch cobre a exigencia de moedas compativeis
// entre o valor movimentado e os saldos.
func TestLedgerEntryRejectsCurrencyMismatch(t *testing.T) {
	tests := []struct {
		name   string
		mutate func(*RestoredLedgerEntryParams)
	}{
		{"valor em outra moeda", func(p *RestoredLedgerEntryParams) { p.Amount = mustMoneyValue(2500, "USD") }},
		{"saldo anterior em outra moeda", func(p *RestoredLedgerEntryParams) { p.BalanceBefore = mustMoneyValue(100000, "USD") }},
		{"saldo posterior em outra moeda", func(p *RestoredLedgerEntryParams) { p.BalanceAfter = mustMoneyValue(97500, "USD") }},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			params := ledgerParams(DirectionDebit)
			tc.mutate(&params)

			if _, err := RestoreLedgerEntry(params); !errors.Is(err, ErrCurrencyMismatch) {
				t.Fatalf("erro = %v, quero %v", err, ErrCurrencyMismatch)
			}
		})
	}
}

// --------------------------------------------------------------------------
// RestoreLedgerEntry
// --------------------------------------------------------------------------

func TestRestoreLedgerEntry_PreservesPersistedState(t *testing.T) {
	params := ledgerParams(DirectionCredit)
	params.BalanceBefore = mustParse(t, "100.00", "BRL")
	params.Amount = mustParse(t, "50.00", "BRL")
	params.BalanceAfter = mustParse(t, "150.00", "BRL")

	entry, err := RestoreLedgerEntry(params)
	if err != nil {
		t.Fatalf("erro inesperado: %v", err)
	}

	if entry.ID() != params.ID || entry.WalletID() != params.WalletID || entry.TransactionID() != params.TransactionID {
		t.Error("a reidratacao nao preservou os identificadores")
	}
	if entry.Amount() != params.Amount || entry.BalanceBefore() != params.BalanceBefore || entry.BalanceAfter() != params.BalanceAfter {
		t.Error("a reidratacao nao preservou os valores")
	}
	if !entry.CreatedAt().Equal(params.CreatedAt) {
		t.Errorf("CreatedAt() = %s, quero %s", entry.CreatedAt(), params.CreatedAt)
	}
}

func TestRestoreLedgerEntry_StoresTimestampInUTC(t *testing.T) {
	zone := time.FixedZone("BRT", -3*60*60)
	params := ledgerParams(DirectionDebit)
	params.CreatedAt = testNow.In(zone)

	entry, err := RestoreLedgerEntry(params)
	if err != nil {
		t.Fatalf("erro inesperado: %v", err)
	}
	if entry.CreatedAt().Location() != time.UTC {
		t.Errorf("CreatedAt().Location() = %s, quero UTC", entry.CreatedAt().Location())
	}
}

// --------------------------------------------------------------------------
// SignedAmount
// --------------------------------------------------------------------------

func TestSignedAmount(t *testing.T) {
	debit, err := RestoreLedgerEntry(ledgerParams(DirectionDebit))
	if err != nil {
		t.Fatalf("RestoreLedgerEntry: %v", err)
	}
	signed, err := debit.SignedAmount()
	if err != nil {
		t.Fatalf("SignedAmount: %v", err)
	}
	if got := signed.AmountString(); got != "-25.00" {
		t.Errorf("SignedAmount() = %s, quero -25.00", got)
	}

	credit, err := RestoreLedgerEntry(creditParams())
	if err != nil {
		t.Fatalf("RestoreLedgerEntry: %v", err)
	}
	signed, err = credit.SignedAmount()
	if err != nil {
		t.Fatalf("SignedAmount: %v", err)
	}
	if got := signed.AmountString(); got != "25.00" {
		t.Errorf("SignedAmount() = %s, quero 25.00", got)
	}
}

// TestSignedAmountOnUninitializedEntry garante que um valor zero nao passe por um
// credito de valor zero numa reconciliacao.
func TestSignedAmountOnUninitializedEntry(t *testing.T) {
	var entry WalletLedgerEntry

	if _, err := entry.SignedAmount(); !errors.Is(err, ErrInvalidDirection) {
		t.Errorf("erro = %v, quero %v", err, ErrInvalidDirection)
	}
}

// --------------------------------------------------------------------------
// ReplayLedgerBalance
// --------------------------------------------------------------------------

// TestReplayLedgerBalance reconstroi o saldo a partir do ledger, que e o que a
// reconciliacao compara com o saldo armazenado.
func TestReplayLedgerBalance(t *testing.T) {
	w := mustWallet(t, "0.00", testNow)
	entries := []*WalletLedgerEntry{
		applyAndRecord(t, w, DirectionCredit, "1000.00"), // abertura
		applyAndRecord(t, w, DirectionDebit, "25.00"),    // BET
		applyAndRecord(t, w, DirectionCredit, "50.00"),   // WIN
		applyAndRecord(t, w, DirectionDebit, "80.00"),    // BET
	}

	balance, err := ReplayLedgerBalance("BRL", entries)
	if err != nil {
		t.Fatalf("erro inesperado: %v", err)
	}
	if got := balance.AmountString(); got != "945.00" {
		t.Errorf("saldo reconstruido = %s, quero 945.00", got)
	}
	// O saldo reconstruido deve bater com o saldo que a carteira guarda.
	if balance != w.Balance() {
		t.Errorf("saldo reconstruido = %s, mas a carteira esta com %s", balance, w.Balance())
	}
}

func TestReplayLedgerBalance_EmptyLedger(t *testing.T) {
	balance, err := ReplayLedgerBalance("BRL", nil)
	if err != nil {
		t.Fatalf("erro inesperado: %v", err)
	}
	if !balance.IsZero() {
		t.Errorf("saldo = %s, quero 0.00", balance)
	}
	if balance.Currency() != "BRL" {
		t.Errorf("Currency() = %s, quero BRL", balance.Currency())
	}
}

// TestReplayLedgerBalance_OrderDoesNotMatter garante que a reconstrucao funcione
// com as entradas vindas paginadas em qualquer ordem.
func TestReplayLedgerBalance_OrderDoesNotMatter(t *testing.T) {
	w := mustWallet(t, "0.00", testNow)
	entries := []*WalletLedgerEntry{
		applyAndRecord(t, w, DirectionCredit, "1000.00"),
		applyAndRecord(t, w, DirectionDebit, "25.00"),
		applyAndRecord(t, w, DirectionCredit, "50.00"),
	}
	reversed := []*WalletLedgerEntry{entries[2], entries[1], entries[0]}

	forward, err := ReplayLedgerBalance("BRL", entries)
	if err != nil {
		t.Fatalf("erro inesperado: %v", err)
	}
	backward, err := ReplayLedgerBalance("BRL", reversed)
	if err != nil {
		t.Fatalf("erro inesperado: %v", err)
	}
	if forward != backward {
		t.Errorf("ordem alterou o resultado: %s vs %s", forward, backward)
	}
}

func TestReplayLedgerBalance_Rejections(t *testing.T) {
	valid, err := RestoreLedgerEntry(ledgerParams(DirectionDebit))
	if err != nil {
		t.Fatalf("RestoreLedgerEntry: %v", err)
	}

	t.Run("moeda invalida", func(t *testing.T) {
		if _, err := ReplayLedgerBalance("brl", nil); !errors.Is(err, ErrInvalidCurrencyFormat) {
			t.Errorf("erro = %v, quero %v", err, ErrInvalidCurrencyFormat)
		}
	})

	t.Run("entrada nula", func(t *testing.T) {
		if _, err := ReplayLedgerBalance("BRL", []*WalletLedgerEntry{valid, nil}); !errors.Is(err, ErrNilLedgerEntry) {
			t.Errorf("erro = %v, quero %v", err, ErrNilLedgerEntry)
		}
	})

	t.Run("lancamento em outra moeda", func(t *testing.T) {
		other := creditParams()
		other.Amount = mustMoneyValue(2500, "USD")
		other.BalanceBefore = mustMoneyValue(0, "USD")
		other.BalanceAfter = mustMoneyValue(2500, "USD")

		entry, err := RestoreLedgerEntry(other)
		if err != nil {
			t.Fatalf("RestoreLedgerEntry: %v", err)
		}
		if _, err := ReplayLedgerBalance("BRL", []*WalletLedgerEntry{entry}); !errors.Is(err, ErrCurrencyMismatch) {
			t.Errorf("erro = %v, quero %v", err, ErrCurrencyMismatch)
		}
	})

	t.Run("entrada nao inicializada", func(t *testing.T) {
		entries := []*WalletLedgerEntry{{}}
		if _, err := ReplayLedgerBalance("BRL", entries); !errors.Is(err, ErrInvalidDirection) {
			t.Errorf("erro = %v, quero %v", err, ErrInvalidDirection)
		}
	})
}

// TestReplayDetectsDivergence simula a deteccao de divergencia da reconciliacao:
// o saldo armazenado nao bate com a soma do ledger.
func TestReplayDetectsDivergence(t *testing.T) {
	w := mustWallet(t, "0.00", testNow)
	entries := []*WalletLedgerEntry{
		applyAndRecord(t, w, DirectionCredit, "1000.00"),
		applyAndRecord(t, w, DirectionDebit, "25.00"),
	}

	calculated, err := ReplayLedgerBalance("BRL", entries)
	if err != nil {
		t.Fatalf("erro inesperado: %v", err)
	}

	// Um saldo armazenado corrompido, como se um debito nao tivesse sido
	// registrado no ledger.
	stored := mustParse(t, "1000.00", "BRL")

	difference, err := stored.Subtract(calculated)
	if err != nil {
		t.Fatalf("Subtract: %v", err)
	}
	if difference.IsZero() {
		t.Fatal("a divergencia deveria ser detectada")
	}
	if got := difference.AmountString(); got != "25.00" {
		t.Errorf("difference = %s, quero 25.00", got)
	}
}

// --------------------------------------------------------------------------
// helpers
// --------------------------------------------------------------------------

// ledgerParams monta um debito valido de 25.00 sobre um saldo de 1000.00.
func ledgerParams(direction Direction) RestoredLedgerEntryParams {
	params := RestoredLedgerEntryParams{
		ID:            uuid.New(),
		WalletID:      uuid.New(),
		TransactionID: uuid.New(),
		Direction:     direction,
		Amount:        mustMoneyValue(2500, "BRL"),
		BalanceBefore: mustMoneyValue(100000, "BRL"),
		BalanceAfter:  mustMoneyValue(97500, "BRL"),
		CreatedAt:     testNow,
	}
	if direction == DirectionCredit {
		params.BalanceAfter = mustMoneyValue(102500, "BRL")
	}
	return params
}

func creditParams() RestoredLedgerEntryParams {
	return ledgerParams(DirectionCredit)
}

// debitChange monta um BalanceChange de debito, para os casos que nao passam
// pela carteira.
func debitChange(walletID uuid.UUID, before, amount, after string, now time.Time) BalanceChange {
	parse := func(value string) Money {
		m, err := ParseMoney(value, "BRL")
		if err != nil {
			panic(err)
		}
		return m
	}
	return BalanceChange{
		WalletID:      walletID,
		Direction:     DirectionDebit,
		Amount:        parse(amount),
		BalanceBefore: parse(before),
		BalanceAfter:  parse(after),
		WalletVersion: 2,
		OccurredAt:    now,
	}
}

// applyAndRecord movimenta a carteira e devolve o lancamento correspondente,
// que e o fluxo real: nenhuma mudanca de saldo existe sem o seu lancamento.
func applyAndRecord(t *testing.T, w *Wallet, direction Direction, amount string) *WalletLedgerEntry {
	t.Helper()

	var (
		change BalanceChange
		err    error
	)
	if direction == DirectionCredit {
		change, err = w.Credit(mustParse(t, amount, "BRL"), testNow)
	} else {
		change, err = w.Debit(mustParse(t, amount, "BRL"), testNow)
	}
	if err != nil {
		t.Fatalf("%s %s: %v", direction, amount, err)
	}

	entry, err := NewLedgerEntry(uuid.New(), uuid.New(), change)
	if err != nil {
		t.Fatalf("NewLedgerEntry: %v", err)
	}
	return entry
}
