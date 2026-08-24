package store

import (
	"errors"
	"testing"

	"github.com/jackc/pgx/v5/pgconn"
)

func TestIsAlreadyExistsAcrossDialects(t *testing.T) {
	for _, err := range []error{
		ErrAlreadyExists,
		errors.New("UNIQUE constraint failed: users.username"),
		&pgconn.PgError{Code: "23505", Message: "duplicate key value violates unique constraint"},
	} {
		if !IsAlreadyExists(err) {
			t.Errorf("IsAlreadyExists(%v) = false", err)
		}
	}
	if IsAlreadyExists(&pgconn.PgError{Code: "23503"}) {
		t.Fatal("foreign-key violation reported as already exists")
	}
}
