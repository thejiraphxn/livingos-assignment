package main

import (
	"log"
	"os"

	"github.com/gin-gonic/gin"

	"billing-api/database"
	"billing-api/routes"
)

func main() {
	databasePath := envOrDefault("DATABASE_PATH", "billing.db")
	port := envOrDefault("PORT", "8080")

	db, err := database.Connect(databasePath)
	if err != nil {
		log.Fatalf("could not open the database: %v", err)
	}
	defer db.Close()

	// Migrations run before the router is built, so the API never handles a
	// request against a schema it does not expect.
	if err := database.Migrate(db); err != nil {
		log.Fatalf("could not migrate: %v", err)
	}

	router := gin.Default()
	routes.Register(router, db)

	log.Printf("listening on :%s using database %s", port, databasePath)
	if err := router.Run(":" + port); err != nil {
		log.Fatalf("server stopped: %v", err)
	}
}

func envOrDefault(key, fallback string) string {
	if value := os.Getenv(key); value != "" {
		return value
	}
	return fallback
}
