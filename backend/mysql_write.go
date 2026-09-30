package main

import (
	"context"
	"database/sql"
	"database/sql/driver"
	"errors"
	"log"
	"net/http"
	"slices"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	mysqldb "gosql/database/mysql"
)

func mysqlKeyParameter(column mysqlColumn) string {
	switch column.DataType {
	case "tinyint", "smallint", "mediumint", "int", "bigint", "integer", "year":
		if strings.Contains(strings.ToLower(column.Type), "unsigned") {
			return "CAST(? AS UNSIGNED)"
		}
		return "CAST(? AS SIGNED)"
	case "decimal":
		kind := strings.SplitN(strings.ToLower(column.Type), " ", 2)[0]
		if validMySQLType(kind) {
			return "CAST(? AS " + strings.ToUpper(kind) + ")"
		}
	case "bit":
		return "CAST(? AS UNSIGNED)"
	case "date":
		return "CAST(? AS DATE)"
	case "time":
		return "CAST(? AS TIME)"
	case "datetime", "timestamp":
		return "CAST(? AS DATETIME)"
	}
	return "?"
}

func mysqlKeyWhere(meta mysqlTableMeta) string {
	parts := make([]string, len(meta.PrimaryKey))
	for i, key := range meta.PrimaryKey {
		column, _ := mysqlSelectedColumn(meta, key)
		parts[i] = mysqldb.Identifier(key) + " <=> " + mysqlKeyParameter(column)
	}
	return strings.Join(parts, " AND ")
}

func mysqlSelectedColumn(meta mysqlTableMeta, name string) (mysqlColumn, bool) {
	for _, column := range meta.Columns {
		if column.Name == name {
			return column, true
		}
	}
	return mysqlColumn{}, false
}

func mysqlValidRow(meta mysqlTableMeta, key tableRowKey) bool {
	return meta.Editable && len(key.Values) == len(meta.PrimaryKey) && len(key.Values) > 0 && len(key.Version) == 64
}

func mysqlValue(column mysqlColumn, value *string) (any, error) {
	if value == nil {
		return nil, nil
	}
	if column.DataType == "bit" {
		bits := 1
		if strings.HasPrefix(column.Type, "bit(") && strings.HasSuffix(column.Type, ")") {
			var err error
			bits, err = strconv.Atoi(column.Type[4 : len(column.Type)-1])
			if err != nil || bits < 1 || bits > 64 {
				return nil, errors.New("invalid BIT width")
			}
		}
		number, err := strconv.ParseUint(*value, 10, 64)
		if err != nil || bits < 64 && number >= uint64(1)<<bits {
			return nil, errors.New("BIT value must be an unsigned integer within the column width")
		}
		return number, nil
	}
	if mysqlBinaryType(column.DataType) {
		return mysqlDecodeBinary(*value)
	}
	if len(*value) > 8<<20 {
		return nil, errors.New("value exceeds 8 MB")
	}
	return *value, nil
}

func (a *application) handleMySQLCell(w http.ResponseWriter, r *http.Request) {
	var input tableMutation
	if !readJSON(w, r, &input) {
		return
	}
	if !mysqldb.ValidName(input.Database) || !mysqldb.ValidName(input.Table) || !mysqldb.ValidName(input.Column) || input.Schema != "" {
		writeError(w, 400, "invalid_cell", "Choose a valid table column.")
		return
	}
	conn, ctx, cleanup := a.openMySQLRequest(w, r, input.Database, false)
	if conn == nil {
		return
	}
	defer cleanup()
	meta, err := readMySQLMeta(ctx, conn, input.Database, input.Table)
	if err != nil {
		writeMySQLError(w, err)
		return
	}
	column, found := mysqlSelectedColumn(meta, input.Column)
	if !found || !column.Editable || !mysqlValidRow(meta, input.Row) {
		writeError(w, 409, "row_changed", "This row changed or cannot be edited. Refresh the table.")
		return
	}
	name := mysqldb.Identifier(input.Database) + "." + mysqldb.Identifier(input.Table)
	selected := mysqldb.Identifier(input.Column)
	if column.DataType == "bit" {
		selected = "CAST(" + selected + " AS UNSIGNED)"
	} else if mysqlBinaryType(column.DataType) {
		selected = "CONCAT(CHAR(92),'x',HEX(SUBSTRING(" + selected + ",1,4194305)))"
	} else {
		selected = "LEFT(" + selected + ",2097153)"
	}
	query := "SELECT " + selected + "," + mysqlVersionSQL(meta.Columns) + " FROM " + name + " WHERE " + mysqlKeyWhere(meta)
	var value sql.NullString
	var version string
	args := make([]any, len(input.Row.Values))
	for i, v := range input.Row.Values {
		args[i] = v
	}
	err = conn.QueryRowContext(ctx, query, args...).Scan(&value, &version)
	if errors.Is(err, sql.ErrNoRows) || err == nil && version != input.Row.Version {
		writeError(w, 409, "row_changed", "This row changed. Refresh the table.")
		return
	}
	if err != nil {
		writeMySQLError(w, err)
		return
	}
	var full *string
	if value.Valid {
		if len(value.String) > 8<<20 || utf8.RuneCountInString(value.String) > 2<<20 {
			writeError(w, 413, "cell_too_large", "This cell exceeds the editor limit of 2 million characters or 8 MB.")
			return
		}
		full = &value.String
	}
	writeJSON(w, 200, struct {
		Value *string `json:"value"`
	}{full})
}

func (a *application) handleMySQLEdit(w http.ResponseWriter, r *http.Request) {
	a.handleMySQLMutation(w, r, false)
}
func (a *application) handleMySQLDelete(w http.ResponseWriter, r *http.Request) {
	a.handleMySQLMutation(w, r, true)
}

func (a *application) handleMySQLMutation(w http.ResponseWriter, r *http.Request, deleting bool) {
	var input tableMutation
	if !readJSON(w, r, &input) {
		return
	}
	if !mysqldb.ValidName(input.Database) || !mysqldb.ValidName(input.Table) || input.Schema != "" || !deleting && !mysqldb.ValidName(input.Column) || deleting && (len(input.Rows) < 1 || len(input.Rows) > 100) {
		writeError(w, 400, "invalid_mutation", "Choose a table, column, and up to 100 rows.")
		return
	}
	conn, ctx, cleanup := a.openMySQLRequest(w, r, input.Database, false)
	if conn == nil {
		return
	}
	defer cleanup()
	meta, err := readMySQLMeta(ctx, conn, input.Database, input.Table)
	if err != nil {
		writeMySQLError(w, err)
		return
	}
	if !meta.Editable {
		writeError(w, 403, "table_read_only", meta.ReadOnlyReason)
		return
	}
	var column mysqlColumn
	var value any
	if !deleting {
		var found bool
		column, found = mysqlSelectedColumn(meta, input.Column)
		if !found || !column.Editable {
			writeError(w, 403, "column_read_only", "This column cannot be edited.")
			return
		}
		value, err = mysqlValue(column, input.Value)
		if err != nil {
			writeError(w, 400, "invalid_value", err.Error())
			return
		}
	}
	keys := input.Rows
	if !deleting {
		keys = []tableRowKey{input.Row}
	}
	for i, key := range keys {
		if !mysqlValidRow(meta, key) {
			writeError(w, 400, "invalid_row", "The row identity is invalid. Refresh the table.")
			return
		}
		for j := 0; j < i; j++ {
			if slices.Equal(keys[j].Values, key.Values) {
				writeError(w, 400, "invalid_row", "The same row was selected more than once.")
				return
			}
		}
	}
	if deleting {
		slices.SortFunc(keys, func(left, right tableRowKey) int {
			for i := range left.Values {
				if difference := strings.Compare(left.Values[i], right.Values[i]); difference != 0 {
					return difference
				}
			}
			return 0
		})
	}
	if strings.EqualFold(meta.Engine, "MyISAM") || strings.EqualFold(meta.Engine, "Aria") {
		a.mutateMySQLNontransactional(w, ctx, conn, meta, input, keys, value, deleting)
		return
	}
	tx, err := conn.BeginTx(ctx, nil)
	if err != nil {
		writeMySQLError(w, err)
		return
	}
	defer tx.Rollback()
	table := mysqldb.Identifier(input.Database) + "." + mysqldb.Identifier(input.Table)
	where := mysqlKeyWhere(meta)
	for _, key := range keys {
		args := make([]any, len(key.Values))
		for i, v := range key.Values {
			args[i] = v
		}
		var version string
		err = tx.QueryRowContext(ctx, "SELECT "+mysqlVersionSQL(meta.Columns)+" FROM "+table+" WHERE "+where+" FOR UPDATE NOWAIT", args...).Scan(&version)
		if errors.Is(err, sql.ErrNoRows) || err == nil && version != key.Version {
			writeError(w, 409, "row_changed", "A selected row changed or no longer exists. Refresh the table.")
			return
		}
		if err != nil {
			writeMySQLError(w, err)
			return
		}
		statement := "DELETE FROM " + table + " WHERE " + where
		if !deleting {
			statement = "UPDATE " + table + " SET " + mysqldb.Identifier(column.Name) + "=? WHERE " + where
			args = append([]any{value}, args...)
		}
		if _, err = tx.ExecContext(ctx, statement, args...); err != nil {
			writeMySQLError(w, err)
			return
		}
	}
	if err = tx.Commit(); err != nil {
		writeMySQLError(w, err)
		return
	}
	writeJSON(w, 200, map[string]int{"affectedRows": len(keys)})
}

func (a *application) mutateMySQLNontransactional(w http.ResponseWriter, ctx context.Context, conn *sql.Conn, meta mysqlTableMeta, input tableMutation, keys []tableRowKey, value any, deleting bool) {
	table := mysqlTableName(input.Database, input.Table)
	where := mysqlKeyWhere(meta)
	if len(keys) == 1 {
		args := make([]any, 0, len(keys[0].Values)+2)
		statement := "DELETE FROM " + table
		if !deleting {
			statement = "UPDATE " + table + " SET " + mysqldb.Identifier(input.Column) + "=?"
			args = append(args, value)
		}
		for _, key := range keys[0].Values {
			args = append(args, key)
		}
		args = append(args, keys[0].Version)
		result, err := conn.ExecContext(ctx, statement+" WHERE "+where+" AND "+mysqlVersionSQL(meta.Columns)+"=?", args...)
		if err != nil {
			writeMySQLError(w, err)
			return
		}
		affected, err := result.RowsAffected()
		if err != nil {
			writeMySQLError(w, err)
		} else if affected == 0 {
			writeError(w, 409, "row_changed", "This row changed or no longer exists. Refresh the table.")
		} else {
			writeJSON(w, 200, map[string]int64{"affectedRows": affected})
		}
		return
	}
	if _, err := conn.ExecContext(ctx, "LOCK TABLES "+table+" WRITE"); err != nil {
		writeMySQLError(w, err)
		return
	}
	defer func() {
		unlockCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		if _, err := conn.ExecContext(unlockCtx, "UNLOCK TABLES"); err != nil {
			log.Printf("nontransactional MySQL table unlock failed: database=%q table=%q engine=%q error=%+v", input.Database, input.Table, meta.Engine, err)
			_ = conn.Raw(func(any) error { return driver.ErrBadConn })
		}
	}()
	clauses := make([]string, len(keys))
	args := make([]any, 0, len(keys)*len(meta.PrimaryKey))
	for i, key := range keys {
		values := make([]any, len(key.Values))
		for j, item := range key.Values {
			values[j] = item
		}
		var version string
		err := conn.QueryRowContext(ctx, "SELECT "+mysqlVersionSQL(meta.Columns)+" FROM "+table+" WHERE "+where, values...).Scan(&version)
		if errors.Is(err, sql.ErrNoRows) || err == nil && version != key.Version {
			writeError(w, 409, "row_changed", "A selected row changed or no longer exists. Refresh the table.")
			return
		}
		if err != nil {
			writeMySQLError(w, err)
			return
		}
		clauses[i] = "(" + where + ")"
		args = append(args, values...)
	}
	result, err := conn.ExecContext(ctx, "DELETE FROM "+table+" WHERE "+strings.Join(clauses, " OR "), args...)
	if err != nil {
		writeMySQLError(w, err)
		return
	}
	affected, err := result.RowsAffected()
	if err != nil {
		writeMySQLError(w, err)
	} else if affected != int64(len(keys)) {
		writeError(w, 409, "row_changed", "Selected rows could not all be deleted. Refresh the table.")
	} else {
		writeJSON(w, 200, map[string]int64{"affectedRows": affected})
	}
}

func mysqlQuickValue(column mysqlColumn) (string, bool) {
	switch column.DataType {
	case "tinyint", "smallint", "mediumint", "int", "integer", "bigint", "decimal", "float", "double", "bit", "boolean":
		return "0", true
	case "char", "varchar", "text", "tinytext", "mediumtext", "longtext":
		return "''", true
	case "binary", "varbinary", "blob", "tinyblob", "mediumblob", "longblob":
		return "X''", true
	case "json":
		return "'{}'", true
	case "date":
		return "CURRENT_DATE", true
	case "time":
		return "CURRENT_TIME", true
	case "datetime", "timestamp":
		return "CURRENT_TIMESTAMP", true
	}
	return "", false
}

func (a *application) handleMySQLInsert(w http.ResponseWriter, r *http.Request) {
	var input struct {
		Database string             `json:"database"`
		Schema   string             `json:"schema"`
		Table    string             `json:"table"`
		Values   map[string]*string `json:"values"`
	}
	if !readJSON(w, r, &input) {
		return
	}
	if !mysqldb.ValidName(input.Database) || !mysqldb.ValidName(input.Table) || input.Schema != "" || len(input.Values) > 128 {
		writeError(w, 400, "invalid_insert", "Choose a table and up to 128 column values.")
		return
	}
	if mysqlSystemDatabase(input.Database) {
		writeError(w, 403, "system_database", "System databases cannot be changed from this form.")
		return
	}
	conn, ctx, cleanup := a.openMySQLRequest(w, r, input.Database, false)
	if conn == nil {
		return
	}
	defer cleanup()
	meta, err := readMySQLMeta(ctx, conn, input.Database, input.Table)
	if err != nil {
		writeMySQLError(w, err)
		return
	}
	if meta.Kind != "BASE TABLE" {
		writeError(w, 400, "invalid_table", "Rows can only be added to tables.")
		return
	}
	names := []string{}
	values := []string{}
	args := []any{}
	if len(input.Values) > 0 {
		for name, value := range input.Values {
			column, found := mysqlSelectedColumn(meta, name)
			if !found || !column.Editable {
				writeError(w, 400, "invalid_column", "A column is unavailable or generated.")
				return
			}
			converted, convertErr := mysqlValue(column, value)
			if convertErr != nil {
				writeError(w, 400, "invalid_value", convertErr.Error())
				return
			}
			names = append(names, name)
			args = append(args, converted)
		}
		slices.Sort(names)
		args = args[:0]
		for _, name := range names {
			column, _ := mysqlSelectedColumn(meta, name)
			converted, _ := mysqlValue(column, input.Values[name])
			args = append(args, converted)
			values = append(values, "?")
		}
	} else {
		for _, column := range meta.Columns {
			if column.Nullable || column.Default != nil || strings.Contains(strings.ToLower(column.Extra), "auto_increment") || !column.Editable {
				continue
			}
			expression, ok := mysqlQuickValue(column)
			if !ok {
				writeError(w, 400, "insert_default_unavailable", "No safe default for column "+column.Name+".")
				return
			}
			names = append(names, column.Name)
			values = append(values, expression)
		}
	}
	table := mysqldb.Identifier(input.Database) + "." + mysqldb.Identifier(input.Table)
	statement := "INSERT INTO " + table + " () VALUES ()"
	if len(names) > 0 {
		quoted := make([]string, len(names))
		for i, name := range names {
			quoted[i] = mysqldb.Identifier(name)
		}
		statement = "INSERT INTO " + table + " (" + strings.Join(quoted, ",") + ") VALUES (" + strings.Join(values, ",") + ")"
	}
	result, err := conn.ExecContext(ctx, statement, args...)
	if err != nil {
		writeMySQLError(w, err)
		return
	}
	keyValues := []any{}
	if len(meta.PrimaryKey) == 1 {
		key := meta.PrimaryKey[0]
		if value, ok := input.Values[key]; ok && value != nil {
			keyValues = append(keyValues, *value)
		} else if column, found := mysqlSelectedColumn(meta, key); found && strings.Contains(strings.ToLower(column.Extra), "auto_increment") {
			if id, idErr := result.LastInsertId(); idErr == nil && id > 0 {
				keyValues = append(keyValues, id)
			}
		}
	} else if len(meta.PrimaryKey) > 1 {
		for _, key := range meta.PrimaryKey {
			value, ok := input.Values[key]
			if !ok || value == nil {
				keyValues = nil
				break
			}
			keyValues = append(keyValues, *value)
		}
	}
	response := map[string]any{"affectedRows": 1, "row": nil, "version": "", "refreshRequired": true}
	if len(keyValues) == len(meta.PrimaryKey) && meta.Editable {
		selected := make([]string, len(meta.Columns)+1)
		for i, column := range meta.Columns {
			name := mysqldb.Identifier(column.Name)
			if column.DataType == "bit" {
				selected[i] = "CAST(" + name + " AS UNSIGNED)"
			} else if mysqlBinaryType(column.DataType) && !slices.Contains(meta.PrimaryKey, column.Name) {
				selected[i] = "CONCAT(CHAR(92),'x',HEX(SUBSTRING(" + name + ",1,257)))"
			} else if mysqlPreviewType(column.DataType) && !slices.Contains(meta.PrimaryKey, column.Name) {
				selected[i] = "LEFT(" + name + ",513)"
			} else {
				selected[i] = name
			}
		}
		selected[len(meta.Columns)] = mysqlVersionSQL(meta.Columns)
		raw := make([][]byte, len(selected))
		dest := make([]any, len(raw))
		for i := range raw {
			dest[i] = &raw[i]
		}
		query := "SELECT " + strings.Join(selected, ",") + " FROM " + table + " WHERE " + mysqlKeyWhere(meta)
		if scanErr := conn.QueryRowContext(ctx, query, keyValues...).Scan(dest...); scanErr == nil {
			row := make([]*string, len(meta.Columns))
			truncated := make([]bool, len(meta.Columns))
			for i := range row {
				if raw[i] != nil {
					value := string(raw[i])
					if !slices.Contains(meta.PrimaryKey, meta.Columns[i].Name) {
						if mysqlBinaryType(meta.Columns[i].DataType) && len(value) > 514 {
							value = value[:514]
							truncated[i] = true
						} else if mysqlPreviewType(meta.Columns[i].DataType) && utf8.RuneCountInString(value) > 512 {
							value = string([]rune(value)[:512])
							truncated[i] = true
						}
					}
					row[i] = &value
				}
			}
			response["row"] = row
			response["version"] = string(raw[len(raw)-1])
			response["truncated"] = truncated
		} else {
			log.Printf("MySQL insert succeeded but row refresh failed: database=%q table=%q error=%+v", input.Database, input.Table, scanErr)
		}
	}
	response["sql"] = statement + ";"
	writeJSON(w, 200, response)
}
