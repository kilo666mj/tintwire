package store

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"slices"
	"testing"
)

func TestMigrateSQLiteToPostgres(t *testing.T) {
	dsn := os.Getenv("TINTWIRE_TEST_POSTGRES_DSN")
	if dsn == "" {
		t.Skip("TINTWIRE_TEST_POSTGRES_DSN is not set")
	}
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "source.db")
	source, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := source.BootstrapWebhook(ctx, "migration-hook", "migration-channel"); err != nil {
		t.Fatal(err)
	}
	if err := source.BootstrapUser(ctx, "migration-admin", "a sufficiently long password"); err != nil {
		t.Fatal(err)
	}
	created, err := source.CreateFromWebhook(ctx, "migration-hook", IncomingNotification{
		Text: "migrated notification", RawPayload: json.RawMessage(`{"source":"sqlite"}`),
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := source.Close(); err != nil {
		t.Fatal(err)
	}

	if err := MigrateSQLiteToPostgres(ctx, path, dsn); err != nil {
		t.Fatal(err)
	}
	destination, err := OpenPostgres(dsn)
	if err != nil {
		t.Fatal(err)
	}
	defer destination.Close()
	assertMigrationSchemaParity(t, ctx, sourceSchema(t, ctx, path), destination)
	listed, err := destination.QueryNotifications(ctx, NotificationQuery{ID: created.ID, Limit: 1})
	if err != nil {
		t.Fatal(err)
	}
	if len(listed) != 1 || listed[0].Text != "migrated notification" {
		t.Fatalf("migrated notification = %#v", listed)
	}
}

func sourceSchema(t *testing.T, ctx context.Context, path string) map[string][]string {
	t.Helper()
	source, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer source.Close()
	rows, err := source.db.QueryContext(ctx, `SELECT name FROM sqlite_master WHERE type='table' AND name NOT LIKE 'sqlite_%'`)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	tables := map[string][]string{}
	for rows.Next() {
		var name string
		if err := rows.Scan(&name); err != nil {
			t.Fatal(err)
		}
		tables[name] = nil
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	if err := rows.Close(); err != nil {
		t.Fatal(err)
	}
	for table := range tables {
		columns, err := sqliteTableColumns(ctx, source.db, table)
		if err != nil {
			t.Fatal(err)
		}
		for _, column := range columns {
			tables[table] = append(tables[table], column.name)
		}
	}
	return tables
}

func assertMigrationSchemaParity(t *testing.T, ctx context.Context, sqliteTables map[string][]string, postgres *Store) {
	t.Helper()
	rows, err := postgres.db.QueryContext(ctx, `SELECT table_name,column_name FROM information_schema.columns WHERE table_schema='public' AND table_name<>'schema_version' ORDER BY table_name,ordinal_position`)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	postgresTables := map[string][]string{}
	for rows.Next() {
		var table, column string
		if err := rows.Scan(&table, &column); err != nil {
			t.Fatal(err)
		}
		postgresTables[table] = append(postgresTables[table], column)
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	if len(sqliteTables) != len(postgresTables) {
		t.Fatalf("schema table count: SQLite=%d PostgreSQL=%d\nSQLite=%v\nPostgreSQL=%v", len(sqliteTables), len(postgresTables), sqliteTables, postgresTables)
	}
	migratedTables := map[string]bool{}
	for _, table := range migrationTables {
		migratedTables[table] = true
	}
	for table, sqliteColumns := range sqliteTables {
		postgresColumns, ok := postgresTables[table]
		if !ok {
			t.Errorf("PostgreSQL schema is missing SQLite table %q", table)
			continue
		}
		if !slices.Equal(sqliteColumns, postgresColumns) {
			t.Errorf("schema columns for %s: SQLite=%v PostgreSQL=%v", table, sqliteColumns, postgresColumns)
		}
		if !migratedTables[table] {
			t.Errorf("migrationTables is missing schema table %q", table)
		}
	}
	for table := range postgresTables {
		if _, ok := sqliteTables[table]; !ok {
			t.Errorf("SQLite schema is missing PostgreSQL table %q", table)
		}
	}
}
