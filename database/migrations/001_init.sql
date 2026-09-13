-- Billing and payment schema.
--
-- Every amount is INTEGER in satang (1/100 baht). The database enforces the
-- rules the application also checks, so a bug or a stray UPDATE cannot leave
-- the ledger in a state the business considers impossible.
--
-- Dates and timestamps are TEXT in ISO-8601. SQLite has no date type, and ISO
-- strings sort chronologically, which is what the allocation ORDER BY needs.

-- --------------------------------------------------------------------------
-- invoices
-- --------------------------------------------------------------------------
CREATE TABLE invoices (
    id             INTEGER PRIMARY KEY AUTOINCREMENT,
    invoice_number TEXT    NOT NULL,
    unit           TEXT    NOT NULL,
    due_date       TEXT    NOT NULL,

    total_amount   INTEGER NOT NULL,
    paid_amount    INTEGER NOT NULL DEFAULT 0,

    -- Derived by the database, so a row whose status disagrees with its
    -- amounts cannot be written. STORED means it can also be indexed.
    status         TEXT GENERATED ALWAYS AS (
                       CASE
                           WHEN paid_amount <= 0            THEN 'UNPAID'
                           WHEN paid_amount >= total_amount THEN 'PAID'
                           ELSE 'PARTIAL'
                       END
                   ) STORED,

    created_at     TEXT    NOT NULL DEFAULT (datetime('now')),
    updated_at     TEXT    NOT NULL DEFAULT (datetime('now')),

    CONSTRAINT invoices_invoice_number_key   UNIQUE (invoice_number),
    CONSTRAINT invoices_invoice_number_check CHECK (length(trim(invoice_number)) > 0),
    CONSTRAINT invoices_unit_check           CHECK (length(trim(unit)) > 0),
    CONSTRAINT invoices_due_date_check       CHECK (due_date GLOB '[0-9][0-9][0-9][0-9]-[0-9][0-9]-[0-9][0-9]'),
    CONSTRAINT invoices_total_amount_check   CHECK (total_amount > 0),
    CONSTRAINT invoices_paid_amount_check    CHECK (paid_amount >= 0),
    -- Overpayment is rejected by the API, so this invariant must always hold.
    CONSTRAINT invoices_not_overpaid         CHECK (paid_amount <= total_amount)
);

-- Keeps updated_at honest without the application having to remember.
CREATE TRIGGER invoices_set_updated_at
AFTER UPDATE ON invoices
FOR EACH ROW
BEGIN
    UPDATE invoices SET updated_at = datetime('now') WHERE id = OLD.id;
END;

-- The allocation query reads one unit's unsettled invoices in settlement
-- order. A partial index covers only the rows that query can return, so it
-- stays small no matter how many settled invoices accumulate behind it.
CREATE INDEX invoices_outstanding_idx
    ON invoices (unit, due_date, invoice_number)
    WHERE paid_amount < total_amount;


-- --------------------------------------------------------------------------
-- invoice_items
-- --------------------------------------------------------------------------
CREATE TABLE invoice_items (
    id          INTEGER PRIMARY KEY AUTOINCREMENT,
    invoice_id  INTEGER NOT NULL REFERENCES invoices (id) ON DELETE CASCADE,
    description TEXT    NOT NULL,
    amount      INTEGER NOT NULL,

    CONSTRAINT invoice_items_description_check CHECK (length(trim(description)) > 0),
    CONSTRAINT invoice_items_amount_check      CHECK (amount > 0)
);

CREATE INDEX invoice_items_invoice_id_idx ON invoice_items (invoice_id);


-- --------------------------------------------------------------------------
-- payments
-- --------------------------------------------------------------------------
CREATE TABLE payments (
    id                INTEGER PRIMARY KEY AUTOINCREMENT,
    -- The caller's own reference. Unique, which is what makes posting a
    -- payment safe to retry.
    payment_reference TEXT    NOT NULL,
    unit              TEXT    NOT NULL,
    amount            INTEGER NOT NULL,
    created_at        TEXT    NOT NULL DEFAULT (datetime('now')),

    CONSTRAINT payments_payment_reference_key   UNIQUE (payment_reference),
    CONSTRAINT payments_payment_reference_check CHECK (length(trim(payment_reference)) > 0),
    CONSTRAINT payments_unit_check              CHECK (length(trim(unit)) > 0),
    CONSTRAINT payments_amount_check            CHECK (amount > 0)
);

CREATE INDEX payments_unit_created_at_idx ON payments (unit, created_at DESC);


-- --------------------------------------------------------------------------
-- payment_allocations
-- --------------------------------------------------------------------------
CREATE TABLE payment_allocations (
    id               INTEGER PRIMARY KEY AUTOINCREMENT,
    payment_id       INTEGER NOT NULL REFERENCES payments (id) ON DELETE CASCADE,
    -- No cascade here on purpose: an invoice that has been paid against must
    -- not be deletable while the money is still recorded.
    invoice_id       INTEGER NOT NULL REFERENCES invoices (id),
    allocated_amount INTEGER NOT NULL,

    CONSTRAINT payment_allocations_amount_check CHECK (allocated_amount > 0),
    -- One payment touches a given invoice at most once.
    CONSTRAINT payment_allocations_unique UNIQUE (payment_id, invoice_id)
);

CREATE INDEX payment_allocations_invoice_id_idx ON payment_allocations (invoice_id);
