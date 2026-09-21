//go:build integration

/*
@Author: Franco Ribeiro Borba
@Description: The guarantees the database enforces on its own. Each test here
tries to corrupt the data by hand, with plain SQL and no application layer in
the way, and expects to be refused. That is the point: these rules have to hold
even if every line of Go above them has a bug, and the only way to show that is
to attack the tables directly.
@Date : 21/09/2026
@Update: -
*/
package integration

import (
	"context"
	"strings"
	"testing"

	"github.com/google/uuid"
)

// O ledger é append-only no banco. Uma correção financeira é um lançamento
// novo, nunca uma edição, e é isso que mantém o histórico auditável.
func TestLedgerRecusaUpdateEDelete(t *testing.T) {
	pool := openPool(t)
	wallet := openWallet(t, pool, "100.00")

	ctx := context.Background()

	var entryID uuid.UUID

	err := pool.QueryRow(ctx, `SELECT id FROM wallet_ledger WHERE wallet_id = $1 LIMIT 1`, wallet.ID()).Scan(&entryID)
	if err != nil {
		t.Fatalf("ler lançamento da abertura erro inesperado: %v", err)
	}

	tests := []struct {
		name string
		sql  string
	}{
		{"update", `UPDATE wallet_ledger SET amount_cents = 1 WHERE id = $1`},
		{"delete", `DELETE FROM wallet_ledger WHERE id = $1`},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			_, err := pool.Exec(ctx, tc.sql, entryID)
			if err == nil {
				t.Fatalf("o banco aceitou um %s no ledger, ele precisa ser append-only", tc.name)
			}
			if !strings.Contains(err.Error(), "append-only") {
				t.Errorf("erro = %v, quero a recusa do trigger de append-only", err)
			}
		})
	}
}

// Última linha de defesa contra saldo negativo: mesmo que o domínio falhe, o
// banco recusa.
func TestBancoRecusaSaldoNegativo(t *testing.T) {
	pool := openPool(t)
	wallet := openWallet(t, pool, "100.00")

	_, err := pool.Exec(context.Background(),
		`UPDATE wallets SET balance_cents = -1, version = version + 1 WHERE id = $1`,
		wallet.ID(),
	)
	if err == nil {
		t.Fatal("o banco aceitou um saldo negativo")
	}
	if !strings.Contains(err.Error(), "balance_non_negative") {
		t.Errorf("erro = %v, quero a violação de wallets_balance_non_negative", err)
	}
}

// Um saldo não pode mudar sem mover a versão: é isso que transforma um lost
// update em erro visível em vez de corrupção silenciosa.
func TestSaldoNaoMudaSemMoverAVersao(t *testing.T) {
	pool := openPool(t)
	wallet := openWallet(t, pool, "100.00")

	_, err := pool.Exec(context.Background(),
		`UPDATE wallets SET balance_cents = balance_cents - 1 WHERE id = $1`,
		wallet.ID(),
	)
	if err == nil {
		t.Fatal("o banco aceitou uma mudança de saldo sem incrementar a versão")
	}
	if !strings.Contains(err.Error(), "version") {
		t.Errorf("erro = %v, quero a recusa do trigger de versão", err)
	}
}

// O snapshot da outbox é imutável. Um publisher só registra progresso, nunca
// reescreve o que o evento diz.
func TestOutboxRecusaReescritaDoPayload(t *testing.T) {
	pool := openPool(t)
	wallet := openWallet(t, pool, "100.00")

	ctx := context.Background()

	var eventID uuid.UUID

	err := pool.QueryRow(ctx, `SELECT event_id FROM outbox WHERE aggregate_id = $1 LIMIT 1`, wallet.ID()).Scan(&eventID)
	if err != nil {
		t.Fatalf("ler evento da abertura erro inesperado: %v", err)
	}

	_, err = pool.Exec(ctx, `UPDATE outbox SET payload = '{"tampered":true}'::jsonb WHERE event_id = $1`, eventID)
	if err == nil {
		t.Fatal("o banco aceitou a reescrita do snapshot de um evento")
	}
	if !strings.Contains(err.Error(), "immutable snapshot") {
		t.Errorf("erro = %v, quero a recusa do trigger de imutabilidade", err)
	}
}

// Uma carteira é aberta uma vez só, senão o crédito inicial seria duplicado.
func TestUmaUnicaAberturaPorCarteira(t *testing.T) {
	pool := openPool(t)
	wallet := openWallet(t, pool, "100.00")

	_, err := pool.Exec(context.Background(),
		`INSERT INTO wager_transactions (id, wallet_id, player_id, currency, amount_cents, kind, state, result_balance_cents, created_at, updated_at)
		 VALUES ($1, $2, $3, 'BRL', 5000, 'OPENING', 'PROCESSED', 5000, now(), now())`,
		uuid.New(), wallet.ID(), wallet.PlayerID(),
	)
	if err == nil {
		t.Fatal("o banco aceitou uma segunda abertura para a mesma carteira")
	}
	if !strings.Contains(err.Error(), "single_opening") {
		t.Errorf("erro = %v, quero a violação de wager_transactions_single_opening", err)
	}
}

// O mesmo jogador não pode ter duas carteiras na mesma moeda.
func TestUmaCarteiraPorJogadorEMoeda(t *testing.T) {
	pool := openPool(t)
	wallet := openWallet(t, pool, "100.00")

	_, err := pool.Exec(context.Background(),
		`INSERT INTO wallets (id, player_id, currency, balance_cents, version, created_at, updated_at)
		 VALUES ($1, $2, 'BRL', 0, 1, now(), now())`,
		uuid.New(), wallet.PlayerID(),
	)
	if err == nil {
		t.Fatal("o banco aceitou uma segunda carteira para o mesmo jogador e moeda")
	}
	if !strings.Contains(err.Error(), "player_currency") {
		t.Errorf("erro = %v, quero a violação de wallets_player_currency_key", err)
	}
}

// Uma transação terminal não aceita mais nenhuma transição.
func TestTransacaoTerminalNaoMudaMais(t *testing.T) {
	pool := openPool(t)
	wallet := openWallet(t, pool, "100.00")

	ctx := context.Background()

	var transactionID uuid.UUID

	err := pool.QueryRow(ctx,
		`SELECT id FROM wager_transactions WHERE wallet_id = $1 AND kind = 'OPENING'`,
		wallet.ID(),
	).Scan(&transactionID)
	if err != nil {
		t.Fatalf("ler a abertura erro inesperado: %v", err)
	}

	_, err = pool.Exec(ctx, `UPDATE wager_transactions SET state = 'PENDING' WHERE id = $1`, transactionID)
	if err == nil {
		t.Fatal("o banco aceitou reabrir uma transação terminal")
	}
	if !strings.Contains(err.Error(), "terminal") {
		t.Errorf("erro = %v, quero a recusa do trigger de estado terminal", err)
	}
}

// A aritmética de um lançamento precisa fechar: saldo depois = saldo antes ± valor.
func TestLedgerRecusaAritmeticaQueNaoFecha(t *testing.T) {
	pool := openPool(t)
	wallet := openWallet(t, pool, "100.00")

	ctx := context.Background()

	var transactionID uuid.UUID

	err := pool.QueryRow(ctx,
		`SELECT id FROM wager_transactions WHERE wallet_id = $1 AND kind = 'OPENING'`,
		wallet.ID(),
	).Scan(&transactionID)
	if err != nil {
		t.Fatalf("ler a abertura erro inesperado: %v", err)
	}

	// 100 - 30 não é 50.
	_, err = pool.Exec(ctx,
		`INSERT INTO wallet_ledger (id, wallet_id, transaction_id, direction, currency,
		        amount_cents, balance_before_cents, balance_after_cents, created_at)
		 VALUES ($1, $2, $3, 'DEBIT', 'BRL', 3000, 10000, 5000, now())`,
		uuid.New(), wallet.ID(), transactionID,
	)
	if err == nil {
		t.Fatal("o banco aceitou um lançamento cuja aritmética não fecha")
	}
	if !strings.Contains(err.Error(), "arithmetic") {
		t.Errorf("erro = %v, quero a violação de wallet_ledger_arithmetic", err)
	}
}
