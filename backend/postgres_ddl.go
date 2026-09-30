package main

import (
	"context"
	"errors"
	"log"
	"net/http"
	"slices"
	"strings"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"

	postgresdb "gosql/database/postgres"
)

func buildCreateTableSQL(input tableChange) (string, error) {
	if len(input.Columns) == 0 || len(input.Columns) > 32 {
		return "", errors.New("choose one to thirty-two columns")
	}
	seen := map[string]bool{}
	definitions := make([]string, 0, len(input.Columns)+1)
	primary := []string{}
	for _, column := range input.Columns {
		if !validSchemaName(column.Name) || seen[column.Name] || !postgresdb.ValidColumnType(column.Type) {
			return "", errors.New("choose distinct column names and supported types")
		}
		seen[column.Name] = true
		definition := pgx.Identifier{column.Name}.Sanitize() + " " + column.Type
		if column.Default != nil {
			literal, err := postgresdb.QuotedLiteral(*column.Default)
			if err != nil {
				return "", err
			}
			definition += " DEFAULT " + literal
		}
		if !column.Nullable || column.Primary {
			definition += " NOT NULL"
		}
		definitions = append(definitions, definition)
		if column.Primary {
			primary = append(primary, pgx.Identifier{column.Name}.Sanitize())
		}
	}
	if len(primary) > 0 {
		definitions = append(definitions, "PRIMARY KEY ("+strings.Join(primary, ", ")+")")
	}
	return "CREATE TABLE " + pgx.Identifier{input.Schema, input.Table}.Sanitize() + " (" + strings.Join(definitions, ", ") + ")", nil
}

func writeTableDDLError(w http.ResponseWriter, err error) {
	log.Printf("PostgreSQL DDL error: %+v", err)
	var pgErr *pgconn.PgError
	if errors.As(err, &pgErr) {
		switch pgErr.Code {
		case "42P07", "42701", "42P04":
			writeError(w, 409, "name_exists", "A table, column, or database with this name already exists.")
			return
		case "2BP01":
			writeError(w, 409, "dependent_objects", "Other objects depend on this table or column. Remove those dependencies first.")
			return
		case "55006":
			writeError(w, 409, "database_in_use", "The database is in use. Close its other connections before deleting it.")
			return
		case "23502":
			writeError(w, 409, "null_values", "Existing rows contain NULL. Fill them before making this column required.")
			return
		}
	}
	writeMutationError(w, err)
}

func writeDatabaseDeleteError(w http.ResponseWriter, err error) {
	log.Printf("PostgreSQL delete database error: %+v", err)
	var pgErr *pgconn.PgError
	if !errors.As(err, &pgErr) {
		writePostgresResult(w, postgresResult{}, err)
		return
	}
	status, code, message := 409, "database_delete_failed", pgErr.Message
	switch pgErr.Code {
	case "42501":
		status, code, message = 403, "database_permission_denied", "This PostgreSQL user must own the database to delete it."
	case "55006":
		code, message = "database_in_use", "The database is in use. Close its other connections before deleting it."
	case "3D000":
		status, code, message = 404, "database_not_found", "The database no longer exists. Refresh the sidebar."
	}
	log.Printf("API error: status=%d code=%s message=%q SQLSTATE=%s detail=%q hint=%q", status, code, message, pgErr.Code, pgErr.Detail, pgErr.Hint)
	writeJSON(w, status, struct {
		Error apiError `json:"error"`
	}{apiError{Code: code, Message: message, Detail: pgErr.Detail, SQLState: pgErr.Code, Hint: pgErr.Hint, DatabaseMessage: pgErr.Message}})
}

func writeDatabaseCreateError(w http.ResponseWriter, err error) {
	log.Printf("PostgreSQL create database error: %+v", err)
	var pgErr *pgconn.PgError
	if !errors.As(err, &pgErr) {
		writePostgresResult(w, postgresResult{}, err)
		return
	}
	status, code, message := 409, "database_create_failed", pgErr.Message
	switch pgErr.Code {
	case "42501":
		status, code, message = 403, "database_permission_denied", "This PostgreSQL user needs CREATEDB privilege to create a database."
	case "42P04":
		code, message = "database_exists", "A database with this name already exists."
	}
	log.Printf("API error: status=%d code=%s message=%q SQLSTATE=%s detail=%q hint=%q", status, code, message, pgErr.Code, pgErr.Detail, pgErr.Hint)
	writeJSON(w, status, struct {
		Error apiError `json:"error"`
	}{apiError{Code: code, Message: message, Detail: pgErr.Detail, SQLState: pgErr.Code, Hint: pgErr.Hint, DatabaseMessage: pgErr.Message}})
}

func writeDatabaseRenameError(w http.ResponseWriter, err error) {
	log.Printf("PostgreSQL rename database error: %+v", err)
	var pgErr *pgconn.PgError
	if !errors.As(err, &pgErr) {
		writePostgresResult(w, postgresResult{}, err)
		return
	}
	status, code, message := 409, "database_rename_failed", pgErr.Message
	switch pgErr.Code {
	case "42501":
		status, code, message = 403, "database_permission_denied", "This PostgreSQL user must own the database to rename it."
	case "55006":
		code, message = "database_in_use", "The database is in use. Close its other connections before renaming it."
	case "42P04":
		code, message = "database_exists", "A database with this name already exists."
	case "3D000":
		status, code, message = 404, "database_not_found", "The database no longer exists. Refresh the sidebar."
	}
	log.Printf("API error: status=%d code=%s message=%q SQLSTATE=%s detail=%q hint=%q", status, code, message, pgErr.Code, pgErr.Detail, pgErr.Hint)
	writeJSON(w, status, struct {
		Error apiError `json:"error"`
	}{apiError{Code: code, Message: message, Detail: pgErr.Detail, SQLState: pgErr.Code, Hint: pgErr.Hint, DatabaseMessage: pgErr.Message}})
}

func (a *application) handlePostgresTable(w http.ResponseWriter, r *http.Request) {
	var input tableChange
	if !readJSON(w, r, &input) {
		return
	}
	if !validSchemaName(input.Database) || !validSchemaName(input.Schema) || !validSchemaName(input.Table) || (r.Method == http.MethodPatch && !validSchemaName(input.NewName)) {
		writeError(w, 400, "invalid_table", "Enter a valid database, schema, and table name.")
		return
	}
	var statement string
	var err error
	if r.Method == http.MethodPost {
		statement, err = buildCreateTableSQL(input)
		if err != nil {
			writeError(w, 400, "invalid_table", err.Error())
			return
		}
	}
	conn, ctx, cleanup := a.openPostgresSchemaConnection(w, r, input.Database)
	if conn == nil {
		return
	}
	defer cleanup()
	if r.Method != http.MethodPost {
		var kind string
		err = conn.QueryRow(ctx, `SELECT c.relkind::text FROM pg_catalog.pg_class c JOIN pg_catalog.pg_namespace n ON n.oid=c.relnamespace WHERE n.nspname=$1 AND c.relname=$2`, input.Schema, input.Table).Scan(&kind)
		if errors.Is(err, pgx.ErrNoRows) {
			writeError(w, 404, "table_not_found", "This table is no longer available.")
			return
		}
		if err != nil {
			writeTableDDLError(w, err)
			return
		}
		if kind != "r" && kind != "p" {
			writeError(w, 400, "invalid_table", "Only tables can be renamed or deleted here.")
			return
		}
		table := pgx.Identifier{input.Schema, input.Table}.Sanitize()
		if r.Method == http.MethodPatch {
			statement = "ALTER TABLE " + table + " RENAME TO " + pgx.Identifier{input.NewName}.Sanitize()
		} else {
			statement = "DROP TABLE " + table
		}
	}
	if _, err = conn.Exec(ctx, statement); err != nil {
		writeTableDDLError(w, err)
		return
	}
	writeJSON(w, 200, map[string]string{"status": "ok"})
}

func (a *application) handlePostgresTableColumn(w http.ResponseWriter, r *http.Request) {
	var input tableColumnChange
	if !readJSON(w, r, &input) {
		return
	}
	if !validSchemaName(input.Database) || !validSchemaName(input.Schema) || !validSchemaName(input.Table) || !validSchemaName(input.Name) || (r.Method == http.MethodPatch && !validSchemaName(input.OldName)) || (r.Method == http.MethodPost && !postgresdb.ValidColumnType(input.Type)) || (r.Method == http.MethodPatch && input.Type != "" && (!postgresdb.ValidColumnType(input.Type) || slices.Contains([]string{"smallserial", "serial", "bigserial"}, input.Type))) {
		writeError(w, 400, "invalid_column", "Enter a valid column name and supported type.")
		return
	}
	if input.ChangeDefault && input.Default != nil {
		if _, err := postgresdb.QuotedLiteral(*input.Default); err != nil {
			writeError(w, 400, "invalid_default", err.Error())
			return
		}
	}
	conn, ctx, cleanup := a.openPostgresSchemaConnection(w, r, input.Database)
	if conn == nil {
		return
	}
	defer cleanup()
	var kind string
	if err := conn.QueryRow(ctx, `SELECT c.relkind::text FROM pg_catalog.pg_class c JOIN pg_catalog.pg_namespace n ON n.oid=c.relnamespace WHERE n.nspname=$1 AND c.relname=$2`, input.Schema, input.Table).Scan(&kind); err != nil {
		if !errors.Is(err, pgx.ErrNoRows) {
			writeTableDDLError(w, err)
			return
		}
		writeError(w, 404, "table_not_found", "Choose an existing table.")
		return
	}
	if kind != "r" && kind != "p" {
		writeError(w, 400, "invalid_table", "Columns can only be changed on tables.")
		return
	}
	table := pgx.Identifier{input.Schema, input.Table}.Sanitize()
	name := pgx.Identifier{input.Name}.Sanitize()
	if r.Method == http.MethodPost {
		statement := "ALTER TABLE " + table + " ADD COLUMN " + name + " " + input.Type
		if input.ChangeDefault && input.Default != nil {
			literal, _ := postgresdb.QuotedLiteral(*input.Default)
			statement += " DEFAULT " + literal
		}
		if input.Nullable != nil && !*input.Nullable {
			statement += " NOT NULL"
		}
		if _, err := conn.Exec(ctx, statement); err != nil {
			writeTableDDLError(w, err)
			return
		}
	} else {
		oldName := input.Name
		if r.Method == http.MethodPatch {
			oldName = input.OldName
		}
		columns, err := readSchemaColumns(ctx, conn, input.Schema, input.Table)
		if err != nil {
			writeTableDDLError(w, err)
			return
		}
		if !slices.ContainsFunc(columns, func(column tableColumn) bool { return column.Name == oldName }) {
			writeError(w, 404, "column_not_found", "This column is no longer available.")
			return
		}
		old := pgx.Identifier{oldName}.Sanitize()
		if r.Method == http.MethodDelete {
			if _, err = conn.Exec(ctx, "ALTER TABLE "+table+" DROP COLUMN "+old); err != nil {
				writeTableDDLError(w, err)
				return
			}
		} else {
			tx, err := conn.Begin(ctx)
			if err != nil {
				writeTableDDLError(w, err)
				return
			}
			defer tx.Rollback(context.Background())
			statements := []string{}
			if input.Name != oldName {
				statements = append(statements, "ALTER TABLE "+table+" RENAME COLUMN "+old+" TO "+name)
			}
			if input.ChangeDefault && input.Type != "" {
				statements = append(statements, "ALTER TABLE "+table+" ALTER COLUMN "+name+" DROP DEFAULT")
			}
			if input.Type != "" {
				statements = append(statements, "ALTER TABLE "+table+" ALTER COLUMN "+name+" TYPE "+input.Type)
			}
			if input.Nullable != nil {
				mode := "DROP NOT NULL"
				if !*input.Nullable {
					mode = "SET NOT NULL"
				}
				statements = append(statements, "ALTER TABLE "+table+" ALTER COLUMN "+name+" "+mode)
			}
			if input.ChangeDefault {
				mode := "DROP DEFAULT"
				if input.Default != nil {
					literal, _ := postgresdb.QuotedLiteral(*input.Default)
					mode = "SET DEFAULT " + literal
				}
				statements = append(statements, "ALTER TABLE "+table+" ALTER COLUMN "+name+" "+mode)
			}
			for _, statement := range statements {
				if _, err = tx.Exec(ctx, statement); err != nil {
					writeTableDDLError(w, err)
					return
				}
			}
			if err = tx.Commit(ctx); err != nil {
				writeTableDDLError(w, err)
				return
			}
		}
	}
	writeJSON(w, 200, map[string]string{"status": "ok"})
}

func (a *application) handlePostgresDatabase(w http.ResponseWriter, r *http.Request) {
	var input struct {
		Database string `json:"database"`
		NewName  string `json:"newName"`
	}
	if !readJSON(w, r, &input) {
		return
	}
	if !validSchemaName(input.Database) || (r.Method == http.MethodPatch && !validSchemaName(input.NewName)) {
		writeError(w, 400, "invalid_database", "Choose a database.")
		return
	}
	actor, ok := a.requireUser(w, r)
	if !ok || !a.beginDatabaseRequest(w) {
		return
	}
	defer func() { <-a.databaseSlots }()
	session := a.getDatabaseSession(w, r, actor)
	if session == nil {
		return
	}
	if r.Method != http.MethodPost {
		if !session.hasDatabase(input.Database) {
			writeError(w, 404, "database_not_available", "This database is not available for this connection.")
			return
		}
		if input.Database == session.fields.Database {
			writeError(w, 409, "initial_database", "Change the connection's initial database before renaming or deleting this database.")
			return
		}
		if r.Method == http.MethodPatch && session.hasDatabase(input.NewName) {
			writeError(w, 409, "database_exists", "A database with this name already exists.")
			return
		}
	} else if session.hasDatabase(input.Database) {
		writeError(w, 409, "database_exists", "A database with this name already exists.")
		return
	}
	select {
	case session.busy <- struct{}{}:
		defer func() { <-session.busy }()
	default:
		writeError(w, 429, "database_busy", "Two database requests are already running. Try again shortly.")
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), postgresRequestTimeout)
	defer cancel()
	stop := context.AfterFunc(session.ctx, cancel)
	defer stop()
	fields := session.fields
	conn, err := connectPostgresMode(ctx, fields, session.password, false)
	if err != nil {
		writePostgresResult(w, postgresResult{}, err)
		return
	}
	defer conn.Close(context.Background())
	statement := "CREATE DATABASE " + pgx.Identifier{input.Database}.Sanitize()
	if r.Method == http.MethodDelete {
		statement = "DROP DATABASE " + pgx.Identifier{input.Database}.Sanitize()
	} else if r.Method == http.MethodPatch {
		statement = "ALTER DATABASE " + pgx.Identifier{input.Database}.Sanitize() + " RENAME TO " + pgx.Identifier{input.NewName}.Sanitize()
	}
	if _, err = conn.Exec(ctx, statement); err != nil {
		if r.Method == http.MethodPost {
			writeDatabaseCreateError(w, err)
		} else if r.Method == http.MethodPatch {
			writeDatabaseRenameError(w, err)
		} else {
			writeDatabaseDeleteError(w, err)
		}
		return
	}
	if r.Method == http.MethodPost {
		session.addDatabase(input.Database)
	} else {
		session.removeDatabase(input.Database)
		if r.Method == http.MethodPatch {
			session.addDatabase(input.NewName)
		}
	}
	writeJSON(w, 200, map[string]string{"status": "ok"})
}
