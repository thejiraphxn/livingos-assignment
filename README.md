# Billing & Payment API

A small REST API for issuing invoices against property units and allocating
payments across them, written in Go with Gin and SQLite, using hand-written SQL.

The interesting part of this problem is not the CRUD. It is the allocation rule
— which invoice gets paid first — and making sure a payment either lands
completely or not at all, even when two of them arrive at the same moment. That
is where most of the care went.

---

## Contents

- [Technology choices](#technology-choices)
- [Project structure](#project-structure)
- [Setup and run](#setup-and-run)
- [API endpoints](#api-endpoints)
- [Error responses](#error-responses)
- [Database schema](#database-schema)
- [Concurrency: two payments at once](#concurrency-two-payments-at-once)
- [Why money is an integer, not a float](#why-money-is-an-integer-not-a-float)
- [How overpayment is handled](#how-overpayment-is-handled)
- [How duplicate payments are prevented](#how-duplicate-payments-are-prevented)
- [Tests](#tests)
- [Assumptions and trade-offs](#assumptions-and-trade-offs)
- [Performance question: 10,000 to 10,000,000 invoices](#performance-question-10000-to-10000000-invoices)
- [What I would do next](#what-i-would-do-next)

---

## Technology choices

| Area | Choice | Why |
| --- | --- | --- |
| Language | **Go 1.25** | Statically typed, and the standard library covers most of what a small service needs. |
| HTTP router | **Gin** | Routing, path parameters and JSON binding with very little ceremony. |
| Database | **SQLite** | Nothing to install and nothing to start: clone the repository and run one command. It still supports everything this schema needs — generated columns, partial indexes, `CHECK` constraints, foreign keys and `RETURNING`. |
| Driver | **`modernc.org/sqlite` via `database/sql`** | A pure-Go SQLite, so there is no cgo and no C compiler needed to build or cross-compile. |
| Data access | **Hand-written SQL — no ORM** | The parts of this problem worth showing *are* the SQL: the ordering, the locking, the partial index, the `RETURNING` clauses. An ORM would hide all of it. |
| Migrations | **Embedded `.sql` files** | Real, reviewable SQL in the repository, applied at startup. No extra tool to install. |
| Money | **`int64` in satang** | Exact integer arithmetic. See [below](#why-money-is-an-integer-not-a-float). |

---

## Project structure

Layered, with each layer having one job:

```
.
├── main.go                       wiring: connect, migrate, build the router, listen
├── go.mod
├── database/
│   ├── database.go               connection pool and the migration runner
│   └── migrations/
│       └── 001_init.sql          the schema, as real SQL
├── models/
│   ├── invoice.go                Invoice, InvoiceItem
│   └── payment.go                Payment, PaymentAllocation
├── handlers/
│   ├── invoice_handler.go        POST /invoices, GET /invoices/:invoiceNumber
│   ├── payment_handler.go        POST /payments
│   └── response.go               the shared error envelope
├── services/
│   └── payment_service.go        payment allocation — the real business logic
├── routes/
│   └── routes.go                 URL to handler
└── tests/
    └── payment_service_test.go   the allocation rules, against a real database
```

**Handlers** read the request, validate it, and turn results and errors into HTTP
responses. They do not decide business outcomes.

**The service** owns payment allocation: what order invoices are settled in, what
counts as too much money, what gets locked, and what happens inside the
transaction.

Invoice creation stays in its handler. It is a validation, a sum and one insert
— wrapping that in a service would add a layer without adding anything to read.

`handlers/response.go` is the one file not in the original outline. Both handlers
return the same error envelope, and having that in one place beats copying the
struct into both.

---

## Setup and run

**Requirements:** Go 1.25+. That is all — there is no database to install.

```bash
git clone <repository-url>
cd <repository-directory>

go mod download
go run .                 # creates billing.db, migrates it, serves on :8080
```

The application creates the database file and its schema on first run, so there
is nothing to set up by hand. Delete `billing.db` to start again from nothing.

| Variable | Default | Meaning |
| --- | --- | --- |
| `DATABASE_PATH` | `billing.db` | SQLite file to use |
| `PORT` | `8080` | Port to listen on |

```bash
go test ./...        # run the tests
gofmt -l .           # anything printed here is badly formatted
go vet ./...         # catch mistakes the compiler allows
```

---

## API endpoints

| Method | Path | Purpose | Success |
| --- | --- | --- | --- |
| `POST` | `/invoices` | Create an invoice from one or more items | `201` |
| `GET` | `/invoices/:invoiceNumber` | Fetch one invoice | `200` |
| `POST` | `/payments` | Record a payment and allocate it | `201` |

**All amounts in requests and responses are integers in satang** — `150000`
means 1,500.00 baht. Dates are `YYYY-MM-DD`.

### Create an invoice

`total_amount` is not accepted from the client; it is the sum of the items.

```bash
curl -X POST localhost:8080/invoices \
  -H 'Content-Type: application/json' \
  -d '{
        "invoice_number": "INV001",
        "unit": "A101",
        "due_date": "2026-08-01",
        "items": [
          {"description": "Common Fee", "amount": 150000},
          {"description": "Water Fee",  "amount": 30000}
        ]
      }'
```

```jsonc
// 201 Created
{
  "invoice_number": "INV001",
  "unit": "A101",
  "due_date": "2026-08-01",
  "items": [
    { "description": "Common Fee", "amount": 150000 },
    { "description": "Water Fee",  "amount": 30000 }
  ],
  "total_amount": 180000,
  "paid_amount": 0,
  "outstanding_amount": 180000,
  "status": "UNPAID"
}
```

Rejected when: the body is not JSON, `invoice_number` / `unit` / `due_date` is
missing or malformed, there are no items, an item has no description, an item
amount is not greater than zero, or the invoice number already exists.

### Get an invoice

```bash
curl localhost:8080/invoices/INV001
```

Returns the same shape, with `paid_amount`, `outstanding_amount` and `status`
reflecting whatever has been paid so far. `404` if there is no such invoice
number.

### Create a payment

The payment is spread across that unit's outstanding invoices, **oldest due date
first**, and where two invoices fall due on the same day, **by invoice number
ascending**.

```bash
curl -X POST localhost:8080/payments \
  -H 'Content-Type: application/json' \
  -d '{"payment_reference": "PAY001", "unit": "A101", "amount": 200000}'
```

With `INV001` owing 180,000 (due 2026-08-01) and `INV002` owing 50,000 (due
2026-08-15):

```jsonc
// 201 Created
{
  "payment_reference": "PAY001",
  "unit": "A101",
  "amount": 200000,
  "allocations": [
    {
      "invoice_number": "INV001",
      "allocated_amount": 180000,
      "outstanding_amount": 0,
      "status": "PAID"
    },
    {
      "invoice_number": "INV002",
      "allocated_amount": 20000,
      "outstanding_amount": 30000,
      "status": "PARTIAL"
    }
  ]
}
```

Rejected when: the body is not JSON, `payment_reference` or `unit` is missing,
`amount` is not greater than zero, the reference has been used before, the unit
has no outstanding invoices, or the amount is larger than everything the unit
owes. **In every rejected case nothing is written** — no payment row, no
allocation, no change to any invoice.

---

## Error responses

Every failure uses the same envelope. The `code` is a stable string, so a client
can branch on it without reading the message.

```json
{
  "error": {
    "code": "INVALID_INPUT",
    "message": "payment amount must be greater than zero"
  }
}
```

| Code | Status | When |
| --- | --- | --- |
| `INVALID_JSON` | `400` | The body could not be parsed |
| `INVALID_INPUT` | `400` | A field is missing, malformed, or not allowed |
| `NOT_FOUND` | `404` | No invoice with that number |
| `DUPLICATE_INVOICE_NUMBER` | `409` | That invoice number already exists |
| `DUPLICATE_PAYMENT_REFERENCE` | `409` | That payment reference was already recorded |
| `NO_OUTSTANDING_INVOICE` | `422` | The unit owes nothing |
| `PAYMENT_EXCEEDS_OUTSTANDING` | `422` | The payment is larger than everything owed |
| `DATABASE_ERROR` | `500` | Something went wrong on our side |

`400` is for a request that could not be understood; `422` is for one that was
understood but asks for something the business rules refuse; `409` is a conflict
with something that already exists.

---

## Database schema

```
invoices 1───* invoice_items
    ▲
    │ (no cascade: an invoice with money against it cannot be deleted)
    *
payment_allocations *───1 payments
```

The full DDL is in [`database/migrations/001_init.sql`](database/migrations/001_init.sql).
Four decisions in it are worth pointing out.

### Status is a generated column

```sql
status TEXT GENERATED ALWAYS AS (
           CASE
               WHEN paid_amount <= 0            THEN 'UNPAID'
               WHEN paid_amount >= total_amount THEN 'PAID'
               ELSE 'PARTIAL'
           END
       ) STORED
```

The status is never written by the application — the database derives it. A row
whose status disagrees with its amounts cannot be created, by anyone, through any
code path. `STORED` also means it can be read back with `RETURNING` and indexed.

### The database enforces the money rules too

```sql
CONSTRAINT invoices_total_amount_check CHECK (total_amount > 0),
CONSTRAINT invoices_paid_amount_check  CHECK (paid_amount >= 0),
CONSTRAINT invoices_not_overpaid       CHECK (paid_amount <= total_amount)
```

The API already refuses these, but application checks only cover the paths the
application takes. A ledger should not depend on that. `payment_allocations` also
has a real foreign key to `invoices` and a `UNIQUE (payment_id, invoice_id)`, so
one payment cannot touch the same invoice twice.

### A partial index for the allocation query

```sql
CREATE INDEX invoices_outstanding_idx
    ON invoices (unit, due_date, invoice_number)
    WHERE paid_amount < total_amount;
```

The hot query is `WHERE unit = ? AND paid_amount < total_amount ORDER BY
due_date, invoice_number`. This index answers it completely: the right unit, the
right rows, already in settlement order and with no sort step.

Due dates are stored as ISO-8601 text (`2026-08-01`). SQLite has no date type,
and ISO strings sort chronologically as text, which is exactly what that
`ORDER BY` needs.

Because it is *partial*, it only contains invoices that still owe money. Settled
invoices — which is nearly all of them after a few years — are not in it at all,
so the index stays roughly constant in size no matter how large the table grows.

### `paid_amount` is stored, not summed

Reading a balance is a single column read rather than a join and an aggregate
over `payment_allocations`. The allocations are still the audit trail: they add
up to `paid_amount`, and that can be verified at any time.

---

## Concurrency: two payments at once

Two payments for the same unit arriving together must not both allocate against
the same balance — otherwise both read "180,000 outstanding" and both pay it.

SQLite has no `SELECT … FOR UPDATE`. What it has is a transaction that takes the
write lock up front, and the connection string asks for exactly that:

```go
const connectionPragmas = "_pragma=foreign_keys(1)" +
	"&_pragma=busy_timeout(5000)" +
	"&_pragma=journal_mode(WAL)" +
	"&_txlock=immediate"
```

`_txlock=immediate` starts every transaction as `BEGIN IMMEDIATE`, so the write
lock is held *before* the first read rather than being upgraded on the first
write. A second payment therefore waits (up to `busy_timeout`) and then reads
balances that already include the first. `journal_mode(WAL)` keeps readers
working while that writer holds the lock.

Verified by firing eight simultaneous payments of 80,000 at a single invoice
owing 100,000: one succeeded, the other seven were correctly refused with
`PAYMENT_EXCEEDS_OUTSTANDING` (only 20,000 remained by then), the invoice ended
at exactly 80,000 paid, and `payment_allocations` summed to the same figure. No
double-charging, no lock errors, no 500s.

On PostgreSQL or SQL Server the same guarantee is expressed per row rather than
per database — the allocation `SELECT` would carry `FOR UPDATE` and different
units would stop contending with each other. That is the single change the
concurrency story needs when this outgrows SQLite, and it is noted in the
performance section below.

---

## Why money is an integer, not a float

All amounts are `int64` in **satang** (1/100 of a baht), stored as `BIGINT`.
`150000` is 1,500.00 baht.

Floating point cannot represent most decimal fractions exactly. In Go:

```go
fmt.Println(0.1 + 0.2)        // 0.30000000000000004
fmt.Println(0.1+0.2 == 0.3)   // false
```

In a billing system those tiny errors accumulate and then surface as a balance
that is one satang off, an invoice that will not close because `paid` is
`999.9999999` against a total of `1000`, or a report that does not reconcile.
Integers have none of these problems: addition, subtraction and comparison are
exact, and `paid_amount = total_amount` means what it says — which matters here,
because the generated `status` column depends on exactly that comparison.

Satang rather than baht because it is the smallest unit money actually comes in,
so nothing ever needs rounding. `int64` is far larger than any realistic amount.

A fixed-point decimal type would also be exact, and is the usual alternative on
databases that have one. Integer satang was chosen because it is exact in Go too,
with no decimal library and no conversion at the boundary — and because SQLite
has no decimal type at all, only integers, reals and text.

The trade-off is that callers send and read satang, and a display layer divides
by 100. That is a small, obvious conversion in one place.

---

## How overpayment is handled

**A payment larger than everything the unit owes is rejected with
`422 Unprocessable Entity`, and nothing is written.**

```jsonc
{
  "error": {
    "code": "PAYMENT_EXCEEDS_OUTSTANDING",
    "message": "payment amount is larger than the total outstanding amount for this unit"
  }
}
```

The alternative would be to take what is owed and keep the rest as credit on the
unit. That is a reasonable design, and a mature system probably wants it — but
credit is a feature in its own right: it needs a balance to live on, rules about
when it gets applied, and a way to reverse it. Half-building it, by silently
swallowing the surplus into a payment record nobody can act on, would be worse
than not building it.

Rejecting is explicit. The caller learns immediately that the number is wrong,
the ledger never contains money that is not accounted for against an invoice, and
adding credit later is a clean addition rather than a correction.

The check happens **before** anything is written and inside the same transaction
that holds the row locks, so a rejected payment leaves the database exactly as it
was — and the `invoices_not_overpaid` constraint would stop it even if the check
were wrong.

---

## How duplicate payments are prevented

`payment_reference` is the **idempotency key**. It has a unique constraint, and
the service checks for it inside the transaction before recording anything:

```go
const query = `SELECT EXISTS (SELECT 1 FROM payments WHERE payment_reference = ?)`

var used bool
if err := tx.QueryRowContext(ctx, query, reference).Scan(&used); err != nil {
	return fmt.Errorf("check payment reference: %w", err)
}
if used {
	return ErrDuplicatePaymentReference
}
```

Sending the same reference twice returns `409 DUPLICATE_PAYMENT_REFERENCE` and
changes nothing — the invoice balances after the second call are identical to
what they were after the first. A retried request, a double-clicked button or a
webhook delivered twice cannot take the money twice.

The unique constraint is the real guarantee. If two identical requests arrive at
the same moment and both pass the check, the second `INSERT` fails with
`SQLITE_CONSTRAINT_UNIQUE`, the transaction rolls back, and the handler still
answers `409` — both paths end at the same result.

The everything-or-nothing part is the transaction: the payment row, every
allocation row and every `paid_amount` update are inside one `BEGIN … COMMIT`, so
a failure at any step leaves no trace of the ones before it.

---

## Tests

```bash
go test ./...
```

The tests cover payment allocation, which is where the business rules live. Each
test gets its own throwaway database file, created and migrated through the same
code path the real application uses.

They are deliberately not mocked: half of what is being tested lives in the SQL —
the allocation ordering, the constraints, the generated status column — and a
fake database would not exercise any of it. Because SQLite is a file, that costs
nothing: the whole suite runs in well under a second with no setup.

| Test | What it proves |
| --- | --- |
| `TestFullPayment` | A payment that exactly clears an invoice marks it `PAID` |
| `TestPartialPayment` | A smaller payment leaves it `PARTIAL` with the right balance |
| `TestPaymentAcrossMultipleInvoices` | One payment settles the first invoice and part-pays the next |
| `TestAllocationOrdersByDueDate` | The oldest due date is paid first, whatever order rows were inserted in |
| `TestAllocationBreaksTiesByInvoiceNumber` | Same due date falls back to invoice number |
| `TestDuplicatePaymentReferenceIsRejected` | A repeated reference is refused and balances do not move |
| `TestPaymentWithNoOutstandingInvoices` | A unit that owes nothing cannot take a payment, and none is saved |
| `TestPaymentLargerThanOutstandingIsRejected` | Too much money is refused and nothing at all is written |

The last three assert on the number of rows afterwards, not just the error — a
rejection that quietly wrote a payment row would still fail the test.

---

## Assumptions and trade-offs

**Assumptions**

1. **The client supplies `invoice_number`**, as the brief's example shows. The
   unique constraint rejects a repeat.
2. **`unit` is a plain string, not a table.** There is no unit master data in the
   requirements, so a payment for a unit with no invoices is simply "nothing
   outstanding" rather than "no such unit".
3. **Payments are made against a unit, not a named invoice.** That is what makes
   the allocation order matter.
4. **One currency**, so there is no currency column.
5. **Invoices cannot be edited or cancelled.** No endpoint for it was asked for,
   and a correction in a real billing system is usually a credit note rather than
   an edit.
6. **No authentication.** Out of scope.

**Trade-offs**

- **Hand-written SQL over an ORM.** More lines, and the queries have to be kept
  in step with the schema by hand. In exchange the locking, the index usage and
  the `RETURNING` clauses are all visible and reviewable, which is most of what
  this problem is about.
- **SQLite over PostgreSQL.** A reviewer clones the repository and runs one
  command; there is no server to install, start or configure, and the tests need
  nothing either. The cost is that the write lock is per database rather than per
  row, so payments for *different* units also queue behind each other. At this
  size that is invisible; at scale it is the first thing to change, and the
  schema and queries port almost unchanged.
- **Migrations applied at startup.** Convenient and reproducible for an exercise.
  A real deployment would run them as a separate step so two instances starting
  together cannot both try to migrate.
- **Rejecting overpayment instead of holding credit.** Simpler and explicit; the
  credit feature can come later.
- **Invoice creation lives in the handler.** It is a sum and an insert. Putting it
  behind a service would be a layer with nothing in it.
- **No pagination.** There is no list endpoint yet, so nothing returns an
  unbounded collection. That changes the moment one is added.

---

## Performance question: 10,000 to 10,000,000 invoices

Some of this is already in place; the rest is what I would change and in what
order.

**1. Keep the allocation query on its partial index — already done.**
`invoices_outstanding_idx (unit, due_date, invoice_number) WHERE paid_amount <
total_amount` answers the hot query with an index scan and no sort, and only
contains rows that still owe money. At ten million invoices the *outstanding* set
for one unit is still a handful of rows. I would confirm with `EXPLAIN QUERY
PLAN` that it is still chosen rather than assume it.

**2. Only read what is needed — mostly done.**
The allocation query selects four columns, not `SELECT *`, and filters in SQL
rather than loading a unit's history into Go. `GET /invoices/:invoiceNumber`
loads items because it must display them; nothing else does.

**3. Paginate every list, with keyset rather than offset.**
There is no list endpoint yet, but one is the obvious next feature. `LIMIT ?
OFFSET ?` makes the database walk and discard every skipped row, so deep pages
get linearly slower. `WHERE (due_date, invoice_number) > (?, ?) ORDER BY
due_date, invoice_number LIMIT ?` costs the same at any depth. I would also avoid an exact
`COUNT(*)` over millions of rows — `has_more` is what a client actually needs.

**4. Move the lock from the database to the row — the first real limit.**
`BEGIN IMMEDIATE` is correct but coarse: it serialises *every* payment, even for
unrelated units. This is the first thing that would hurt, and well before ten
million rows. Moving to PostgreSQL and adding `FOR UPDATE` to the allocation
`SELECT` makes the lock per invoice, so different units stop contending; nothing
else in the query changes. Beyond that, optimistic concurrency — `UPDATE … WHERE
paid_amount = $expected` plus a retry — trades blocking for occasional replays.
I would not reach for that pre-emptively: pessimistic locking is easier to reason
about, and correctness matters more than throughput in a ledger.

**5. Keep the hot table small.**
Most of ten million invoices are settled and never read again. The partial index
already keeps the *index* small. Beyond that: **archive** invoices settled more
than N years ago into a history table the API reads only on request, and — once
on a database that supports it — **partition** `invoices` by `due_date` range so
recent rows stay in cache.

**6. Add logging and monitoring before it is needed.**
A request log with a request id, endpoint, status and duration; counters for
payments recorded and payments rejected by reason; and, on a server database,
statement-level timing such as `pg_stat_statements`. Guessing where the time goes
costs far more than measuring it.

**7. Split reads from writes once one server is not enough.**
`GET /invoices/:invoiceNumber` is safe on a read replica; only payment posting
needs the primary. Before that, batch work — raising monthly fees for every unit
— should be one transaction inserting in batches, not thousands of API calls.

**8. Expect the single-file database to be the ceiling.**
SQLite is one file on one machine: it cannot be shared between application
instances, and there is no replica to read from. Everything above assumes the
move to a server database happens at the point where a second instance is needed
— which, for this workload, would arrive before ten million invoices do.

**The thing I would not change:** storing `paid_amount` on the invoice. Deriving
it from `SUM(payment_allocations)` on every read is the one decision that would
genuinely hurt at ten million rows, and it was avoided from the start.

---

## What I would do next

1. **HTTP-level tests** for the handlers, using `httptest`, covering the status
   codes and the error envelope. The service is well covered; the handlers were
   verified by hand.
2. **A list endpoint** — `GET /invoices?unit=A101` — with keyset pagination.
3. **Credit for overpayment**, with rules for when it gets applied and a way to
   reverse it, replacing the current rejection.
4. **Migrations as a separate command** rather than at startup, which also
   matters more once there is more than one instance.
5. **Structured logging** with a request id, replacing Gin's default line.
