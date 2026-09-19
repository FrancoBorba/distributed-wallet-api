/*
@Author: Franco Ribeiro Borba
@Description: Unit tests for the money domain
*/
package domain

import (
	"encoding/json"
	"errors"
	"math"
	"testing"
)

// --------------------------------------------------------------------------
// ParseMoney
// --------------------------------------------------------------------------

func TestParseMoney_Valid(t *testing.T) {
	tests := []struct {
		name     string
		amount   string
		currency string
		want     int64
	}{
		{"valor inteiro", "25.00", "BRL", 2500},
		{"zero", "0.00", "BRL", 0},
		{"apenas centavos", "0.05", "BRL", 5},
		{"noventa e nove centavos", "0.99", "BRL", 99},
		{"milhar", "1000.00", "BRL", 100000},
		{"centavos significativos", "975.43", "BRL", 97543},
		{"outra moeda", "25.00", "USD", 2500},
		{"limite alto suportado", "92233720368547757.99", "BRL", 9223372036854775799},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got, err := ParseMoney(tc.amount, tc.currency)
			if err != nil {
				t.Fatalf("ParseMoney(%q, %q) erro inesperado: %v", tc.amount, tc.currency, err)
			}
			if got.Cents() != tc.want {
				t.Errorf("cents = %d, quero %d", got.Cents(), tc.want)
			}
			if got.Currency() != tc.currency {
				t.Errorf("currency = %q, quero %q", got.Currency(), tc.currency)
			}
		})
	}
}

func TestParseMoney_InvalidAmount(t *testing.T) {
	tests := []struct {
		name    string
		amount  string
		wantErr error
	}{
		{"vazio", "", ErrInvalidAmount},
		{"sem escala", "25", ErrInvalidAmount},
		{"escala insuficiente", "25.0", ErrInvalidAmount},
		{"escala excedente", "25.000", ErrInvalidAmount},
		{"ponto sem decimais", "25.", ErrInvalidAmount},
		{"sem parte inteira", ".00", ErrInvalidAmount},
		{"dois separadores", "25.00.00", ErrInvalidAmount},
		{"separador decimal virgula", "25,00", ErrInvalidAmount},
		{"texto", "abc", ErrInvalidAmount},
		{"texto com escala", "ab.cd", ErrInvalidAmount},
		{"notacao cientifica", "2e5", ErrInvalidAmount},
		{"notacao cientifica com escala", "2.5e10", ErrInvalidAmount},
		{"NaN", "NaN", ErrInvalidAmount},
		{"Infinity", "Infinity", ErrInvalidAmount},
		{"espaco a esquerda", " 25.00", ErrInvalidAmount},
		{"espaco a direita", "25.00 ", ErrInvalidAmount},
		{"sinal positivo explicito", "+25.00", ErrInvalidAmount},
		{"hexadecimal", "0x1F.00", ErrInvalidAmount},
		{"underscore", "1_000.00", ErrInvalidAmount},
		{"zeros a esquerda", "0025.00", ErrInvalidAmount},
		{"zero nao canonico", "00.00", ErrInvalidAmount},
		{"estouro de int64", "99999999999999999999.00", ErrInvalidAmount},
		{"negativo", "-25.00", ErrNegativeAmount},
		{"negativo zero", "-0.00", ErrNegativeAmount},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got, err := ParseMoney(tc.amount, "BRL")
			if !errors.Is(err, tc.wantErr) {
				t.Fatalf("ParseMoney(%q) erro = %v, quero %v", tc.amount, err, tc.wantErr)
			}
			if got != (Money{}) {
				t.Errorf("em caso de erro deve retornar Money zero, obtive %+v", got)
			}
		})
	}
}

func TestParseMoney_InvalidCurrency(t *testing.T) {
	tests := []struct {
		name     string
		currency string
	}{
		{"vazia", ""},
		{"duas letras", "BR"},
		{"quatro letras", "BRLL"},
		{"minuscula", "brl"},
		{"mista", "Brl"},
		{"com digito", "BR1"},
		{"com simbolo", "BR$"},
		{"nao ASCII com 3 bytes", "ÉA"},
		{"espacos", "   "},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := ParseMoney("25.00", tc.currency); !errors.Is(err, ErrInvalidCurrencyFormat) {
				t.Errorf("ParseMoney(_, %q) erro = %v, quero %v", tc.currency, err, ErrInvalidCurrencyFormat)
			}
		})
	}
}

// --------------------------------------------------------------------------
// NewMoney
// --------------------------------------------------------------------------

func TestNewMoney(t *testing.T) {
	t.Run("valores validos", func(t *testing.T) {
		m, err := NewMoney(2500, "BRL")
		if err != nil {
			t.Fatalf("erro inesperado: %v", err)
		}
		if m.Cents() != 2500 || m.Currency() != "BRL" {
			t.Errorf("obtive %d/%q, quero 2500/BRL", m.Cents(), m.Currency())
		}
	})

	t.Run("negativo permitido em calculo interno", func(t *testing.T) {
		m, err := NewMoney(-500, "BRL")
		if err != nil {
			t.Fatalf("erro inesperado: %v", err)
		}
		if m.Cents() != -500 {
			t.Errorf("cents = %d, quero -500", m.Cents())
		}
	})

	t.Run("moeda invalida", func(t *testing.T) {
		if _, err := NewMoney(2500, "xx"); !errors.Is(err, ErrInvalidCurrencyFormat) {
			t.Errorf("erro = %v, quero %v", err, ErrInvalidCurrencyFormat)
		}
	})
}

// --------------------------------------------------------------------------
// Add
// --------------------------------------------------------------------------

func TestAdd(t *testing.T) {
	tests := []struct {
		name string
		a, b int64
		want int64
	}{
		{"soma simples", 2500, 1000, 3500},
		{"soma com zero", 2500, 0, 2500},
		{"zero com zero", 0, 0, 0},
		{"soma de centavos", 5, 95, 100},
		{"parcela negativa", 2500, -1000, 1500},
		{"resultado negativo", 1000, -2500, -1500},
		{"ambas negativas", -1000, -2500, -3500},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			a := mustMoney(t, tc.a, "BRL")
			b := mustMoney(t, tc.b, "BRL")

			got, err := a.Add(b)
			if err != nil {
				t.Fatalf("Add erro inesperado: %v", err)
			}
			if got.Cents() != tc.want {
				t.Errorf("%d + %d = %d, quero %d", tc.a, tc.b, got.Cents(), tc.want)
			}
			if got.Currency() != "BRL" {
				t.Errorf("currency = %q, quero BRL", got.Currency())
			}
		})
	}
}

func TestAdd_Commutative(t *testing.T) {
	a := mustMoney(t, 7350, "BRL")
	b := mustMoney(t, 1299, "BRL")

	ab, err := a.Add(b)
	if err != nil {
		t.Fatalf("a.Add(b): %v", err)
	}
	ba, err := b.Add(a)
	if err != nil {
		t.Fatalf("b.Add(a): %v", err)
	}
	if ab.Cents() != ba.Cents() {
		t.Errorf("soma nao comutativa: %d != %d", ab.Cents(), ba.Cents())
	}
}

func TestAdd_CurrencyMismatch(t *testing.T) {
	brl := mustMoney(t, 2500, "BRL")
	usd := mustMoney(t, 2500, "USD")

	if _, err := brl.Add(usd); !errors.Is(err, ErrCurrencyMismatch) {
		t.Errorf("erro = %v, quero %v", err, ErrCurrencyMismatch)
	}
}

func TestAdd_Overflow(t *testing.T) {
	tests := []struct {
		name string
		a, b int64
	}{
		{"estouro positivo", math.MaxInt64, 1},
		{"estouro positivo maximo", math.MaxInt64, math.MaxInt64},
		{"estouro negativo", math.MinInt64, -1},
		{"estouro negativo maximo", math.MinInt64, math.MinInt64},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			a := mustMoney(t, tc.a, "BRL")
			b := mustMoney(t, tc.b, "BRL")

			if _, err := a.Add(b); !errors.Is(err, ErrArithmeticOverflow) {
				t.Errorf("Add(%d, %d) erro = %v, quero %v", tc.a, tc.b, err, ErrArithmeticOverflow)
			}
		})
	}
}

func TestAdd_NoOverflowOnLimits(t *testing.T) {
	a := mustMoney(t, math.MaxInt64, "BRL")
	b := mustMoney(t, math.MinInt64, "BRL")

	got, err := a.Add(b)
	if err != nil {
		t.Fatalf("MaxInt64 + MinInt64 nao deveria estourar: %v", err)
	}
	if got.Cents() != -1 {
		t.Errorf("cents = %d, quero -1", got.Cents())
	}
}

// --------------------------------------------------------------------------
// Subtract
// --------------------------------------------------------------------------

func TestSubtract(t *testing.T) {
	tests := []struct {
		name string
		a, b int64
		want int64
	}{
		{"debito simples", 100000, 2500, 97500},
		{"subtracao de zero", 2500, 0, 2500},
		{"zera o saldo", 2500, 2500, 0},
		{"resultado negativo permitido", 1000, 2500, -1500},
		{"subtrai negativo", 1000, -500, 1500},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			a := mustMoney(t, tc.a, "BRL")
			b := mustMoney(t, tc.b, "BRL")

			got, err := a.Subtract(b)
			if err != nil {
				t.Fatalf("Subtract erro inesperado: %v", err)
			}
			if got.Cents() != tc.want {
				t.Errorf("%d - %d = %d, quero %d", tc.a, tc.b, got.Cents(), tc.want)
			}
		})
	}
}

func TestSubtract_CurrencyMismatch(t *testing.T) {
	brl := mustMoney(t, 2500, "BRL")
	usd := mustMoney(t, 2500, "USD")

	if _, err := brl.Subtract(usd); !errors.Is(err, ErrCurrencyMismatch) {
		t.Errorf("erro = %v, quero %v", err, ErrCurrencyMismatch)
	}
}

func TestSubtract_Overflow(t *testing.T) {
	tests := []struct {
		name string
		a, b int64
	}{
		{"zero menos MinInt64", 0, math.MinInt64},
		{"positivo menos MinInt64", 1, math.MinInt64},
		{"MinInt64 menos positivo", math.MinInt64, 1},
		{"MaxInt64 menos negativo", math.MaxInt64, -1},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			a := mustMoney(t, tc.a, "BRL")
			b := mustMoney(t, tc.b, "BRL")

			if _, err := a.Subtract(b); !errors.Is(err, ErrArithmeticOverflow) {
				t.Errorf("Subtract(%d, %d) erro = %v, quero %v", tc.a, tc.b, err, ErrArithmeticOverflow)
			}
		})
	}
}

// --------------------------------------------------------------------------
// Serializacao e imutabilidade
// --------------------------------------------------------------------------

func TestAmountString(t *testing.T) {
	tests := []struct {
		name  string
		cents int64
		want  string
	}{
		{"zero", 0, "0.00"},
		{"um centavo", 1, "0.01"},
		{"centavos", 5, "0.05"},
		{"noventa e nove centavos", 99, "0.99"},
		{"uma unidade", 100, "1.00"},
		{"valor do contrato", 2500, "25.00"},
		{"saldo", 97500, "975.00"},
		{"negativo", -2500, "-25.00"},
		{"negativo em centavos", -5, "-0.05"},
		{"limite superior", math.MaxInt64, "92233720368547758.07"},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			m := mustMoney(t, tc.cents, "BRL")
			if got := m.AmountString(); got != tc.want {
				t.Errorf("AmountString() = %q, quero %q", got, tc.want)
			}
		})
	}
}

func TestParseAmountStringRoundTrip(t *testing.T) {
	inputs := []string{"0.00", "0.01", "0.99", "1.00", "25.00", "975.43", "1000.00"}

	for _, in := range inputs {
		t.Run(in, func(t *testing.T) {
			m, err := ParseMoney(in, "BRL")
			if err != nil {
				t.Fatalf("ParseMoney(%q): %v", in, err)
			}
			if got := m.AmountString(); got != in {
				t.Errorf("round-trip de %q resultou em %q", in, got)
			}
		})
	}
}

func TestMoneyIsImmutable(t *testing.T) {
	a := mustMoney(t, 10000, "BRL")
	b := mustMoney(t, 2500, "BRL")

	if _, err := a.Add(b); err != nil {
		t.Fatalf("Add: %v", err)
	}
	if _, err := a.Subtract(b); err != nil {
		t.Fatalf("Subtract: %v", err)
	}

	if a.Cents() != 10000 || b.Cents() != 2500 {
		t.Errorf("operandos mutados: a=%d b=%d", a.Cents(), b.Cents())
	}
}

func TestZeroValueMoneyIsRejected(t *testing.T) {
	var uninitialized Money
	valid := mustMoney(t, 2500, "BRL")

	t.Run("como operando a direita", func(t *testing.T) {
		if _, err := valid.Add(uninitialized); !errors.Is(err, ErrUninitializedMoney) {
			t.Errorf("Add erro = %v, quero %v", err, ErrUninitializedMoney)
		}
	})

	t.Run("como receptor", func(t *testing.T) {
		if _, err := uninitialized.Subtract(valid); !errors.Is(err, ErrUninitializedMoney) {
			t.Errorf("Subtract erro = %v, quero %v", err, ErrUninitializedMoney)
		}
	})

	t.Run("nos dois lados", func(t *testing.T) {
		if _, err := uninitialized.Add(uninitialized); !errors.Is(err, ErrUninitializedMoney) {
			t.Errorf("Add erro = %v, quero %v", err, ErrUninitializedMoney)
		}
	})

	t.Run("negacao", func(t *testing.T) {
		if _, err := uninitialized.Negate(); !errors.Is(err, ErrUninitializedMoney) {
			t.Errorf("Negate erro = %v, quero %v", err, ErrUninitializedMoney)
		}
	})

	t.Run("comparacao", func(t *testing.T) {
		if _, err := valid.Compare(uninitialized); !errors.Is(err, ErrUninitializedMoney) {
			t.Errorf("Compare erro = %v, quero %v", err, ErrUninitializedMoney)
		}
	})

	t.Run("serializacao", func(t *testing.T) {
		if _, err := json.Marshal(uninitialized); err == nil {
			t.Error("serializar Money nao inicializado deveria falhar")
		}
	})
}

// --------------------------------------------------------------------------
// Zero
// --------------------------------------------------------------------------

func TestZero(t *testing.T) {
	t.Run("cria valor neutro", func(t *testing.T) {
		m, err := Zero("BRL")
		if err != nil {
			t.Fatalf("erro inesperado: %v", err)
		}
		if m.Cents() != 0 || m.Currency() != "BRL" {
			t.Errorf("obtive %d/%q, quero 0/BRL", m.Cents(), m.Currency())
		}
		if got := m.AmountString(); got != "0.00" {
			t.Errorf("AmountString() = %q, quero \"0.00\"", got)
		}
	})

	t.Run("moeda invalida", func(t *testing.T) {
		if _, err := Zero("brl"); !errors.Is(err, ErrInvalidCurrencyFormat) {
			t.Errorf("erro = %v, quero %v", err, ErrInvalidCurrencyFormat)
		}
	})

	t.Run("elemento neutro da soma", func(t *testing.T) {
		zero, err := Zero("BRL")
		if err != nil {
			t.Fatalf("Zero: %v", err)
		}
		valor := mustMoney(t, 2500, "BRL")

		got, err := valor.Add(zero)
		if err != nil {
			t.Fatalf("Add: %v", err)
		}
		if got.Cents() != 2500 {
			t.Errorf("cents = %d, quero 2500", got.Cents())
		}
	})
}

// --------------------------------------------------------------------------
// Negate
// --------------------------------------------------------------------------

func TestNegate(t *testing.T) {
	tests := []struct {
		name  string
		cents int64
		want  int64
	}{
		{"positivo vira negativo", 2500, -2500},
		{"negativo vira positivo", -2500, 2500},
		{"zero permanece zero", 0, 0},
		{"limite superior", math.MaxInt64, -math.MaxInt64},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			m := mustMoney(t, tc.cents, "BRL")

			got, err := m.Negate()
			if err != nil {
				t.Fatalf("Negate erro inesperado: %v", err)
			}
			if got.Cents() != tc.want {
				t.Errorf("Negate(%d) = %d, quero %d", tc.cents, got.Cents(), tc.want)
			}
			if got.Currency() != "BRL" {
				t.Errorf("currency = %q, quero BRL", got.Currency())
			}
		})
	}
}

func TestNegate_Overflow(t *testing.T) {
	m := mustMoney(t, math.MinInt64, "BRL")

	if _, err := m.Negate(); !errors.Is(err, ErrArithmeticOverflow) {
		t.Errorf("erro = %v, quero %v", err, ErrArithmeticOverflow)
	}
}

func TestNegate_IsInvolutive(t *testing.T) {
	original := mustMoney(t, 7350, "BRL")

	once, err := original.Negate()
	if err != nil {
		t.Fatalf("primeira negacao: %v", err)
	}
	twice, err := once.Negate()
	if err != nil {
		t.Fatalf("segunda negacao: %v", err)
	}
	if twice.Cents() != original.Cents() {
		t.Errorf("negacao dupla = %d, quero %d", twice.Cents(), original.Cents())
	}
}

// --------------------------------------------------------------------------
// Comparacao
// --------------------------------------------------------------------------

func TestCompare(t *testing.T) {
	tests := []struct {
		name string
		a, b int64
		want int
	}{
		{"menor", 1000, 2500, -1},
		{"maior", 2500, 1000, 1},
		{"igual", 2500, 2500, 0},
		{"zero contra positivo", 0, 1, -1},
		{"negativo contra zero", -1, 0, -1},
		{"limites opostos", math.MinInt64, math.MaxInt64, -1},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			a := mustMoney(t, tc.a, "BRL")
			b := mustMoney(t, tc.b, "BRL")

			got, err := a.Compare(b)
			if err != nil {
				t.Fatalf("Compare erro inesperado: %v", err)
			}
			if got != tc.want {
				t.Errorf("Compare(%d, %d) = %d, quero %d", tc.a, tc.b, got, tc.want)
			}
		})
	}
}

func TestCompare_CurrencyMismatch(t *testing.T) {
	brl := mustMoney(t, 2500, "BRL")
	usd := mustMoney(t, 2500, "USD")

	if _, err := brl.Compare(usd); !errors.Is(err, ErrCurrencyMismatch) {
		t.Errorf("Compare erro = %v, quero %v", err, ErrCurrencyMismatch)
	}
	if _, err := brl.Equals(usd); !errors.Is(err, ErrCurrencyMismatch) {
		t.Errorf("Equals erro = %v, quero %v", err, ErrCurrencyMismatch)
	}
}

func TestEquals(t *testing.T) {
	a := mustMoney(t, 2500, "BRL")
	b := mustMoney(t, 2500, "BRL")
	c := mustMoney(t, 2501, "BRL")

	equal, err := a.Equals(b)
	if err != nil {
		t.Fatalf("Equals: %v", err)
	}
	if !equal {
		t.Error("valores iguais deveriam ser considerados iguais")
	}

	equal, err = a.Equals(c)
	if err != nil {
		t.Fatalf("Equals: %v", err)
	}
	if equal {
		t.Error("valores diferentes nao deveriam ser considerados iguais")
	}
}

func TestSignPredicates(t *testing.T) {
	tests := []struct {
		name                      string
		cents                     int64
		zero, negative, positive_ bool
	}{
		{"zero", 0, true, false, false},
		{"positivo", 1, false, false, true},
		{"negativo", -1, false, true, false},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			m := mustMoney(t, tc.cents, "BRL")

			if m.IsZero() != tc.zero {
				t.Errorf("IsZero() = %v, quero %v", m.IsZero(), tc.zero)
			}
			if m.IsNegative() != tc.negative {
				t.Errorf("IsNegative() = %v, quero %v", m.IsNegative(), tc.negative)
			}
			if m.IsPositive() != tc.positive_ {
				t.Errorf("IsPositive() = %v, quero %v", m.IsPositive(), tc.positive_)
			}
		})
	}
}

// --------------------------------------------------------------------------
// JSON
// --------------------------------------------------------------------------

func TestMarshalJSON(t *testing.T) {
	tests := []struct {
		name     string
		cents    int64
		currency string
		want     string
	}{
		{"valor do contrato", 2500, "BRL", `{"amount":"25.00","currency":"BRL"}`},
		{"zero", 0, "BRL", `{"amount":"0.00","currency":"BRL"}`},
		{"centavos", 5, "USD", `{"amount":"0.05","currency":"USD"}`},
		{"negativo", -2500, "BRL", `{"amount":"-25.00","currency":"BRL"}`},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			m := mustMoney(t, tc.cents, tc.currency)

			got, err := json.Marshal(m)
			if err != nil {
				t.Fatalf("Marshal erro inesperado: %v", err)
			}
			if string(got) != tc.want {
				t.Errorf("Marshal = %s, quero %s", got, tc.want)
			}
		})
	}
}

func TestUnmarshalJSON(t *testing.T) {
	t.Run("payload valido", func(t *testing.T) {
		var m Money
		if err := json.Unmarshal([]byte(`{"amount":"25.00","currency":"BRL"}`), &m); err != nil {
			t.Fatalf("Unmarshal erro inesperado: %v", err)
		}
		if m.Cents() != 2500 || m.Currency() != "BRL" {
			t.Errorf("obtive %d/%q, quero 2500/BRL", m.Cents(), m.Currency())
		}
	})

	t.Run("dentro de um payload maior", func(t *testing.T) {
		var payload struct {
			RoundID string `json:"roundId"`
			Money   Money  `json:"money"`
		}
		raw := `{"roundId":"round-987","money":{"amount":"25.00","currency":"BRL"}}`

		if err := json.Unmarshal([]byte(raw), &payload); err != nil {
			t.Fatalf("Unmarshal erro inesperado: %v", err)
		}
		if payload.Money.Cents() != 2500 {
			t.Errorf("cents = %d, quero 2500", payload.Money.Cents())
		}
	})

	t.Run("json malformado", func(t *testing.T) {
		// O stdlib rejeita a sintaxe antes de chamar UnmarshalJSON, entao aqui
		// basta garantir que o valor nao e construido.
		var m Money
		if err := json.Unmarshal([]byte(`{"amount":`), &m); err == nil {
			t.Error("json malformado deveria falhar")
		}
		if m != (Money{}) {
			t.Errorf("valor nao deveria ser alterado, obtive %+v", m)
		}
	})

	t.Run("payloads invalidos", func(t *testing.T) {
		tests := []struct {
			name    string
			raw     string
			wantErr error
		}{
			{"amount negativo", `{"amount":"-25.00","currency":"BRL"}`, ErrNegativeAmount},
			{"escala excedente", `{"amount":"25.000","currency":"BRL"}`, ErrInvalidAmount},
			{"sem escala", `{"amount":"25","currency":"BRL"}`, ErrInvalidAmount},
			{"amount vazio", `{"amount":"","currency":"BRL"}`, ErrInvalidAmount},
			{"amount ausente", `{"currency":"BRL"}`, ErrInvalidAmount},
			{"notacao cientifica", `{"amount":"2.5e10","currency":"BRL"}`, ErrInvalidAmount},
			{"moeda invalida", `{"amount":"25.00","currency":"brl"}`, ErrInvalidCurrencyFormat},
			{"moeda ausente", `{"amount":"25.00"}`, ErrInvalidCurrencyFormat},
			{"amount numerico", `{"amount":25.00,"currency":"BRL"}`, ErrInvalidAmount},
			{"amount booleano", `{"amount":true,"currency":"BRL"}`, ErrInvalidAmount},
			{"objeto nulo", `null`, ErrInvalidCurrencyFormat},
		}

		for _, tc := range tests {
			t.Run(tc.name, func(t *testing.T) {
				var m Money
				if err := json.Unmarshal([]byte(tc.raw), &m); !errors.Is(err, tc.wantErr) {
					t.Errorf("Unmarshal(%s) erro = %v, quero %v", tc.raw, err, tc.wantErr)
				}
				if m != (Money{}) {
					t.Errorf("em caso de erro o valor nao deve ser alterado, obtive %+v", m)
				}
			})
		}
	})
}

func TestJSONRoundTrip(t *testing.T) {
	original := mustMoney(t, 97543, "BRL")

	raw, err := json.Marshal(original)
	if err != nil {
		t.Fatalf("Marshal: %v", err)
	}

	var decoded Money
	if err := json.Unmarshal(raw, &decoded); err != nil {
		t.Fatalf("Unmarshal: %v", err)
	}

	if decoded != original {
		t.Errorf("round-trip resultou em %+v, quero %+v", decoded, original)
	}
}

func TestString(t *testing.T) {
	m := mustMoney(t, 2500, "BRL")

	if got := m.String(); got != "25.00 BRL" {
		t.Errorf("String() = %q, quero \"25.00 BRL\"", got)
	}
}

// --------------------------------------------------------------------------
// helpers
// --------------------------------------------------------------------------

func mustMoney(t *testing.T, cents int64, currency string) Money {
	t.Helper()

	m, err := NewMoney(cents, currency)
	if err != nil {
		t.Fatalf("NewMoney(%d, %q): %v", cents, currency, err)
	}
	return m
}
