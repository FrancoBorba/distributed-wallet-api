//go:build integration

/*
@Author: Franco Ribeiro Borba
@Description: The queue and the deduplication, against the real broker and the
real database. What is verified here is the behaviour of at-least-once
delivery: a message that arrives twice must move money once, an operation that
arrives through both transports must be recognised as one, and an event written
to the outbox must survive the trip to SQS and come back readable on the other
side.
@Date : 21/09/2026
@Update: -
*/
package integration

import (
	"context"
	"encoding/json"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/FrancoBorba/distributed-wallet-api/cmd/internal/config"
	"github.com/FrancoBorba/distributed-wallet-api/cmd/internal/domain"
	"github.com/FrancoBorba/distributed-wallet-api/cmd/internal/events"
	"github.com/FrancoBorba/distributed-wallet-api/cmd/internal/infra/messaging"
	"github.com/FrancoBorba/distributed-wallet-api/cmd/internal/usecase"
	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/sqs"
	"github.com/google/uuid"
)

// awsConfig points at the LocalStack of the Compose environment.
func awsConfig() config.AWS {
	endpoint := os.Getenv("AWS_ENDPOINT_URL")
	if endpoint == "" {
		endpoint = "http://localhost:4566"
	}

	return config.AWS{
		Region:          "us-east-1",
		Endpoint:        endpoint,
		AccessKeyID:     "test",
		SecretAccessKey: "test",
		InboundQueueURL: endpoint + "/000000000000/wager-transactions.fifo",
		EventsQueueURL:  endpoint + "/000000000000/wallet-events.fifo",
	}
}

// openSQS connects and skips the test when the broker is not there.
func openSQS(t *testing.T) (*sqs.Client, config.AWS) {
	t.Helper()

	cfg := awsConfig()

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	client, err := messaging.NewSQSClient(ctx, cfg)
	if err != nil {
		t.Skipf("SQS is not reachable, start the environment with docker compose up -d: %v", err)
	}

	_, err = client.GetQueueAttributes(ctx, &sqs.GetQueueAttributesInput{
		QueueUrl:       aws.String(cfg.EventsQueueURL),
		AttributeNames: nil,
	})
	if err != nil {
		t.Skipf("the queues are not provisioned, start the environment with docker compose up -d: %v", err)
	}

	return client, cfg
}

// A mesma mensagem entregue duas vezes move dinheiro uma vez só. É a
// deduplicação da aplicação, no banco, e não a janela do broker.
func TestReentregaDaMesmaMensagemMoveDinheiroUmaVez(t *testing.T) {
	pool := openPool(t)
	processor := newProcessor(t, pool)
	wallet := openWallet(t, pool, "100.00")

	cmd := command(t, wallet, "provider-a", "inbox-"+uuid.NewString(), domain.KindBet, "25.00", hashOf(9))
	messageID := "msg-" + uuid.NewString()

	first, err := processor.ExecuteFromMessage(context.Background(), "test-consumer", cmd, messageID, cmd.PayloadHash)
	if err != nil {
		t.Fatalf("primeira entrega erro inesperado: %v", err)
	}
	if first.State != domain.StateProcessed {
		t.Fatalf("state = %q, quero %q", first.State, domain.StateProcessed)
	}

	second, err := processor.ExecuteFromMessage(context.Background(), "test-consumer", cmd, messageID, cmd.PayloadHash)
	if err != nil {
		t.Fatalf("reentrega erro inesperado: %v", err)
	}
	if !second.Replayed {
		t.Error("a reentrega deveria ter sido reconhecida pelo inbox")
	}

	if got := balanceOf(t, pool, wallet.ID()); got != "75.00" {
		t.Errorf("saldo = %s, quero 75.00", got)
	}
	if got := countLedger(t, pool, wallet.ID()); got != 2 {
		t.Errorf("lançamentos = %d, quero 2 (a abertura e um único débito)", got)
	}
}

// O mesmo messageId voltando com outro conteúdo é conflito, não duplicata:
// tratá-lo como repetição descartaria uma operação real em silêncio.
func TestMesmoMessageIdComConteudoDiferenteEConflito(t *testing.T) {
	pool := openPool(t)
	processor := newProcessor(t, pool)
	wallet := openWallet(t, pool, "100.00")

	cmd := command(t, wallet, "provider-a", "conflict-"+uuid.NewString(), domain.KindBet, "25.00", hashOf(10))
	messageID := "msg-" + uuid.NewString()

	if _, err := processor.ExecuteFromMessage(context.Background(), "test-consumer", cmd, messageID, cmd.PayloadHash); err != nil {
		t.Fatalf("primeira entrega erro inesperado: %v", err)
	}

	_, err := processor.ExecuteFromMessage(context.Background(), "test-consumer", cmd, messageID, hashOf(11))
	if err == nil {
		t.Fatal("o mesmo messageId com outro hash deveria ser recusado")
	}
	if !strings.Contains(err.Error(), "message id was already received") {
		t.Errorf("erro = %v, quero o conflito do inbox", err)
	}
}

// A mesma operação chegando pelos dois transportes é um único movimento. Esta
// é a equivalência que o hash canônico garante, verificada contra o banco.
func TestMesmaOperacaoPorHTTPEPorSQSEUmMovimentoSo(t *testing.T) {
	pool := openPool(t)
	processor := newProcessor(t, pool)
	wallet := openWallet(t, pool, "100.00")

	cmd := command(t, wallet, "provider-a", "cross-"+uuid.NewString(), domain.KindBet, "30.00", hashOf(12))

	// Primeiro pelo caminho HTTP.
	if _, err := processor.Execute(context.Background(), cmd); err != nil {
		t.Fatalf("entrada HTTP erro inesperado: %v", err)
	}

	// Depois a mesma operação pela fila, com um messageId novo.
	result, err := processor.ExecuteFromMessage(context.Background(), "test-consumer", cmd, "msg-"+uuid.NewString(), cmd.PayloadHash)
	if err != nil {
		t.Fatalf("entrada SQS erro inesperado: %v", err)
	}
	if !result.Replayed {
		t.Error("a operação vinda pela fila deveria ter sido reconhecida como replay da que veio por HTTP")
	}

	if got := balanceOf(t, pool, wallet.ID()); got != "70.00" {
		t.Errorf("saldo = %s, quero 70.00: a operação não pode ser aplicada por cada transporte", got)
	}
}

// Um evento gravado na outbox precisa sobreviver à viagem até o SQS e voltar
// legível, com o mesmo eventId, que é como o consumidor deduplica.
func TestEventoPublicadoChegaLegivelNaFila(t *testing.T) {
	client, cfg := openSQS(t)

	publisher := messaging.NewPublisher(client, cfg.EventsQueueURL)

	change := domain.BalanceChange{
		WalletID:      uuid.New(),
		Direction:     domain.DirectionDebit,
		Amount:        brl(t, "25.00"),
		BalanceBefore: brl(t, "100.00"),
		BalanceAfter:  brl(t, "75.00"),
		WalletVersion: 2,
		OccurredAt:    time.Now().UTC(),
	}

	envelope, err := events.NewWalletBalanceChanged(change, uuid.New(), events.NewMetadata(uuid.New()))
	if err != nil {
		t.Fatalf("NewWalletBalanceChanged erro inesperado: %v", err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	if err := publisher.Publish(ctx, envelope); err != nil {
		t.Fatalf("Publish erro inesperado: %v", err)
	}

	// The queue is shared with whatever else is running, so the test looks for
	// its own event rather than assuming the first message is the one.
	deadline := time.Now().Add(20 * time.Second)

	for time.Now().Before(deadline) {
		output, err := client.ReceiveMessage(ctx, &sqs.ReceiveMessageInput{
			QueueUrl:            aws.String(cfg.EventsQueueURL),
			MaxNumberOfMessages: 10,
			WaitTimeSeconds:     2,
		})
		if err != nil {
			t.Fatalf("ReceiveMessage erro inesperado: %v", err)
		}

		for _, message := range output.Messages {
			var received map[string]any
			if err := json.Unmarshal([]byte(aws.ToString(message.Body)), &received); err != nil {
				continue
			}

			if received["eventId"] != envelope.ID().String() {
				continue
			}

			if received["eventType"] != string(events.TypeWalletBalanceChanged) {
				t.Errorf("eventType = %v, quero %q", received["eventType"], events.TypeWalletBalanceChanged)
			}

			data, ok := received["data"].(map[string]any)
			if !ok {
				t.Fatalf("data = %T, quero um objeto", received["data"])
			}

			money, ok := data["money"].(map[string]any)
			if !ok {
				t.Fatalf("money = %T, quero um objeto", data["money"])
			}
			if money["amount"] != "25.00" {
				t.Errorf("amount = %v, quero a string decimal 25.00", money["amount"])
			}

			_, _ = client.DeleteMessage(ctx, &sqs.DeleteMessageInput{
				QueueUrl:      aws.String(cfg.EventsQueueURL),
				ReceiptHandle: message.ReceiptHandle,
			})

			return
		}
	}

	t.Fatalf("o evento %s não apareceu na fila dentro do prazo", envelope.ID())
}

// O saldo armazenado precisa bater com créditos menos débitos do ledger,
// incluindo a abertura. É a conferência final que o desafio pede.
func TestReconciliacaoFechaDepoisDeVariasOperacoes(t *testing.T) {
	pool := openPool(t)
	processor := newProcessor(t, pool)
	wallet := openWallet(t, pool, "200.00")

	operations := []struct {
		kind   domain.TransactionKind
		amount string
	}{
		{domain.KindBet, "50.00"},
		{domain.KindWin, "30.00"},
		{domain.KindLoss, "0.00"},
		{domain.KindBet, "20.00"},
	}

	for i, operation := range operations {
		cmd := command(t, wallet, "provider-a", "rec-"+uuid.NewString(), operation.kind, operation.amount, hashOf(byte(20+i)))

		if _, err := processor.Execute(context.Background(), cmd); err != nil {
			t.Fatalf("operação %d (%s) erro inesperado: %v", i, operation.kind, err)
		}
	}

	reconcile := usecase.NewReconcileWallet(newQueries(t, pool))

	result, err := reconcile.Execute(context.Background(), wallet.ID())
	if err != nil {
		t.Fatalf("reconciliação erro inesperado: %v", err)
	}

	if !result.Consistent {
		t.Errorf("reconciliação divergiu: armazenado %s, calculado %s, diferença %s",
			result.StoredBalance, result.CalculatedBalance, result.Difference)
	}

	// 200 - 50 + 30 - 20 = 160. LOSS não move nada e não cria lançamento.
	if got := result.StoredBalance.AmountString(); got != "160.00" {
		t.Errorf("saldo = %s, quero 160.00", got)
	}
	if result.CheckedEntries != 4 {
		t.Errorf("lançamentos conferidos = %d, quero 4 (abertura, aposta, ganho, aposta)", result.CheckedEntries)
	}
}
