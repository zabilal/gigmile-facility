# Facility — payment application service

A REST API that receives payment notifications from a bank and applies them to an
asset-financing customer's outstanding balance.

Go 1.26 · PostgreSQL 17 · no framework

---

## The approach, briefly

The brief describes what looks like a counter to decrement. It is really a **loan
ledger**, and three decisions follow from that:

**1. The ledger is the truth; the balance is a projection of it.**
`ledger_entries` is append-only — enforced by a trigger, not a comment — and
`loan_accounts.total_paid_kobo` is derived from it. `UPDATE ... SET balance = balance - x`
would destroy the audit trail and make disputes, reversals and reconciliation
unanswerable. Corrections are posted as compensating entries; history is never
rewritten. A `ReconcileAccount` check asserts the two agree, and every test that
moves money runs it.

**2. Idempotency is a correctness requirement, not an optimisation.**
`transaction_reference` carries a `UNIQUE` index, and duplicate detection is the
database refusing a second insert — never an application-level "does this exist?"
check, which races between the read and the write. A retry returns `200` with the
*original* outcome, so a well-behaved provider stops retrying; `409` would make it
retry forever. A reference replayed with a *different* amount is not a duplicate —
it is a provider defect or someone probing whether a bigger number clears a debt —
so it is refused, alerted, and never applied.

**3. Money never moves in two steps.**
The payment record, the ledger entry and the balance movement commit in one
transaction. Resolution and update fuse into a single statement keyed on
`customer_id AND status = 'ACTIVE'`, so there is no read-modify-write to lose
updates under concurrency. `LEAST`/`GREATEST` split a payment across the
obligation and the credit bucket inline, which is what keeps the
`total_paid <= total_payable` constraint satisfiable without a pre-read.

**On the 100,000/minute constraint.** That is 1,667 writes/second — the number
that tempts people into Kafka before doing the arithmetic. With one virtual
account per customer and one active deployment each, the write set is a single row
per payment and contention is effectively zero. So the service applies payments
**synchronously**, which also gives read-your-writes — literally what "instantly
updating the current position" asks for. The measured result is below.

The async path stays a config flag (`APPLY_MODE`) behind the same interface,
because it buys three things worth having if the figure turns out to be sustained
rather than a peak: availability decoupled from the database, per-customer
batching, and replay through a dead-letter queue. `internal/domain` imports
nothing from the database, so that switch is a wiring change rather than a
rewrite.

**Nothing is dropped.** A payment for an unknown customer, or one whose deployment
has completed, is recorded and routed to suspense with a `202` — not rejected. The
money arrived; our inability to map it is our problem to reconcile, not the bank's
problem to retry.

---

## Running it

Needs Docker and Go. Nothing else.

```bash
make up                  # Postgres + migrations
make seed                # 100,000 deployments (~1.3s)
make run                 # API on :8080

make test                # everything (needs Docker)
make test-unit           # domain only, no Docker
make load                # 100k req/min for 60s
```

```bash
./scripts/pay.sh                          # sample payload
./scripts/pay.sh GIG000042 25000          # customer, naira
./scripts/pay.sh GIG000042 25000 SAME-REF # repeat to see idempotency
```

The webhook is always signature-verified — there is no development bypass, because
a bypass flag is exactly the thing that survives into production on an endpoint
that can clear a debt. `scripts/pay.sh` signs the way the provider would.

---

## Measured throughput

The brief's constraint is the headline claim, so it is measured rather than
asserted. `cmd/loadgen` drives the real endpoint with real signatures against a
seeded book of 100,000 customers.

**60 seconds at the required rate:**

| | |
|---|---|
| Sustained | **99,993 req/min** (1,667/sec), target 100,000 |
| Requests | 100,000 |
| Errors | **0** |
| Success | 100% (99,991 × `200`, 9 × `202` suspense) |
| p50 | 3.6 ms |
| p90 | 6.6 ms |
| p95 | 17.2 ms |
| p99 | 100 ms |
| max | 294 ms |

**Headroom — where it actually breaks:**

| Offered | Achieved | Errors | p50 | Verdict |
|---|---|---|---|---|
| 100k/min | 99,993 | 0 | 3.6 ms | required rate, comfortable |
| 150k/min | 149,945 | 0 | 4.4 ms | still clean |
| 200k/min | 194,847 | 0 | 64 ms | past the knee |
| 250k/min | 221,416 | 0 | 66 ms | saturated |

So the requirement sits at roughly **45% of measured capacity** on a single
laptop with the database, the API and the load generator all competing for the
same 10 cores.

**Consistency after 250,000 applied payments:**

```
accounts checked   99,046      balance drift from ledger      0
over-paid                0      negative balances              0
duplicate references     0      payments recorded        250,000
```

Latency is measured from each request's *intended* send time, not from when a
worker got to it — measuring from the actual send hides coordinated omission, so a
backlogged system reports fast service times while real callers wait. Both figures
are reported.

<details>
<summary>Environment and what the numbers do not prove</summary>

Apple M5, 10 cores, 16 GB. Postgres 17 in Docker, `synchronous_commit=on`.
Everything co-resident on one machine.

Profiling during the run showed the Docker VM layer consuming **200–242% CPU
against Postgres's own 113%** — the virtualisation boundary costs roughly twice
what the database does. Two hypotheses were tested and rejected before landing
there: disabling `synchronous_commit` moved p50 only 5.9 ms → 4.2 ms, and a single
query round trip measured 85 µs, so neither fsync nor round-trip count explains a
4 ms floor. On native Linux with a real disk these figures should improve
materially, but this repo does not prove that.

The `p99` of 100 ms against a `p50` of 3.6 ms is checkpoint-driven tail latency,
not a queueing collapse — throughput and error rate stay flat through it.

</details>

---

## API

| | |
|---|---|
| `POST /v1/payments` | The webhook. HMAC-signed. |
| `GET /v1/customers/{id}/position` | Outstanding, arrears, weeks behind, next instalment |
| `GET /v1/customers/{id}/ledger` | Statement, keyset-paginated |
| `GET /healthz` `GET /readyz` `GET /metrics` | Liveness, readiness, Prometheus |

`/healthz` deliberately does not touch the database: a database outage should
drain an instance from the load balancer, not have the orchestrator restart every
replica in a loop.

**Amounts are returned in both representations.** Kobo is authoritative and is
what a client should compute with; naira is for humans. Returning only a formatted
string invites clients to parse it back into a float, which is how money becomes
wrong.

```jsonc
// POST /v1/payments  → 200
{
  "outcome": "applied",              // applied | duplicate | ignored | suspense
  "transaction_reference": "VPAY25110713542114478761522000",
  "payment_id": 1,
  "applied_amount": { "kobo": 2000000, "naira": "20000.00" },
  "excess_amount":  { "kobo": 0,       "naira": "0.00" },
  "position": {
    "status": "ACTIVE",
    "outstanding":     { "kobo": 98000000, "naira": "980000.00" },
    "arrears":         { "kobo": 0,        "naira": "0.00" },
    "ahead_by":        { "kobo": 0,        "naira": "0.00" },
    "weeks_elapsed": 1, "instalments_met": 1, "next_due_week": 2, "weeks_behind": 0
  }
}
```

The position rides along on the write, so the caller needs no second round trip.

| Status | When |
|---|---|
| `200` | Applied, duplicate, or recorded-but-not-actionable |
| `202` | Recorded, routed to suspense — needs ops, not a retry |
| `400` | Malformed payload; the body names the offending `field` |
| `401` | Missing, stale or invalid signature |
| `409` | Reference already recorded with a **different** amount |
| `413` | Body over 16 KB |
| `503` | Not durably recorded — the provider **must** retry (`Retry-After`) |

`503` rather than `500` on a store failure is deliberate: we did not record the
payment, so the provider has to retry, and some providers give up on a `500`.

### Signing

`X-Signature: hex(HMAC-SHA256(secret, timestamp + "." + body))` with
`X-Timestamp` in Unix seconds, valid for 5 minutes. The timestamp is bound into
the signed material so a captured request cannot be replayed with a fresh header,
and comparison uses `hmac.Equal` — a short-circuiting compare leaks how much of a
guessed signature was right, one byte at a time.

---

## Layout

```
cmd/api        cmd/migrate      cmd/seed        cmd/loadgen
internal/domain          pure business rules — no database imports
internal/store/postgres  statements, transactions, idempotency
internal/transport/http  routing, validation, signing, metrics
migrations/              embedded in the binary; no goose CLI needed
```

`internal/domain` imports nothing from the database, so the FIFO arithmetic and
the position calculation are testable in microseconds and the sync/async choice is
a wiring change rather than a rewrite.

## Tests

```
domain      86 tests   95.3% statement coverage, 1.6M fuzz executions clean
store       25 tests   integration, against real Postgres
transport   23 tests   over a live HTTP server
```

Integration tests run against a real database rather than a mock. A mock would
verify that we call the functions we wrote, which was never in doubt; what is in
doubt is whether the unique index, the CHECK constraints, the row locking and the
transaction boundaries behave as designed **under concurrency**. Only the database
can answer that. Tests use per-test customer identifiers, so they share a database
without sharing state and run in parallel without truncation.

The ones that carry their weight:

- **`TestIdempotencyConcurrent`** — 60 simultaneous deliveries of one reference;
  asserts exactly one application. A `SELECT`-then-`INSERT` implementation passes
  every sequential test and fails this one.
- **`TestConcurrentDistinctPaymentsDoNotLoseUpdates`** — 50 concurrent payments
  against one account; asserts the balance is exact. This is the lost-update test.
- **`TestSQLAllocationMatchesDomain`** — the allocation rule exists twice, in
  tested Go and in fast SQL. Keeping both is a deliberate trade, valid only if they
  cannot silently drift. This enforces that.
- **`TestAllocateOrderIndependence`** — 500 randomised trials asserting the final
  balance does not depend on arrival order, since notifications are not guaranteed
  to arrive in settlement order.
- **`TestLedgerIncludesEntriesWithoutAPayment`** — regression: an inner join
  dropped opening balances from statements, so they silently stopped summing to
  the balance they explain.

## Assumptions

Two ambiguities were raised with the business and answered. Both **narrowed** the
design — the answers deleted work rather than adding it.

1. **One active deployment per customer** — so `customer_id` is a sufficient
   routing key, and the rule is a partial unique index rather than application
   discipline.
2. **₦1m is the amount repayable, interest-inclusive** — so no accrual engine;
   `weekly_due` is ₦20,000, frozen at deployment.

Assumed, and stated because a wrong guess is consequential:

- `"transaction_amount": "10000"` is **naira**, stored as `int64` kobo. Never a float.
- `transaction_date` carries no zone; read as **WAT** (UTC+1, no DST). A wrong
  guess shifts a payment across a week boundary and mislabels a customer as
  delinquent. Ordering uses our `received_at`; the provider's timestamp is
  informational, because provider clocks drift.
- Instalment *n* falls due at `start_date + n×7 days`. Rounding remainders go to
  the **final** instalment, so instalments sum exactly to the obligation — a floor
  would forgive debt, a ceiling would overcharge.

**Customer and deployment origination are out of this service's bounded context.**
In production they arrive by API or event from onboarding; a seeder stands in for
them here, sized to make the load test's contention profile realistic. Ten seeded
accounts under 100k payments/minute would benchmark row-lock contention rather
than the system.

## Deliberately not built

- **Async apply mode** — the interface and config flag exist; the worker pool does not.
- **Reversal workflow** — `REVERSED` routes to suspense rather than guessing which
  entry to compensate from a single reference. The ledger supports compensating
  entries; the operator flow does not exist.
- **Anomaly table** — reference-replayed-with-different-amount is logged, counted
  and alerted, but not persisted as a row.
- **Credit carry-over** — when an overpayment settles an account, whether the
  residual applies to the customer's next deployment or is refunded is a
  collections policy question, not an engineering one. Until it is answered the
  money sits in `overpayment_kobo` and is surfaced to ops. It is never absorbed
  and never discarded.
- **Table partitioning** — and *not* for the obvious reason. Postgres requires a
  unique index on a partitioned table to include every partition key column, so
  `PARTITION BY RANGE (received_at)` would only give uniqueness *per partition* —
  a retry landing in tomorrow's partition would be applied twice. That trades the
  system's most important correctness property for an operational convenience. The
  two correct options at that scale are documented in
  [`migrations/00001_init.sql`](migrations/00001_init.sql).
