/*
@Author: Franco Ribeiro Borba
@Description: Unit tests for the outbox publisher backoff
*/
package worker

import (
	"testing"
	"time"
)

func TestBackoffDelay(t *testing.T) {
	const (
		base = 2 * time.Second
		max  = 5 * time.Minute
	)

	tests := []struct {
		name     string
		attempts int
		want     time.Duration
	}{
		{"primeira tentativa", 1, 2 * time.Second},
		{"segunda", 2, 4 * time.Second},
		{"terceira", 3, 8 * time.Second},
		{"quarta", 4, 16 * time.Second},
		{"cresce até o teto", 8, 256 * time.Second},
		{"para no teto", 9, max},
		{"muito além do teto", 40, max},
		{"contador zerado conta como a primeira", 0, 2 * time.Second},
		{"contador negativo não vira atraso negativo", -5, 2 * time.Second},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := backoffDelay(tc.attempts, base, max)
			if got != tc.want {
				t.Errorf("backoffDelay(%d) = %s, quero %s", tc.attempts, got, tc.want)
			}
		})
	}
}

// Um deslocamento grande transborda o int64 e produziria um atraso negativo,
// que o banco aceitaria como "tente de novo no passado": o evento entraria em
// laço apertado exatamente quando já está falhando.
func TestBackoffDelay_NuncaNegativo(t *testing.T) {
	for attempts := 1; attempts <= 128; attempts++ {
		got := backoffDelay(attempts, time.Second, time.Hour)
		if got <= 0 {
			t.Fatalf("backoffDelay(%d) = %s, o atraso precisa ser positivo", attempts, got)
		}
		if got > time.Hour {
			t.Fatalf("backoffDelay(%d) = %s, passou do teto", attempts, got)
		}
	}
}
