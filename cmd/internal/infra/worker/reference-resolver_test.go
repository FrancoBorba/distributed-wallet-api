/*
@Author: Franco Ribeiro Borba
@Description: Unit tests for the retry budget of pending references
*/
package worker

import (
	"testing"
	"time"

	"github.com/FrancoBorba/distributed-wallet-api/cmd/internal/config"
	"github.com/FrancoBorba/distributed-wallet-api/cmd/internal/usecase"
)

// fixedClock pins the instant so the deadline is compared against a known now.
type fixedClock struct {
	now time.Time
}

func (c fixedClock) Now() time.Time { return c.now }

// Os dois limites protegem contra falhas diferentes: o contador de tentativas
// limita uma referência que nunca chega, e o prazo limita um worker que
// reiniciou tantas vezes que o contador nunca cresceu.
func TestReferenceResolver_Exhausted(t *testing.T) {
	now := time.Date(2026, 9, 21, 12, 0, 0, 0, time.UTC)

	tests := []struct {
		name      string
		cfg       config.Reference
		attempts  int
		createdAt time.Time
		want      bool
	}{
		{
			name:      "dentro dos dois limites",
			cfg:       config.Reference{MaxAttempts: 10, TTL: time.Hour},
			attempts:  3,
			createdAt: now.Add(-10 * time.Minute),
			want:      false,
		},
		{
			name:      "tentativas esgotadas",
			cfg:       config.Reference{MaxAttempts: 10, TTL: time.Hour},
			attempts:  11,
			createdAt: now.Add(-10 * time.Minute),
			want:      true,
		},
		{
			name:      "prazo estourado mesmo com poucas tentativas",
			cfg:       config.Reference{MaxAttempts: 10, TTL: time.Hour},
			attempts:  2,
			createdAt: now.Add(-2 * time.Hour),
			want:      true,
		},
		{
			name:      "sem limite de tentativas, só o prazo",
			cfg:       config.Reference{MaxAttempts: 0, TTL: time.Hour},
			attempts:  999,
			createdAt: now.Add(-10 * time.Minute),
			want:      false,
		},
		{
			name:      "sem prazo, só as tentativas",
			cfg:       config.Reference{MaxAttempts: 5, TTL: 0},
			attempts:  3,
			createdAt: now.Add(-100 * time.Hour),
			want:      false,
		},
		{
			name:      "exatamente no limite ainda tenta",
			cfg:       config.Reference{MaxAttempts: 5, TTL: time.Hour},
			attempts:  5,
			createdAt: now.Add(-time.Minute),
			want:      false,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			resolver := &ReferenceResolver{clock: fixedClock{now: now}, cfg: tc.cfg}

			got := resolver.exhausted(usecase.PendingReference{
				Attempts:  tc.attempts,
				CreatedAt: tc.createdAt,
			})
			if got != tc.want {
				t.Errorf("exhausted = %v, quero %v", got, tc.want)
			}
		})
	}
}
