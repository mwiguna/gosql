package main

import (
	"context"
	"errors"
	"log"
	"net/http"
	"slices"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
)

func validSchemaName(value string) bool {
	return value != "" && len(value) <= 63 && !strings.ContainsRune(value, 0)
}

func safeCheckExpression(value string) bool {
	depth, quote := 0, byte(0)
	for i := 0; i < len(value); i++ {
		char := value[i]
		if quote != 0 {
			if char == quote {
				if i+1 < len(value) && value[i+1] == quote {
					i++
				} else {
					quote = 0
				}
			}
			continue
		}
		if char == '\'' || char == '"' {
			quote = char
			continue
		}
		if char == ';' || char == '$' || i+1 < len(value) && (value[i:i+2] == "--" || value[i:i+2] == "/*" || value[i:i+2] == "*/") {
			return false
		}
		if char == '(' {
			depth++
		}
		if char == ')' {
			depth--
			if depth < 0 {
				return false
			}
		}
	}
	return quote == 0 && depth == 0
}

func (a *application) openPostgresSchemaConnection(w http.ResponseWriter, r *http.Request, databaseName string) (*pgx.Conn, context.Context, func()) {
	return a.openPostgresConnection(w, r, databaseName, postgresRequestTimeout)
}

func (a *application) openPostgresConnection(w http.ResponseWriter, r *http.Request, databaseName string, timeout time.Duration) (*pgx.Conn, context.Context, func()) {
	actor, ok := a.requireUser(w, r)
	if !ok {
		return nil, nil, nil
	}
	if !a.beginDatabaseRequest(w) {
		return nil, nil, nil
	}
	database := a.getDatabaseSession(w, r, actor)
	if database == nil {
		<-a.databaseSlots
		return nil, nil, nil
	}
	if !database.hasDatabase(databaseName) {
		<-a.databaseSlots
		writeError(w, 404, "database_not_available", "This database is not available for this connection.")
		return nil, nil, nil
	}
	select {
	case database.busy <- struct{}{}:
	default:
		<-a.databaseSlots
		writeError(w, 429, "database_busy", "Two database requests are already running. Try again shortly.")
		return nil, nil, nil
	}
	ctx, cancel := context.WithTimeout(r.Context(), timeout)
	stop := context.AfterFunc(database.ctx, cancel)
	fields := database.fields
	fields.Database = databaseName
	conn, err := connectPostgresMode(ctx, fields, database.password, false)
	if err != nil {
		stop()
		cancel()
		<-database.busy
		<-a.databaseSlots
		writePostgresResult(w, postgresResult{}, err)
		return nil, nil, nil
	}
	cleanup := func() { conn.Close(context.Background()); stop(); cancel(); <-database.busy; <-a.databaseSlots }
	return conn, ctx, cleanup
}

func schemaColumns(columns []string, available []tableColumn) (string, error) {
	if len(columns) == 0 || len(columns) > 16 {
		return "", errors.New("choose one to sixteen columns")
	}
	seen := map[string]bool{}
	quoted := make([]string, len(columns))
	for i, column := range columns {
		if !validSchemaName(column) || seen[column] || !slices.ContainsFunc(available, func(item tableColumn) bool { return item.Name == column }) {
			return "", errors.New("choose distinct columns from the table")
		}
		seen[column] = true
		quoted[i] = pgx.Identifier{column}.Sanitize()
	}
	return strings.Join(quoted, ", "), nil
}

func readSchemaColumns(ctx context.Context, conn *pgx.Conn, schema, table string) ([]tableColumn, error) {
	rows, err := conn.Query(ctx, `SELECT a.attname, pg_catalog.format_type(a.atttypid,a.atttypmod) FROM pg_catalog.pg_attribute a JOIN pg_catalog.pg_class c ON c.oid=a.attrelid JOIN pg_catalog.pg_namespace n ON n.oid=c.relnamespace WHERE n.nspname=$1 AND c.relname=$2 AND a.attnum>0 AND NOT a.attisdropped ORDER BY a.attnum`, schema, table)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	result := []tableColumn{}
	for rows.Next() {
		var item tableColumn
		if err = rows.Scan(&item.Name, &item.Type); err != nil {
			return nil, err
		}
		result = append(result, item)
	}
	return result, rows.Err()
}

func (a *application) handlePostgresColumns(w http.ResponseWriter, r *http.Request) {
	query := r.URL.Query()
	database, schema, table := query.Get("database"), query.Get("schema"), query.Get("table")
	if !validSchemaName(database) || !validSchemaName(schema) || !validSchemaName(table) {
		writeError(w, 400, "invalid_table", "Choose a table.")
		return
	}
	conn, ctx, cleanup := a.openPostgresSchemaConnection(w, r, database)
	if conn == nil {
		return
	}
	defer cleanup()
	columns, err := readSchemaColumns(ctx, conn, schema, table)
	if err != nil {
		writeMutationError(w, err)
		return
	}
	writeJSON(w, 200, columns)
}

func (a *application) handlePostgresSchema(w http.ResponseWriter, r *http.Request) {
	var input struct {
		Database string `json:"database"`
		Schema   string `json:"schema"`
		NewName  string `json:"newName"`
	}
	if !readJSON(w, r, &input) {
		return
	}
	if !validSchemaName(input.Database) || !validSchemaName(input.Schema) || (r.Method == http.MethodPatch && !validSchemaName(input.NewName)) {
		writeError(w, 400, "invalid_schema", "Enter a valid database and schema name.")
		return
	}
	conn, ctx, cleanup := a.openPostgresSchemaConnection(w, r, input.Database)
	if conn == nil {
		return
	}
	defer cleanup()
	statement := "CREATE SCHEMA " + pgx.Identifier{input.Schema}.Sanitize()
	if r.Method == http.MethodPatch {
		statement = "ALTER SCHEMA " + pgx.Identifier{input.Schema}.Sanitize() + " RENAME TO " + pgx.Identifier{input.NewName}.Sanitize()
	} else if r.Method == http.MethodDelete {
		statement = "DROP SCHEMA " + pgx.Identifier{input.Schema}.Sanitize()
	}
	if _, err := conn.Exec(ctx, statement); err != nil {
		log.Printf("PostgreSQL schema change error: %+v", err)
		var pgErr *pgconn.PgError
		if !errors.As(err, &pgErr) {
			writePostgresResult(w, postgresResult{}, err)
			return
		}
		status, code, message := 409, "schema_create_failed", pgErr.Message
		switch pgErr.Code {
		case "42501":
			status, code, message = 403, "schema_permission_denied", "This PostgreSQL user does not have permission to change this schema."
		case "42P06":
			code, message = "schema_exists", "A schema with this name already exists."
		case "3F000":
			status, code, message = 404, "schema_not_found", "This schema no longer exists. Refresh the sidebar."
		case "2BP01":
			code, message = "schema_not_empty", "This schema contains objects. Remove them before deleting the schema."
		}
		writeJSON(w, status, struct {
			Error apiError `json:"error"`
		}{apiError{Code: code, Message: message, Detail: pgErr.Detail, SQLState: pgErr.Code, Hint: pgErr.Hint, DatabaseMessage: pgErr.Message}})
		return
	}
	writeJSON(w, 200, map[string]string{"status": "ok"})
}

func buildConstraintSQL(input schemaChange, columns, reference []tableColumn) (string, error) {
	table := pgx.Identifier{input.Schema, input.Table}.Sanitize()
	name := pgx.Identifier{input.Name}.Sanitize()
	switch input.Type {
	case "UNIQUE", "PRIMARY KEY":
		selected, err := schemaColumns(input.Columns, columns)
		if err != nil {
			return "", err
		}
		return "ALTER TABLE " + table + " ADD CONSTRAINT " + name + " " + input.Type + " (" + selected + ")", nil
	case "FOREIGN KEY":
		if !validSchemaName(input.ReferenceSchema) || !validSchemaName(input.ReferenceTable) || len(input.Columns) != len(input.ReferenceColumns) {
			return "", errors.New("complete every foreign key column pair")
		}
		local, err := schemaColumns(input.Columns, columns)
		if err != nil {
			return "", err
		}
		foreign, err := schemaColumns(input.ReferenceColumns, reference)
		if err != nil {
			return "", err
		}
		actions, err := foreignKeyActions(input, true)
		if err != nil {
			return "", err
		}
		return "ALTER TABLE " + table + " ADD CONSTRAINT " + name + " FOREIGN KEY (" + local + ") REFERENCES " + pgx.Identifier{input.ReferenceSchema, input.ReferenceTable}.Sanitize() + " (" + foreign + ")" + actions, nil
	case "CHECK":
		expression := strings.TrimSpace(input.Expression)
		if expression == "" || len(expression) > 2000 || strings.ContainsRune(expression, 0) || !safeCheckExpression(expression) {
			return "", errors.New("enter a valid CHECK expression without extra statements")
		}
		return "ALTER TABLE " + table + " ADD CONSTRAINT " + name + " CHECK (" + expression + ")", nil
	}
	return "", errors.New("unsupported constraint type")
}

func (a *application) handlePostgresConstraint(w http.ResponseWriter, r *http.Request) {
	var input schemaChange
	if !readJSON(w, r, &input) {
		return
	}
	if !validSchemaName(input.Database) || !validSchemaName(input.Schema) || !validSchemaName(input.Table) || !validSchemaName(input.Name) || (r.Method == http.MethodPatch && !input.ValidateOnly && !validSchemaName(input.OldName)) {
		writeError(w, 400, "invalid_constraint", "Enter a valid constraint name and table.")
		return
	}
	conn, ctx, cleanup := a.openPostgresSchemaConnection(w, r, input.Database)
	if conn == nil {
		return
	}
	defer cleanup()
	meta, err := readPostgresTableMeta(ctx, conn, input.Schema, input.Table)
	if err != nil {
		writeMutationError(w, err)
		return
	}
	if meta.Kind != "r" && meta.Kind != "p" {
		writeError(w, 400, "invalid_table", "Constraints can only be changed on tables.")
		return
	}
	if input.ValidateOnly {
		if r.Method != http.MethodPatch || !slices.ContainsFunc(meta.Constraints, func(item tableConstraint) bool {
			return item.Name == input.Name && item.Type == "FOREIGN KEY" && !item.Validated
		}) {
			writeError(w, 404, "constraint_not_available", "Choose an unvalidated foreign key. Refresh the table.")
			return
		}
		if _, err := conn.Exec(ctx, "SET lock_timeout = '2s'"); err != nil {
			writeMutationError(w, err)
			return
		}
		if _, err := conn.Exec(ctx, "ALTER TABLE "+pgx.Identifier{input.Schema, input.Table}.Sanitize()+" VALIDATE CONSTRAINT "+pgx.Identifier{input.Name}.Sanitize()); err != nil {
			writeMutationError(w, err)
			return
		}
		writeJSON(w, 200, map[string]string{"status": "ok"})
		return
	}
	var statement string
	if r.Method != http.MethodDelete {
		columns, readErr := readSchemaColumns(ctx, conn, input.Schema, input.Table)
		if readErr != nil {
			writeMutationError(w, readErr)
			return
		}
		var reference []tableColumn
		if input.Type == "FOREIGN KEY" {
			reference, readErr = readSchemaColumns(ctx, conn, input.ReferenceSchema, input.ReferenceTable)
			if readErr != nil {
				writeMutationError(w, readErr)
				return
			}
		}
		statement, err = buildConstraintSQL(input, columns, reference)
		if err != nil {
			writeError(w, 400, "invalid_constraint", err.Error())
			return
		}
	}
	oldName := input.Name
	if r.Method == http.MethodPatch {
		oldName = input.OldName
	}
	if r.Method != http.MethodPost && !slices.ContainsFunc(meta.Constraints, func(item tableConstraint) bool {
		return item.Name == oldName && slices.Contains([]string{"UNIQUE", "PRIMARY KEY", "FOREIGN KEY", "CHECK"}, item.Type)
	}) {
		writeError(w, 404, "constraint_not_found", "This constraint is no longer available.")
		return
	}
	tx, err := conn.Begin(ctx)
	if err != nil {
		writeMutationError(w, err)
		return
	}
	defer tx.Rollback(context.Background())
	table := pgx.Identifier{input.Schema, input.Table}.Sanitize()
	if _, err = tx.Exec(ctx, "SET LOCAL lock_timeout = '2s'"); err != nil {
		writeMutationError(w, err)
		return
	}
	if r.Method != http.MethodPost {
		var kind string
		err = tx.QueryRow(ctx, `SELECT con.contype::text FROM pg_catalog.pg_constraint con JOIN pg_catalog.pg_class c ON c.oid=con.conrelid JOIN pg_catalog.pg_namespace n ON n.oid=c.relnamespace WHERE n.nspname=$1 AND c.relname=$2 AND con.conname=$3`, input.Schema, input.Table, oldName).Scan(&kind)
		if err != nil || !slices.Contains([]string{"u", "p", "f", "c"}, kind) {
			writeError(w, 409, "constraint_changed", "This constraint changed. Refresh the table.")
			return
		}
		_, err = tx.Exec(ctx, "ALTER TABLE "+table+" DROP CONSTRAINT "+pgx.Identifier{oldName}.Sanitize())
		if err != nil {
			writeMutationError(w, err)
			return
		}
	}
	if r.Method != http.MethodDelete {
		// Extended protocol menolak beberapa statement dari ekspresi CHECK.
		if input.Type == "FOREIGN KEY" {
			statement += " NOT VALID"
		}
		_, err = tx.Exec(ctx, statement, pgx.QueryExecModeExec)
		if err != nil {
			writeMutationError(w, err)
			return
		}
	}
	if err = tx.Commit(ctx); err != nil {
		writeMutationError(w, err)
		return
	}
	if r.Method != http.MethodDelete && input.Type == "FOREIGN KEY" {
		// Validasi sesudah commit tidak mempertahankan lock penambahan FK selama pemindaian tabel.
		_, err = conn.Exec(ctx, "SET lock_timeout = '2s'")
		if err == nil {
			_, err = conn.Exec(ctx, "ALTER TABLE "+table+" VALIDATE CONSTRAINT "+pgx.Identifier{input.Name}.Sanitize())
		}
		if err != nil {
			writeError(w, 409, "foreign_key_unvalidated", "The foreign key was added, but existing rows could not be validated. It remains active for new changes; refresh the table and fix existing rows or remove the constraint.")
			return
		}
	}
	writeJSON(w, 200, map[string]string{"status": "ok"})
}

func (a *application) handlePostgresIndex(w http.ResponseWriter, r *http.Request) {
	var input schemaChange
	if !readJSON(w, r, &input) {
		return
	}
	if !validSchemaName(input.Database) || !validSchemaName(input.Schema) || !validSchemaName(input.Table) || !validSchemaName(input.Name) || (r.Method == http.MethodPatch && !validSchemaName(input.OldName)) {
		writeError(w, 400, "invalid_index", "Enter a valid index name and table.")
		return
	}
	conn, ctx, cleanup := a.openPostgresSchemaConnection(w, r, input.Database)
	if conn == nil {
		return
	}
	defer cleanup()
	meta, err := readPostgresTableMeta(ctx, conn, input.Schema, input.Table)
	if err != nil {
		writeMutationError(w, err)
		return
	}
	if meta.Kind != "r" && meta.Kind != "p" {
		writeError(w, 400, "invalid_table", "Indexes can only be changed on tables.")
		return
	}
	var statement string
	if r.Method != http.MethodDelete {
		columns, readErr := readSchemaColumns(ctx, conn, input.Schema, input.Table)
		if readErr != nil {
			writeMutationError(w, readErr)
			return
		}
		selected, selectErr := schemaColumns(input.Columns, columns)
		if selectErr != nil {
			writeError(w, 400, "invalid_index", selectErr.Error())
			return
		}
		prefix := "CREATE INDEX "
		if input.Unique {
			prefix = "CREATE UNIQUE INDEX "
		}
		statement = prefix + pgx.Identifier{input.Name}.Sanitize() + " ON " + pgx.Identifier{input.Schema, input.Table}.Sanitize() + " USING btree (" + selected + ")"
	}
	oldName := input.Name
	if r.Method == http.MethodPatch {
		oldName = input.OldName
	}
	if r.Method != http.MethodPost && !slices.ContainsFunc(meta.Indexes, func(item tableIndex) bool {
		return item.Name == oldName && !item.Managed && !item.Primary && item.Editable
	}) {
		writeError(w, 404, "index_not_editable", "This index is unavailable or managed by a constraint.")
		return
	}
	if _, err = conn.Exec(ctx, "SET lock_timeout = '2s'"); err != nil {
		writeMutationError(w, err)
		return
	}
	if meta.Kind == "r" && r.Method != http.MethodPatch {
		if r.Method == http.MethodPost {
			statement = strings.Replace(statement, " INDEX ", " INDEX CONCURRENTLY ", 1)
		} else {
			statement = "DROP INDEX CONCURRENTLY " + pgx.Identifier{input.Schema, oldName}.Sanitize()
		}
		if _, err = conn.Exec(ctx, statement); err != nil {
			writeMutationError(w, err)
			return
		}
		writeJSON(w, 200, map[string]string{"status": "ok"})
		return
	}
	tx, err := conn.Begin(ctx)
	if err != nil {
		writeMutationError(w, err)
		return
	}
	defer tx.Rollback(context.Background())
	if r.Method != http.MethodPost {
		_, err = tx.Exec(ctx, "DROP INDEX "+pgx.Identifier{input.Schema, oldName}.Sanitize())
		if err != nil {
			writeMutationError(w, err)
			return
		}
	}
	if r.Method != http.MethodDelete {
		if _, err = tx.Exec(ctx, statement); err != nil {
			writeMutationError(w, err)
			return
		}
	}
	if err = tx.Commit(ctx); err != nil {
		writeMutationError(w, err)
		return
	}
	writeJSON(w, 200, map[string]string{"status": "ok"})
}
