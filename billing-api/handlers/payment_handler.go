package handlers

import (
	"database/sql"
	"errors"
	"net/http"
	"strings"

	"github.com/gin-gonic/gin"

	"billing-api/database"
	"billing-api/models"
	"billing-api/services"
)

type createPaymentRequest struct {
	PaymentReference string `json:"payment_reference"`
	Unit             string `json:"unit"`
	Amount           int64  `json:"amount"`
}

type allocationResponse struct {
	InvoiceNumber     string `json:"invoice_number"`
	AllocatedAmount   int64  `json:"allocated_amount"`
	OutstandingAmount int64  `json:"outstanding_amount"`
	Status            string `json:"status"`
}

type paymentResponse struct {
	PaymentReference string               `json:"payment_reference"`
	Unit             string               `json:"unit"`
	Amount           int64                `json:"amount"`
	Allocations      []allocationResponse `json:"allocations"`
}

func newPaymentResponse(payment *models.Payment) paymentResponse {
	allocations := make([]allocationResponse, 0, len(payment.PaymentAllocations))
	for _, allocation := range payment.PaymentAllocations {
		allocations = append(allocations, allocationResponse{
			InvoiceNumber:     allocation.InvoiceNumber,
			AllocatedAmount:   allocation.AllocatedAmount,
			OutstandingAmount: allocation.OutstandingAmount,
			Status:            allocation.Status,
		})
	}

	return paymentResponse{
		PaymentReference: payment.PaymentReference,
		Unit:             payment.Unit,
		Amount:           payment.Amount,
		Allocations:      allocations,
	}
}

// CreatePayment handles POST /payments.
func CreatePayment(db *sql.DB) gin.HandlerFunc {
	return func(c *gin.Context) {
		var request createPaymentRequest
		if err := c.ShouldBindJSON(&request); err != nil {
			respondError(c, http.StatusBadRequest, CodeInvalidJSON, "request body is not valid JSON")
			return
		}

		if strings.TrimSpace(request.PaymentReference) == "" {
			respondError(c, http.StatusBadRequest, CodeInvalidInput, "payment_reference is required")
			return
		}
		if strings.TrimSpace(request.Unit) == "" {
			respondError(c, http.StatusBadRequest, CodeInvalidInput, "unit is required")
			return
		}
		if request.Amount <= 0 {
			respondError(c, http.StatusBadRequest, CodeInvalidInput,
				"payment amount must be greater than zero")
			return
		}

		payment, err := services.CreatePayment(
			c.Request.Context(), db, request.PaymentReference, request.Unit, request.Amount)

		switch {
		// The unique index catches two identical requests that race past the
		// service's own check, so both paths end at the same answer.
		case errors.Is(err, services.ErrDuplicatePaymentReference), database.IsUniqueViolation(err):
			respondError(c, http.StatusConflict, CodeDuplicatePaymentReference,
				"a payment with this payment_reference has already been recorded")
			return
		case errors.Is(err, services.ErrNoOutstandingInvoices):
			respondError(c, http.StatusUnprocessableEntity, CodeNoOutstandingInvoice,
				"this unit has no outstanding invoices")
			return
		case errors.Is(err, services.ErrPaymentExceedsOutstanding):
			respondError(c, http.StatusUnprocessableEntity, CodePaymentExceedsOutstanding,
				"payment amount is larger than the total outstanding amount for this unit")
			return
		case err != nil:
			respondError(c, http.StatusInternalServerError, CodeDatabaseError,
				"could not record the payment")
			return
		}

		c.JSON(http.StatusCreated, newPaymentResponse(payment))
	}
}
