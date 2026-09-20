# Migrations

Versioned migrations for the wager wallet schema, in the `golang-migrate` file
layout (`{version}_{name}.{up|down}.sql`). Every migration has a reversal, and
each file runs inside a single SQL transaction.

| Version | Table | What it protects |
| --- | --- | --- |
| `000001` | `wallets` | Non-negative balance, one wallet per `(playerId, currency)`, version that only moves with the balance |
| `000002` | `wager_transactions` | Idempotency, state machine, per-kind rules, single opening, single reversal per reference |
| `000003` | `wallet_ledger` | Append-only history, one entry per `(walletId, transactionId)`, balance arithmetic |
| `000004` | `outbox` | Immutable event snapshot, retry with backoff, publisher claim |
| `000005` | `inbox` | Deduplication of at-least-once delivery by `(consumerName, messageId)` |

## Applying and reverting

With the CLI (`go install -tags 'postgres' github.com/golang-migrate/migrate/v4/cmd/migrate@latest`):

```bash
export DATABASE_URL='postgres://wager_user:wager_password@localhost:5432/wager_db?sslmode=disable'

migrate -path ./migrations -database "$DATABASE_URL" up        # apply everything
migrate -path ./migrations -database "$DATABASE_URL" down 1    # revert the last one
migrate -path ./migrations -database "$DATABASE_URL" down -all # revert everything
migrate -path ./migrations -database "$DATABASE_URL" version   # current version
```

Without installing anything, through the official image on the compose network:

```bash
docker run --rm -v "$PWD/migrations:/migrations" --network host \
  migrate/migrate -path=/migrations \
  -database 'postgres://wager_user:wager_password@localhost:5432/wager_db?sslmode=disable' up
```

If a migration fails halfway, `migrate` marks the version as dirty and refuses
to continue. Fix the SQL, then `migrate ... force <previous version>` and run
`up` again.

## Design notes

**Money is never a floating point value.** Every amount is `BIGINT` in minor
units, matching the `int64` cents of the `Money` value object. `NUMERIC` would
also be exact, but `BIGINT` maps to the domain type with no conversion.

**Currency agreement is a foreign key, not a check.** `wallets` carries a
`UNIQUE (id, currency)` whose only purpose is to be the target of the composite
foreign keys in `wager_transactions` and `wallet_ledger`. A movement in a
currency other than the wallet's own cannot be inserted at all.

**Internal and external origins share one table.** `OPENING` carries no
provider metadata and every external kind carries all of it, enforced by
`wager_transactions_origin_metadata`. Because `NULL` never collides in a
`UNIQUE` constraint, internal rows stay out of the idempotency rules for free.

**One reversal per reference.** `wager_transactions_single_successful_reversal`
indexes `reference_transaction_id` alone, not the kind, so a bet that already
has a processed `REFUND` cannot also get a processed `ROLLBACK`. This is what
stops the same debit from being returned twice.

**The ledger refuses `UPDATE`, `DELETE` and `TRUNCATE`.** Integration tests
that need to reset the database should drop and re-apply the migrations rather
than truncating. To truncate deliberately:

```sql
ALTER TABLE wallet_ledger DISABLE TRIGGER wallet_ledger_deny_truncate;
```

**Triggers cover what `CHECK` cannot.** A `CHECK` sees one row in isolation, so
the rules that compare the new row against the previous one live in `BEFORE
UPDATE` triggers: the terminal state of a transaction, the immutability of the
outbox snapshot, and the wallet version that must move by exactly one whenever
the balance changes. That last one turns a lost update into a visible error
instead of silent corruption.

**Cursor pagination.** `wallet_ledger.seq` is a `BIGSERIAL` that gives the
ledger a stable ordering for the opaque cursor of
`GET /wallets/:walletId/ledger`. Reads should use a consistent snapshot, since
sequence values can become visible out of order under concurrency.

## Verification

Applied, exercised and reverted against `postgres:15-alpine`:

- 54 constraint cases, each trying to corrupt the data and expecting a refusal.
- Full `up` → `down -all` → `up` cycle, leaving no orphan table or function.
