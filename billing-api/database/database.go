package database

import (
	"context"
	"database/sql"
	"embed"
	"errors"
	"fmt"
	"io/fs"
	"net/url"
	"sort"
	"strings"
	"time"

	sqlite "modernc.org/sqlite"
	sqlitelib "modernc.org/sqlite/lib"
)

//go:embed migrations/*.sql
var migrationsFS embed.FS

// connectionPragmas are applied to every connection in the pool.
//
//	foreign_keys   SQLite ignores foreign keys unless they are switched on,
//	               and it does so per connection rather than per database.
//	busy_timeout   Wait for a writer to finish instead of failing immediately.
//	journal_mode   WAL lets readers work while a writer holds the database.
//	_txlock         Every transaction starts as BEGIN IMMEDIATE, taking the
//	               write lock up front. This is SQLite's answer to
//	               SELECT ... FOR UPDATE: two payments for the same unit are
//	               serialised rather than both reading the same balances.
const connectionPragmas = "_pragma=foreign_keys(1)" +
	"&_pragma=busy_timeout(5000)" +
	"&_pragma=journal_mode(WAL)" +
	"&_txlock=immediate"

// DSN turns a database file path into a connection string carrying the pragmas
// above.
func DSN(path string) string {
	return "file:" + url.PathEscape(path) + "?" + connectionPragmas
}

// Connect opens the database file and returns a pool ready for use.
func Connect(path string) (*sql.DB, error) {
	db, err := sql.Open("sqlite", DSN(path))
	if err != nil {
		return nil, fmt.Errorf("open database: %w", err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := db.PingContext(ctx); err != nil {
		db.Close()
		return nil, fmt.Errorf("open database: %w", err)
	}
	return db, nil
}

// DateLayout is how a due date is stored: ISO-8601, which also sorts
// chronologically as text — what the allocation ORDER BY relies on.
const DateLayout = "2006-01-02"

// TimestampLayout is how SQLite's datetime('now') writes a timestamp.
const TimestampLayout = "2006-01-02 15:04:05"

// ParseTimestamp reads a stored timestamp. SQLite has no time type, so these
// come back as text and are converted here rather than in every caller.
func ParseTimestamp(value string) time.Time {
	parsed, err := time.Parse(TimestampLayout, value)
	if err != nil {
		return time.Time{}
	}
	return parsed.UTC()
}

// ParseDate reads a stored due date.
func ParseDate(value string) time.Time {
	parsed, err := time.Parse(DateLayout, value)
	if err != nil {
		return time.Time{}
	}
	return parsed.UTC()
}

// IsUniqueViolation reports whether the error came from a broken UNIQUE
// constraint, which is how a duplicate invoice number or payment reference
// surfaces when two requests race past the application's own check.
func IsUniqueViolation(err error) bool {
	var sqliteErr *sqlite.Error
	if !errors.As(err, &sqliteErr) {
		return false
	}
	return sqliteErr.Code() == sqlitelib.SQLITE_CONSTRAINT_UNIQUE ||
		sqliteErr.Code() == sqlitelib.SQLITE_CONSTRAINT_PRIMARYKEY
}

// Migrate applies every migration the database has not run yet, in file-name
// order, and records what it applied.
func Migrate(db *sql.DB) error {
	const createLedger = `
		CREATE TABLE IF NOT EXISTS schema_migrations (
			name       TEXT NOT NULL PRIMARY KEY,
			applied_at TEXT NOT NULL DEFAULT (datetime('now'))
		)`
	if _, err := db.Exec(createLedger); err != nil {
		return fmt.Errorf("create schema_migrations: %w", err)
	}

	applied, err := appliedMigrations(db)
	if err != nil {
		return err
	}

	names, err := migrationNames()
	if err != nil {
		return err
	}

	for _, name := range names {
		if applied[name] {
			continue
		}

		body, err := migrationsFS.ReadFile("migrations/" + name)
		if err != nil {
			return fmt.Errorf("read migration %s: %w", name, err)
		}

		// Each migration runs in its own transaction: a half-applied schema
		// change is worse than a failed startup.
		tx, err := db.Begin()
		if err != nil {
			return fmt.Errorf("begin migration %s: %w", name, err)
		}
		for _, statement := range splitStatements(string(body)) {
			if _, err := tx.Exec(statement); err != nil {
				tx.Rollback()
				return fmt.Errorf("apply migration %s: %w", name, err)
			}
		}
		if _, err := tx.Exec("INSERT INTO schema_migrations (name) VALUES (?)", name); err != nil {
			tx.Rollback()
			return fmt.Errorf("record migration %s: %w", name, err)
		}
		if err := tx.Commit(); err != nil {
			return fmt.Errorf("commit migration %s: %w", name, err)
		}
	}

	return nil
}

// splitStatements breaks a migration file into individual statements.
//
// A plain split on ";" would cut a trigger body in half, because the BEGIN ...
// END block contains semicolons of its own. Tracking that block is enough for
// the SQL this project uses.
func splitStatements(script string) []string {
	var statements []string
	var current strings.Builder
	insideTriggerBody := false

	for _, line := range strings.Split(script, "\n") {
		trimmed := strings.TrimSpace(line)
		if strings.HasPrefix(trimmed, "--") || trimmed == "" {
			continue
		}

		current.WriteString(line)
		current.WriteString("\n")

		upper := strings.ToUpper(trimmed)
		switch {
		case strings.HasPrefix(upper, "CREATE TRIGGER"):
			insideTriggerBody = true
		case insideTriggerBody && strings.HasPrefix(upper, "END;"):
			insideTriggerBody = false
			statements = append(statements, current.String())
			current.Reset()
		case !insideTriggerBody && strings.HasSuffix(trimmed, ";"):
			statements = append(statements, current.String())
			current.Reset()
		}
	}

	if leftover := strings.TrimSpace(current.String()); leftover != "" {
		statements = append(statements, leftover)
	}
	return statements
}

func appliedMigrations(db *sql.DB) (map[string]bool, error) {
	rows, err := db.Query("SELECT name FROM schema_migrations")
	if err != nil {
		return nil, fmt.Errorf("read schema_migrations: %w", err)
	}
	defer rows.Close()

	applied := make(map[string]bool)
	for rows.Next() {
		var name string
		if err := rows.Scan(&name); err != nil {
			return nil, err
		}
		applied[name] = true
	}
	return applied, rows.Err()
}

func migrationNames() ([]string, error) {
	entries, err := fs.ReadDir(migrationsFS, "migrations")
	if err != nil {
		return nil, err
	}

	names := make([]string, 0, len(entries))
	for _, entry := range entries {
		if !entry.IsDir() {
			names = append(names, entry.Name())
		}
	}
	sort.Strings(names)
	return names, nil
}
