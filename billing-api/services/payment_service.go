package services

import (
	"context"
	"database/sql"
	"errors"
	"fmt"

	"billing-api/database"
	"billing-api/models"
)

// Business rules a caller can hit. The handler turns these into HTTP status
// codes; this package does not know what a status code is.
var (
	ErrDuplicatePaymentReference = errors.New("payment reference already exists")
	ErrNoOutstandingInvoices     = errors.New("unit has no outstanding invoices")
	ErrPaymentExceedsOutstanding = errors.New("payment is larger than the total outstanding amount")
)

// CreatePayment records a payment and allocates it across the unit's
// outstanding invoices, oldest due date first and, where two invoices fall due
// on the same day, by invoice number ascending.
//
// Everything runs in one transaction: the payment row, its allocations and the
// updated invoice balances are all written, or none of them are. The connection
// is configured with _txlock=immediate, so the transaction takes SQLite's write
// lock before it reads anything — two payments for the same unit are therefore
// serialised rather than both allocating against the same balances.
func CreatePayment(
	ctx context.Context,
	db *sql.DB,
	reference, unit string,
	amount int64,
) (payment *models.Payment, err error) {
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return nil, fmt.Errorf("begin transaction: %w", err)
	}
	// Roll back unless the function reaches its successful commit below.
	defer func() {
		if err != nil {
			tx.Rollback()
		}
	}()

	if err = rejectIfReferenceUsed(ctx, tx, reference); err != nil {
		return nil, err
	}

	invoices, err := outstandingInvoices(ctx, tx, unit)
	if err != nil {
		return nil, err
	}
	if len(invoices) == 0 {
		err = ErrNoOutstandingInvoices
		return nil, err
	}

	// Money that cannot be fully applied is refused rather than partly taken,
	// so the caller never has to guess what happened to the rest.
	var totalOutstanding int64
	for _, invoice := range invoices {
		totalOutstanding += invoice.OutstandingAmount()
	}
	if amount > totalOutstanding {
		err = ErrPaymentExceedsOutstanding
		return nil, err
	}

	payment = &models.Payment{PaymentReference: reference, Unit: unit, Amount: amount}
	const insertPayment = `
		INSERT INTO payments (payment_reference, unit, amount)
		VALUES (?, ?, ?)
		RETURNING id, created_at`
	var createdAt string
	if err = tx.QueryRowContext(ctx, insertPayment, reference, unit, amount).
		Scan(&payment.ID, &createdAt); err != nil {
		return nil, fmt.Errorf("insert payment: %w", err)
	}
	payment.CreatedAt = database.ParseTimestamp(createdAt)

	remaining := amount
	for _, invoice := range invoices {
		if remaining == 0 {
			break
		}

		// Each invoice takes what it still owes, or whatever is left of the
		// payment — whichever is smaller.
		applied := invoice.OutstandingAmount()
		if applied > remaining {
			applied = remaining
		}

		allocation, allocErr := applyToInvoice(ctx, tx, payment.ID, invoice, applied)
		if allocErr != nil {
			err = allocErr
			return nil, err
		}

		payment.PaymentAllocations = append(payment.PaymentAllocations, allocation)
		remaining -= applied
	}

	if err = tx.Commit(); err != nil {
		return nil, fmt.Errorf("commit transaction: %w", err)
	}
	return payment, nil
}

// rejectIfReferenceUsed enforces idempotency: a reference already on record
// means the money was taken before and must not be taken again.
//
// The unique index on payment_reference is the real guarantee — if two
// identical requests race past this check, the second INSERT fails and the
// whole transaction rolls back.
func rejectIfReferenceUsed(ctx context.Context, tx *sql.Tx, reference string) error {
	const query = `SELECT EXISTS (SELECT 1 FROM payments WHERE payment_reference = ?)`

	var used bool
	if err := tx.QueryRowContext(ctx, query, reference).Scan(&used); err != nil {
		return fmt.Errorf("check payment reference: %w", err)
	}
	if used {
		return ErrDuplicatePaymentReference
	}
	return nil
}

// outstandingInvoices reads the unit's unsettled invoices in the order they will
// be settled.
//
// There is no FOR UPDATE here because SQLite has none: the write lock was
// already taken when the transaction began, which gives the same guarantee for
// a single-writer database. On PostgreSQL or SQL Server this query would carry
// FOR UPDATE instead.
//
// The WHERE clause matches the partial index on invoices, so only outstanding
// rows are scanned however many settled ones sit behind them.
func outstandingInvoices(ctx context.Context, tx *sql.Tx, unit string) ([]models.Invoice, error) {
	const query = `
		SELECT id, invoice_number, total_amount, paid_amount
		FROM invoices
		WHERE unit = ? AND paid_amount < total_amount
		ORDER BY due_date, invoice_number`

	rows, err := tx.QueryContext(ctx, query, unit)
	if err != nil {
		return nil, fmt.Errorf("read outstanding invoices: %w", err)
	}
	defer rows.Close()

	var invoices []models.Invoice
	for rows.Next() {
		var invoice models.Invoice
		if err := rows.Scan(
			&invoice.ID, &invoice.InvoiceNumber,
			&invoice.TotalAmount, &invoice.PaidAmount,
		); err != nil {
			return nil, fmt.Errorf("scan outstanding invoice: %w", err)
		}
		invoices = append(invoices, invoice)
	}
	return invoices, rows.Err()
}

// applyToInvoice records one allocation and moves the invoice's balance.
func applyToInvoice(
	ctx context.Context,
	tx *sql.Tx,
	paymentID int64,
	invoice models.Invoice,
	applied int64,
) (models.PaymentAllocation, error) {
	allocation := models.PaymentAllocation{
		PaymentID:       paymentID,
		InvoiceID:       invoice.ID,
		AllocatedAmount: applied,
		InvoiceNumber:   invoice.InvoiceNumber,
	}

	const insertAllocation = `
		INSERT INTO payment_allocations (payment_id, invoice_id, allocated_amount)
		VALUES (?, ?, ?)
		RETURNING id`
	if err := tx.QueryRowContext(ctx, insertAllocation, paymentID, invoice.ID, applied).
		Scan(&allocation.ID); err != nil {
		return allocation, fmt.Errorf("insert payment allocation: %w", err)
	}

	// Adding in SQL rather than writing a total worked out in Go keeps the
	// update correct even if the row changed since it was read. RETURNING hands
	// back the new balance and the generated status in the same round trip.
	const updateInvoice = `
		UPDATE invoices
		SET paid_amount = paid_amount + ?
		WHERE id = ?
		RETURNING total_amount - paid_amount, status`
	if err := tx.QueryRowContext(ctx, updateInvoice, applied, invoice.ID).
		Scan(&allocation.OutstandingAmount, &allocation.Status); err != nil {
		return allocation, fmt.Errorf("update invoice paid amount: %w", err)
	}

	return allocation, nil
}
