package store

import (
	"context"
	"database/sql/driver"
	"strings"

	"github.com/jackc/pgx/v5/stdlib"
)

// postgresDriver keeps the store's deliberately portable database/sql calls
// readable while translating SQLite-style positional parameters at the driver
// boundary. PostgreSQL-only expression differences are normalized here too;
// schema creation remains explicit in postgres.go.
type postgresDriver struct{ inner driver.Driver }

func (d postgresDriver) Open(name string) (driver.Conn, error) {
	conn, err := d.inner.Open(name)
	if err != nil {
		return nil, err
	}
	return postgresConn{Conn: conn}, nil
}

type postgresConn struct{ driver.Conn }

func (c postgresConn) Prepare(query string) (driver.Stmt, error) {
	return c.Conn.Prepare(postgresQuery(query))
}

func (c postgresConn) PrepareContext(ctx context.Context, query string) (driver.Stmt, error) {
	if conn, ok := c.Conn.(driver.ConnPrepareContext); ok {
		return conn.PrepareContext(ctx, postgresQuery(query))
	}
	return c.Prepare(query)
}

func (c postgresConn) ExecContext(ctx context.Context, query string, args []driver.NamedValue) (driver.Result, error) {
	return c.Conn.(driver.ExecerContext).ExecContext(ctx, postgresQuery(query), args)
}

func (c postgresConn) QueryContext(ctx context.Context, query string, args []driver.NamedValue) (driver.Rows, error) {
	return c.Conn.(driver.QueryerContext).QueryContext(ctx, postgresQuery(query), args)
}

func (c postgresConn) Ping(ctx context.Context) error {
	return c.Conn.(driver.Pinger).Ping(ctx)
}

func (c postgresConn) ResetSession(ctx context.Context) error {
	if conn, ok := c.Conn.(driver.SessionResetter); ok {
		return conn.ResetSession(ctx)
	}
	return nil
}

func (c postgresConn) IsValid() bool {
	if conn, ok := c.Conn.(driver.Validator); ok {
		return conn.IsValid()
	}
	return true
}

func (c postgresConn) CheckNamedValue(value *driver.NamedValue) error {
	if conn, ok := c.Conn.(driver.NamedValueChecker); ok {
		return conn.CheckNamedValue(value)
	}
	return driver.ErrSkip
}

func newPostgresDriver() driver.Driver {
	return postgresDriver{inner: stdlib.GetDefaultDriver()}
}

func postgresQuery(query string) string {
	query = strings.ReplaceAll(query, " COLLATE NOCASE", "")
	query = strings.ReplaceAll(query, " LIKE ", " ILIKE ")
	query = strings.ReplaceAll(query, "X''", "decode('', 'hex')")
	query = strings.ReplaceAll(query, " AS BLOB)", " AS BYTEA)")
	query = strings.ReplaceAll(query, "LOWER(HEX(w.token_hash))", "LOWER(ENCODE(w.token_hash, 'hex'))")
	query = strings.ReplaceAll(query,
		"json_extract(CASE WHEN json_valid(CAST(n.card_json AS TEXT)) THEN CAST(n.card_json AS TEXT) ELSE '{}' END, '$.severity')",
		"(CASE WHEN octet_length(n.card_json) > 0 THEN convert_from(n.card_json, 'UTF8')::jsonb->>'severity' ELSE NULL END)")
	query = strings.ReplaceAll(query,
		"json_valid(n.card_json) AND json_extract(n.card_json, '$.severity') = 'critical'",
		"octet_length(n.card_json) > 0 AND convert_from(n.card_json, 'UTF8')::jsonb->>'severity' = 'critical'")
	query = strings.ReplaceAll(query, "CAST(n.card_json AS TEXT)", "convert_from(n.card_json, 'UTF8')")
	query = strings.ReplaceAll(query, "CAST(n.attachments_json AS TEXT)", "convert_from(n.attachments_json, 'UTF8')")
	query = postgresScalarFunctions(query)
	if strings.Contains(query, "INSERT INTO channel_read_state") {
		query = strings.ReplaceAll(query, "GREATEST(read_at, excluded.read_at)", "GREATEST(channel_read_state.read_at, excluded.read_at)")
		query = strings.ReplaceAll(query, "GREATEST(read_at,excluded.read_at)", "GREATEST(channel_read_state.read_at,excluded.read_at)")
	}
	if strings.Contains(query, "INSERT INTO notification_user_state") {
		query = strings.ReplaceAll(query, "GREATEST(read_at, excluded.read_at)", "GREATEST(notification_user_state.read_at, excluded.read_at)")
		query = strings.ReplaceAll(query, "GREATEST(read_at,excluded.read_at)", "GREATEST(notification_user_state.read_at,excluded.read_at)")
	}
	return postgresParameters(query)
}

func postgresParameters(query string) string {
	var out strings.Builder
	out.Grow(len(query) + 16)
	parameter := 1
	inSingle, inDouble, inLineComment, inBlockComment := false, false, false, false
	for i := 0; i < len(query); i++ {
		current := query[i]
		if inLineComment {
			out.WriteByte(current)
			if current == '\n' {
				inLineComment = false
			}
			continue
		}
		if inBlockComment {
			out.WriteByte(current)
			if current == '*' && i+1 < len(query) && query[i+1] == '/' {
				out.WriteByte('/')
				i++
				inBlockComment = false
			}
			continue
		}
		if !inSingle && !inDouble && current == '-' && i+1 < len(query) && query[i+1] == '-' {
			out.WriteString("--")
			i++
			inLineComment = true
			continue
		}
		if !inSingle && !inDouble && current == '/' && i+1 < len(query) && query[i+1] == '*' {
			out.WriteString("/*")
			i++
			inBlockComment = true
			continue
		}
		if current == '\'' && !inDouble {
			out.WriteByte(current)
			if inSingle && i+1 < len(query) && query[i+1] == '\'' {
				out.WriteByte(query[i+1])
				i++
			} else {
				inSingle = !inSingle
			}
			continue
		}
		if current == '"' && !inSingle {
			inDouble = !inDouble
			out.WriteByte(current)
			continue
		}
		if current == '?' && !inSingle && !inDouble {
			out.WriteByte('$')
			out.WriteString(intString(parameter))
			parameter++
			continue
		}
		out.WriteByte(current)
	}
	return out.String()
}

func postgresScalarFunctions(query string) string {
	for _, function := range []string{"MAX", "MIN"} {
		searchFrom := 0
		for {
			index := strings.Index(query[searchFrom:], function+"(")
			if index < 0 {
				break
			}
			index += searchFrom
			depth, comma, end := 0, false, -1
			for i := index + len(function); i < len(query); i++ {
				switch query[i] {
				case '(':
					depth++
				case ')':
					depth--
					if depth == 0 {
						end = i
					}
				case ',':
					if depth == 1 {
						comma = true
					}
				}
				if end >= 0 {
					break
				}
			}
			if comma && end >= 0 {
				replacement := map[string]string{"MAX": "GREATEST", "MIN": "LEAST"}[function]
				query = query[:index] + replacement + query[index+len(function):]
				searchFrom = end + len(replacement) - len(function) + 1
			} else {
				searchFrom = index + len(function) + 1
			}
		}
	}
	return query
}

func intString(value int) string {
	if value < 10 {
		return string(rune('0' + value))
	}
	return intString(value/10) + intString(value%10)
}
