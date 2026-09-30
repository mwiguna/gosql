package main

import (
	"context"
	"database/sql"
	"encoding/hex"
	"errors"
	"net/http"
	"slices"
	"strings"

	sqlitedb "gosql/database/sqlite"
)

func sqliteReferenceValid(ctx context.Context, conn *sql.Conn, table string, columns []tableColumnInput, column tableColumnInput) bool {
	if column.ReferenceTable == "" {
		return true
	}
	if strings.EqualFold(column.ReferenceTable, table) {
		primary := 0
		for _, candidate := range columns {
			if candidate.Primary {
				primary++
			}
		}
		for _, candidate := range columns {
			if strings.EqualFold(candidate.Name, column.ReferenceColumn) {
				return candidate.Unique || candidate.Primary && primary == 1
			}
		}
		return false
	}
	meta, err := readSQLiteMeta(ctx, conn, column.ReferenceTable)
	if err != nil || meta.Kind != "table" {
		return false
	}
	primary := 0
	foundPrimary := false
	for _, candidate := range meta.Columns {
		if candidate.Key == "PRIMARY KEY" {
			primary++
			if candidate.Name == column.ReferenceColumn {
				foundPrimary = true
			}
		}
	}
	if foundPrimary && primary == 1 {
		return true
	}
	return slices.ContainsFunc(meta.Indexes, func(index tableIndex) bool {
		return index.Unique && !index.Partial && !index.Expression && len(index.Columns) == 1 && index.Columns[0] == column.ReferenceColumn
	})
}

func sqliteType(kind string, strict bool) bool {
	if strict {
		return slices.Contains([]string{"INT", "INTEGER", "REAL", "TEXT", "BLOB", "ANY"}, strings.ToUpper(kind))
	}
	return slices.Contains([]string{"INT", "INTEGER", "REAL", "TEXT", "BLOB", "NUMERIC"}, strings.ToUpper(kind))
}

func sqliteLiteral(value string) (string, error) {
	if len(value) > 4096 || strings.ContainsRune(value, 0) {
		return "", errors.New("default literal is too long or contains a null byte")
	}
	return "'" + strings.ReplaceAll(value, "'", "''") + "'", nil
}

func sqliteColumnDefinition(column tableColumnInput, strict bool, autoincrement bool) (string, error) {
	if !sqlitedb.ValidName(column.Name) || !sqliteType(column.Type, strict) {
		return "", errors.New("choose a valid SQLite column name and type")
	}
	result := sqlitedb.Quote(column.Name) + " " + strings.ToUpper(column.Type)
	if column.Primary && autoincrement {
		result += " PRIMARY KEY AUTOINCREMENT"
	}
	if !column.Nullable && !column.Primary {
		result += " NOT NULL"
	}
	if column.Unique && column.Primary {
		return "", errors.New("a primary key does not need a separate UNIQUE constraint")
	}
	if column.Unique {
		result += " UNIQUE"
	}
	if column.ReferenceTable != "" || column.ReferenceColumn != "" {
		if !sqlitedb.ValidName(column.ReferenceTable) || !sqlitedb.ValidName(column.ReferenceColumn) {
			return "", errors.New("choose a valid referenced table and column")
		}
		result += " REFERENCES " + sqlitedb.Quote(column.ReferenceTable) + "(" + sqlitedb.Quote(column.ReferenceColumn) + ")"
	}
	if column.Check != "" {
		if len(column.Check) > 2048 || !safeCheckExpression(column.Check) {
			return "", errors.New("enter a safe CHECK expression")
		}
		result += " CHECK (" + column.Check + ")"
	}
	if column.DefaultExpression && column.Default == nil {
		return "", errors.New("enter a default expression")
	}
	if column.Default != nil {
		value := ""
		var err error
		if column.DefaultExpression {
			if len(*column.Default) > 2048 || !safeCheckExpression(*column.Default) {
				return "", errors.New("enter a safe default expression")
			}
			value = *column.Default
			if !slices.Contains([]string{"CURRENT_TIME", "CURRENT_DATE", "CURRENT_TIMESTAMP"}, strings.ToUpper(value)) {
				value = "(" + value + ")"
			}
		} else if strings.EqualFold(column.Type, "BLOB") && strings.HasPrefix(*column.Default, `\x`) {
			blob, decodeErr := hex.DecodeString((*column.Default)[2:])
			if decodeErr != nil {
				return "", errors.New("BLOB default must contain valid hexadecimal bytes")
			}
			value = "X'" + strings.ToUpper(hex.EncodeToString(blob)) + "'"
		} else {
			value, err = sqliteLiteral(*column.Default)
			if err != nil {
				return "", err
			}
		}
		result += " DEFAULT " + value
	}
	return result, nil
}

func (a *application) handleSQLiteTable(w http.ResponseWriter, r *http.Request) {
	var input struct {
		tableChange
		Strict       bool `json:"strict"`
		WithoutRowID bool `json:"withoutRowid"`
	}
	if !readJSON(w, r, &input) {
		return
	}
	if !sqlitedb.ValidName(input.Database) || !sqlitedb.ValidName(input.Table) || input.Schema != "" || r.Method == http.MethodPatch && !sqlitedb.ValidName(input.NewName) {
		writeError(w, 400, "invalid_table", "Choose a SQLite table and name.")
		return
	}
	statement := ""
	if r.Method == http.MethodPost {
		if len(input.Columns) < 1 || len(input.Columns) > 32 {
			writeError(w, 400, "invalid_table", "Choose one to thirty-two columns.")
			return
		}
		seen := map[string]bool{}
		definitions := []string{}
		primary := []string{}
		auto := false
		for _, column := range input.Columns {
			key := strings.ToLower(column.Name)
			if seen[key] {
				writeError(w, 400, "invalid_table", "Column names must be unique.")
				return
			}
			seen[key] = true
			if column.Primary {
				primary = append(primary, sqlitedb.Quote(column.Name))
			}
			if column.AutoIncrement {
				auto = true
			}
		}
		if auto && (len(primary) != 1 || input.WithoutRowID) {
			writeError(w, 400, "invalid_table", "AUTOINCREMENT requires one INTEGER PRIMARY KEY in a rowid table.")
			return
		}
		if input.WithoutRowID && len(primary) == 0 {
			writeError(w, 400, "invalid_table", "WITHOUT ROWID requires a primary key.")
			return
		}
		for _, column := range input.Columns {
			if column.AutoIncrement && (!column.Primary || !strings.EqualFold(column.Type, "INTEGER") || column.Default != nil) {
				writeError(w, 400, "invalid_table", "AUTOINCREMENT requires INTEGER PRIMARY KEY without a default.")
				return
			}
			definition, err := sqliteColumnDefinition(column, input.Strict, column.AutoIncrement)
			if err != nil {
				writeError(w, 400, "invalid_column", err.Error())
				return
			}
			definitions = append(definitions, definition)
		}
		if len(primary) > 0 && !auto {
			definitions = append(definitions, "PRIMARY KEY ("+strings.Join(primary, ",")+")")
		}
		statement = "CREATE TABLE main." + sqlitedb.Quote(input.Table) + " (" + strings.Join(definitions, ",") + ")"
		if input.WithoutRowID && input.Strict {
			statement += " WITHOUT ROWID, STRICT"
		} else if input.WithoutRowID {
			statement += " WITHOUT ROWID"
		} else if input.Strict {
			statement += " STRICT"
		}
	}
	conn, ctx, cleanup := a.openSQLiteRequest(w, r, input.Database, false)
	if conn == nil {
		return
	}
	defer cleanup()
	if r.Method == http.MethodPost {
		for _, column := range input.Columns {
			if !sqliteReferenceValid(ctx, conn, input.Table, input.Columns, column) {
				writeError(w, 400, "invalid_reference", "A referenced column must exist and have a single-column PRIMARY KEY or UNIQUE constraint.")
				return
			}
		}
		if _, err := conn.ExecContext(ctx, "BEGIN IMMEDIATE"); err != nil {
			writeSQLiteError(w, err)
			return
		}
		defer conn.ExecContext(context.Background(), "ROLLBACK")
		if _, err := conn.ExecContext(ctx, statement); err != nil {
			writeSQLiteError(w, err)
			return
		}
		check, err := conn.QueryContext(ctx, "PRAGMA main.foreign_key_check("+sqlitedb.Quote(input.Table)+")")
		if err != nil {
			writeSQLiteError(w, err)
			return
		}
		if check.Next() {
			check.Close()
			writeError(w, 400, "invalid_reference", "The new table has a foreign key violation.")
			return
		}
		err = check.Err()
		check.Close()
		if err != nil {
			writeSQLiteError(w, err)
			return
		}
		if _, err = conn.ExecContext(ctx, "COMMIT"); err != nil {
			writeSQLiteError(w, err)
			return
		}
		writeJSON(w, 200, map[string]string{"status": "ok", "sql": statement + ";"})
		return
	}
	if r.Method != http.MethodPost {
		meta, err := readSQLiteMeta(ctx, conn, input.Table)
		if err != nil {
			writeSQLiteError(w, err)
			return
		}
		if meta.Kind != "table" {
			writeError(w, 400, "invalid_table", "Only tables can be changed here.")
			return
		}
		if r.Method == http.MethodPatch {
			statement = "ALTER TABLE main." + sqlitedb.Quote(input.Table) + " RENAME TO " + sqlitedb.Quote(input.NewName)
		} else {
			statement = "DROP TABLE main." + sqlitedb.Quote(input.Table)
		}
	}
	if _, err := conn.ExecContext(ctx, statement); err != nil {
		writeSQLiteError(w, err)
		return
	}
	writeJSON(w, 200, map[string]string{"status": "ok", "sql": statement + ";"})
}

func (a *application) handleSQLiteTableColumn(w http.ResponseWriter, r *http.Request) {
	var input tableColumnChange
	if !readJSON(w, r, &input) {
		return
	}
	if !sqlitedb.ValidName(input.Database) || !sqlitedb.ValidName(input.Table) || input.Schema != "" {
		writeError(w, 400, "invalid_table", "Choose a SQLite table.")
		return
	}
	conn, ctx, cleanup := a.openSQLiteRequest(w, r, input.Database, false)
	if conn == nil {
		return
	}
	defer cleanup()
	meta, err := readSQLiteMeta(ctx, conn, input.Table)
	if err != nil {
		writeSQLiteError(w, err)
		return
	}
	if meta.Kind != "table" {
		writeError(w, 400, "invalid_table", "Columns can only be changed on tables.")
		return
	}
	statement := "ALTER TABLE main." + sqlitedb.Quote(input.Table) + " "
	switch r.Method {
	case http.MethodPost:
		if !sqlitedb.ValidName(input.Name) || !sqliteType(input.Type, meta.Strict) || input.Nullable == nil {
			writeError(w, 400, "invalid_column", "Choose a SQLite name, type, and nullable setting.")
			return
		}
		column := tableColumnInput{Name: input.Name, Type: input.Type, Nullable: *input.Nullable, Default: input.Default, DefaultExpression: input.DefaultExpression}
		definition, err := sqliteColumnDefinition(column, meta.Strict, false)
		if err != nil {
			writeError(w, 400, "invalid_column", err.Error())
			return
		}
		statement += "ADD COLUMN " + definition
	case http.MethodPatch:
		if !sqlitedb.ValidName(input.OldName) || !sqlitedb.ValidName(input.Name) {
			writeError(w, 400, "invalid_column", "Choose an existing column and a valid name.")
			return
		}
		if input.Type != "" || input.ChangeDefault || input.Nullable != nil {
			a.handleSQLiteColumnRebuild(w, ctx, conn, meta, input)
			return
		}
		if input.PreviewOnly {
			writeJSON(w, 200, map[string]any{"sql": statement + "RENAME COLUMN " + sqlitedb.Quote(input.OldName) + " TO " + sqlitedb.Quote(input.Name) + ";", "objects": []string{}})
			return
		}
		statement += "RENAME COLUMN " + sqlitedb.Quote(input.OldName) + " TO " + sqlitedb.Quote(input.Name)
	case http.MethodDelete:
		if !sqlitedb.ValidName(input.Name) {
			writeError(w, 400, "invalid_column", "Choose a column.")
			return
		}
		statement += "DROP COLUMN " + sqlitedb.Quote(input.Name)
	}
	if _, err := conn.ExecContext(ctx, statement); err != nil {
		writeSQLiteError(w, err)
		return
	}
	writeJSON(w, 200, map[string]string{"status": "ok", "sql": statement + ";"})
}

func (a *application) handleSQLiteIndex(w http.ResponseWriter, r *http.Request) {
	var input schemaChange
	if !readJSON(w, r, &input) {
		return
	}
	if !sqlitedb.ValidName(input.Database) || !sqlitedb.ValidName(input.Table) || input.Schema != "" || !sqlitedb.ValidName(input.Name) {
		writeError(w, 400, "invalid_index", "Choose an index and table.")
		return
	}
	conn, ctx, cleanup := a.openSQLiteRequest(w, r, input.Database, false)
	if conn == nil {
		return
	}
	defer cleanup()
	meta, err := readSQLiteMeta(ctx, conn, input.Table)
	if err != nil {
		writeSQLiteError(w, err)
		return
	}
	if meta.Kind != "table" {
		writeError(w, 400, "invalid_index", "Indexes can only be changed on tables.")
		return
	}
	statement := ""
	if r.Method == http.MethodDelete {
		index := slices.IndexFunc(meta.Indexes, func(item tableIndex) bool { return item.Name == input.Name })
		if index < 0 || !meta.Indexes[index].Editable {
			writeError(w, 403, "managed_index", "Only user-created indexes can be deleted.")
			return
		}
		statement = "DROP INDEX main." + sqlitedb.Quote(input.Name)
	} else if r.Method == http.MethodPost {
		if len(input.Columns) < 1 || len(input.Columns) > 16 || input.Expression != "" {
			writeError(w, 400, "invalid_index", "Choose one to sixteen columns. Expression indexes are not available in this form.")
			return
		}
		if len(input.Directions) != 0 && len(input.Directions) != len(input.Columns) {
			writeError(w, 400, "invalid_index", "Choose one direction for each index column.")
			return
		}
		selected := make([]string, len(input.Columns))
		seen := map[string]bool{}
		for i, name := range input.Columns {
			if seen[name] || !slices.ContainsFunc(meta.Columns, func(c tableColumn) bool { return c.Name == name && c.Key != "ROWID" }) {
				writeError(w, 400, "invalid_index", "Choose distinct existing columns.")
				return
			}
			seen[name] = true
			direction := ""
			if len(input.Directions) != 0 {
				direction = strings.ToUpper(input.Directions[i])
			}
			if direction != "" && direction != "ASC" && direction != "DESC" {
				writeError(w, 400, "invalid_index", "Index order must be default, ASC, or DESC.")
				return
			}
			selected[i] = sqlitedb.Quote(name)
			if direction != "" {
				selected[i] += " " + direction
			}
		}
		unique := ""
		if input.Unique {
			unique = "UNIQUE "
		}
		statement = "CREATE " + unique + "INDEX main." + sqlitedb.Quote(input.Name) + " ON " + sqlitedb.Quote(input.Table) + " (" + strings.Join(selected, ",") + ")"
	} else {
		writeError(w, 400, "sqlite_rebuild_required", "SQLite indexes cannot be edited in place. Delete and recreate this index.")
		return
	}
	if r.Method == http.MethodDelete {
		if _, err := conn.ExecContext(ctx, "BEGIN IMMEDIATE"); err != nil {
			writeSQLiteError(w, err)
			return
		}
		defer conn.ExecContext(context.Background(), "ROLLBACK")
	}
	if _, err := conn.ExecContext(ctx, statement); err != nil {
		writeSQLiteError(w, err)
		return
	}
	if r.Method == http.MethodDelete {
		check, err := conn.QueryContext(ctx, "PRAGMA main.foreign_key_check")
		if err != nil {
			writeSQLiteError(w, err)
			return
		}
		broken := check.Next()
		err = check.Err()
		check.Close()
		if err != nil {
			writeSQLiteError(w, err)
			return
		}
		if broken {
			writeError(w, 400, "sqlite_foreign_key_violation", "This index is required by a foreign key.")
			return
		}
		if _, err := conn.ExecContext(ctx, "COMMIT"); err != nil {
			writeSQLiteError(w, err)
			return
		}
	}
	writeJSON(w, 200, map[string]string{"status": "ok", "sql": statement + ";"})
}

func (a *application) handleSQLiteConstraint(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeError(w, 400, "sqlite_rebuild_required", "Existing SQLite constraints need a separate table migration.")
		return
	}
	var input schemaChange
	if !readJSON(w, r, &input) {
		return
	}
	if !sqlitedb.ValidName(input.Database) || !sqlitedb.ValidName(input.Table) || input.Schema != "" || !sqlitedb.ValidName(input.Name) {
		writeError(w, 400, "invalid_constraint", "Choose a valid SQLite table and constraint name.")
		return
	}
	conn, ctx, cleanup := a.openSQLiteRequest(w, r, input.Database, false)
	if conn == nil {
		return
	}
	defer cleanup()
	meta, err := readSQLiteMeta(ctx, conn, input.Table)
	if err != nil {
		writeSQLiteError(w, err)
		return
	}
	if meta.Kind != "table" {
		writeError(w, 400, "invalid_constraint", "Constraints can only be added to tables.")
		return
	}
	parts, tail, err := sqlitedb.TableParts(meta.Definition)
	if err != nil {
		writeError(w, 400, "sqlite_rebuild_unsafe", err.Error())
		return
	}
	for _, part := range parts {
		tokens, parseErr := sqlitedb.DDLTokens(part)
		if parseErr == nil {
			for i := 0; i+1 < len(tokens); i++ {
				if strings.EqualFold(tokens[i].Text, "CONSTRAINT") && strings.EqualFold(sqlitedb.DDLName(tokens[i+1].Text), input.Name) {
					writeError(w, 400, "invalid_constraint", "A constraint with this name already exists.")
					return
				}
			}
		}
	}
	selected := []string{}
	if input.Type != "CHECK" {
		if len(input.Columns) < 1 || len(input.Columns) > 16 {
			writeError(w, 400, "invalid_constraint", "Choose one to sixteen columns.")
			return
		}
		seen := map[string]bool{}
		for _, name := range input.Columns {
			key := strings.ToLower(name)
			if seen[key] || !slices.ContainsFunc(meta.Columns, func(c tableColumn) bool {
				return strings.EqualFold(c.Name, name) && c.Key != "ROWID" && (input.Type != "PRIMARY KEY" || !c.Hidden)
			}) {
				writeError(w, 400, "invalid_constraint", "Choose distinct existing columns.")
				return
			}
			seen[key] = true
			selected = append(selected, sqlitedb.Quote(name))
		}
	}
	clause := "CONSTRAINT " + sqlitedb.Quote(input.Name) + " "
	switch input.Type {
	case "UNIQUE":
		clause += "UNIQUE (" + strings.Join(selected, ", ") + ")"
	case "PRIMARY KEY":
		if slices.ContainsFunc(meta.Constraints, func(c tableConstraint) bool { return c.Type == "PRIMARY KEY" }) {
			writeError(w, 400, "invalid_constraint", "This table already has a primary key.")
			return
		}
		clause += "PRIMARY KEY (" + strings.Join(selected, ", ") + ")"
	case "CHECK":
		if len(input.Expression) == 0 || len(input.Expression) > 2048 || !safeCheckExpression(input.Expression) {
			writeError(w, 400, "invalid_constraint", "Enter a valid CHECK expression.")
			return
		}
		clause += "CHECK (" + input.Expression + ")"
	case "FOREIGN KEY":
		if input.ReferenceSchema != "" || !sqlitedb.ValidName(input.ReferenceTable) || len(input.ReferenceColumns) != len(input.Columns) {
			writeError(w, 400, "invalid_reference", "Choose matching referenced columns in this database.")
			return
		}
		parent := meta
		if !strings.EqualFold(input.ReferenceTable, input.Table) {
			parent, err = readSQLiteMeta(ctx, conn, input.ReferenceTable)
			if err != nil {
				writeError(w, 400, "invalid_reference", "Referenced table was not found.")
				return
			}
		}
		if parent.Kind != "table" {
			writeError(w, 400, "invalid_reference", "Referenced object must be a table.")
			return
		}
		references := make([]string, len(input.ReferenceColumns))
		seen := map[string]bool{}
		for i, name := range input.ReferenceColumns {
			key := strings.ToLower(name)
			if seen[key] || !slices.ContainsFunc(parent.Columns, func(c tableColumn) bool { return strings.EqualFold(c.Name, name) && c.Key != "ROWID" }) {
				writeError(w, 400, "invalid_reference", "Choose distinct existing referenced columns.")
				return
			}
			seen[key] = true
			references[i] = sqlitedb.Quote(name)
		}
		validKey := slices.ContainsFunc(parent.Constraints, func(c tableConstraint) bool {
			return c.Type == "PRIMARY KEY" && slices.EqualFunc(c.Columns, input.ReferenceColumns, strings.EqualFold)
		})
		for _, index := range parent.Indexes {
			if index.Unique && !index.Partial && !index.Expression && slices.EqualFunc(index.Columns, input.ReferenceColumns, strings.EqualFold) {
				validKey = true
			}
		}
		if !validKey {
			writeError(w, 400, "invalid_reference", "Referenced columns must form a primary or unique key.")
			return
		}
		clause += "FOREIGN KEY (" + strings.Join(selected, ", ") + ") REFERENCES " + sqlitedb.Quote(input.ReferenceTable) + " (" + strings.Join(references, ", ") + ")"
	default:
		writeError(w, 400, "invalid_constraint", "Choose UNIQUE, CHECK, FOREIGN KEY, or PRIMARY KEY.")
		return
	}
	temporary, err := sqliteRebuildName(input.RebuildName)
	if err != nil {
		writeError(w, 400, "invalid_rebuild_name", err.Error())
		return
	}
	create := "CREATE TABLE main." + sqlitedb.Quote(temporary) + " (" + strings.Join(append(parts, clause), ", ") + ")" + tail
	a.runSQLiteRebuild(w, ctx, conn, meta, sqliteRebuildRequest{input.Table, temporary, input.PreviewOnly, input.PreviewHash}, create)
}

func (a *application) handleSQLiteUnsupported(w http.ResponseWriter, r *http.Request) {
	writeError(w, 400, "sqlite_unsupported", "This server-level or table rebuild operation is not available for SQLite.")
}
