//go:build integration

/*
@Author: Franco Ribeiro Borba
@Description: Shared setup of the integration tests. These run against the
real PostgreSQL and the real SQS of the Docker Compose environment, never
against a mock, because most of what they verify lives in the database itself:
the constraints, the triggers, the isolation levels and the behaviour of
FOR UPDATE SKIP LOCKED. A test double for any of those would only prove that
the double behaves as the double was written to behave.

They never truncate anything. The ledger refuses DELETE and TRUNCATE by design,
so each test works with identifiers of its own and asserts only on the rows it
created, which also lets the whole file run against a database that already
holds data.

Run them with:

	go test -tags=integration ./cmd/internal/test/integration/...
*/
package integration

import (
	"context"
	"log/slog"
	"os"
	"testing"
	"time"

	"github.com/FrancoBorba/distributed-wallet-api/cmd/internal/config"
	"github.com/FrancoBorba/distributed-wallet-api/cmd/internal/domain"
	"github.com/FrancoBorba/distributed-wallet-api/cmd/internal/infra/postgres"
	"github.com/FrancoBorba/distributed-wallet-api/cmd/internal/usecase"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
)

// databaseURL points at the PostgreSQL of the Compose environment unless the
// environment says otherwise.
func databaseURL() string {
	if url := os.Getenv("DATABASE_URL"); url != "" {
		return url
	}

	return "postgres://wager_user:wager_password@localhost:5432/wager_db?sslmode=disable"
}

// openPool connects and skips the test when the database is not there, so a
// checkout without the environment running reports "skipped" rather than a wall
// of failures that look like broken code.
func openPool(t *testing.T) *pgxpool.Pool {
	t.Helper()

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	pool, err := postgres.NewPool(ctx, config.Database{
		URL:            databaseURL(),
		MaxConnections: 30,
		ConnectTimeout: 5 * time.Second,
	})
	if err != nil {
		t.Skipf("PostgreSQL is not reachable, start the environment with docker compose up -d: %v", err)
	}

	t.Cleanup(pool.Close)

	return pool
}

// testClock is the instant the use cases stamp on everything they write.
type testClock struct{}

func (testClock) Now() time.Time { return time.Now().UTC() }

// newProcessor builds the real use case over the real transaction boundary.
func newProcessor(t *testing.T, pool *pgxpool.Pool) *usecase.ProcessWagerTransaction {
	t.Helper()

	return usecase.NewProcessWagerTransaction(postgres.NewUnitOfWork(pool), testClock{}, usecase.UUIDGenerator{})
}

// newOpener builds the real wallet opening use case.
func newOpener(t *testing.T, pool *pgxpool.Pool) *usecase.OpenWallet {
	t.Helper()

	return usecase.NewOpenWallet(postgres.NewUnitOfWork(pool), testClock{}, usecase.UUIDGenerator{})
}

// discardLogger keeps the test output readable. It is kept for tests that need
// to build a component which takes a logger.
//
//nolint:unused
func discardLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelError}))
}

// brl builds a monetary value or fails the test, since an invalid fixture is a
// bug in the test and not a case under verification.
func brl(t *testing.T, amount string) domain.Money {
	t.Helper()

	value, err := domain.ParseMoney(amount, "BRL")
	if err != nil {
		t.Fatalf("ParseMoney(%q) erro inesperado: %v", amount, err)
	}

	return value
}

// openWallet creates a wallet with a balance and returns it.
func openWallet(t *testing.T, pool *pgxpool.Pool, balance string) *domain.Wallet {
	t.Helper()

	wallet, err := newOpener(t, pool).Execute(context.Background(), usecase.OpenWalletCommand{
		PlayerID:       uuid.New(),
		InitialBalance: brl(t, balance),
	})
	if err != nil {
		t.Fatalf("abrir carteira erro inesperado: %v", err)
	}

	return wallet
}

// hashOf is a payload hash in the format the schema requires. Each test uses
// one of its own, since a different hash on the same key is a conflict.
func hashOf(seed byte) string {
	hash := make([]byte, 64)
	for i := range hash {
		hash[i] = "0123456789abcdef"[(int(seed)+i)%16]
	}

	return string(hash)
}

// command builds a provider operation over a wallet.
func command(t *testing.T, wallet *domain.Wallet, providerID, externalID string, kind domain.TransactionKind, amount string, hash string) usecase.WagerCommand {
	t.Helper()

	return usecase.WagerCommand{
		ProviderID:            providerID,
		ExternalTransactionID: externalID,
		IdempotencyKey:        providerID + ":" + externalID,
		PayloadHash:           hash,
		PlayerID:              wallet.PlayerID(),
		WalletID:              wallet.ID(),
		RoundID:               "round-" + externalID,
		GameID:                "fortune-chimp",
		Kind:                  kind,
		Money:                 brl(t, amount),
	}
}

// balanceOf reads the stored balance straight from the table, bypassing every
// layer, which is what makes it a fact and not an assertion about our own code.
func balanceOf(t *testing.T, pool *pgxpool.Pool, walletID uuid.UUID) string {
	t.Helper()

	var cents int64

	err := pool.QueryRow(context.Background(), `SELECT balance_cents FROM wallets WHERE id = $1`, walletID).Scan(&cents)
	if err != nil {
		t.Fatalf("ler saldo erro inesperado: %v", err)
	}

	money, err := domain.NewMoney(cents, "BRL")
	if err != nil {
		t.Fatalf("NewMoney erro inesperado: %v", err)
	}

	return money.AmountString()
}

// countLedger counts the entries of a wallet.
func countLedger(t *testing.T, pool *pgxpool.Pool, walletID uuid.UUID) int {
	t.Helper()

	var count int

	err := pool.QueryRow(context.Background(), `SELECT COUNT(*) FROM wallet_ledger WHERE wallet_id = $1`, walletID).Scan(&count)
	if err != nil {
		t.Fatalf("contar ledger erro inesperado: %v", err)
	}

	return count
}

// newQueries builds the read side over the pool.
func newQueries(t *testing.T, pool *pgxpool.Pool) usecase.WalletQueries {
	t.Helper()

	return postgres.NewQueryRepository(pool)
}
