/*
@Author: Franco Ribeiro Borba
@Description: Unit tests for the wallet aggregate
*/
package domain

import (
	"errors"
	"math"
	"testing"
	"time"

	"github.com/google/uuid"
)

var (
	testNow   = time.Date(2026, 9, 19, 10, 0, 0, 0, time.UTC)
	testLater = testNow.Add(time.Minute)
)

// --------------------------------------------------------------------------
// NewWallet
// --------------------------------------------------------------------------

func TestNewWallet(t *testing.T) {
	id := uuid.New()
	playerID := uuid.New()
	initial := mustParse(t, "1000.00", "BRL")

	w, err := NewWallet(id, playerID, initial, testNow)
	if err != nil {
		t.Fatalf("erro inesperado: %v", err)
	}

	if w.ID() != id {
		t.Errorf("ID() = %s, quero %s", w.ID(), id)
	}
	if w.PlayerID() != playerID {
		t.Errorf("PlayerID() = %s, quero %s", w.PlayerID(), playerID)
	}
	if w.Currency() != "BRL" {
		t.Errorf("Currency() = %s, quero BRL", w.Currency())
	}
	if w.Balance() != initial {
		t.Errorf("Balance() = %s, quero %s", w.Balance(), initial)
	}
	if w.Version() != 1 {
		t.Errorf("Version() = %d, quero 1", w.Version())
	}
	if !w.CreatedAt().Equal(testNow) || !w.UpdatedAt().Equal(testNow) {
		t.Errorf("timestamps = %s/%s, quero %s", w.CreatedAt(), w.UpdatedAt(), testNow)
	}
}

func TestNewWallet_ZeroInitialBalance(t *testing.T) {
	zero, err := Zero("BRL")
	if err != nil {
		t.Fatalf("Zero: %v", err)
	}

	w, err := NewWallet(uuid.New(), uuid.New(), zero, testNow)
	if err != nil {
		t.Fatalf("saldo inicial zero deveria ser aceito: %v", err)
	}
	if !w.Balance().IsZero() {
		t.Errorf("Balance() = %s, quero 0.00", w.Balance())
	}
	if w.Version() != 1 {
		t.Errorf("Version() = %d, quero 1", w.Version())
	}
}

func TestNewWallet_Rejections(t *testing.T) {
	valid := mustMoneyValue(100000, "BRL")

	tests := []struct {
		name     string
		id       uuid.UUID
		playerID uuid.UUID
		balance  Money
		now      time.Time
		wantErr  error
	}{
		{"id ausente", uuid.Nil, uuid.New(), valid, testNow, ErrInvalidWalletID},
		{"jogador ausente", uuid.New(), uuid.Nil, valid, testNow, ErrInvalidPlayerID},
		{"saldo nao inicializado", uuid.New(), uuid.New(), Money{}, testNow, ErrUninitializedMoney},
		{"saldo negativo", uuid.New(), uuid.New(), mustMoneyValue(-1, "BRL"), testNow, ErrNegativeInitialBalance},
		{"instante ausente", uuid.New(), uuid.New(), valid, time.Time{}, ErrInvalidTimestamp},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			w, err := NewWallet(tc.id, tc.playerID, tc.balance, tc.now)
			if !errors.Is(err, tc.wantErr) {
				t.Fatalf("erro = %v, quero %v", err, tc.wantErr)
			}
			if w != nil {
				t.Errorf("em caso de erro deve retornar nil, obtive %+v", w)
			}
		})
	}
}

func TestNewWallet_NegativeBalanceIsNotInsufficientFunds(t *testing.T) {
	_, err := NewWallet(uuid.New(), uuid.New(), mustMoneyValue(-1, "BRL"), testNow)

	if errors.Is(err, ErrInsufficientFunds) {
		t.Error("saldo inicial negativo e entrada invalida, nao falta de saldo")
	}
}

func TestNewWallet_StoresTimestampsInUTC(t *testing.T) {
	saoPaulo := time.FixedZone("BRT", -3*60*60)
	local := time.Date(2026, 9, 19, 7, 0, 0, 0, saoPaulo)

	w := mustWallet(t, "100.00", local)

	if w.CreatedAt().Location() != time.UTC || w.UpdatedAt().Location() != time.UTC {
		t.Errorf("timestamps deveriam estar em UTC, obtive %s/%s", w.CreatedAt().Location(), w.UpdatedAt().Location())
	}
	if !w.CreatedAt().Equal(local) {
		t.Errorf("CreatedAt() = %s, quero o mesmo instante de %s", w.CreatedAt(), local)
	}
}

// --------------------------------------------------------------------------
// RestoreWallet
// --------------------------------------------------------------------------

func TestRestoreWallet_PreservesPersistedState(t *testing.T) {
	id := uuid.New()
	playerID := uuid.New()
	balance := mustParse(t, "975.00", "BRL")
	updatedAt := testNow.Add(time.Hour)

	w, err := RestoreWallet(id, playerID, balance, 7, testNow, updatedAt)
	if err != nil {
		t.Fatalf("erro inesperado: %v", err)
	}

	if w.ID() != id || w.PlayerID() != playerID {
		t.Errorf("identidades nao preservadas: %s/%s", w.ID(), w.PlayerID())
	}
	if w.Balance() != balance || w.Currency() != "BRL" {
		t.Errorf("Balance() = %s, quero %s", w.Balance(), balance)
	}
	if w.Version() != 7 {
		t.Errorf("reidratacao nao deve alterar a versao: Version() = %d, quero 7", w.Version())
	}
	if !w.CreatedAt().Equal(testNow) || !w.UpdatedAt().Equal(updatedAt) {
		t.Errorf("timestamps = %s/%s, quero %s/%s", w.CreatedAt(), w.UpdatedAt(), testNow, updatedAt)
	}
}

func TestRestoreWallet_AcceptsBoundaryState(t *testing.T) {
	zero := mustMoneyValue(0, "BRL")

	w, err := RestoreWallet(uuid.New(), uuid.New(), zero, 1, testNow, testNow)
	if err != nil {
		t.Fatalf("saldo zero, versao 1 e timestamps iguais deveriam ser aceitos: %v", err)
	}
	if !w.Balance().IsZero() || w.Version() != 1 {
		t.Errorf("obtive %s/v%d, quero 0.00/v1", w.Balance(), w.Version())
	}
}

func TestRestoreWallet_Rejections(t *testing.T) {
	valid := mustMoneyValue(97500, "BRL")

	tests := []struct {
		name      string
		id        uuid.UUID
		playerID  uuid.UUID
		balance   Money
		version   int64
		createdAt time.Time
		updatedAt time.Time
		wantErr   error
	}{
		{"id ausente", uuid.Nil, uuid.New(), valid, 1, testNow, testNow, ErrInvalidWalletID},
		{"jogador ausente", uuid.New(), uuid.Nil, valid, 1, testNow, testNow, ErrInvalidPlayerID},
		{"saldo nao inicializado", uuid.New(), uuid.New(), Money{}, 1, testNow, testNow, ErrUninitializedMoney},
		{"saldo negativo", uuid.New(), uuid.New(), mustMoneyValue(-1, "BRL"), 1, testNow, testNow, ErrInvalidWalletState},
		{"versao zero", uuid.New(), uuid.New(), valid, 0, testNow, testNow, ErrInvalidWalletState},
		{"versao negativa", uuid.New(), uuid.New(), valid, -1, testNow, testNow, ErrInvalidWalletState},
		{"createdAt ausente", uuid.New(), uuid.New(), valid, 1, time.Time{}, testNow, ErrInvalidTimestamp},
		{"updatedAt ausente", uuid.New(), uuid.New(), valid, 1, testNow, time.Time{}, ErrInvalidTimestamp},
		{"updatedAt antes de createdAt", uuid.New(), uuid.New(), valid, 1, testLater, testNow, ErrInvalidWalletState},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			w, err := RestoreWallet(tc.id, tc.playerID, tc.balance, tc.version, tc.createdAt, tc.updatedAt)
			if !errors.Is(err, tc.wantErr) {
				t.Fatalf("erro = %v, quero %v", err, tc.wantErr)
			}
			if w != nil {
				t.Errorf("em caso de erro deve retornar nil, obtive %+v", w)
			}
		})
	}
}

func TestRestoreWallet_StoresTimestampsInUTC(t *testing.T) {
	saoPaulo := time.FixedZone("BRT", -3*60*60)
	local := time.Date(2026, 9, 19, 7, 0, 0, 0, saoPaulo)

	w, err := RestoreWallet(uuid.New(), uuid.New(), mustParse(t, "1.00", "BRL"), 1, local, local)
	if err != nil {
		t.Fatalf("erro inesperado: %v", err)
	}

	if w.CreatedAt().Location() != time.UTC || w.UpdatedAt().Location() != time.UTC {
		t.Errorf("timestamps deveriam estar em UTC, obtive %s/%s", w.CreatedAt().Location(), w.UpdatedAt().Location())
	}
}

func TestRestoredWalletAcceptsMovements(t *testing.T) {
	w, err := RestoreWallet(uuid.New(), uuid.New(), mustParse(t, "975.00", "BRL"), 3, testNow, testNow)
	if err != nil {
		t.Fatalf("RestoreWallet: %v", err)
	}

	change, err := w.Debit(mustParse(t, "75.00", "BRL"), testLater)
	if err != nil {
		t.Fatalf("Debit: %v", err)
	}

	assertBalance(t, w, "900.00")
	if w.Version() != 4 || change.WalletVersion != 4 {
		t.Errorf("versoes = %d/%d, quero 4/4", w.Version(), change.WalletVersion)
	}
}

// --------------------------------------------------------------------------
// Debit
// --------------------------------------------------------------------------

func TestDebit(t *testing.T) {
	tests := []struct {
		name    string
		initial string
		amount  string
		want    string
	}{
		{"aposta do contrato", "1000.00", "25.00", "975.00"},
		{"um centavo", "1.00", "0.01", "0.99"},
		{"zera o saldo", "80.00", "80.00", "0.00"},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			w := mustWallet(t, tc.initial, testNow)

			if _, err := w.Debit(mustParse(t, tc.amount, "BRL"), testLater); err != nil {
				t.Fatalf("Debit erro inesperado: %v", err)
			}

			assertBalance(t, w, tc.want)
			if w.Version() != 2 {
				t.Errorf("Version() = %d, quero 2", w.Version())
			}
			if !w.UpdatedAt().Equal(testLater) {
				t.Errorf("UpdatedAt() = %s, quero %s", w.UpdatedAt(), testLater)
			}
			if !w.CreatedAt().Equal(testNow) {
				t.Errorf("CreatedAt() nao deveria mudar: %s", w.CreatedAt())
			}
		})
	}
}

func TestDebit_ReturnsBalanceChange(t *testing.T) {
	w := mustWallet(t, "1000.00", testNow)
	amount := mustParse(t, "25.00", "BRL")

	change, err := w.Debit(amount, testLater)
	if err != nil {
		t.Fatalf("Debit: %v", err)
	}

	assertChange(t, change, BalanceChange{
		WalletID:      w.ID(),
		Direction:     DirectionDebit,
		Amount:        amount,
		BalanceBefore: mustParse(t, "1000.00", "BRL"),
		BalanceAfter:  mustParse(t, "975.00", "BRL"),
		WalletVersion: 2,
		OccurredAt:    testLater,
	})
	assertChangeMatchesWallet(t, change, w)
}

func TestDebit_Rejections(t *testing.T) {
	tests := []struct {
		name    string
		amount  Money
		now     time.Time
		wantErr error
	}{
		{"saldo insuficiente", mustMoneyValue(50000, "BRL"), testLater, ErrInsufficientFunds},
		{"saldo insuficiente por um centavo", mustMoneyValue(10001, "BRL"), testLater, ErrInsufficientFunds},
		{"valor zero", mustMoneyValue(0, "BRL"), testLater, ErrInvalidDebit},
		{"valor negativo", mustMoneyValue(-2500, "BRL"), testLater, ErrInvalidDebit},
		{"valor nao inicializado", Money{}, testLater, ErrInvalidDebit},
		{"moeda diferente", mustMoneyValue(2500, "USD"), testLater, ErrCurrencyMismatch},
		{"instante ausente", mustMoneyValue(2500, "BRL"), time.Time{}, ErrInvalidTimestamp},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			w := mustWallet(t, "100.00", testNow)
			before := *w

			change, err := w.Debit(tc.amount, tc.now)
			if !errors.Is(err, tc.wantErr) {
				t.Fatalf("Debit(%s) erro = %v, quero %v", tc.amount, err, tc.wantErr)
			}
			if change != (BalanceChange{}) {
				t.Errorf("em caso de erro deve retornar BalanceChange zero, obtive %+v", change)
			}
			if *w != before {
				t.Errorf("debito rejeitado alterou a carteira: antes %+v, depois %+v", before, *w)
			}
		})
	}
}

// --------------------------------------------------------------------------
// Credit
// --------------------------------------------------------------------------

func TestCredit(t *testing.T) {
	tests := []struct {
		name    string
		initial string
		amount  string
		want    string
	}{
		{"premio", "975.00", "50.00", "1025.00"},
		{"sobre saldo zero", "0.00", "0.01", "0.01"},
		{"centavos com transporte", "0.99", "0.01", "1.00"},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			w := mustWallet(t, tc.initial, testNow)

			if _, err := w.Credit(mustParse(t, tc.amount, "BRL"), testLater); err != nil {
				t.Fatalf("Credit erro inesperado: %v", err)
			}

			assertBalance(t, w, tc.want)
			if w.Version() != 2 {
				t.Errorf("Version() = %d, quero 2", w.Version())
			}
			if !w.UpdatedAt().Equal(testLater) {
				t.Errorf("UpdatedAt() = %s, quero %s", w.UpdatedAt(), testLater)
			}
		})
	}
}

func TestCredit_ReturnsBalanceChange(t *testing.T) {
	w := mustWallet(t, "975.00", testNow)
	amount := mustParse(t, "50.00", "BRL")

	change, err := w.Credit(amount, testLater)
	if err != nil {
		t.Fatalf("Credit: %v", err)
	}

	assertChange(t, change, BalanceChange{
		WalletID:      w.ID(),
		Direction:     DirectionCredit,
		Amount:        amount,
		BalanceBefore: mustParse(t, "975.00", "BRL"),
		BalanceAfter:  mustParse(t, "1025.00", "BRL"),
		WalletVersion: 2,
		OccurredAt:    testLater,
	})
	assertChangeMatchesWallet(t, change, w)
}

func TestCredit_Rejections(t *testing.T) {
	tests := []struct {
		name    string
		amount  Money
		now     time.Time
		wantErr error
	}{
		{"valor zero", mustMoneyValue(0, "BRL"), testLater, ErrInvalidCredit},
		{"valor negativo", mustMoneyValue(-2500, "BRL"), testLater, ErrInvalidCredit},
		{"valor nao inicializado", Money{}, testLater, ErrInvalidCredit},
		{"moeda diferente", mustMoneyValue(2500, "USD"), testLater, ErrCurrencyMismatch},
		{"instante ausente", mustMoneyValue(2500, "BRL"), time.Time{}, ErrInvalidTimestamp},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			w := mustWallet(t, "100.00", testNow)
			before := *w

			change, err := w.Credit(tc.amount, tc.now)
			if !errors.Is(err, tc.wantErr) {
				t.Fatalf("Credit(%s) erro = %v, quero %v", tc.amount, err, tc.wantErr)
			}
			if change != (BalanceChange{}) {
				t.Errorf("em caso de erro deve retornar BalanceChange zero, obtive %+v", change)
			}
			if *w != before {
				t.Errorf("credito rejeitado alterou a carteira: antes %+v, depois %+v", before, *w)
			}
		})
	}
}

func TestCredit_Overflow(t *testing.T) {
	w, err := RestoreWallet(uuid.New(), uuid.New(), mustMoney(t, math.MaxInt64, "BRL"), 1, testNow, testNow)
	if err != nil {
		t.Fatalf("RestoreWallet: %v", err)
	}
	before := *w

	if _, err := w.Credit(mustMoney(t, 1, "BRL"), testLater); !errors.Is(err, ErrArithmeticOverflow) {
		t.Fatalf("erro = %v, quero %v", err, ErrArithmeticOverflow)
	}
	if *w != before {
		t.Errorf("credito com overflow alterou a carteira: antes %+v, depois %+v", before, *w)
	}
}

// --------------------------------------------------------------------------
// Instante de atualizacao
// --------------------------------------------------------------------------

func TestUpdatedAtNeverMovesBackwards(t *testing.T) {
	w := mustWallet(t, "100.00", testNow)
	earlier := testNow.Add(-time.Minute) // relogio de outra instancia atrasado

	change, err := w.Credit(mustParse(t, "10.00", "BRL"), earlier)
	if err != nil {
		t.Fatalf("Credit: %v", err)
	}

	if !w.UpdatedAt().Equal(testNow) {
		t.Errorf("UpdatedAt() = %s, quero %s (nao pode retroceder)", w.UpdatedAt(), testNow)
	}
	if !change.OccurredAt.Equal(testNow) {
		t.Errorf("OccurredAt = %s, quero %s", change.OccurredAt, testNow)
	}

	// O estado resultante precisa continuar reidratavel.
	if _, err := RestoreWallet(w.ID(), w.PlayerID(), w.Balance(), w.Version(), w.CreatedAt(), w.UpdatedAt()); err != nil {
		t.Errorf("estado apos relogio atrasado deveria ser reidratavel: %v", err)
	}
}

func TestBalanceChangeUsesUTC(t *testing.T) {
	w := mustWallet(t, "100.00", testNow)
	saoPaulo := time.FixedZone("BRT", -3*60*60)

	change, err := w.Debit(mustParse(t, "10.00", "BRL"), testLater.In(saoPaulo))
	if err != nil {
		t.Fatalf("Debit: %v", err)
	}

	if change.OccurredAt.Location() != time.UTC || w.UpdatedAt().Location() != time.UTC {
		t.Errorf("instantes deveriam estar em UTC, obtive %s/%s", change.OccurredAt.Location(), w.UpdatedAt().Location())
	}
}

// --------------------------------------------------------------------------
// Cenarios
// --------------------------------------------------------------------------

// Versao sequencial do teste obrigatorio da secao 8: duas apostas de 80.00
// sobre 100.00. A disputa concorrente real fica nos testes de integracao.
func TestTwoBetsOfEightyOverOneHundred(t *testing.T) {
	w := mustWallet(t, "100.00", testNow)
	bet := mustParse(t, "80.00", "BRL")

	accepted, err := w.Debit(bet, testLater)
	if err != nil {
		t.Fatalf("primeira aposta deveria ser processada: %v", err)
	}
	if _, err := w.Debit(bet, testLater.Add(time.Second)); !errors.Is(err, ErrInsufficientFunds) {
		t.Fatalf("segunda aposta erro = %v, quero %v", err, ErrInsufficientFunds)
	}

	assertBalance(t, w, "20.00")
	if w.Version() != 2 {
		t.Errorf("Version() = %d, quero 2 (apenas um debito aplicado)", w.Version())
	}
	if !w.UpdatedAt().Equal(testLater) {
		t.Errorf("UpdatedAt() = %s, quero o instante do debito aceito %s", w.UpdatedAt(), testLater)
	}
	assertChangeMatchesWallet(t, accepted, w)
}

func TestVersionIncrementsOnlyOnBalanceChange(t *testing.T) {
	w := mustWallet(t, "100.00", testNow)
	brl := func(amount string) Money { return mustParse(t, amount, "BRL") }

	steps := []struct {
		name        string
		apply       func() error
		wantVersion int64
	}{
		{"aposta", func() error { _, err := w.Debit(brl("30.00"), testLater); return err }, 2},
		{"premio", func() error { _, err := w.Credit(brl("60.00"), testLater); return err }, 3},
		{"aposta sem saldo", func() error { _, err := w.Debit(brl("500.00"), testLater); return err }, 3},
		{"credito zero", func() error { _, err := w.Credit(brl("0.00"), testLater); return err }, 3},
		{"moeda diferente", func() error { _, err := w.Credit(mustMoney(t, 100, "USD"), testLater); return err }, 3},
		{"estorno", func() error { _, err := w.Debit(brl("60.00"), testLater); return err }, 4},
	}

	for _, step := range steps {
		_ = step.apply()
		if w.Version() != step.wantVersion {
			t.Fatalf("apos %q: Version() = %d, quero %d", step.name, w.Version(), step.wantVersion)
		}
	}

	assertBalance(t, w, "70.00")
}

// Cada BalanceChange precisa encadear com o anterior, como o ledger exige:
// o saldo posterior de um lancamento e o saldo anterior do seguinte.
func TestBalanceChangesFormAChain(t *testing.T) {
	w := mustWallet(t, "100.00", testNow)
	brl := func(amount string) Money { return mustParse(t, amount, "BRL") }

	var changes []BalanceChange
	for _, op := range []func() (BalanceChange, error){
		func() (BalanceChange, error) { return w.Debit(brl("25.00"), testLater) },
		func() (BalanceChange, error) { return w.Credit(brl("40.00"), testLater) },
		func() (BalanceChange, error) { return w.Debit(brl("115.00"), testLater) },
	} {
		change, err := op()
		if err != nil {
			t.Fatalf("operacao %d: %v", len(changes)+1, err)
		}
		changes = append(changes, change)
	}

	for i := 1; i < len(changes); i++ {
		if changes[i].BalanceBefore != changes[i-1].BalanceAfter {
			t.Errorf("lancamento %d comeca em %s, mas o anterior terminou em %s", i+1, changes[i].BalanceBefore, changes[i-1].BalanceAfter)
		}
		if changes[i].WalletVersion != changes[i-1].WalletVersion+1 {
			t.Errorf("lancamento %d tem versao %d, quero %d", i+1, changes[i].WalletVersion, changes[i-1].WalletVersion+1)
		}
	}

	assertBalance(t, w, "0.00")
	assertChangeMatchesWallet(t, changes[len(changes)-1], w)
}

func TestDebitAndCreditAreInverse(t *testing.T) {
	w := mustWallet(t, "100.00", testNow)
	amount := mustParse(t, "37.45", "BRL")

	if _, err := w.Debit(amount, testLater); err != nil {
		t.Fatalf("Debit: %v", err)
	}
	if _, err := w.Credit(amount, testLater); err != nil {
		t.Fatalf("Credit: %v", err)
	}

	assertBalance(t, w, "100.00")
	if w.Version() != 3 {
		t.Errorf("Version() = %d, quero 3", w.Version())
	}
}

func TestUninitializedWalletRejectsMovements(t *testing.T) {
	var w Wallet
	amount := mustParse(t, "10.00", "BRL")

	if _, err := w.Debit(amount, testLater); !errors.Is(err, ErrUninitializedMoney) {
		t.Errorf("Debit erro = %v, quero %v", err, ErrUninitializedMoney)
	}
	if _, err := w.Credit(amount, testLater); !errors.Is(err, ErrUninitializedMoney) {
		t.Errorf("Credit erro = %v, quero %v", err, ErrUninitializedMoney)
	}
	if w != (Wallet{}) {
		t.Errorf("carteira nao inicializada foi alterada: %+v", w)
	}
}

// --------------------------------------------------------------------------
// helpers
// --------------------------------------------------------------------------

func mustParse(t *testing.T, amount, currency string) Money {
	t.Helper()

	m, err := ParseMoney(amount, currency)
	if err != nil {
		t.Fatalf("ParseMoney(%q, %q): %v", amount, currency, err)
	}
	return m
}

// mustMoneyValue builds Money outside of a test context, for table fields.
func mustMoneyValue(cents int64, currency string) Money {
	m, err := NewMoney(cents, currency)
	if err != nil {
		panic(err)
	}
	return m
}

func mustWallet(t *testing.T, initial string, now time.Time) *Wallet {
	t.Helper()

	w, err := NewWallet(uuid.New(), uuid.New(), mustParse(t, initial, "BRL"), now)
	if err != nil {
		t.Fatalf("NewWallet(%s): %v", initial, err)
	}
	return w
}

func assertBalance(t *testing.T, w *Wallet, want string) {
	t.Helper()

	if got := w.Balance().AmountString(); got != want {
		t.Errorf("Balance() = %s, quero %s", got, want)
	}
	if got := w.Balance().Currency(); got != "BRL" {
		t.Errorf("Balance().Currency() = %s, quero BRL", got)
	}
}

func assertChange(t *testing.T, got, want BalanceChange) {
	t.Helper()

	if got.WalletID != want.WalletID {
		t.Errorf("WalletID = %s, quero %s", got.WalletID, want.WalletID)
	}
	if got.Direction != want.Direction {
		t.Errorf("Direction = %s, quero %s", got.Direction, want.Direction)
	}
	if got.Amount != want.Amount {
		t.Errorf("Amount = %s, quero %s", got.Amount, want.Amount)
	}
	if got.BalanceBefore != want.BalanceBefore {
		t.Errorf("BalanceBefore = %s, quero %s", got.BalanceBefore, want.BalanceBefore)
	}
	if got.BalanceAfter != want.BalanceAfter {
		t.Errorf("BalanceAfter = %s, quero %s", got.BalanceAfter, want.BalanceAfter)
	}
	if got.WalletVersion != want.WalletVersion {
		t.Errorf("WalletVersion = %d, quero %d", got.WalletVersion, want.WalletVersion)
	}
	if !got.OccurredAt.Equal(want.OccurredAt) {
		t.Errorf("OccurredAt = %s, quero %s", got.OccurredAt, want.OccurredAt)
	}
}

// assertChangeMatchesWallet checks that the last change describes the current
// wallet state, which is what gets persisted in the same SQL transaction.
func assertChangeMatchesWallet(t *testing.T, change BalanceChange, w *Wallet) {
	t.Helper()

	if change.BalanceAfter != w.Balance() {
		t.Errorf("BalanceAfter = %s, mas a carteira esta com %s", change.BalanceAfter, w.Balance())
	}
	if change.WalletVersion != w.Version() {
		t.Errorf("WalletVersion = %d, mas a carteira esta na versao %d", change.WalletVersion, w.Version())
	}
	if !change.OccurredAt.Equal(w.UpdatedAt()) {
		t.Errorf("OccurredAt = %s, mas UpdatedAt() = %s", change.OccurredAt, w.UpdatedAt())
	}
}
