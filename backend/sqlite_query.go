package main

import (
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

// Console SQLite hanya menerima satu SELECT agar statement tidak dapat membuka berkas lain.
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
	statement = strings.TrimSuffix(statement, ";")
	if len(statement) < 7 || !strings.EqualFold(statement[:6], "SELECT") || statement[6] != ' ' && statement[6] != '\n' && statement[6] != '\t' || strings.Contains(statement, ";") {
		writeError(w, 400, "sqlite_query_read_only", "SQLite query console currently accepts one SELECT statement. Use the table editor for changes.")
		return
	}
	conn, ctx, cleanup := a.openSQLiteRequest(w, r, input.Database, true)
	if conn == nil {
		return
	}
	defer cleanup()
	if _, err := sqlite.Limit(conn, sqlite3.SQLITE_LIMIT_LENGTH, 8<<20); err != nil {
		writeSQLiteError(w, err)
		return
	}
	started := time.Now()
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
	writeJSON(w, 200, map[string]any{"rows": output, "columns": columns, "command": "SELECT", "affectedRows": len(output), "duration": time.Since(started).Milliseconds()})
}
