package routes

import (
	"database/sql"

	"github.com/gin-gonic/gin"

	"billing-api/handlers"
)

// Register wires every endpoint onto the router.
func Register(router *gin.Engine, db *sql.DB) {
	router.POST("/invoices", handlers.CreateInvoice(db))
	router.GET("/invoices/:invoiceNumber", handlers.GetInvoice(db))
	router.POST("/payments", handlers.CreatePayment(db))
}
