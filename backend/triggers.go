package main

import (
	"database/sql"
	"errors"
	"net/http"
	"regexp"
	"strings"
	"unicode"

	"github.com/jackc/pgx/v5"

	mysqldb "gosql/database/mysql"
	sqlitedb "gosql/database/sqlite"
)

type triggerChange struct {
	Database   string `json:"database"`
	Schema     string `json:"schema"`
	Table      string `json:"table"`
	Name       string `json:"name"`
	Definition string `json:"definition"`
}

var mysqlTriggerHeader = regexp.MustCompile(`(?is)\bTRIGGER\s+(.+?)\s+(?:BEFORE|AFTER)\s+\w+\s+ON\s+(.+?)\s+FOR\s+EACH\s+ROW\b`)

func validTriggerDefinition(definition string, method string) bool {
	if method == http.MethodDelete {
		return true
	}
	definition = strings.TrimSpace(definition)
	if len(definition) == 0 || len(definition) > 50000 || strings.ContainsRune(definition, 0) {
		return false
	}
	words := strings.Fields(definition)
	if len(words) < 2 || !strings.EqualFold(words[0], "CREATE") {
		return false
	}
	return strings.EqualFold(words[1], "TRIGGER") || len(words) > 2 && strings.EqualFold(words[1], "CONSTRAINT") && strings.EqualFold(words[2], "TRIGGER") || len(words) > 3 && strings.EqualFold(words[1], "OR") && strings.EqualFold(words[2], "REPLACE") && strings.EqualFold(words[3], "TRIGGER") || strings.HasPrefix(strings.ToUpper(words[1]), "DEFINER=") && mysqlTriggerHeader.MatchString(definition)
}

func (a *application) changePostgresTrigger(w http.ResponseWriter, r *http.Request) {
	var input triggerChange
	if !readJSON(w, r, &input) {
		return
	}
	if !validSchemaName(input.Database) || !validSchemaName(input.Schema) || !validSchemaName(input.Table) || !validSchemaName(input.Name) || !validTriggerDefinition(input.Definition, r.Method) {
		writeError(w, 400, "invalid_trigger", "Choose a trigger, table, and valid CREATE TRIGGER statement.")
		return
	}
	conn, ctx, cleanup := a.openPostgresConnection(w, r, input.Database, postgresRequestTimeout)
	if conn == nil {
		return
	}
	defer cleanup()
	tx, err := conn.Begin(ctx)
	if err != nil {
		writeMutationError(w, err)
		return
	}
	defer tx.Rollback(ctx)
	drop := "DROP TRIGGER " + (pgx.Identifier{input.Name}).Sanitize() + " ON " + (pgx.Identifier{input.Schema, input.Table}).Sanitize()
	if r.Method != http.MethodPost {
		var exists bool
		err = tx.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM pg_catalog.pg_trigger t
			JOIN pg_catalog.pg_class c ON c.oid=t.tgrelid JOIN pg_catalog.pg_namespace n ON n.oid=c.relnamespace
			WHERE n.nspname=$1 AND c.relname=$2 AND t.tgname=$3 AND NOT t.tgisinternal)`, input.Schema, input.Table, input.Name).Scan(&exists)
		if err != nil {
			writeMutationError(w, err)
			return
		}
		if !exists {
			writeError(w, 404, "trigger_not_found", "This trigger no longer exists. Refresh the sidebar.")
			return
		}
		if _, err = tx.Exec(ctx, drop); err != nil {
			writeMutationError(w, err)
			return
		}
	}
	if r.Method != http.MethodDelete {
		if _, err = tx.Exec(ctx, input.Definition, pgx.QueryExecModeExec); err != nil {
			writeMutationError(w, err)
			return
		}
		var exists bool
		err = tx.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM pg_catalog.pg_trigger t
			JOIN pg_catalog.pg_class c ON c.oid=t.tgrelid JOIN pg_catalog.pg_namespace n ON n.oid=c.relnamespace
			WHERE n.nspname=$1 AND c.relname=$2 AND t.tgname=$3 AND NOT t.tgisinternal)`, input.Schema, input.Table, input.Name).Scan(&exists)
		if err != nil {
			writeMutationError(w, err)
			return
		}
		if !exists {
			writeError(w, 400, "trigger_mismatch", "The SQL must create the selected trigger on the selected table.")
			return
		}
	}
	if err = tx.Commit(ctx); err != nil {
		writeMutationError(w, err)
		return
	}
	writeJSON(w, 200, map[string]string{"status": "ok"})
}

func (a *application) changeMySQLTrigger(w http.ResponseWriter, r *http.Request) {
	var input triggerChange
	if !readJSON(w, r, &input) {
		return
	}
	if !mysqldb.ValidName(input.Database) || !mysqldb.ValidName(input.Table) || !mysqldb.ValidName(input.Name) || !validTriggerDefinition(input.Definition, r.Method) {
		writeError(w, 400, "invalid_trigger", "Choose a trigger, table, and valid CREATE TRIGGER statement.")
		return
	}
	if rejectMySQLSystemDDL(w, input.Database) {
		return
	}
	if r.Method != http.MethodDelete && !mysqlTriggerTarget(input.Definition, input.Database, input.Table, input.Name) {
		writeError(w, 400, "trigger_mismatch", "The SQL must create the selected trigger on the selected table.")
		return
	}
	conn, ctx, cleanup := a.openMySQLRequest(w, r, input.Database, false)
	if conn == nil {
		return
	}
	defer cleanup()
	var original string
	if r.Method != http.MethodPost {
		var actualTable string
		err := conn.QueryRowContext(ctx, `SELECT EVENT_OBJECT_TABLE FROM INFORMATION_SCHEMA.TRIGGERS WHERE TRIGGER_SCHEMA=? AND TRIGGER_NAME=?`, input.Database, input.Name).Scan(&actualTable)
		if errors.Is(err, sql.ErrNoRows) {
			writeError(w, 404, "trigger_not_found", "This trigger no longer exists. Refresh the sidebar.")
			return
		}
		if err != nil {
			writeMySQLError(w, err)
			return
		}
		if actualTable != input.Table {
			writeError(w, 400, "trigger_mismatch", "The selected trigger belongs to another table.")
			return
		}
	}
	if r.Method == http.MethodPatch {
		var err error
		original, err = readMySQLCreate(ctx, conn, "SHOW CREATE TRIGGER "+mysqldb.Identifier(input.Database)+"."+mysqldb.Identifier(input.Name), "SQL Original Statement")
		if err != nil {
			writeMySQLError(w, err)
			return
		}
		if original == "" {
			writeError(w, 403, "trigger_definition_unavailable", "The existing trigger definition is needed to edit it safely.")
			return
		}
	}
	if r.Method != http.MethodPost {
		if _, err := conn.ExecContext(ctx, "DROP TRIGGER "+mysqldb.Identifier(input.Database)+"."+mysqldb.Identifier(input.Name)); err != nil {
			writeMySQLError(w, err)
			return
		}
	}
	if r.Method != http.MethodDelete {
		if _, err := conn.ExecContext(ctx, input.Definition); err != nil {
			if original != "" {
				if _, restoreErr := conn.ExecContext(ctx, original); restoreErr != nil {
					writeError(w, 500, "trigger_restore_failed", "The trigger could not be restored. Check the database before retrying.")
					return
				}
			}
			writeMySQLError(w, err)
			return
		}
		var count int
		err := conn.QueryRowContext(ctx, `SELECT COUNT(*) FROM INFORMATION_SCHEMA.TRIGGERS WHERE TRIGGER_SCHEMA=? AND EVENT_OBJECT_TABLE=? AND TRIGGER_NAME=?`, input.Database, input.Table, input.Name).Scan(&count)
		if err != nil {
			writeMySQLError(w, err)
			return
		}
		if count == 0 {
			writeError(w, 400, "trigger_mismatch", "The SQL must create the selected trigger on the selected table.")
			return
		}
	}
	writeJSON(w, 200, map[string]string{"status": "ok"})
}

func mysqlTriggerTarget(definition, database, table, name string) bool {
	parts := mysqlTriggerHeader.FindStringSubmatch(definition)
	if len(parts) != 3 {
		return false
	}
	triggerName, tableName := strings.TrimSpace(parts[1]), strings.TrimSpace(parts[2])
	return (triggerName == mysqldb.Identifier(name) || triggerName == mysqldb.Identifier(database)+"."+mysqldb.Identifier(name)) &&
		(tableName == mysqldb.Identifier(table) || tableName == mysqldb.Identifier(database)+"."+mysqldb.Identifier(table))
}

func (a *application) changeSQLiteTrigger(w http.ResponseWriter, r *http.Request) {
	var input triggerChange
	if !readJSON(w, r, &input) {
		return
	}
	if !sqlitedb.ValidName(input.Database) || !sqlitedb.ValidName(input.Table) || !sqlitedb.ValidName(input.Name) || !validTriggerDefinition(input.Definition, r.Method) || r.Method != http.MethodDelete && !singleSQLiteTrigger(input.Definition) {
		writeError(w, 400, "invalid_trigger", "Enter one valid CREATE TRIGGER statement without extra SQL.")
		return
	}
	conn, ctx, cleanup := a.openSQLiteRequest(w, r, input.Database, false)
	if conn == nil {
		return
	}
	defer cleanup()
	tx, err := conn.BeginTx(ctx, nil)
	if err != nil {
		writeSQLiteError(w, err)
		return
	}
	defer tx.Rollback()
	if r.Method != http.MethodPost {
		var actualTable string
		err = tx.QueryRowContext(ctx, "SELECT tbl_name FROM main.sqlite_schema WHERE type='trigger' AND name=?", input.Name).Scan(&actualTable)
		if errors.Is(err, sql.ErrNoRows) {
			writeError(w, 404, "trigger_not_found", "This trigger no longer exists. Refresh the sidebar.")
			return
		}
		if err != nil {
			writeSQLiteError(w, err)
			return
		}
		if actualTable != input.Table {
			writeError(w, 400, "trigger_mismatch", "The selected trigger belongs to another table.")
			return
		}
		if _, err = tx.ExecContext(ctx, "DROP TRIGGER main."+sqlitedb.Quote(input.Name)); err != nil {
			writeSQLiteError(w, err)
			return
		}
	}
	if r.Method != http.MethodDelete {
		if _, err = tx.ExecContext(ctx, input.Definition); err != nil {
			writeSQLiteError(w, err)
			return
		}
		var actualTable string
		err = tx.QueryRowContext(ctx, "SELECT tbl_name FROM main.sqlite_schema WHERE type='trigger' AND name=?", input.Name).Scan(&actualTable)
		if errors.Is(err, sql.ErrNoRows) || err == nil && actualTable != input.Table {
			writeError(w, 400, "trigger_mismatch", "The SQL must create the selected trigger on the selected table.")
			return
		}
		if err != nil {
			writeSQLiteError(w, err)
			return
		}
	}
	if err = tx.Commit(); err != nil {
		writeSQLiteError(w, err)
		return
	}
	writeJSON(w, 200, map[string]string{"status": "ok"})
}

// Pemindai membatasi SQLite pada satu CREATE TRIGGER, termasuk semikolon di dalam body.
func singleSQLiteTrigger(definition string) bool {
	statement := strings.TrimSpace(definition)
	words := strings.Fields(statement)
	if len(words) < 2 || !strings.EqualFold(words[0], "CREATE") || !strings.EqualFold(words[1], "TRIGGER") {
		return false
	}
	begin, cases, ended, terminated := false, 0, false, false
	for index := 0; index < len(statement); {
		char := statement[index]
		if unicode.IsSpace(rune(char)) {
			index++
			continue
		}
		if char == '-' && index+1 < len(statement) && statement[index+1] == '-' {
			index += 2
			for index < len(statement) && statement[index] != '\n' {
				index++
			}
			continue
		}
		if char == '/' && index+1 < len(statement) && statement[index+1] == '*' {
			end := strings.Index(statement[index+2:], "*/")
			if end < 0 {
				return false
			}
			index += end + 4
			continue
		}
		if char == '\'' || char == '"' || char == '`' || char == '[' {
			closing := char
			if char == '[' {
				closing = ']'
			}
			index++
			found := false
			for index < len(statement) {
				if statement[index] == closing {
					index++
					if index < len(statement) && statement[index] == closing {
						index++
						continue
					}
					found = true
					break
				}
				index++
			}
			if !found || ended {
				return false
			}
			continue
		}
		if char == ';' {
			if !begin {
				return false
			}
			if ended {
				if terminated {
					return false
				}
				terminated = true
			}
			index++
			continue
		}
		if ended {
			return false
		}
		if char >= 'A' && char <= 'Z' || char >= 'a' && char <= 'z' || char == '_' {
			start := index
			for index < len(statement) && (statement[index] >= 'A' && statement[index] <= 'Z' || statement[index] >= 'a' && statement[index] <= 'z' || statement[index] >= '0' && statement[index] <= '9' || statement[index] == '_') {
				index++
			}
			switch strings.ToUpper(statement[start:index]) {
			case "BEGIN":
				if !begin {
					begin = true
				}
			case "CASE":
				if begin {
					cases++
				}
			case "END":
				if cases > 0 {
					cases--
				} else if begin {
					ended = true
				}
			}
			continue
		}
		index++
	}
	return begin && ended && cases == 0
}
