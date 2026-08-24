package store

import "testing"

func TestPostgresQueryTranslation(t *testing.T) {
	input := `SELECT MAX(?, COALESCE(read_at, 0)), MAX(created_at), CAST(payload AS BLOB), '?' FROM events
WHERE text LIKE ? ESCAPE '\' COLLATE NOCASE AND id=? -- ?`
	want := `SELECT GREATEST($1, COALESCE(read_at, 0)), MAX(created_at), CAST(payload AS BYTEA), '?' FROM events
WHERE text ILIKE $2 ESCAPE '\' AND id=$3 -- ?`
	if got := postgresQuery(input); got != want {
		t.Fatalf("postgres query:\n%s\nwant:\n%s", got, want)
	}
}

func TestPostgresQueryTranslatesWebhookHashEncoding(t *testing.T) {
	input := `SELECT LOWER(HEX(w.token_hash)) FROM webhooks w`
	want := `SELECT LOWER(ENCODE(w.token_hash, 'hex')) FROM webhooks w`
	if got := postgresQuery(input); got != want {
		t.Fatalf("postgres query: %q, want %q", got, want)
	}
}
