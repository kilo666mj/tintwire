package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"net/url"
	"os"

	_ "modernc.org/sqlite"
)

// Backup creates a transactionally consistent SQLite snapshot. The destination
// must not exist, which prevents an operator typo from overwriting a good copy.
func (s *Store) Backup(ctx context.Context, destination string) error {
	if _, err := os.Stat(destination); err == nil {
		return fmt.Errorf("backup destination already exists: %s", destination)
	} else if !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("inspect backup destination: %w", err)
	}
	if _, err := s.db.ExecContext(ctx, `VACUUM INTO ?`, destination); err != nil {
		return fmt.Errorf("create SQLite backup: %w", err)
	}
	if err := VerifyDatabase(ctx, destination); err != nil {
		return fmt.Errorf("verify SQLite backup: %w", err)
	}
	return nil
}

// VerifyDatabase opens a database read-only and runs SQLite's integrity check.
func VerifyDatabase(ctx context.Context, path string) error {
	dsn := (&url.URL{Scheme: "file", Path: path, RawQuery: "mode=ro"}).String()
	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		return err
	}
	defer db.Close()
	var result string
	if err := db.QueryRowContext(ctx, `PRAGMA integrity_check`).Scan(&result); err != nil {
		return err
	}
	if result != "ok" {
		return fmt.Errorf("integrity_check returned %q", result)
	}
	return nil
}
