/*
@Author: Franco Ribeiro Borba
@Description: Money value object. An immutable monetary amount stored as int64
minor units (cents) with a fixed scale of two decimal places and an ISO 4217
currency code. The representable range is "-92233720368547758.08" to
"92233720368547758.07"; every operation that could leave this range returns
ErrArithmeticOverflow instead of wrapping around. External input is accepted
only in canonical form ("25.00"), so the same payload always produces the same
idempotency hash. Negative amounts are allowed in internal arithmetic but
rejected when parsing external financial input.
@Date : 19/09/2026
@Update: -
*/
package domain

import (
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"strconv"
	"strings"
)

// scale is the fixed number of decimal places used by every monetary value.
const scale = 2

type Money struct {
	cents    int64 // We will represent monetary values in cents.
	currency string
}

// moneyJSON is the external contract representation of a monetary value.
type moneyJSON struct {
	Amount   string `json:"amount"`
	Currency string `json:"currency"`
}

var (
	ErrInvalidCurrencyFormat = errors.New("currency must be a 3-letter")
	ErrInvalidAmount         = errors.New("invalid money amount format")
	ErrNegativeAmount        = errors.New("money amount cannot be negative")
	ErrCurrencyMismatch      = errors.New("currencies do not match")
	ErrArithmeticOverflow    = errors.New("arithmetic operation overflow")
	ErrUninitializedMoney    = errors.New("money value is not initialized")
)

// NewMoney builds a Money from minor units. Negative amounts are accepted
// because internal calculations such as differences may produce them.
func NewMoney(cents int64, currency string) (Money, error) {
	if !isValidISO4217(currency) {
		return Money{}, ErrInvalidCurrencyFormat
	}

	return Money{
		cents:    cents,
		currency: currency,
	}, nil
}

// Zero builds the neutral amount of a currency, used by operations that must
// not move the balance, such as the LOSS transaction type.
func Zero(currency string) (Money, error) {
	return NewMoney(0, currency)
}

// ParseMoney builds a Money from the decimal string used by the external
// contract. It accepts only the canonical form: digits, a single dot and
// exactly two decimal places, with no sign, no leading zeros, no spaces and no
// scientific notation.
func ParseMoney(amount string, currency string) (Money, error) {
	if !isValidISO4217(currency) {
		return Money{}, ErrInvalidCurrencyFormat
	}
	if strings.HasPrefix(amount, "-") {
		return Money{}, ErrNegativeAmount
	}

	separator := strings.IndexByte(amount, '.')
	if separator < 1 || len(amount)-separator-1 != scale {
		return Money{}, ErrInvalidAmount
	}

	units := amount[:separator]
	fraction := amount[separator+1:]

	if !isCanonicalUnits(units) || !isDigits(fraction) {
		return Money{}, ErrInvalidAmount
	}

	// ParseInt also rejects values that do not fit in int64.
	cents, err := strconv.ParseInt(units+fraction, 10, 64)
	if err != nil {
		return Money{}, ErrInvalidAmount
	}

	return Money{
		cents:    cents,
		currency: currency,
	}, nil
}

// isValidISO4217 checks if the currency follows the international pattern of
// three uppercase ASCII letters.
func isValidISO4217(currency string) bool {
	if len(currency) != 3 {
		return false
	}
	for i := 0; i < len(currency); i++ {
		if currency[i] < 'A' || currency[i] > 'Z' {
			return false
		}
	}
	return true
}

// isDigits reports whether the text is a non empty sequence of ASCII digits.
func isDigits(text string) bool {
	if text == "" {
		return false
	}
	for i := 0; i < len(text); i++ {
		if text[i] < '0' || text[i] > '9' {
			return false
		}
	}
	return true
}

// isCanonicalUnits reports whether the integer part is a sequence of digits
// without redundant leading zeros, so that every amount has a single accepted
// spelling.
func isCanonicalUnits(units string) bool {
	if !isDigits(units) {
		return false
	}
	return len(units) == 1 || units[0] != '0'
}

// validate rejects a Money that was never built through a constructor, since
// its currency would be empty.
func (m Money) validate() error {
	if m.currency == "" {
		return ErrUninitializedMoney
	}
	return nil
}

// requireSameCurrency validates both operands and rejects arithmetic or
// comparison between different currencies.
func (m Money) requireSameCurrency(other Money) error {
	if err := m.validate(); err != nil {
		return err
	}
	if err := other.validate(); err != nil {
		return err
	}
	if m.currency != other.currency {
		return ErrCurrencyMismatch
	}
	return nil
}

// Add returns the sum of two amounts of the same currency.
func (m Money) Add(other Money) (Money, error) {
	if err := m.requireSameCurrency(other); err != nil {
		return Money{}, err
	}

	sum := m.cents + other.cents

	if (other.cents > 0 && sum < m.cents) || (other.cents < 0 && sum > m.cents) {
		return Money{}, ErrArithmeticOverflow
	}

	return Money{
		cents:    sum,
		currency: m.currency,
	}, nil
}

// Subtract returns the difference between two amounts of the same currency.
func (m Money) Subtract(other Money) (Money, error) {
	if err := m.requireSameCurrency(other); err != nil {
		return Money{}, err
	}

	diff := m.cents - other.cents

	if (other.cents < 0 && diff < m.cents) || (other.cents > 0 && diff > m.cents) {
		return Money{}, ErrArithmeticOverflow
	}

	return Money{
		cents:    diff,
		currency: m.currency,
	}, nil
}

// Negate invert the signal, used to turn a credit into
// a debit on reversals.
func (m Money) Negate() (Money, error) {
	if err := m.validate(); err != nil {
		return Money{}, err
	}
	if m.cents == math.MinInt64 {
		return Money{}, ErrArithmeticOverflow
	}

	return Money{
		cents:    -m.cents,
		currency: m.currency,
	}, nil
}

// Compare returns a negative number when the receiver is smaller, zero when
// both are equal and a positive number when the receiver is greater.
func (m Money) Compare(other Money) (int, error) {
	if err := m.requireSameCurrency(other); err != nil {
		return 0, err
	}

	switch {
	case m.cents < other.cents:
		return -1, nil
	case m.cents > other.cents:
		return 1, nil
	default:
		return 0, nil
	}
}

// Equals reports whether both amounts hold the same value in the same
// currency.
func (m Money) Equals(other Money) (bool, error) {
	result, err := m.Compare(other)
	if err != nil {
		return false, err
	}
	return result == 0, nil
}

// IsZero reports whether the amount has no value.
func (m Money) IsZero() bool {
	return m.cents == 0
}

// IsNegative reports whether the amount is below zero.
func (m Money) IsNegative() bool {
	return m.cents < 0
}

// IsPositive reports whether the amount is above zero.
func (m Money) IsPositive() bool {
	return m.cents > 0
}

// AmountString returns the decimal representation of the value, for example
// 1500 becomes "15.00".
func (m Money) AmountString() string {
	digits := strconv.FormatInt(m.cents, 10)

	sign := ""
	if m.cents < 0 {
		sign = "-"
		digits = digits[1:]
	}

	// Pad so that there is at least one digit before the decimal separator.
	for len(digits) <= scale {
		digits = "0" + digits
	}

	return sign + digits[:len(digits)-scale] + "." + digits[len(digits)-scale:]
}

// MarshalJSON serializes the value into the external contract format
// {"amount":"25.00","currency":"BRL"}.
func (m Money) MarshalJSON() ([]byte, error) {
	if err := m.validate(); err != nil {
		return nil, err
	}

	return json.Marshal(moneyJSON{
		Amount:   m.AmountString(),
		Currency: m.currency,
	})
}

// UnmarshalJSON builds the value from the external contract format, applying
// the same validation rules as ParseMoney.
func (m *Money) UnmarshalJSON(data []byte) error {
	var payload moneyJSON
	if err := json.Unmarshal(data, &payload); err != nil {
		return ErrInvalidAmount
	}

	parsed, err := ParseMoney(payload.Amount, payload.Currency)
	if err != nil {
		return err
	}

	*m = parsed
	return nil
}

// String returns a readable representation for logs and test failures.
func (m Money) String() string {
	return fmt.Sprintf("%s %s", m.AmountString(), m.currency)
}

// Currency returns the ISO 4217 code of the value.
func (m Money) Currency() string {
	return m.currency
}

// Cents returns the value in minor units, used by the persistence layer.
func (m Money) Cents() int64 {
	return m.cents
}
