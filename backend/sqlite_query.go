package main

import (
	"context"
	"encoding/hex"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	sqlite "modernc.org/sqlite"
	sqlite3 "modernc.org/sqlite/lib"

	sqlitedb "gosql/database/sqlite"
)

// sqliteConsoleCommand accepts one statement, ignoring separators in quotes and comments.
func sqliteConsoleCommand(statement string) string {
	command := ""
	ended := false
	for i := 0; i < len(statement); {
		c := statement[i]
		if strings.ContainsRune(" \t\r\n\f", rune(c)) {
			i++
			continue
		}
		if c == '-' && i+1 < len(statement) && statement[i+1] == '-' {
			i += 2
			for i < len(statement) && statement[i] != '\n' && statement[i] != '\r' {
				i++
			}
			continue
		}
		if c == '/' && i+1 < len(statement) && statement[i+1] == '*' {
			end := strings.Index(statement[i+2:], "*/")
			if end < 0 {
				return ""
			}
			i += end + 4
			continue
		}
		if ended {
			return ""
		}
		if command == "" {
			start := i
			for i < len(statement) && (statement[i] >= 'a' && statement[i] <= 'z' || statement[i] >= 'A' && statement[i] <= 'Z') {
				i++
			}
			command = strings.ToUpper(statement[start:i])
			switch command {
			case "SELECT", "INSERT", "UPDATE", "DELETE":
			default:
				return ""
			}
			continue
		}
		if c == ';' {
			ended = true
			i++
			continue
		}
		if c == '\'' || c == '"' || c == '`' || c == '[' {
			close := c
			if c == '[' {
				close = ']'
			}
			i++
			closed := false
			for i < len(statement) {
				if statement[i] == close {
					i++
					if c != '[' && i < len(statement) && statement[i] == close {
						i++
						continue
					}
					closed = true
					break
				}
				i++
			}
			if !closed {
				return ""
			}
			continue
		}
		i++
	}
	return command
}

// Console SQLite accepts one SELECT or data mutation; file and schema commands are excluded.
func (a *application) handleSQLiteQuery(w http.ResponseWriter, r *http.Request) {
	var input struct {
		Database string `json:"database"`
		SQL      string `json:"sql"`
	}
	if !readJSON(w, r, &input) {
		return
	}
	statement := strings.TrimSpace(input.SQL)
	if !sqlitedb.ValidName(input.Database) || utf8.RuneCountInString(statement) > maxSQLLength || statement == "" {
		writeError(w, 400, "invalid_query", "Choose a SQLite file and enter SQL (maximum 50,000 characters).")
		return
	}
	command := sqliteConsoleCommand(statement)
	if command == "" {
		writeError(w, 400, "sqlite_query_unsupported", "SQLite query console accepts one SELECT, INSERT, UPDATE, or DELETE statement.")
		return
	}
	writing := command != "SELECT"
	conn, ctx, cleanup := a.openSQLiteRequest(w, r, input.Database, !writing)
	if conn == nil {
		return
	}
	defer cleanup()
	if _, err := sqlite.Limit(conn, sqlite3.SQLITE_LIMIT_LENGTH, 8<<20); err != nil {
		writeSQLiteError(w, err)
		return
	}
	started := time.Now()
	if writing {
		if _, err := conn.ExecContext(ctx, "BEGIN IMMEDIATE"); err != nil {
			writeSQLiteError(w, err)
			return
		}
		defer conn.ExecContext(context.Background(), "ROLLBACK")
	}
	rows, err := conn.QueryContext(ctx, statement)
	if err != nil {
		writeSQLiteError(w, err)
		return
	}
	defer rows.Close()
	names, err := rows.Columns()
	if err != nil {
		writeSQLiteError(w, err)
		return
	}
	columns := uniqueQueryColumns(names)
	output := []map[string]any{}
	bytesTotal := 0
	raw := make([]any, len(columns))
	dest := make([]any, len(columns))
	for i := range raw {
		dest[i] = &raw[i]
	}
	for rows.Next() {
		if len(output) >= 10000 {
			writeError(w, 413, "query_result_too_large", "Query result exceeds 10,000 rows. Add LIMIT.")
			return
		}
		if err = rows.Scan(dest...); err != nil {
			writeSQLiteError(w, err)
			return
		}
		result := make(map[string]any, len(columns))
		for i, value := range raw {
			var converted any
			switch data := value.(type) {
			case nil:
				converted = nil
			case []byte:
				converted = `\x` + strings.ToUpper(hex.EncodeToString(data))
			case int64:
				converted = strconv.FormatInt(data, 10)
			case float64:
				converted = strconv.FormatFloat(data, 'g', -1, 64)
			default:
				converted = fmt.Sprint(data)
			}
			if text, ok := converted.(string); ok {
				bytesTotal += len(text)
			}
			if bytesTotal > 8<<20 {
				writeError(w, 413, "query_result_too_large", "Query result exceeds 8 MB. Select fewer columns.")
				return
			}
			result[columns[i]] = converted
		}
		output = append(output, result)
	}
	if err = rows.Err(); err != nil {
		writeSQLiteError(w, err)
		return
	}
	if err = rows.Close(); err != nil {
		writeSQLiteError(w, err)
		return
	}
	affected := int64(len(output))
	if writing {
		if err = conn.QueryRowContext(ctx, "SELECT changes()").Scan(&affected); err != nil {
			writeSQLiteError(w, err)
			return
		}
		if _, err = conn.ExecContext(ctx, "COMMIT"); err != nil {
			writeSQLiteError(w, err)
			return
		}
	}
	writeJSON(w, 200, map[string]any{"rows": output, "columns": columns, "command": command, "affectedRows": affected, "duration": time.Since(started).Milliseconds()})
}
