package models

import "time"

// Payment is money received from a unit, together with how it was applied.
type Payment struct {
	ID int64

	// PaymentReference is the caller's own identifier for this payment. It is
	// unique in the database, which is what makes posting a payment safe to
	// retry: the same reference cannot record the money twice.
	PaymentReference string

	Unit      string
	Amount    int64
	CreatedAt time.Time

	PaymentAllocations []PaymentAllocation
}

// PaymentAllocation records how much of one payment went to one invoice.
// Together these rows are the audit trail behind Invoice.PaidAmount.
type PaymentAllocation struct {
	ID              int64
	PaymentID       int64
	InvoiceID       int64
	AllocatedAmount int64

	// InvoiceNumber and the invoice state after allocation, filled in when the
	// allocation is read back for a response.
	InvoiceNumber     string
	OutstandingAmount int64
	Status            string
}
