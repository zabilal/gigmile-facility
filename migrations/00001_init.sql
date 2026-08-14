-- +goose Up

-- ---------------------------------------------------------------------------
-- loan_accounts: the materialised position. One row per asset deployment.
--
-- customer_id is a bare identifier, not a foreign key. Customer identity is the
-- onboarding service's domain; nothing in the payment path reads a customer
-- record, so a local customers table would only add a join no query needs.
-- ---------------------------------------------------------------------------
CREATE TABLE loan_accounts (
    id                 UUID        PRIMARY KEY,
    customer_id        TEXT        NOT NULL,
    asset_value_kobo   BIGINT      NOT NULL CHECK (asset_value_kobo > 0),
    total_payable_kobo BIGINT      NOT NULL CHECK (total_payable_kobo > 0),
    -- Frozen at deployment. Deriving it on read would let a later pricing
    -- change retroactively rewrite an existing customer's schedule.
    weekly_due_kobo    BIGINT      NOT NULL CHECK (weekly_due_kobo > 0),
    total_paid_kobo    BIGINT      NOT NULL DEFAULT 0 CHECK (total_paid_kobo >= 0),
    overpayment_kobo   BIGINT      NOT NULL DEFAULT 0 CHECK (overpayment_kobo >= 0),
    term_weeks         INT         NOT NULL CHECK (term_weeks > 0),
    start_date         DATE        NOT NULL,
    status             TEXT        NOT NULL CHECK (status IN ('ACTIVE', 'COMPLETED', 'WRITTEN_OFF')),
    version            BIGINT      NOT NULL DEFAULT 0,
    created_at         TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at         TIMESTAMPTZ NOT NULL DEFAULT now(),

    -- The invariant that makes the payment path's single-statement apply safe.
    CONSTRAINT paid_within_obligation CHECK (total_paid_kobo <= total_payable_kobo)
);

-- "A customer has exactly one active deployment" as a database guarantee rather
-- than an application convention, so it holds against every writer: the API, the
-- seeder, a backfill, an ops SQL session. Partial, so a customer's completed
-- history never blocks their next deployment.
CREATE UNIQUE INDEX one_active_deployment_per_customer
    ON loan_accounts (customer_id)
    WHERE status = 'ACTIVE';

-- Serves the position/statement reads, which look up by customer across history.
CREATE INDEX loan_accounts_customer_idx ON loan_accounts (customer_id);


-- ---------------------------------------------------------------------------
-- payments: append-only record of every payload we ever received, applied or
-- not. `raw` is retained verbatim so any future incident is replayable.
--
-- NOT PARTITIONED, deliberately. The obvious move is RANGE (received_at) for
-- cheap retention, but Postgres requires a unique index on a partitioned table
-- to include every partition key column -- so the best available constraint is
-- UNIQUE (transaction_reference, received_at), which is only unique *per
-- partition*. A provider retry landing in tomorrow's partition would then be
-- accepted as new and applied twice. That trades the system's single most
-- important correctness property for an operational convenience.
--
-- At a volume that genuinely needs partitioning, the two correct options are:
--   1. PARTITION BY HASH (transaction_reference) -- keeps global uniqueness and
--      spreads the hot random-insert index across partitions, but gives up
--      time-based retention (DELETE in batches instead of DROP PARTITION).
--   2. A narrow, unpartitioned dedup table holding references only, alongside a
--      time-partitioned archive for the payload bodies.
-- Both are a migration away; neither is worth the complexity at 1,667/s.
-- ---------------------------------------------------------------------------
CREATE TABLE payments (
    id                    BIGSERIAL   PRIMARY KEY,
    transaction_reference TEXT        NOT NULL,
    customer_id           TEXT        NOT NULL,
    amount_kobo           BIGINT      NOT NULL CHECK (amount_kobo > 0),
    payment_status        TEXT        NOT NULL,
    transaction_at        TIMESTAMPTZ NOT NULL,
    received_at           TIMESTAMPTZ NOT NULL DEFAULT now(),
    -- PENDING  -- durably recorded, not yet applied (async mode only)
    -- APPLIED  -- moved money; a ledger entry exists
    -- IGNORED  -- valid but non-actionable, e.g. payment_status != COMPLETE
    -- SUSPENSE -- nothing to apply it against, or unsafe to guess; needs ops
    --
    -- There is deliberately no QUARANTINED state. A reference replayed with a
    -- different amount cannot produce a row at all -- the unique index rejects
    -- the insert, which is the entire point of it -- so the anomaly is reported
    -- through logs, an alertable counter, and a 409 to the caller rather than
    -- through a row that can never exist.
    state                 TEXT        NOT NULL CHECK (state IN ('PENDING', 'APPLIED', 'IGNORED', 'SUSPENSE')),
    account_id            UUID        REFERENCES loan_accounts (id),
    outcome_reason        TEXT,
    raw                   JSONB       NOT NULL
);

-- The idempotency authority. Not an application-level "does it exist?" check,
-- which races; the database refuses the second insert.
CREATE UNIQUE INDEX payments_reference_key ON payments (transaction_reference);

-- Statement/history reads, newest first.
CREATE INDEX payments_customer_idx ON payments (customer_id, received_at DESC);

-- Async applier claim path. Partial, so it stays tiny: it indexes only the
-- backlog, not the entire applied history.
CREATE INDEX payments_pending_idx ON payments (id) WHERE state = 'PENDING';

-- Ops triage queue. Partial, so it indexes only what needs a human.
CREATE INDEX payments_suspense_idx ON payments (received_at DESC)
    WHERE state = 'SUSPENSE';


-- ---------------------------------------------------------------------------
-- ledger_entries: the immutable truth. Balances are a projection of this table,
-- never the other way around.
-- ---------------------------------------------------------------------------
CREATE TABLE ledger_entries (
    id                 BIGSERIAL   PRIMARY KEY,
    account_id         UUID        NOT NULL REFERENCES loan_accounts (id),
    -- Nullable: a REPAYMENT always originates in a payment, but an ADJUSTMENT or
    -- WRITE_OFF does not. Opening balances carried in from a prior system are
    -- the common case -- they must sit in the ledger like everything else, or
    -- the balance stops being a projection of it.
    payment_id         BIGINT      REFERENCES payments (id),
    entry_type         TEXT        NOT NULL CHECK (entry_type IN ('REPAYMENT', 'REVERSAL', 'ADJUSTMENT', 'WRITE_OFF')),
    -- Signed: REPAYMENT is positive, REVERSAL negative. No CHECK on sign.
    amount_kobo        BIGINT      NOT NULL CHECK (amount_kobo <> 0),
    -- Running balance at the moment this entry was posted. Makes statements and
    -- point-in-time audits a single indexed read instead of a fold.
    balance_after_kobo BIGINT      NOT NULL CHECK (balance_after_kobo >= 0),
    created_at         TIMESTAMPTZ NOT NULL DEFAULT now(),

    CONSTRAINT repayment_originates_in_a_payment
        CHECK (entry_type <> 'REPAYMENT' OR payment_id IS NOT NULL)
);

-- A payment produces at most one entry of a given type, forever. Belt to the
-- payments unique index's braces: even if the dedup check were bypassed, the
-- ledger itself refuses to double-post. NULL payment_ids compare as distinct,
-- so non-payment adjustments are unaffected.
CREATE UNIQUE INDEX ledger_entries_payment_key ON ledger_entries (payment_id, entry_type);

-- Keyset pagination for statements: WHERE account_id = $1 AND id < $2 ORDER BY id DESC.
CREATE INDEX ledger_entries_account_idx ON ledger_entries (account_id, id DESC);


-- "Append-only" enforced rather than merely documented. Corrections are posted
-- as compensating REVERSAL entries; history is never rewritten.
-- +goose StatementBegin
CREATE FUNCTION ledger_entries_reject_mutation() RETURNS trigger AS $$
BEGIN
    RAISE EXCEPTION 'ledger_entries is append-only: % rejected. Post a compensating entry instead.', TG_OP
        USING ERRCODE = 'restrict_violation';
END;
$$ LANGUAGE plpgsql;
-- +goose StatementEnd

CREATE TRIGGER ledger_entries_immutable
    BEFORE UPDATE OR DELETE ON ledger_entries
    FOR EACH ROW EXECUTE FUNCTION ledger_entries_reject_mutation();


-- +goose Down
DROP TRIGGER IF EXISTS ledger_entries_immutable ON ledger_entries;
DROP FUNCTION IF EXISTS ledger_entries_reject_mutation();
DROP TABLE IF EXISTS ledger_entries;
DROP TABLE IF EXISTS payments;
DROP TABLE IF EXISTS loan_accounts;
