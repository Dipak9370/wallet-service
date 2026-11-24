# Wallet Transfer Service (Go + PostgreSQL)

## Summary
A small service for wallet-to-wallet transfers with:
- idempotent `POST /transfers` (exactly-once at the API level when an `idempotencyKey` is given)
- double-entry ledger (every processed transfer writes exactly one DEBIT and one CREDIT)
- stored wallet balances that stay correct under concurrent requests
- a transfer state machine: `PENDING -> PROCESSED | FAILED`

Stack: Go 1.22, standard `net/http`, PostgreSQL 16, `pgx/v5`.

Also included (optional items): `GET /wallets/{id}/balance`, `POST /wallets` (demo helper), structured logging via `slog`.

## Architecture
```
handler    -> HTTP validation, JSON, error-to-status mapping (thin)
service    -> workflow: idempotency, lock order, balance decision, orchestration
repository -> SQL only, no business rules
domain     -> Transfer entity, state machine, validation, sentinel errors
```
`handler` depends on a `Service` interface. The service runs all work through `Store.WithTx`, which commits on success and rolls back on any error.

## Database schema design

| Table | Purpose | Key constraints |
|---|---|---|
| `wallets` | current balance | PK `id`; `CHECK (balance >= 0)` |
| `transfers` | one row per transfer, with status | `CHECK (amount > 0)`, `CHECK (from <> to)`, `CHECK (status IN (...))` |
| `idempotency_records` | key to transfer mapping | **PK on `idempotency_key`**; stores `request_hash` |
| `ledger_entries` | append-only double entry | `UNIQUE (transfer_id, entry_type)` |

Decisions:
- **Stored balance plus ledger.** Reads are O(1) and a wallet row lock gives a natural serialization point. The ledger is the audit trail, and tests verify `balance = opening_balance + credits - debits`.
- **Money is `BIGINT` in minor units.** No floats.
- **The DB is the last line of defence.** Even a code bug cannot create a negative balance or a second DEBIT for a transfer.
- **`idempotency_records.transfer_id` is a `DEFERRABLE INITIALLY DEFERRED` FK.** The key is claimed first and the transfer inserted second, inside one transaction; the FK is checked at commit.
- **Indexes:** `transfers(from_wallet_id, created_at DESC)`, `transfers(to_wallet_id, created_at DESC)`, `ledger_entries(wallet_id, id)` to support history and balance lookups.

## Idempotency strategy
- **Storage:** `idempotency_records` with the key as PRIMARY KEY, a SHA-256 `request_hash` of `(from, to, amount)`, and the `transfer_id`.
- **Detection:** `INSERT ... ON CONFLICT (idempotency_key) DO NOTHING` as the first statement of the transaction. The unique constraint decides the winner, not an "if exists" check in code (which would race).
  - 1 row inserted: this is the first request, so continue.
  - 0 rows inserted: duplicate. Load the stored record and return the original transfer (`200`, `idempotentReplay: true`) with no side effects.
- **Concurrent duplicates:** a second transaction inserting the same key blocks until the first commits or rolls back, then sees the committed result. Test: 10 parallel requests with one key produce exactly 1 execution.
- **Same key, different payload:** hash mismatch returns `422`. It is a client bug and we do not return the wrong transfer.
- **Atomicity:** the key, transfer, balance changes, ledger entries and status all live in one transaction. If anything fails (including unknown wallet), everything rolls back and the key is not burned, so a retry with the same key works.
- **Insufficient funds** is a business outcome, not an error: the transfer is stored as `FAILED` with `INSUFFICIENT_FUNDS` and no ledger entries. Replaying the same key returns the same FAILED result. To retry after funding, the client uses a new key.

## Concurrency handling
- **Pessimistic row locks:** `SELECT balance FROM wallets WHERE id=$1 FOR UPDATE` on both wallets. The balance check happens after the lock, so the value cannot change between check and spend.
- **Deadlock avoidance:** wallets are always locked in sorted ID order, so A->B and B->A running together cannot deadlock.
- **Isolation level:** `READ COMMITTED` is sufficient because we lock the rows we read. No serialization retries are needed.
- **Guarded state transition:** `UPDATE transfers SET status=... WHERE id=? AND status='PENDING'`. If 0 rows change, the call fails instead of silently overwriting. The domain layer also rejects illegal transitions.
- **No in-process mutexes** protect money. Correctness comes from the database, so it also holds with multiple server instances. (Goroutines appear only in `main` for the HTTP server, implicitly in `net/http` per request, and in the tests.)
- **Why not optimistic locking:** hot wallets would cause retry storms, and pessimistic locking is simpler and safe for money.

## Testing
Integration tests run against real PostgreSQL. Each test creates its own schema (`t_<uuid>`) and drops it afterwards, so tests are isolated.

- **Domain:** validation rules, allowed and forbidden state transitions.
- **Service:**
  - successful transfer: balances and 2 ledger entries
  - insufficient funds: FAILED, no ledger entries
  - unknown wallet: full rollback, key not burned, retry succeeds
  - invalid input rejected before touching the DB
  - duplicate key returns original, charged once
  - duplicate after FAILED returns the same FAILED result
  - same key with different payload is rejected
- **Concurrency:**
  - 20 parallel transfers of 10 from a wallet holding 100: exactly 10 succeed and 10 fail, with no overdraft
  - 10 parallel requests with the same key: exactly 1 executes
  - A->B and B->A in parallel: no deadlock, and total money is unchanged
- **Handler:** HTTP status mapping (201, 200, 400, 404, 422) and the replay flow.
- **Invariants checked after most tests** (`AssertLedgerConsistent`): total debits = total credits; PROCESSED transfers have exactly 2 entries and FAILED have 0; stored balance equals opening plus credits minus debits for every wallet.

Run:
```bash
docker compose up -d
go mod tidy
go test ./... -count=1 -v
go run ./cmd/server
```

## API
| Endpoint | Success | Errors |
|---|---|---|
| `POST /transfers` | `201` new, `200` replay | `400` invalid, `404` wallet not found, `422` key reused with different payload |
| `GET /wallets/{id}/balance` | `200` | `404` |
| `POST /wallets` (demo helper) | `201` | `400`, `409` |

## Assumptions and trade-offs
- **No visible PENDING phase:** the whole transfer is one transaction, so a transfer goes straight to PROCESSED or FAILED. A production system with async processing would commit PENDING first and process it with a worker (outbox pattern). I chose atomic execution for clarity and correctness.
- **Opening balances** are set directly via the demo endpoint and stored in `opening_balance` so the ledger invariant stays checkable. Real systems would fund wallets via a deposit transfer from a system wallet.
- **No TTL** on idempotency keys. Production would expire them (e.g. after 24h).
- **No authentication, rate limiting or metrics.** Out of scope.
- **Single currency** and positive integer amounts in minor units.
- **Transfer IDs are UUID strings** stored as TEXT to keep the Go code simple.
- **FAILED on insufficient funds is stored and replayed**, matching how payment APIs treat a key as one logical attempt.

## AI usage
1. **Tool:** Claude (claude.ai chat).
2. **How I generally use it:** <write honestly, e.g. "to discuss design options, generate first drafts of boilerplate, and explain unfamiliar concepts; I review and run everything myself">.
3. **Transcript:** full session in `docs/ai-transcript.md`.

How I used it for this task:
- Discussed the design (stored balance vs derived, locking strategy, idempotency approach) and chose the approach above.
- Generated the first version of the code and tests, then ran the tests locally and read through the code.
- <add anything you changed or fixed yourself, e.g. port change, bugs found, edits you made>

I can explain every part of this PR.

## Checklist
- [ ] `go test ./... -count=1` passes locally
- [ ] Verified manually with Postman (transfer, replay, 422, insufficient funds)
- [ ] Checked data in pgAdmin (2 ledger rows per processed transfer, debits = credits)
- [ ] AI transcript added to the repo