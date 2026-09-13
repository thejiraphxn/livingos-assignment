package tests

import (
	"context"
	"database/sql"
	"errors"
	"path/filepath"
	"testing"
	"time"

	"billing-api/database"
	"billing-api/models"
	"billing-api/services"
)

// newTestDB gives each test its own throwaway database file, created and
// migrated through the same code path the real application uses.
//
// The database is deliberately real rather than mocked: part of what is being
// tested lives in the SQL — the allocation ordering, the constraints and the
// generated status column — and a fake would not exercise any of it.
func newTestDB(t *testing.T) *sql.DB {
	t.Helper()

	db, err := database.Connect(filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatalf("could not open the test database: %v", err)
	}
	t.Cleanup(func() { db.Close() })

	if err := database.Migrate(db); err != nil {
		t.Fatalf("could not migrate the test database: %v", err)
	}
	return db
}

// addInvoice inserts an unpaid invoice with a single item covering its total.
func addInvoice(t *testing.T, db *sql.DB, number, unit, dueDate string, total int64) {
	t.Helper()

	due, err := time.Parse("2006-01-02", dueDate)
	if err != nil {
		t.Fatalf("bad due date %q in the test setup: %v", dueDate, err)
	}

	var invoiceID int64
	if err := db.QueryRow(
		`INSERT INTO invoices (invoice_number, unit, due_date, total_amount)
		 VALUES (?, ?, ?, ?) RETURNING id`,
		number, unit, due.Format("2006-01-02"), total,
	).Scan(&invoiceID); err != nil {
		t.Fatalf("could not create invoice %s: %v", number, err)
	}

	if _, err := db.Exec(
		`INSERT INTO invoice_items (invoice_id, description, amount) VALUES (?, ?, ?)`,
		invoiceID, "Common Fee", total,
	); err != nil {
		t.Fatalf("could not create the item for invoice %s: %v", number, err)
	}
}

// assertInvoice checks what an invoice looks like after a payment. The status
// comes from the generated column, so this also proves the database and the
// application agree.
func assertInvoice(t *testing.T, db *sql.DB, number string, wantPaid, wantOutstanding int64, wantStatus string) {
	t.Helper()

	var paid, outstanding int64
	var status string
	if err := db.QueryRow(
		`SELECT paid_amount, total_amount - paid_amount, status
		 FROM invoices WHERE invoice_number = ?`, number,
	).Scan(&paid, &outstanding, &status); err != nil {
		t.Fatalf("could not read invoice %s: %v", number, err)
	}

	if paid != wantPaid {
		t.Errorf("%s: paid amount = %d, want %d", number, paid, wantPaid)
	}
	if outstanding != wantOutstanding {
		t.Errorf("%s: outstanding = %d, want %d", number, outstanding, wantOutstanding)
	}
	if status != wantStatus {
		t.Errorf("%s: status = %s, want %s", number, status, wantStatus)
	}
}

// countPayments reports how many payments exist, to prove a rejected request
// wrote nothing.
func countPayments(t *testing.T, db *sql.DB) int {
	t.Helper()

	var count int
	if err := db.QueryRow(`SELECT count(*) FROM payments`).Scan(&count); err != nil {
		t.Fatalf("could not count payments: %v", err)
	}
	return count
}

func createPayment(db *sql.DB, reference, unit string, amount int64) (*models.Payment, error) {
	return services.CreatePayment(context.Background(), db, reference, unit, amount)
}

// 1. A payment that exactly clears the invoice.
func TestFullPayment(t *testing.T) {
	db := newTestDB(t)
	addInvoice(t, db, "INV001", "A101", "2026-08-01", 100000)

	payment, err := createPayment(db, "PAY001", "A101", 100000)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(payment.PaymentAllocations) != 1 {
		t.Fatalf("got %d allocations, want 1", len(payment.PaymentAllocations))
	}

	assertInvoice(t, db, "INV001", 100000, 0, models.StatusPaid)
}

// 2. A payment that covers only part of the invoice.
func TestPartialPayment(t *testing.T) {
	db := newTestDB(t)
	addInvoice(t, db, "INV001", "A101", "2026-08-01", 100000)

	if _, err := createPayment(db, "PAY001", "A101", 40000); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	assertInvoice(t, db, "INV001", 40000, 60000, models.StatusPartial)
}

// 3. One payment spread across several invoices — the example from the brief.
func TestPaymentAcrossMultipleInvoices(t *testing.T) {
	db := newTestDB(t)
	addInvoice(t, db, "INV001", "A101", "2026-08-01", 100000)
	addInvoice(t, db, "INV002", "A101", "2026-08-15", 50000)

	payment, err := createPayment(db, "PAY001", "A101", 120000)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(payment.PaymentAllocations) != 2 {
		t.Fatalf("got %d allocations, want 2", len(payment.PaymentAllocations))
	}

	assertInvoice(t, db, "INV001", 100000, 0, models.StatusPaid)
	assertInvoice(t, db, "INV002", 20000, 30000, models.StatusPartial)
}

// 4. The oldest due date is settled first, whatever order the rows were created in.
func TestAllocationOrdersByDueDate(t *testing.T) {
	db := newTestDB(t)
	// Inserted newest-first on purpose: the order must come from the due date,
	// not from insertion order.
	addInvoice(t, db, "INV002", "A101", "2026-09-01", 50000)
	addInvoice(t, db, "INV001", "A101", "2026-07-01", 50000)

	if _, err := createPayment(db, "PAY001", "A101", 50000); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	assertInvoice(t, db, "INV001", 50000, 0, models.StatusPaid)
	assertInvoice(t, db, "INV002", 0, 50000, models.StatusUnpaid)
}

// 5. Invoices falling due on the same day are settled by invoice number.
func TestAllocationBreaksTiesByInvoiceNumber(t *testing.T) {
	db := newTestDB(t)
	addInvoice(t, db, "INV002", "A101", "2026-08-01", 50000)
	addInvoice(t, db, "INV001", "A101", "2026-08-01", 50000)

	if _, err := createPayment(db, "PAY001", "A101", 50000); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	assertInvoice(t, db, "INV001", 50000, 0, models.StatusPaid)
	assertInvoice(t, db, "INV002", 0, 50000, models.StatusUnpaid)
}

// 6. Re-sending the same payment reference must not take the money twice.
func TestDuplicatePaymentReferenceIsRejected(t *testing.T) {
	db := newTestDB(t)
	addInvoice(t, db, "INV001", "A101", "2026-08-01", 100000)

	if _, err := createPayment(db, "PAY001", "A101", 40000); err != nil {
		t.Fatalf("unexpected error on the first payment: %v", err)
	}

	_, err := createPayment(db, "PAY001", "A101", 40000)
	if !errors.Is(err, services.ErrDuplicatePaymentReference) {
		t.Fatalf("got error %v, want ErrDuplicatePaymentReference", err)
	}

	if got := countPayments(t, db); got != 1 {
		t.Errorf("got %d payments, want 1", got)
	}
	assertInvoice(t, db, "INV001", 40000, 60000, models.StatusPartial)
}

// 7. A unit with nothing outstanding cannot take a payment.
func TestPaymentWithNoOutstandingInvoices(t *testing.T) {
	db := newTestDB(t)
	addInvoice(t, db, "INV001", "A101", "2026-08-01", 100000)

	if _, err := createPayment(db, "PAY001", "A101", 100000); err != nil {
		t.Fatalf("unexpected error on the first payment: %v", err)
	}

	_, err := createPayment(db, "PAY002", "A101", 10000)
	if !errors.Is(err, services.ErrNoOutstandingInvoices) {
		t.Fatalf("got error %v, want ErrNoOutstandingInvoices", err)
	}

	if got := countPayments(t, db); got != 1 {
		t.Errorf("got %d payments, want 1 — the rejected payment must not be saved", got)
	}
	assertInvoice(t, db, "INV001", 100000, 0, models.StatusPaid)
}

// 8. Money larger than everything owed is refused outright, not partly taken.
func TestPaymentLargerThanOutstandingIsRejected(t *testing.T) {
	db := newTestDB(t)
	addInvoice(t, db, "INV001", "A101", "2026-08-01", 100000)
	addInvoice(t, db, "INV002", "A101", "2026-08-15", 50000)

	_, err := createPayment(db, "PAY001", "A101", 200000)
	if !errors.Is(err, services.ErrPaymentExceedsOutstanding) {
		t.Fatalf("got error %v, want ErrPaymentExceedsOutstanding", err)
	}

	// Nothing was written: the whole transaction rolled back.
	if got := countPayments(t, db); got != 0 {
		t.Errorf("got %d payments, want 0", got)
	}
	assertInvoice(t, db, "INV001", 0, 100000, models.StatusUnpaid)
	assertInvoice(t, db, "INV002", 0, 50000, models.StatusUnpaid)
}
