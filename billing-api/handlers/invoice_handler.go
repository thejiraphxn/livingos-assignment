package handlers

import (
	"context"
	"database/sql"
	"errors"
	"net/http"
	"strings"
	"time"

	"github.com/gin-gonic/gin"

	"billing-api/database"
	"billing-api/models"
)

// dueDateLayout is the only date format the API accepts or returns.
const dueDateLayout = "2006-01-02"

type createInvoiceRequest struct {
	InvoiceNumber string              `json:"invoice_number"`
	Unit          string              `json:"unit"`
	DueDate       string              `json:"due_date"`
	Items         []createInvoiceItem `json:"items"`
}

type createInvoiceItem struct {
	Description string `json:"description"`
	Amount      int64  `json:"amount"`
}

type invoiceItemResponse struct {
	Description string `json:"description"`
	Amount      int64  `json:"amount"`
}

type invoiceResponse struct {
	InvoiceNumber     string                `json:"invoice_number"`
	Unit              string                `json:"unit"`
	DueDate           string                `json:"due_date"`
	Items             []invoiceItemResponse `json:"items"`
	TotalAmount       int64                 `json:"total_amount"`
	PaidAmount        int64                 `json:"paid_amount"`
	OutstandingAmount int64                 `json:"outstanding_amount"`
	Status            string                `json:"status"`
}

func newInvoiceResponse(invoice models.Invoice) invoiceResponse {
	items := make([]invoiceItemResponse, 0, len(invoice.InvoiceItems))
	for _, item := range invoice.InvoiceItems {
		items = append(items, invoiceItemResponse{
			Description: item.Description,
			Amount:      item.Amount,
		})
	}

	return invoiceResponse{
		InvoiceNumber:     invoice.InvoiceNumber,
		Unit:              invoice.Unit,
		DueDate:           invoice.DueDate.Format(dueDateLayout),
		Items:             items,
		TotalAmount:       invoice.TotalAmount,
		PaidAmount:        invoice.PaidAmount,
		OutstandingAmount: invoice.OutstandingAmount(),
		Status:            invoice.Status,
	}
}

// CreateInvoice handles POST /invoices.
//
// The handler takes the database and returns the function Gin will call, which
// is how a route gets its dependency without a DI framework.
func CreateInvoice(db *sql.DB) gin.HandlerFunc {
	return func(c *gin.Context) {
		var request createInvoiceRequest
		if err := c.ShouldBindJSON(&request); err != nil {
			respondError(c, http.StatusBadRequest, CodeInvalidJSON, "request body is not valid JSON")
			return
		}

		if strings.TrimSpace(request.InvoiceNumber) == "" {
			respondError(c, http.StatusBadRequest, CodeInvalidInput, "invoice_number is required")
			return
		}
		if strings.TrimSpace(request.Unit) == "" {
			respondError(c, http.StatusBadRequest, CodeInvalidInput, "unit is required")
			return
		}

		dueDate, err := time.Parse(dueDateLayout, request.DueDate)
		if err != nil {
			respondError(c, http.StatusBadRequest, CodeInvalidInput,
				"due_date must be a date in YYYY-MM-DD format")
			return
		}

		if len(request.Items) == 0 {
			respondError(c, http.StatusBadRequest, CodeInvalidInput, "at least one item is required")
			return
		}

		// The total is worked out here rather than accepted from the client, so
		// the two can never disagree about the arithmetic.
		var totalAmount int64
		items := make([]models.InvoiceItem, 0, len(request.Items))
		for _, item := range request.Items {
			if strings.TrimSpace(item.Description) == "" {
				respondError(c, http.StatusBadRequest, CodeInvalidInput, "every item needs a description")
				return
			}
			if item.Amount <= 0 {
				respondError(c, http.StatusBadRequest, CodeInvalidInput,
					"every item amount must be greater than zero")
				return
			}
			totalAmount += item.Amount
			items = append(items, models.InvoiceItem{Description: item.Description, Amount: item.Amount})
		}

		invoice := models.Invoice{
			InvoiceNumber: request.InvoiceNumber,
			Unit:          request.Unit,
			DueDate:       dueDate,
			TotalAmount:   totalAmount,
			InvoiceItems:  items,
		}

		if err := insertInvoice(c.Request.Context(), db, &invoice); err != nil {
			if database.IsUniqueViolation(err) {
				respondError(c, http.StatusConflict, CodeDuplicateInvoiceNumber,
					"an invoice with this invoice_number already exists")
				return
			}
			respondError(c, http.StatusInternalServerError, CodeDatabaseError, "could not save the invoice")
			return
		}

		c.JSON(http.StatusCreated, newInvoiceResponse(invoice))
	}
}

// insertInvoice writes the invoice and its items in one transaction, so an
// invoice can never exist without the lines that add up to its total.
func insertInvoice(ctx context.Context, db *sql.DB, invoice *models.Invoice) error {
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()

	const insertInvoiceSQL = `
		INSERT INTO invoices (invoice_number, unit, due_date, total_amount)
		VALUES (?, ?, ?, ?)
		RETURNING id, paid_amount, status, created_at, updated_at`

	var createdAt, updatedAt string
	if err := tx.QueryRowContext(ctx, insertInvoiceSQL,
		invoice.InvoiceNumber, invoice.Unit,
		invoice.DueDate.Format(dueDateLayout), invoice.TotalAmount,
	).Scan(
		&invoice.ID, &invoice.PaidAmount, &invoice.Status, &createdAt, &updatedAt,
	); err != nil {
		return err
	}
	invoice.CreatedAt, invoice.UpdatedAt = database.ParseTimestamp(createdAt), database.ParseTimestamp(updatedAt)

	// One statement for every line rather than one round trip per line.
	placeholders := make([]string, 0, len(invoice.InvoiceItems))
	args := make([]any, 0, len(invoice.InvoiceItems)*3)
	for _, item := range invoice.InvoiceItems {
		placeholders = append(placeholders, "(?, ?, ?)")
		args = append(args, invoice.ID, item.Description, item.Amount)
	}

	insertItemsSQL := `INSERT INTO invoice_items (invoice_id, description, amount) VALUES ` +
		strings.Join(placeholders, ", ")
	if _, err := tx.ExecContext(ctx, insertItemsSQL, args...); err != nil {
		return err
	}

	return tx.Commit()
}

// GetInvoice handles GET /invoices/:invoiceNumber.
func GetInvoice(db *sql.DB) gin.HandlerFunc {
	return func(c *gin.Context) {
		ctx := c.Request.Context()
		invoiceNumber := c.Param("invoiceNumber")

		const selectInvoice = `
			SELECT id, invoice_number, unit, due_date, total_amount, paid_amount,
			       status, created_at, updated_at
			FROM invoices
			WHERE invoice_number = ?`

		var invoice models.Invoice
		var dueDate, createdAt, updatedAt string
		err := db.QueryRowContext(ctx, selectInvoice, invoiceNumber).Scan(
			&invoice.ID, &invoice.InvoiceNumber, &invoice.Unit, &dueDate,
			&invoice.TotalAmount, &invoice.PaidAmount, &invoice.Status,
			&createdAt, &updatedAt,
		)
		if errors.Is(err, sql.ErrNoRows) {
			respondError(c, http.StatusNotFound, CodeNotFound, "invoice not found")
			return
		}
		if err != nil {
			respondError(c, http.StatusInternalServerError, CodeDatabaseError, "could not read the invoice")
			return
		}

		invoice.DueDate = database.ParseDate(dueDate)
		invoice.CreatedAt, invoice.UpdatedAt = database.ParseTimestamp(createdAt), database.ParseTimestamp(updatedAt)

		invoice.InvoiceItems, err = selectInvoiceItems(ctx, db, invoice.ID)
		if err != nil {
			respondError(c, http.StatusInternalServerError, CodeDatabaseError, "could not read the invoice items")
			return
		}

		c.JSON(http.StatusOK, newInvoiceResponse(invoice))
	}
}

func selectInvoiceItems(ctx context.Context, db *sql.DB, invoiceID int64) ([]models.InvoiceItem, error) {
	const query = `
		SELECT id, invoice_id, description, amount
		FROM invoice_items
		WHERE invoice_id = ?
		ORDER BY id`

	rows, err := db.QueryContext(ctx, query, invoiceID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var items []models.InvoiceItem
	for rows.Next() {
		var item models.InvoiceItem
		if err := rows.Scan(&item.ID, &item.InvoiceID, &item.Description, &item.Amount); err != nil {
			return nil, err
		}
		items = append(items, item)
	}
	return items, rows.Err()
}
