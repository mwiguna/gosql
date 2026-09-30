package main

import (
	"net/http"
	"strings"
	"time"
	"unicode/utf8"
)

// Query console menerima beberapa statement dan menampilkan hasil statement terakhir.
func (a *application) handlePostgresQuery(w http.ResponseWriter, r *http.Request) {
	var input struct {
		Database string `json:"database"`
		SQL      string `json:"sql"`
	}
	if !readJSON(w, r, &input) {
		return
	}
	if !validSchemaName(input.Database) || strings.TrimSpace(input.SQL) == "" || utf8.RuneCountInString(input.SQL) > maxSQLLength {
		writeError(w, 400, "invalid_query", "Choose a database and enter SQL (maximum 50,000 characters).")
		return
	}
	conn, ctx, cleanup := a.openPostgresConnection(w, r, input.Database, postgresRequestTimeout)
	if conn == nil {
		return
	}
	defer cleanup()
	started := time.Now()
	results := conn.PgConn().Exec(ctx, input.SQL)
	rows := []map[string]any{}
	var columns []string
	var command string
	var affected int64
	for results.NextResult() {
		result := results.ResultReader()
		fields := result.FieldDescriptions()
		names := make([]string, len(fields))
		for i, field := range fields {
			names[i] = field.Name
		}
		columns = uniqueQueryColumns(names)
		rows = []map[string]any{}
		for result.NextRow() {
			values := result.Values()
			row := make(map[string]any, len(columns))
			for i, value := range values {
				if value == nil {
					row[columns[i]] = nil
				} else {
					row[columns[i]] = string(value)
				}
			}
			rows = append(rows, row)
		}
		tag, err := result.Close()
		if err != nil {
			writeMutationError(w, err)
			return
		}
		command = tag.String()
		affected = tag.RowsAffected()
	}
	if err := results.Close(); err != nil {
		writeMutationError(w, err)
		return
	}
	writeJSON(w, 200, map[string]any{
		"rows": rows, "columns": columns, "command": command,
		"affectedRows": affected,
		"duration":     float64(time.Since(started).Milliseconds()),
	})
}
