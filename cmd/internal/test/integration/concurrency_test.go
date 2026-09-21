//go:build integration

/*
@Author: Franco Ribeiro Borba
@Description: Concurrency and recovery against the real database. Every test
here exists because the behaviour it verifies cannot be observed anywhere else:
the row lock, the version guard, the partial unique indexes and the claim with
SKIP LOCKED are all properties of PostgreSQL, and a test double would only
prove that the double does what it was written to do.
@Date : 21/09/2026
@Update: -
*/
package integration

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/FrancoBorba/distributed-wallet-api/cmd/internal/domain"
	"github.com/FrancoBorba/distributed-wallet-api/cmd/internal/infra/postgres"
	"github.com/FrancoBorba/distributed-wallet-api/cmd/internal/usecase"
	"github.com/google/uuid"
)

// O teste obrigatório do desafio: uma carteira com 100.00 recebe, ao mesmo
// tempo, duas apostas distintas de 80.00. Uma precisa passar, a outra precisa
// ser rejeitada por saldo, o saldo final precisa ser 20.00 e o ledger precisa
// conter um único débito.
func TestDuasApostasDisputandoOMesmoSaldo(t *testing.T) {
	pool := openPool(t)
	processor := newProcessor(t, pool)
	wallet := openWallet(t, pool, "100.00")

	first := command(t, wallet, "provider-a", "race-"+uuid.NewString(), domain.KindBet, "80.00", hashOf(1))
	second := command(t, wallet, "provider-a", "race-"+uuid.NewString(), domain.KindBet, "80.00", hashOf(2))

	var (
		wg      sync.WaitGroup
		results = make([]usecase.WagerResult, 2)
		errs    = make([]error, 2)
		start   = make(chan struct{})
	)

	for i, cmd := range []usecase.WagerCommand{first, second} {
		wg.Add(1)

		go func(index int, cmd usecase.WagerCommand) {
			defer wg.Done()

			// Both goroutines are released at the same instant, so they really
			// do contend instead of running one after the other.
			<-start

			results[index], errs[index] = processor.Execute(context.Background(), cmd)
		}(i, cmd)
	}

	close(start)
	wg.Wait()

	processed, rejected := 0, 0

	for i := range results {
		if errs[i] != nil {
			t.Fatalf("aposta %d erro inesperado: %v", i, errs[i])
		}

		switch results[i].State {
		case domain.StateProcessed:
			processed++
		case domain.StateRejected:
			rejected++

			if results[i].FailureCode != domain.FailureInsufficientFunds {
				t.Errorf("failureCode = %q, quero %q", results[i].FailureCode, domain.FailureInsufficientFunds)
			}
		}
	}

	if processed != 1 || rejected != 1 {
		t.Errorf("processadas = %d, rejeitadas = %d, quero exatamente 1 de cada", processed, rejected)
	}
	if got := balanceOf(t, pool, wallet.ID()); got != "20.00" {
		t.Errorf("saldo final = %s, quero 20.00", got)
	}

	// A abertura criou uma entrada de crédito, então o débito é a segunda.
	if got := countLedger(t, pool, wallet.ID()); got != 2 {
		t.Errorf("lançamentos = %d, quero 2 (a abertura e um único débito)", got)
	}
}

// A mesma aposta enviada 50 vezes em paralelo precisa produzir um único débito.
// É o cenário obrigatório de duplicidade, e ele exercita a deduplicação da
// aplicação, não a do broker.
func TestMesmaApostaCinquentaVezesEmParalelo(t *testing.T) {
	pool := openPool(t)
	processor := newProcessor(t, pool)
	wallet := openWallet(t, pool, "100.00")

	cmd := command(t, wallet, "provider-a", "dup-"+uuid.NewString(), domain.KindBet, "25.00", hashOf(3))

	const attempts = 50

	var (
		wg      sync.WaitGroup
		mu      sync.Mutex
		applied int
		replays int
		errored int
		start   = make(chan struct{})
	)

	for i := 0; i < attempts; i++ {
		wg.Add(1)

		go func() {
			defer wg.Done()
			<-start

			result, err := processor.Execute(context.Background(), cmd)

			mu.Lock()
			defer mu.Unlock()

			switch {
			case err != nil:
				// A loser of the race for the very first insert is a
				// transient conflict the caller retries, not a movement.
				errored++
			case result.Replayed:
				replays++
			default:
				applied++
			}
		}()
	}

	close(start)
	wg.Wait()

	// Exactly one of the fifty is the movement; every other one that succeeded
	// recognised it and answered with the stored result.
	if applied != 1 {
		t.Errorf("aplicadas = %d, quero exatamente 1 entre %d envios", applied, attempts)
	}

	if got := balanceOf(t, pool, wallet.ID()); got != "75.00" {
		t.Errorf("saldo = %s, quero 75.00: 50 envios da mesma aposta são um único débito", got)
	}
	if got := countLedger(t, pool, wallet.ID()); got != 2 {
		t.Errorf("lançamentos = %d, quero 2 (a abertura e um único débito)", got)
	}

	t.Logf("de %d envios: %d aplicado, %d replays idempotentes, %d conflitos transitorios", attempts, applied, replays, errored)
}

// Carteiras diferentes não podem se bloquear: o lock é de linha, não de tabela.
func TestCarteirasDiferentesProcessamEmParalelo(t *testing.T) {
	pool := openPool(t)
	processor := newProcessor(t, pool)

	const wallets = 8

	created := make([]*domain.Wallet, wallets)
	for i := range created {
		created[i] = openWallet(t, pool, "100.00")
	}

	var wg sync.WaitGroup

	start := make(chan struct{})
	started := time.Now()

	for i, wallet := range created {
		wg.Add(1)

		go func(index int, wallet *domain.Wallet) {
			defer wg.Done()
			<-start

			cmd := command(t, wallet, "provider-a", "par-"+uuid.NewString(), domain.KindBet, "10.00", hashOf(byte(index+4)))

			if _, err := processor.Execute(context.Background(), cmd); err != nil {
				t.Errorf("carteira %d erro inesperado: %v", index, err)
			}
		}(i, wallet)
	}

	close(start)
	wg.Wait()

	for i, wallet := range created {
		if got := balanceOf(t, pool, wallet.ID()); got != "90.00" {
			t.Errorf("carteira %d saldo = %s, quero 90.00", i, got)
		}
	}

	t.Logf("%d carteiras processadas em paralelo em %s", wallets, time.Since(started))
}

// Dois publishers disputando a mesma outbox precisam pegar conjuntos
// disjuntos. É o que o SKIP LOCKED garante, e é o que impede o mesmo evento de
// ser publicado duas vezes por instâncias diferentes.
func TestDoisPublishersPegamEventosDisjuntos(t *testing.T) {
	pool := openPool(t)
	store := postgres.NewOutboxStore(pool)

	// A abertura de uma carteira com saldo positivo grava dois eventos.
	openWallet(t, pool, "500.00")

	var (
		wg    sync.WaitGroup
		mu    sync.Mutex
		seen  = map[uuid.UUID]string{}
		dupes []uuid.UUID
		start = make(chan struct{})
	)

	for _, publisher := range []string{"publisher-1", "publisher-2"} {
		wg.Add(1)

		go func(name string) {
			defer wg.Done()
			<-start

			claimed, err := store.ClaimPending(context.Background(), name, 100, 30*time.Second)
			if err != nil {
				t.Errorf("%s erro inesperado: %v", name, err)

				return
			}

			mu.Lock()
			defer mu.Unlock()

			for _, event := range claimed {
				if owner, taken := seen[event.Envelope.ID()]; taken {
					dupes = append(dupes, event.Envelope.ID())
					t.Errorf("evento %s foi reivindicado por %s e por %s", event.Envelope.ID(), owner, name)

					continue
				}

				seen[event.Envelope.ID()] = name
			}
		}(publisher)
	}

	close(start)
	wg.Wait()

	if len(dupes) > 0 {
		t.Fatalf("%d eventos foram reivindicados duas vezes", len(dupes))
	}
	if len(seen) == 0 {
		t.Fatal("nenhum evento foi reivindicado, a abertura deveria ter gravado dois")
	}

	t.Logf("%d eventos reivindicados, nenhum em duplicidade", len(seen))
}

// Um publisher que morre segurando eventos não pode travá-los para sempre: o
// lease vence e outra instância assume o trabalho abandonado.
func TestLeaseVencidoLiberaTrabalhoAbandonado(t *testing.T) {
	pool := openPool(t)
	store := postgres.NewOutboxStore(pool)

	openWallet(t, pool, "300.00")

	// Lease de duração nula: equivale a um publisher que morreu no instante
	// seguinte ao claim.
	abandoned, err := store.ClaimPending(context.Background(), "publisher-que-morreu", 100, 0)
	if err != nil {
		t.Fatalf("ClaimPending erro inesperado: %v", err)
	}
	if len(abandoned) == 0 {
		t.Fatal("nada foi reivindicado, a abertura deveria ter gravado eventos")
	}

	taken := map[uuid.UUID]bool{}
	for _, event := range abandoned {
		taken[event.Envelope.ID()] = true
	}

	recovered, err := store.ClaimPending(context.Background(), "publisher-que-assumiu", 100, 30*time.Second)
	if err != nil {
		t.Fatalf("segundo ClaimPending erro inesperado: %v", err)
	}

	recoveredAny := false

	for _, event := range recovered {
		if taken[event.Envelope.ID()] {
			recoveredAny = true

			// O segundo claim conta como mais uma tentativa do mesmo evento.
			if event.Attempts < 2 {
				t.Errorf("tentativas = %d, quero pelo menos 2", event.Attempts)
			}
		}
	}

	if !recoveredAny {
		t.Error("nenhum evento abandonado foi assumido depois do vencimento do lease")
	}
}
