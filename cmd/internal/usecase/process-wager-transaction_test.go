/*
@Author: Franco Ribeiro Borba
@Description: Unit tests for the wager processing flow
*/
package usecase

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/FrancoBorba/distributed-wallet-api/cmd/internal/domain"
	"github.com/FrancoBorba/distributed-wallet-api/cmd/internal/events"
	"github.com/google/uuid"
)

var (
	testPlayerID = uuid.MustParse("0192f28f-5dc0-7d58-bdb2-814ad6a0f4a1")
	testWalletID = uuid.MustParse("0192f291-27dd-7d3f-8071-5f8685deef37")
	testNow      = time.Date(2026, 9, 20, 12, 0, 0, 0, time.UTC)
)

// hashOf is a valid payload hash, in the format the schema requires.
const hashOf = "1111111111111111111111111111111111111111111111111111111111111111"

func brl(t *testing.T, amount string) domain.Money {
	t.Helper()

	value, err := domain.ParseMoney(amount, "BRL")
	if err != nil {
		t.Fatalf("ParseMoney(%q) erro inesperado: %v", amount, err)
	}

	return value
}

// setup builds the use case over a store already holding a wallet with the
// given balance.
func setup(t *testing.T, balance string) (*ProcessWagerTransaction, *store) {
	t.Helper()

	memory := newStore()

	wallet, err := domain.NewWallet(testWalletID, testPlayerID, brl(t, balance), testNow)
	if err != nil {
		t.Fatalf("NewWallet erro inesperado: %v", err)
	}
	memory.putWallet(wallet)

	return NewProcessWagerTransaction(memory, fixedClock{now: testNow}, &sequentialIDs{}), memory
}

// command builds a provider operation with sensible defaults.
func command(t *testing.T, externalID string, kind domain.TransactionKind, amount string) WagerCommand {
	t.Helper()

	return WagerCommand{
		ProviderID:            "provider-a",
		ExternalTransactionID: externalID,
		IdempotencyKey:        "provider-a:" + externalID,
		PayloadHash:           hashOf,
		PlayerID:              testPlayerID,
		WalletID:              testWalletID,
		RoundID:               "round-987",
		GameID:                "fortune-chimp",
		Kind:                  kind,
		Money:                 brl(t, amount),
		Metadata:              events.NewMetadata(uuid.New()),
	}
}

// --------------------------------------------------------------------------
// BET
// --------------------------------------------------------------------------

func TestExecute_BetDebitaEGeraEventos(t *testing.T) {
	processor, memory := setup(t, "100.00")

	result, err := processor.Execute(context.Background(), command(t, "transaction-1", domain.KindBet, "25.00"))
	if err != nil {
		t.Fatalf("Execute erro inesperado: %v", err)
	}

	if result.State != domain.StateProcessed {
		t.Errorf("state = %q, quero %q", result.State, domain.StateProcessed)
	}
	if got := memory.balanceOf(testWalletID).AmountString(); got != "75.00" {
		t.Errorf("saldo = %s, quero 75.00", got)
	}
	if len(memory.ledger) != 1 {
		t.Fatalf("lançamentos = %d, quero 1", len(memory.ledger))
	}
	if memory.ledger[0].Direction() != domain.DirectionDebit {
		t.Errorf("direção = %q, quero DEBIT", memory.ledger[0].Direction())
	}

	// Uma operação concluída que moveu dinheiro produz os dois eventos.
	if len(memory.eventsOfType(events.TypeWagerTransactionProcessed)) != 1 {
		t.Error("faltou WagerTransactionProcessed")
	}
	if len(memory.eventsOfType(events.TypeWalletBalanceChanged)) != 1 {
		t.Error("faltou WalletBalanceChanged")
	}
}

func TestExecute_BetSemSaldoRejeita(t *testing.T) {
	processor, memory := setup(t, "10.00")

	result, err := processor.Execute(context.Background(), command(t, "transaction-1", domain.KindBet, "80.00"))
	if err != nil {
		t.Fatalf("Execute erro inesperado: %v", err)
	}

	if result.State != domain.StateRejected {
		t.Errorf("state = %q, quero %q", result.State, domain.StateRejected)
	}
	if result.FailureCode != domain.FailureInsufficientFunds {
		t.Errorf("failureCode = %q, quero %q", result.FailureCode, domain.FailureInsufficientFunds)
	}
	if got := memory.balanceOf(testWalletID).AmountString(); got != "10.00" {
		t.Errorf("saldo = %s, quero 10.00 intacto", got)
	}
	if len(memory.ledger) != 0 {
		t.Errorf("lançamentos = %d, quero 0: rejeição não entra no ledger", len(memory.ledger))
	}
	if len(memory.eventsOfType(events.TypeWagerTransactionRejected)) != 1 {
		t.Error("faltou WagerTransactionRejected")
	}
	if len(memory.eventsOfType(events.TypeWalletBalanceChanged)) != 0 {
		t.Error("rejeição não pode publicar WalletBalanceChanged")
	}
}

func TestExecute_ReenvioNaoMoveSaldoDeNovo(t *testing.T) {
	processor, memory := setup(t, "100.00")
	cmd := command(t, "transaction-1", domain.KindBet, "25.00")

	first, err := processor.Execute(context.Background(), cmd)
	if err != nil {
		t.Fatalf("Execute erro inesperado: %v", err)
	}

	second, err := processor.Execute(context.Background(), cmd)
	if err != nil {
		t.Fatalf("Execute (reenvio) erro inesperado: %v", err)
	}

	if !second.Replayed {
		t.Error("o reenvio deveria ser reconhecido como replay")
	}
	if second.TransactionID != first.TransactionID {
		t.Errorf("transactionId = %s, quero o mesmo do original %s", second.TransactionID, first.TransactionID)
	}
	if got := memory.balanceOf(testWalletID).AmountString(); got != "75.00" {
		t.Errorf("saldo = %s, quero 75.00: o débito não pode ser aplicado duas vezes", got)
	}
	if len(memory.ledger) != 1 {
		t.Errorf("lançamentos = %d, quero 1", len(memory.ledger))
	}

	// O replay responde com o saldo observado quando a operação foi aplicada.
	if got := second.Balance.AmountString(); got != "75.00" {
		t.Errorf("saldo respondido = %s, quero 75.00", got)
	}
}

func TestExecute_MesmaOperacaoComPayloadDiferenteEConflito(t *testing.T) {
	processor, _ := setup(t, "100.00")

	if _, err := processor.Execute(context.Background(), command(t, "transaction-1", domain.KindBet, "25.00")); err != nil {
		t.Fatalf("Execute erro inesperado: %v", err)
	}

	conflicting := command(t, "transaction-1", domain.KindBet, "25.00")
	conflicting.PayloadHash = "2222222222222222222222222222222222222222222222222222222222222222"

	_, err := processor.Execute(context.Background(), conflicting)
	if !errors.Is(err, ErrPayloadConflict) {
		t.Errorf("erro = %v, quero %v", err, ErrPayloadConflict)
	}
}

// --------------------------------------------------------------------------
// LOSS
// --------------------------------------------------------------------------

func TestExecute_LossConcluiSemMovimentar(t *testing.T) {
	processor, memory := setup(t, "100.00")

	result, err := processor.Execute(context.Background(), command(t, "transaction-1", domain.KindLoss, "0.00"))
	if err != nil {
		t.Fatalf("Execute erro inesperado: %v", err)
	}

	if result.State != domain.StateProcessed {
		t.Errorf("state = %q, quero %q", result.State, domain.StateProcessed)
	}
	if got := memory.balanceOf(testWalletID).AmountString(); got != "100.00" {
		t.Errorf("saldo = %s, quero 100.00", got)
	}
	if len(memory.ledger) != 0 {
		t.Errorf("lançamentos = %d, quero 0: LOSS não cria ledger", len(memory.ledger))
	}
	if len(memory.eventsOfType(events.TypeWagerTransactionProcessed)) != 1 {
		t.Error("LOSS processado ainda produz WagerTransactionProcessed")
	}
	if len(memory.eventsOfType(events.TypeWalletBalanceChanged)) != 0 {
		t.Error("LOSS não altera saldo, não pode publicar WalletBalanceChanged")
	}
}

// --------------------------------------------------------------------------
// Referências
// --------------------------------------------------------------------------

func TestExecute_ReversaoSemReferenciaEsperaEmPendingReference(t *testing.T) {
	processor, memory := setup(t, "100.00")

	cmd := command(t, "refund-1", domain.KindRefund, "25.00")
	cmd.ReferenceExternalID = "transaction-1"

	result, err := processor.Execute(context.Background(), cmd)
	if err != nil {
		t.Fatalf("Execute erro inesperado: %v", err)
	}

	if result.State != domain.StatePendingRef {
		t.Errorf("state = %q, quero %q", result.State, domain.StatePendingRef)
	}
	if got := memory.balanceOf(testWalletID).AmountString(); got != "100.00" {
		t.Errorf("saldo = %s, quero 100.00 intacto", got)
	}
	if len(memory.eventsOfType(events.TypeWagerTransactionPendingReference)) != 1 {
		t.Error("faltou WagerTransactionPendingReference")
	}
}

func TestExecute_RefundDevolveApostaProcessada(t *testing.T) {
	processor, memory := setup(t, "100.00")

	if _, err := processor.Execute(context.Background(), command(t, "transaction-1", domain.KindBet, "25.00")); err != nil {
		t.Fatalf("Execute (aposta) erro inesperado: %v", err)
	}

	refund := command(t, "refund-1", domain.KindRefund, "25.00")
	refund.ReferenceExternalID = "transaction-1"

	result, err := processor.Execute(context.Background(), refund)
	if err != nil {
		t.Fatalf("Execute (refund) erro inesperado: %v", err)
	}

	if result.State != domain.StateProcessed {
		t.Errorf("state = %q, quero %q", result.State, domain.StateProcessed)
	}
	if got := memory.balanceOf(testWalletID).AmountString(); got != "100.00" {
		t.Errorf("saldo = %s, quero 100.00 devolvido", got)
	}
	if len(memory.ledger) != 2 {
		t.Errorf("lançamentos = %d, quero 2 (débito e crédito)", len(memory.ledger))
	}
}

func TestExecute_RollbackInverteADirecaoDaReferencia(t *testing.T) {
	processor, memory := setup(t, "100.00")

	// A referência é um WIN, que creditou: o rollback precisa debitar.
	if _, err := processor.Execute(context.Background(), command(t, "win-1", domain.KindWin, "50.00")); err != nil {
		t.Fatalf("Execute (win) erro inesperado: %v", err)
	}
	if got := memory.balanceOf(testWalletID).AmountString(); got != "150.00" {
		t.Fatalf("saldo após o win = %s, quero 150.00", got)
	}

	rollback := command(t, "rollback-1", domain.KindRollback, "50.00")
	rollback.ReferenceExternalID = "win-1"

	if _, err := processor.Execute(context.Background(), rollback); err != nil {
		t.Fatalf("Execute (rollback) erro inesperado: %v", err)
	}

	if got := memory.balanceOf(testWalletID).AmountString(); got != "100.00" {
		t.Errorf("saldo = %s, quero 100.00: o rollback de um crédito é um débito", got)
	}
	if memory.ledger[1].Direction() != domain.DirectionDebit {
		t.Errorf("direção do rollback = %q, quero DEBIT", memory.ledger[1].Direction())
	}
}

func TestExecute_SegundaReversaoDaMesmaApostaERejeitada(t *testing.T) {
	processor, _ := setup(t, "100.00")

	if _, err := processor.Execute(context.Background(), command(t, "transaction-1", domain.KindBet, "25.00")); err != nil {
		t.Fatalf("Execute (aposta) erro inesperado: %v", err)
	}

	refund := command(t, "refund-1", domain.KindRefund, "25.00")
	refund.ReferenceExternalID = "transaction-1"

	if _, err := processor.Execute(context.Background(), refund); err != nil {
		t.Fatalf("Execute (refund) erro inesperado: %v", err)
	}

	// Um ROLLBACK da mesma aposta devolveria o mesmo débito uma segunda vez.
	rollback := command(t, "rollback-1", domain.KindRollback, "25.00")
	rollback.ReferenceExternalID = "transaction-1"

	result, err := processor.Execute(context.Background(), rollback)
	if err != nil {
		t.Fatalf("Execute (rollback) erro inesperado: %v", err)
	}

	if result.FailureCode != domain.FailureAlreadyReversed {
		t.Errorf("failureCode = %q, quero %q", result.FailureCode, domain.FailureAlreadyReversed)
	}
}

// --------------------------------------------------------------------------
// Carteira e moeda
// --------------------------------------------------------------------------

func TestExecute_CarteiraInexistenteEMoedaDivergente(t *testing.T) {
	tests := []struct {
		name    string
		mutate  func(WagerCommand) WagerCommand
		wantErr domain.FailureCode
	}{
		{
			name: "carteira inexistente",
			mutate: func(c WagerCommand) WagerCommand {
				c.WalletID = uuid.MustParse("0192f291-0000-7000-8000-000000000099")
				return c
			},
			wantErr: domain.FailureWalletNotFound,
		},
		{
			name: "carteira de outro jogador",
			mutate: func(c WagerCommand) WagerCommand {
				c.PlayerID = uuid.MustParse("0192f28f-0000-7000-8000-000000000099")
				return c
			},
			wantErr: domain.FailureWalletNotFound,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			processor, _ := setup(t, "100.00")

			result, err := processor.Execute(context.Background(), tc.mutate(command(t, "transaction-1", domain.KindBet, "25.00")))
			if err != nil {
				t.Fatalf("Execute erro inesperado: %v", err)
			}
			if result.FailureCode != tc.wantErr {
				t.Errorf("failureCode = %q, quero %q", result.FailureCode, tc.wantErr)
			}
		})
	}
}

func TestExecute_MoedaDivergenteRejeita(t *testing.T) {
	processor, _ := setup(t, "100.00")

	cmd := command(t, "transaction-1", domain.KindBet, "25.00")

	usd, err := domain.ParseMoney("25.00", "USD")
	if err != nil {
		t.Fatalf("ParseMoney erro inesperado: %v", err)
	}
	cmd.Money = usd

	result, err := processor.Execute(context.Background(), cmd)
	if err != nil {
		t.Fatalf("Execute erro inesperado: %v", err)
	}

	if result.FailureCode != domain.FailureCurrencyMismatch {
		t.Errorf("failureCode = %q, quero %q", result.FailureCode, domain.FailureCurrencyMismatch)
	}
}

// --------------------------------------------------------------------------
// Entrada por SQS
// --------------------------------------------------------------------------

func TestExecuteFromMessage_ReentregaNaoAplicaDeNovo(t *testing.T) {
	processor, memory := setup(t, "100.00")
	cmd := command(t, "transaction-1", domain.KindBet, "25.00")

	first, err := processor.ExecuteFromMessage(context.Background(), "consumer", cmd, "msg-123", hashOf)
	if err != nil {
		t.Fatalf("ExecuteFromMessage erro inesperado: %v", err)
	}
	if first.State != domain.StateProcessed {
		t.Fatalf("state = %q, quero %q", first.State, domain.StateProcessed)
	}

	second, err := processor.ExecuteFromMessage(context.Background(), "consumer", cmd, "msg-123", hashOf)
	if err != nil {
		t.Fatalf("ExecuteFromMessage (reentrega) erro inesperado: %v", err)
	}
	if !second.Replayed {
		t.Error("a reentrega deveria ser reconhecida pelo inbox")
	}
	if got := memory.balanceOf(testWalletID).AmountString(); got != "75.00" {
		t.Errorf("saldo = %s, quero 75.00", got)
	}
}

func TestExecuteFromMessage_MesmoIdComConteudoDiferenteEConflito(t *testing.T) {
	processor, _ := setup(t, "100.00")
	cmd := command(t, "transaction-1", domain.KindBet, "25.00")

	if _, err := processor.ExecuteFromMessage(context.Background(), "consumer", cmd, "msg-123", hashOf); err != nil {
		t.Fatalf("ExecuteFromMessage erro inesperado: %v", err)
	}

	other := "3333333333333333333333333333333333333333333333333333333333333333"

	_, err := processor.ExecuteFromMessage(context.Background(), "consumer", cmd, "msg-123", other)
	if !errors.Is(err, ErrMessageConflict) {
		t.Errorf("erro = %v, quero %v", err, ErrMessageConflict)
	}
}
