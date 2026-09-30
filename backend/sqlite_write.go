package main

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"errors"
	"fmt"
	"net/http"
	"slices"
	"strconv"
	"strings"

	sqlitedb "gosql/database/sqlite"
)

var errSQLiteRowLarge = errors.New("SQLite row exceeds 8 MB")

func sqliteRowVersion(values []any) string {
	hash := sha256.New()
	for _, value := range values {
		var data []byte
		switch typed := value.(type) {
		case nil:
		case []byte:
			data = typed
		case string:
			data = []byte(typed)
		case int64:
			data = strconv.AppendInt(nil, typed, 10)
		case float64:
			data = strconv.AppendFloat(nil, typed, 'g', -1, 64)
		default:
			data = []byte(fmt.Sprint(typed))
		}
		fmt.Fprintf(hash, "%T:%d:", value, len(data))
		hash.Write(data)
	}
	return hex.EncodeToString(hash.Sum(nil))
}

func sqliteRowIdentity(meta sqliteMeta, key tableRowKey) ([]any, error) {
	if !meta.Editable || len(key.Values) != len(meta.PrimaryKey) || len(key.Version) != 64 {
		return nil, errors.New("row identity is unavailable")
	}
	args := make([]any, len(key.Values))
	for i, value := range key.Values {
		kind := ""
		if len(key.Types) == len(key.Values) {
			kind = key.Types[i]
		}
		if meta.RowIDName != "" {
			kind = "integer"
		}
		switch kind {
		case "integer":
			parsed, err := strconv.ParseInt(value, 10, 64)
			if err != nil {
				return nil, err
			}
			args[i] = parsed
		case "real":
			parsed, err := strconv.ParseFloat(value, 64)
			if err != nil {
				return nil, err
			}
			args[i] = parsed
		case "blob":
			parsed, err := hex.DecodeString(value)
			if err != nil {
				return nil, err
			}
			args[i] = parsed
		case "text":
			args[i] = value
		default:
			return nil, errors.New("row key type is unavailable")
		}
	}
	return args, nil
}

func sqliteKeyWhere(meta sqliteMeta) string {
	parts := make([]string, len(meta.PrimaryKey))
	for i, name := range meta.PrimaryKey {
		parts[i] = sqlitedb.Quote(name) + "=?"
	}
	return strings.Join(parts, " AND ")
}

func sqliteReadRow(ctx context.Context, conn *sql.Conn, meta sqliteMeta, table string, keys []any) ([]any, error) {
	selected := make([]string, len(meta.Columns))
	lengths := make([]string, len(meta.Columns))
	for i, column := range meta.Columns {
		selected[i] = sqlitedb.Quote(column.Name)
		lengths[i] = "COALESCE(length(CAST(" + selected[i] + " AS BLOB)),0)"
	}
	base := " FROM main." + sqlitedb.Quote(table) + " WHERE " + sqliteKeyWhere(meta)
	var byteCount int64
	if err := conn.QueryRowContext(ctx, "SELECT "+strings.Join(lengths, "+")+base, keys...).Scan(&byteCount); err != nil {
		return nil, err
	}
	if byteCount > 8<<20 {
		return nil, errSQLiteRowLarge
	}
	statement := "SELECT " + strings.Join(selected, ",") + base
	raw := make([]any, len(selected))
	dest := make([]any, len(selected))
	for i := range raw {
		dest[i] = &raw[i]
	}
	if err := conn.QueryRowContext(ctx, statement, keys...).Scan(dest...); err != nil {
		return nil, err
	}
	return raw, nil
}

func sqliteInputValue(column tableColumn, value *string) (any, error) {
	if value == nil {
		return nil, nil
	}
	if len(*value) > 8<<20 {
		return nil, errors.New("cell exceeds 8 MB")
	}
	if strings.EqualFold(column.Type, "BLOB") && strings.HasPrefix(*value, `\x`) {
		return hex.DecodeString((*value)[2:])
	}
	return *value, nil
}

func (a *application) handleSQLiteCell(w http.ResponseWriter, r *http.Request) {
	var input tableMutation
	if !readJSON(w, r, &input) {
		return
	}
	if !sqlitedb.ValidName(input.Table) || !sqlitedb.ValidName(input.Column) || input.Schema != "" {
		writeError(w, 400, "invalid_cell", "Choose a SQLite table column.")
		return
	}
	conn, ctx, cleanup := a.openSQLiteRequest(w, r, input.Database, true)
	if conn == nil {
		return
	}
	defer cleanup()
	meta, err := readSQLiteMetaBasic(ctx, conn, input.Table)
	if err != nil {
		writeSQLiteError(w, err)
		return
	}
	keys, err := sqliteRowIdentity(meta, input.Row)
	if err != nil {
		writeError(w, 400, "invalid_row", err.Error())
		return
	}
	index := slices.IndexFunc(meta.Columns, func(c tableColumn) bool { return c.Name == input.Column })
	if index < 0 {
		writeError(w, 400, "invalid_column", "Column not found.")
		return
	}
	raw, err := sqliteReadRow(ctx, conn, meta, input.Table, keys)
	if errors.Is(err, sql.ErrNoRows) || err == nil && sqliteRowVersion(raw) != input.Row.Version {
		writeError(w, 409, "row_changed", "This row changed. Refresh the table.")
		return
	}
	if err != nil {
		writeSQLiteError(w, err)
		return
	}
	value, _, length := sqliteValue(raw[index])
	if length > 8<<20 {
		writeError(w, 413, "cell_too_large", "This cell exceeds 8 MB.")
		return
	}
	if blob, ok := raw[index].([]byte); ok {
		result := `\x` + strings.ToUpper(hex.EncodeToString(blob))
		value = &result
	}
	if text, ok := raw[index].(string); ok {
		value = &text
	}
	writeJSON(w, 200, map[string]any{"value": value})
}

func (a *application) handleSQLiteEdit(w http.ResponseWriter, r *http.Request) {
	a.handleSQLiteMutation(w, r, false)
}
func (a *application) handleSQLiteDelete(w http.ResponseWriter, r *http.Request) {
	a.handleSQLiteMutation(w, r, true)
}

func (a *application) handleSQLiteMutation(w http.ResponseWriter, r *http.Request, deleting bool) {
	var input tableMutation
	if !readJSON(w, r, &input) {
		return
	}
	if !sqlitedb.ValidName(input.Table) || input.Schema != "" || deleting && (len(input.Rows) < 1 || len(input.Rows) > 100) {
		writeError(w, 400, "invalid_mutation", "Choose a table and up to 100 rows.")
		return
	}
	conn, ctx, cleanup := a.openSQLiteRequest(w, r, input.Database, false)
	if conn == nil {
		return
	}
	defer cleanup()
	meta, err := readSQLiteMetaBasic(ctx, conn, input.Table)
	if err != nil {
		writeSQLiteError(w, err)
		return
	}
	if !meta.Editable {
		writeError(w, 403, "table_read_only", meta.ReadOnlyReason)
		return
	}
	var value any
	if !deleting {
		index := slices.IndexFunc(meta.Columns, func(c tableColumn) bool { return c.Name == input.Column })
		if index < 0 || !meta.Columns[index].Editable {
			writeError(w, 403, "column_read_only", "This column cannot be edited.")
			return
		}
		value, err = sqliteInputValue(meta.Columns[index], input.Value)
		if err != nil {
			writeError(w, 400, "invalid_value", err.Error())
			return
		}
	}
	keys := input.Rows
	if !deleting {
		keys = []tableRowKey{input.Row}
	}
	identities := make([][]any, len(keys))
	seen := map[string]bool{}
	for i, key := range keys {
		identities[i], err = sqliteRowIdentity(meta, key)
		if err != nil {
			writeError(w, 400, "invalid_row", "Row identity is invalid or duplicated.")
			return
		}
		fingerprint := sqliteRowVersion(identities[i])
		if seen[fingerprint] {
			writeError(w, 400, "invalid_row", "The same row was selected twice.")
			return
		}
		seen[fingerprint] = true
	}
	if _, err = conn.ExecContext(ctx, "BEGIN IMMEDIATE"); err != nil {
		writeSQLiteError(w, err)
		return
	}
	defer conn.ExecContext(context.Background(), "ROLLBACK")
	for i, key := range keys {
		raw, readErr := sqliteReadRow(ctx, conn, meta, input.Table, identities[i])
		if errors.Is(readErr, sql.ErrNoRows) || readErr == nil && sqliteRowVersion(raw) != key.Version {
			writeError(w, 409, "row_changed", "A selected row changed. Refresh the table.")
			return
		}
		if readErr != nil {
			writeSQLiteError(w, readErr)
			return
		}
		if !deleting {
			index := slices.IndexFunc(meta.Columns, func(column tableColumn) bool { return column.Name == input.Column })
			if _, isBlob := raw[index].([]byte); isBlob && input.Value != nil && strings.HasPrefix(*input.Value, `\x`) {
				value, err = hex.DecodeString((*input.Value)[2:])
				if err != nil {
					writeError(w, 400, "invalid_value", "Enter a valid hexadecimal BLOB value.")
					return
				}
			}
		}
		statement := "DELETE FROM main." + sqlitedb.Quote(input.Table)
		args := slices.Clone(identities[i])
		if !deleting {
			statement = "UPDATE main." + sqlitedb.Quote(input.Table) + " SET " + sqlitedb.Quote(input.Column) + "=?"
			args = append([]any{value}, args...)
		}
		statement += " WHERE " + sqliteKeyWhere(meta)
		if _, err = conn.ExecContext(ctx, statement, args...); err != nil {
			writeSQLiteError(w, err)
			return
		}
	}
	if _, err = conn.ExecContext(ctx, "COMMIT"); err != nil {
		writeSQLiteError(w, err)
		return
	}
	writeJSON(w, 200, map[string]int{"affectedRows": len(keys)})
}

func (a *application) handleSQLiteInsert(w http.ResponseWriter, r *http.Request) {
	var input struct {
		Database string             `json:"database"`
		Schema   string             `json:"schema"`
		Table    string             `json:"table"`
		Values   map[string]*string `json:"values"`
	}
	if !readJSON(w, r, &input) {
		return
	}
	if !sqlitedb.ValidName(input.Table) || input.Schema != "" || len(input.Values) > 128 {
		writeError(w, 400, "invalid_insert", "Choose a table and up to 128 values.")
		return
	}
	conn, ctx, cleanup := a.openSQLiteRequest(w, r, input.Database, false)
	if conn == nil {
		return
	}
	defer cleanup()
	meta, err := readSQLiteMetaBasic(ctx, conn, input.Table)
	if err != nil {
		writeSQLiteError(w, err)
		return
	}
	if meta.Kind != "table" || !meta.Editable {
		writeError(w, 403, "table_read_only", meta.ReadOnlyReason)
		return
	}
	names := make([]string, 0, len(input.Values))
	for name := range input.Values {
		names = append(names, name)
	}
	slices.Sort(names)
	statement := "INSERT INTO main." + sqlitedb.Quote(input.Table)
	args := []any{}
	if len(names) == 0 {
		statement += " DEFAULT VALUES"
	} else {
		quoted := make([]string, len(names))
		parameters := make([]string, len(names))
		for i, name := range names {
			index := slices.IndexFunc(meta.Columns, func(c tableColumn) bool { return c.Name == name })
			if index < 0 || !meta.Columns[index].Editable {
				writeError(w, 400, "invalid_column", "A column cannot be inserted.")
				return
			}
			quoted[i] = sqlitedb.Quote(name)
			parameters[i] = "?"
			value, valueErr := sqliteInputValue(meta.Columns[index], input.Values[name])
			if valueErr != nil {
				writeError(w, 400, "invalid_value", valueErr.Error())
				return
			}
			args = append(args, value)
		}
		statement += " (" + strings.Join(quoted, ",") + ") VALUES (" + strings.Join(parameters, ",") + ")"
	}
	result, err := conn.ExecContext(ctx, statement, args...)
	if err != nil {
		writeSQLiteError(w, err)
		return
	}
	affected, _ := result.RowsAffected()
	writeJSON(w, 200, map[string]int64{"affectedRows": affected})
}
