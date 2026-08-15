-- +goose Up

-- loan_accounts: the materialised position, one row per deployment. customer_id
-- is a bare identifier: customer identity belongs to the onboarding service.
CREATE TABLE loan_accounts (
    id                 UUID        PRIMARY KEY,
    customer_id        TEXT        NOT NULL,
    asset_value_kobo   BIGINT      NOT NULL CHECK (asset_value_kobo > 0),
    total_payable_kobo BIGINT      NOT NULL CHECK (total_payable_kobo > 0),
    -- Frozen at deployment, so a later pricing change cannot retroactively
    -- rewrite an existing customer's schedule.
    weekly_due_kobo    BIGINT      NOT NULL CHECK (weekly_due_kobo > 0),
    total_paid_kobo    BIGINT      NOT NULL DEFAULT 0 CHECK (total_paid_kobo >= 0),
    overpayment_kobo   BIGINT      NOT NULL DEFAULT 0 CHECK (overpayment_kobo >= 0),
    term_weeks         INT         NOT NULL CHECK (term_weeks > 0),
    start_date         DATE        NOT NULL,
    status             TEXT        NOT NULL CHECK (status IN ('ACTIVE', 'COMPLETED', 'WRITTEN_OFF')),
    version            BIGINT      NOT NULL DEFAULT 0,
    created_at         TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at         TIMESTAMPTZ NOT NULL DEFAULT now(),

    CONSTRAINT paid_within_obligation CHECK (total_paid_kobo <= total_payable_kobo)
);

-- "One active deployment per customer" as a database guarantee, so it holds
-- against every writer. Partial, so completed history never blocks redeployment.
CREATE UNIQUE INDEX one_active_deployment_per_customer
    ON loan_accounts (customer_id)
    WHERE status = 'ACTIVE';

CREATE INDEX loan_accounts_customer_idx ON loan_accounts (customer_id);


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
CREATE TABLE payments (
    id                    BIGSERIAL   PRIMARY KEY,
    transaction_reference TEXT        NOT NULL,
    customer_id           TEXT        NOT NULL,
    amount_kobo           BIGINT      NOT NULL CHECK (amount_kobo > 0),
    payment_status        TEXT        NOT NULL,
    transaction_at        TIMESTAMPTZ NOT NULL,
    received_at           TIMESTAMPTZ NOT NULL DEFAULT now(),
    -- PENDING (async only) | APPLIED | IGNORED (non-actionable) | SUSPENSE.
    -- No QUARANTINED: a mismatched replay cannot produce a row at all.
    state                 TEXT        NOT NULL CHECK (state IN ('PENDING', 'APPLIED', 'IGNORED', 'SUSPENSE')),
    account_id            UUID        REFERENCES loan_accounts (id),
    outcome_reason        TEXT,
    raw                   JSONB       NOT NULL
);

-- The idempotency authority: the database refuses the second insert, rather than
-- an application check that races between the read and the write.
CREATE UNIQUE INDEX payments_reference_key ON payments (transaction_reference);

CREATE INDEX payments_customer_idx ON payments (customer_id, received_at DESC);

-- Partial, so each indexes only the backlog or the triage queue rather than all
-- of history.
CREATE INDEX payments_pending_idx ON payments (id) WHERE state = 'PENDING';
CREATE INDEX payments_suspense_idx ON payments (received_at DESC) WHERE state = 'SUSPENSE';


-- ledger_entries: the immutable truth. Balances are a projection of this table,
-- never the other way around.
CREATE TABLE ledger_entries (
    id                 BIGSERIAL   PRIMARY KEY,
    account_id         UUID        NOT NULL REFERENCES loan_accounts (id),
    -- Nullable: ADJUSTMENT and WRITE_OFF have no originating payment, and
    -- opening balances must still sit in the ledger to keep it derivable.
    payment_id         BIGINT      REFERENCES payments (id),
    entry_type         TEXT        NOT NULL CHECK (entry_type IN ('REPAYMENT', 'REVERSAL', 'ADJUSTMENT', 'WRITE_OFF')),
    -- Signed: REPAYMENT positive, REVERSAL negative.
    amount_kobo        BIGINT      NOT NULL CHECK (amount_kobo <> 0),
    -- Running balance at the time of posting, making statements and
    -- point-in-time audits an indexed read instead of a fold.
    balance_after_kobo BIGINT      NOT NULL CHECK (balance_after_kobo >= 0),
    created_at         TIMESTAMPTZ NOT NULL DEFAULT now(),

    CONSTRAINT repayment_originates_in_a_payment
        CHECK (entry_type <> 'REPAYMENT' OR payment_id IS NOT NULL)
);

-- A payment produces at most one entry of a given type, forever. Belt to the
-- unique index's braces; NULL payment_ids compare as distinct.
CREATE UNIQUE INDEX ledger_entries_payment_key ON ledger_entries (payment_id, entry_type);

-- Keyset pagination: WHERE account_id = $1 AND id < $2 ORDER BY id DESC.
CREATE INDEX ledger_entries_account_idx ON ledger_entries (account_id, id DESC);


-- "Append-only" enforced rather than merely documented. Corrections are posted
-- as compensating entries; history is never rewritten.
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
