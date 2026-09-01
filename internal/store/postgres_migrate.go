package store

import (
	"context"
	"database/sql"
	"fmt"
	"net/url"
	"path/filepath"
	"strings"
)

// migrationTables is dependency ordered so PostgreSQL foreign keys remain
// enabled throughout the copy. Keep this list in sync with postgresSchema.
var migrationTables = []string{
	"app_settings", "users", "saved_views", "admin_audit_events", "channels", "agents", "agent_credentials", "agent_runs",
	"webhooks", "notifications", "notification_events", "push_subscriptions", "sessions",
	"oidc_login_states", "channel_read_state", "channel_memberships", "action_targets",
	"action_executions", "mattermost_bot_tokens", "mattermost_channel_aliases",
	"mattermost_posts", "mattermost_reactions", "slash_commands", "slash_command_executions",
	"slash_command_responses", "notification_user_state", "agent_run_events", "channel_messages",
	"agent_tool_invocations", "channel_notification_preferences", "replication_operations",
	"replication_cursors", "replication_quarantine", "replication_peer_status",
	"replication_snapshot_status",
}

// MigrateSQLiteToPostgres copies a complete SQLite store into a new PostgreSQL
// store. The destination must be empty; the copy and verification are atomic.
func MigrateSQLiteToPostgres(ctx context.Context, sqlitePath, postgresDSN string) error {
	absolutePath, err := filepath.Abs(sqlitePath)
	if err != nil {
		return fmt.Errorf("resolve SQLite path: %w", err)
	}
	sourceURL := (&url.URL{Scheme: "file", Path: absolutePath}).String() + "?mode=ro"
	source, err := sql.Open("sqlite", sourceURL)
	if err != nil {
		return fmt.Errorf("open SQLite source: %w", err)
	}
	defer func() { _ = source.Close() }()
	if err := source.PingContext(ctx); err != nil {
		return fmt.Errorf("connect SQLite source: %w", err)
	}

	destination, err := OpenPostgres(postgresDSN)
	if err != nil {
		return err
	}
	defer func() { _ = destination.Close() }()

	tx, err := destination.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("begin PostgreSQL migration: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	for _, table := range migrationTables {
		var count int64
		if err := tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM `+table).Scan(&count); err != nil {
			return fmt.Errorf("inspect PostgreSQL table %s: %w", table, err)
		}
		allowed := int64(0)
		if table == "users" {
			allowed = 1 // OpenPostgres creates the reserved local inbox identity.
		}
		if count != allowed {
			return fmt.Errorf("PostgreSQL destination is not empty: %s has %d rows", table, count)
		}
	}
	if _, err := tx.ExecContext(ctx, `DELETE FROM users WHERE id=?`, localInboxUserID); err != nil {
		return fmt.Errorf("prepare PostgreSQL users: %w", err)
	}

	for _, table := range migrationTables {
		columns, err := sqliteTableColumns(ctx, source, table)
		if err != nil {
			return err
		}
		if len(columns) == 0 {
			return fmt.Errorf("SQLite source is missing table %s", table)
		}
		quoted := make([]string, len(columns))
		placeholders := make([]string, len(columns))
		for i, column := range columns {
			quoted[i] = `"` + strings.ReplaceAll(column.name, `"`, `""`) + `"`
			placeholders[i] = "?"
		}
		rows, err := source.QueryContext(ctx, `SELECT `+strings.Join(quoted, ",")+` FROM "`+table+`"`)
		if err != nil {
			return fmt.Errorf("read SQLite table %s: %w", table, err)
		}
		insert := `INSERT INTO "` + table + `" (` + strings.Join(quoted, ",") + `) VALUES (` + strings.Join(placeholders, ",") + `)`
		var copied int64
		for rows.Next() {
			values := make([]any, len(columns))
			destinations := make([]any, len(columns))
			for i := range values {
				destinations[i] = &values[i]
			}
			if err := rows.Scan(destinations...); err != nil {
				_ = rows.Close()
				return fmt.Errorf("scan SQLite table %s: %w", table, err)
			}
			for i, column := range columns {
				required := column.notNull || (table == "users" && column.name == "password_hash")
				if blob, ok := values[i].([]byte); ok && blob == nil && required {
					values[i] = []byte{}
					continue
				}
				if values[i] != nil || !required {
					continue
				}
				switch strings.ToUpper(column.dataType) {
				case "BLOB":
					values[i] = []byte{}
				case "INTEGER":
					values[i] = int64(0)
				default:
					values[i] = ""
				}
			}
			if _, err := tx.ExecContext(ctx, insert, values...); err != nil {
				_ = rows.Close()
				return fmt.Errorf("copy SQLite table %s: %w", table, err)
			}
			copied++
		}
		if err := rows.Close(); err != nil {
			return fmt.Errorf("close SQLite table %s: %w", table, err)
		}
		if err := rows.Err(); err != nil {
			return fmt.Errorf("read SQLite table %s: %w", table, err)
		}
		var destinationCount int64
		if err := tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM "`+table+`"`).Scan(&destinationCount); err != nil {
			return fmt.Errorf("verify PostgreSQL table %s: %w", table, err)
		}
		if destinationCount != copied {
			return fmt.Errorf("verify PostgreSQL table %s: copied %d rows, found %d", table, copied, destinationCount)
		}
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("commit PostgreSQL migration: %w", err)
	}
	return nil
}

type sqliteColumn struct {
	name     string
	dataType string
	notNull  bool
}

func sqliteTableColumns(ctx context.Context, db *sql.DB, table string) ([]sqliteColumn, error) {
	rows, err := db.QueryContext(ctx, `PRAGMA table_info("`+table+`")`)
	if err != nil {
		return nil, fmt.Errorf("inspect SQLite table %s: %w", table, err)
	}
	defer func() { _ = rows.Close() }()
	var columns []sqliteColumn
	for rows.Next() {
		var position, notNull, primaryKey int
		var name, dataType string
		var defaultValue any
		if err := rows.Scan(&position, &name, &dataType, &notNull, &defaultValue, &primaryKey); err != nil {
			return nil, fmt.Errorf("inspect SQLite table %s: %w", table, err)
		}
		columns = append(columns, sqliteColumn{name: name, dataType: dataType, notNull: notNull != 0})
	}
	return columns, rows.Err()
}
