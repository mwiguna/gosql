package main

import (
	"database/sql"
	"encoding/hex"
	"net/http"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	mysqldb "gosql/database/mysql"
)

func (a *application) handleMySQLQuery(w http.ResponseWriter, r *http.Request) {
	var input struct {
		Database string `json:"database"`
		SQL      string `json:"sql"`
	}
	if !readJSON(w, r, &input) {
		return
	}
	if !mysqldb.ValidName(input.Database) || strings.TrimSpace(input.SQL) == "" || utf8.RuneCountInString(input.SQL) > maxSQLLength {
		writeError(w, 400, "invalid_query", "Choose a database and enter SQL (maximum 50,000 characters).")
		return
	}
	conn, ctx, cleanup := a.openMySQLRequest(w, r, input.Database, true)
	if conn == nil {
		return
	}
	defer cleanup()
	started := time.Now()
	results, err := conn.QueryContext(ctx, input.SQL)
	if err != nil {
		writeMySQLError(w, err)
		return
	}
	defer results.Close()
	output := []map[string]any{}
	columns := []string{}
	command := ""
	affected := int64(0)
	resultBytes := 0
	for {
		names, nameErr := results.Columns()
		if nameErr != nil {
			writeMySQLError(w, nameErr)
			return
		}
		types, typeErr := results.ColumnTypes()
		if typeErr != nil {
			writeMySQLError(w, typeErr)
			return
		}
		columns = uniqueQueryColumns(names)
		output = []map[string]any{}
		resultBytes = 0
		raw := make([]sql.RawBytes, len(names))
		dest := make([]any, len(names))
		for i := range raw {
			dest[i] = &raw[i]
		}
		for results.Next() {
			if len(output) >= 10000 {
				writeError(w, 413, "query_result_too_large", "The query result exceeds 10,000 rows. Add a LIMIT clause.")
				return
			}
			if err = results.Scan(dest...); err != nil {
				writeMySQLError(w, err)
				return
			}
			row := make(map[string]any, len(names))
			for i, value := range raw {
				resultBytes += len(value)
				if resultBytes > 8<<20 {
					writeError(w, 413, "query_result_too_large", "The query result exceeds 8 MB. Add a LIMIT clause or select fewer columns.")
					return
				}
				if value == nil {
					row[columns[i]] = nil
				} else if i < len(types) && strings.EqualFold(types[i].DatabaseTypeName(), "BIT") {
					var number uint64
					for _, digit := range value {
						number = number<<8 | uint64(digit)
					}
					row[columns[i]] = strconv.FormatUint(number, 10)
				} else if i < len(types) && mysqlBinaryType(strings.ToLower(types[i].DatabaseTypeName())) {
					row[columns[i]] = "\\x" + strings.ToUpper(hex.EncodeToString(value))
				} else {
					row[columns[i]] = string(value)
				}
			}
			output = append(output, row)
		}
		if err = results.Err(); err != nil {
			writeMySQLError(w, err)
			return
		}
		if len(columns) > 0 {
			command = "SELECT"
			affected = int64(len(output))
		} else {
			command = "OK"
			affected = 0
		}
		if !results.NextResultSet() {
			break
		}
	}
	if err = results.Err(); err != nil {
		writeMySQLError(w, err)
		return
	}
	if len(columns) == 0 {
		if err = results.Close(); err != nil {
			writeMySQLError(w, err)
			return
		}
		if err = conn.QueryRowContext(ctx, "SELECT ROW_COUNT()").Scan(&affected); err != nil {
			writeMySQLError(w, err)
			return
		}
	}
	writeJSON(w, 200, map[string]any{"rows": output, "columns": columns, "command": command, "affectedRows": affected, "duration": float64(time.Since(started).Milliseconds())})
}
