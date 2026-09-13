package models

import "time"

// Invoice status values. The database derives the status from the amounts in a
// generated column; these constants name the values it produces.
const (
	StatusUnpaid  = "UNPAID"
	StatusPartial = "PARTIAL"
	StatusPaid    = "PAID"
)

// Invoice is a bill raised against a property unit.
//
// All money is int64 in satang (1/100 of a baht). See the README for why this
// is not a float.
type Invoice struct {
	ID            int64
	InvoiceNumber string
	Unit          string
	DueDate       time.Time
	TotalAmount   int64
	PaidAmount    int64

	// Status is read from the database rather than worked out here. The
	// generated column is the single source of truth, so the two cannot drift.
	Status string

	CreatedAt    time.Time
	UpdatedAt    time.Time
	InvoiceItems []InvoiceItem
}

// InvoiceItem is one billed line on an invoice.
type InvoiceItem struct {
	ID          int64
	InvoiceID   int64
	Description string
	Amount      int64
}

// OutstandingAmount is how much of the invoice is still owed.
func (i Invoice) OutstandingAmount() int64 {
	return i.TotalAmount - i.PaidAmount
}
