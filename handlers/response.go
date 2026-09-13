package handlers

import "github.com/gin-gonic/gin"

// Error codes. These are stable strings, so a client can branch on the code
// without reading the message.
const (
	CodeInvalidJSON               = "INVALID_JSON"
	CodeInvalidInput              = "INVALID_INPUT"
	CodeDuplicateInvoiceNumber    = "DUPLICATE_INVOICE_NUMBER"
	CodeDuplicatePaymentReference = "DUPLICATE_PAYMENT_REFERENCE"
	CodeNotFound                  = "NOT_FOUND"
	CodeNoOutstandingInvoice      = "NO_OUTSTANDING_INVOICE"
	CodePaymentExceedsOutstanding = "PAYMENT_EXCEEDS_OUTSTANDING"
	CodeDatabaseError             = "DATABASE_ERROR"
)

// errorBody is the shape every failing endpoint returns.
type errorBody struct {
	Error errorDetail `json:"error"`
}

type errorDetail struct {
	Code    string `json:"code"`
	Message string `json:"message"`
}

func respondError(c *gin.Context, status int, code, message string) {
	c.JSON(status, errorBody{Error: errorDetail{Code: code, Message: message}})
}
