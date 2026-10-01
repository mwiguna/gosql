package main

import (
	"database/sql"
	"errors"
	"net/http"
	"regexp"
	"slices"
	"strconv"
	"strings"

	mysqldb "gosql/database/mysql"
)

var mysqlTypePattern = regexp.MustCompile(`^((tinyint|smallint|mediumint|int|integer|bigint)(\([1-9][0-9]{0,2}\))?( unsigned)?|float|double|bit(\([1-9][0-9]?\))?|boolean|text|blob|date|time|datetime|timestamp|year|json|char\([1-9][0-9]{0,2}\)|varchar\([1-9][0-9]{0,4}\)|binary\([1-9][0-9]{0,2}\)|varbinary\([1-9][0-9]{0,4}\)|decimal\([1-9][0-9]?(,[0-9]{1,2})?\)( unsigned)?)$`)

func mysqlSystemDatabase(database string) bool {
	return slices.Contains([]string{"mysql", "information_schema", "performance_schema", "sys"}, strings.ToLower(database))
}

func rejectMySQLSystemDDL(w http.ResponseWriter, database string) bool {
	if !mysqlSystemDatabase(database) {
		return false
	}
	writeError(w, 403, "system_database", "System databases cannot be changed from this form.")
	return true
}

func validMySQLType(kind string) bool {
	kind = strings.ToLower(kind)
	if !mysqlTypePattern.MatchString(kind) {
		return false
	}
	kind = strings.TrimSuffix(kind, " unsigned")
	if strings.HasPrefix(kind, "decimal(") {
		parts := strings.Split(strings.TrimSuffix(strings.TrimPrefix(kind, "decimal("), ")"), ",")
		precision, _ := strconv.Atoi(parts[0])
		scale := 0
		if len(parts) > 1 {
			scale, _ = strconv.Atoi(parts[1])
		}
		return precision <= 65 && scale <= 30 && scale <= precision
	}
	if strings.HasPrefix(kind, "bit(") {
		size, _ := strconv.Atoi(kind[4 : len(kind)-1])
		return size <= 64
	}
	if strings.HasPrefix(kind, "varchar(") || strings.HasPrefix(kind, "varbinary(") {
		size, _ := strconv.Atoi(kind[strings.IndexByte(kind, '(')+1 : len(kind)-1])
		return size <= 16383
	}
	if strings.HasPrefix(kind, "char(") || strings.HasPrefix(kind, "binary(") {
		size, _ := strconv.Atoi(kind[strings.IndexByte(kind, '(')+1 : len(kind)-1])
		return size <= 255
	}
	return true
}

func validMySQLTableEngine(engine string) bool {
	return engine == "InnoDB" || engine == "MyISAM" || engine == "Aria"
}

func mysqlColumnDefinition(column tableColumnInput) (string, error) {
	if !mysqldb.ValidName(column.Name) || !validMySQLType(column.Type) {
		return "", errors.New("choose a valid column name and MySQL type")
	}
	integerType := strings.ToLower(column.Type)
	integerType = strings.TrimSuffix(integerType, " unsigned")
	integerType, _, _ = strings.Cut(integerType, "(")
	if column.AutoIncrement && (!column.Primary || column.Default != nil || !slices.Contains([]string{"tinyint", "smallint", "mediumint", "int", "integer", "bigint"}, integerType)) {
		return "", errors.New("AUTO_INCREMENT requires an integer primary key without a default")
	}
	definition := mysqldb.Identifier(column.Name) + " " + column.Type
	if !column.Nullable || column.Primary {
		definition += " NOT NULL"
	} else {
		definition += " NULL"
	}
	if column.Default != nil {
		value, err := mysqldb.Literal(*column.Default)
		if err != nil {
			return "", err
		}
		if kind := strings.ToLower(column.Type); strings.Contains(kind, "text") || strings.Contains(kind, "blob") || kind == "json" {
			definition += " DEFAULT (" + value + ")"
		} else {
			definition += " DEFAULT " + value
		}
	}
	if column.AutoIncrement {
		definition += " AUTO_INCREMENT"
	}
	return definition, nil
}

func mysqlTableName(database, table string) string {
	return mysqldb.Identifier(database) + "." + mysqldb.Identifier(table)
}
func mysqlColumns(input []string, available []mysqlColumn) (string, error) {
	if len(input) < 1 || len(input) > 16 {
		return "", errors.New("choose one to sixteen columns")
	}
	seen := map[string]bool{}
	selected := make([]string, len(input))
	for i, name := range input {
		if seen[name] || !slices.ContainsFunc(available, func(c mysqlColumn) bool { return c.Name == name }) {
			return "", errors.New("choose distinct existing columns")
		}
		seen[name] = true
		selected[i] = mysqldb.Identifier(name)
	}
	return strings.Join(selected, ","), nil
}

func (a *application) handleMySQLColumns(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	database, table := q.Get("database"), q.Get("table")
	if !mysqldb.ValidName(database) || !mysqldb.ValidName(table) || q.Get("schema") != "" {
		writeError(w, 400, "invalid_table", "Choose a table.")
		return
	}
	conn, ctx, cleanup := a.openMySQLRequest(w, r, database, false)
	if conn == nil {
		return
	}
	defer cleanup()
	meta, err := readMySQLMeta(ctx, conn, database, table)
	if err != nil {
		writeMySQLError(w, err)
		return
	}
	result := make([]tableColumn, len(meta.Columns))
	for i, c := range meta.Columns {
		result[i] = c.tableColumn
	}
	writeJSON(w, 200, result)
}

func (a *application) handleMySQLTable(w http.ResponseWriter, r *http.Request) {
	var input tableChange
	if !readJSON(w, r, &input) {
		return
	}
	if !mysqldb.ValidName(input.Database) || !mysqldb.ValidName(input.Table) || input.Schema != "" || r.Method == http.MethodPatch && !mysqldb.ValidName(input.NewName) {
		writeError(w, 400, "invalid_table", "Choose a database and table.")
		return
	}
	if rejectMySQLSystemDDL(w, input.Database) {
		return
	}
	statement := ""
	if r.Method == http.MethodPost {
		if input.Engine == "" {
			input.Engine = "InnoDB"
		}
		if !validMySQLTableEngine(input.Engine) {
			writeError(w, 400, "invalid_engine", "Choose InnoDB, MyISAM, or Aria as the table engine.")
			return
		}
		if len(input.Columns) < 1 || len(input.Columns) > 32 {
			writeError(w, 400, "invalid_table", "Choose one to thirty-two columns.")
			return
		}
		seen := map[string]bool{}
		definitions := []string{}
		primary := []string{}
		autoIncrement := false
		for _, column := range input.Columns {
			if seen[column.Name] {
				writeError(w, 400, "invalid_table", "Column names must be unique.")
				return
			}
			seen[column.Name] = true
			if column.AutoIncrement {
				if autoIncrement {
					writeError(w, 400, "invalid_table", "A table can have only one AUTO_INCREMENT column.")
					return
				}
				autoIncrement = true
			}
			definition, err := mysqlColumnDefinition(column)
			if err != nil {
				writeError(w, 400, "invalid_table", err.Error())
				return
			}
			definitions = append(definitions, definition)
			if column.Primary {
				primary = append(primary, mysqldb.Identifier(column.Name))
			}
		}
		if len(primary) > 0 {
			definitions = append(definitions, "PRIMARY KEY ("+strings.Join(primary, ",")+")")
		}
		statement = "CREATE TABLE " + mysqlTableName(input.Database, input.Table) + " (" + strings.Join(definitions, ",") + ") ENGINE=" + input.Engine
	}
	conn, ctx, cleanup := a.openMySQLRequest(w, r, input.Database, false)
	if conn == nil {
		return
	}
	defer cleanup()
	if input.Engine == "Aria" {
		mariaDB, _, _, _, err := mysqldb.ReadServerVersion(ctx, conn)
		if err != nil {
			writeMySQLError(w, err)
			return
		}
		if !mariaDB {
			writeError(w, 400, "invalid_engine", "Aria is available only on MariaDB servers.")
			return
		}
	}
	if r.Method != http.MethodPost {
		meta, err := readMySQLMeta(ctx, conn, input.Database, input.Table)
		if err != nil {
			writeMySQLError(w, err)
			return
		}
		if meta.Kind != "BASE TABLE" {
			writeError(w, 400, "invalid_table", "Only tables can be changed here.")
			return
		}
		if r.Method == http.MethodPatch {
			if input.Engine != "" && !validMySQLTableEngine(input.Engine) {
				writeError(w, 400, "invalid_engine", "Choose InnoDB, MyISAM, or Aria as the table engine.")
				return
			}
			engineChanged := input.Engine != "" && !strings.EqualFold(input.Engine, meta.Engine)
			nameChanged := input.NewName != input.Table
			if !engineChanged && !nameChanged {
				writeError(w, 400, "unchanged_table", "Change the table name or engine.")
				return
			}
			if engineChanged {
				statement = "ALTER TABLE " + mysqlTableName(input.Database, input.Table) + " ENGINE=" + input.Engine
				if nameChanged {
					statement += ", RENAME TO " + mysqlTableName(input.Database, input.NewName)
				}
			} else {
				statement = "RENAME TABLE " + mysqlTableName(input.Database, input.Table) + " TO " + mysqlTableName(input.Database, input.NewName)
			}
		} else {
			statement = "DROP TABLE " + mysqlTableName(input.Database, input.Table)
		}
	}
	if _, err := conn.ExecContext(ctx, statement); err != nil {
		writeMySQLError(w, err)
		return
	}
	writeJSON(w, 200, map[string]string{"status": "ok", "sql": statement + ";"})
}

func (a *application) handleMySQLTableColumn(w http.ResponseWriter, r *http.Request) {
	var input tableColumnChange
	if !readJSON(w, r, &input) {
		return
	}
	if !mysqldb.ValidName(input.Database) || !mysqldb.ValidName(input.Table) || !mysqldb.ValidName(input.Name) || input.Schema != "" || r.Method == http.MethodPatch && !mysqldb.ValidName(input.OldName) {
		writeError(w, 400, "invalid_column", "Choose a valid column.")
		return
	}
	if rejectMySQLSystemDDL(w, input.Database) {
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
		writeError(w, 400, "invalid_table", "Columns can only be changed on tables.")
		return
	}
	statement := "ALTER TABLE " + mysqlTableName(input.Database, input.Table) + " "
	if r.Method == http.MethodPost {
		if !validMySQLType(input.Type) {
			writeError(w, 400, "invalid_column", "Choose a supported MySQL type.")
			return
		}
		column := tableColumnInput{Name: input.Name, Type: input.Type, Nullable: input.Nullable == nil || *input.Nullable}
		if input.ChangeDefault {
			column.Default = input.Default
		}
		definition, defErr := mysqlColumnDefinition(column)
		if defErr != nil {
			writeError(w, 400, "invalid_column", defErr.Error())
			return
		}
		statement += "ADD COLUMN " + definition
	} else {
		oldName := input.Name
		if r.Method == http.MethodPatch {
			oldName = input.OldName
		}
		old, found := mysqlSelectedColumn(meta, oldName)
		if !found {
			writeError(w, 404, "column_not_found", "This column is no longer available.")
			return
		}
		if r.Method == http.MethodDelete {
			dependent := slices.Contains(meta.PrimaryKey, oldName) ||
				slices.ContainsFunc(meta.Indexes, func(index tableIndex) bool { return slices.Contains(index.Columns, oldName) }) ||
				slices.ContainsFunc(meta.Constraints, func(constraint tableConstraint) bool {
					return slices.Contains(constraint.Columns, oldName) || constraint.Type == "CHECK"
				})
			if dependent {
				writeError(w, 409, "dependent_objects", "An index or constraint may depend on this column. Remove the dependency first or use SQL console.")
				return
			}
			statement += "DROP COLUMN " + mysqldb.Identifier(oldName)
		} else {
			if !old.DefinitionEditable {
				writeError(w, 409, "column_definition_unsupported", "This column has a generated, auto increment, or primary key definition. Use SQL console to edit it safely.")
				return
			}
			kind := old.Type
			if input.Type != "" {
				kind = input.Type
			}
			if !validMySQLType(kind) {
				writeError(w, 400, "column_definition_unsupported", "This type cannot be safely changed from the form.")
				return
			}
			nullable := old.Nullable
			if input.Nullable != nil {
				nullable = *input.Nullable
			}
			defaultValue := old.Default
			if input.ChangeDefault {
				defaultValue = input.Default
			}
			definition, defErr := mysqlColumnDefinition(tableColumnInput{Name: input.Name, Type: kind, Nullable: nullable, Default: defaultValue})
			if defErr != nil {
				writeError(w, 400, "invalid_column", defErr.Error())
				return
			}
			if old.Comment != "" {
				literal, quoteErr := mysqldb.Literal(old.Comment)
				if quoteErr != nil {
					writeError(w, 409, "column_definition_unsupported", "The column comment cannot be preserved by this form.")
					return
				}
				definition += " COMMENT " + literal
			}
			lowerKind := strings.ToLower(kind)
			if old.Collation != "" && (strings.Contains(lowerKind, "char") || strings.Contains(lowerKind, "text") || strings.HasPrefix(lowerKind, "enum") || strings.HasPrefix(lowerKind, "set")) {
				if !mysqldb.ValidName(old.Collation) {
					writeError(w, 409, "column_definition_unsupported", "The column collation cannot be preserved by this form.")
					return
				}
				definition += " COLLATE " + mysqldb.Identifier(old.Collation)
			}
			statement += "CHANGE COLUMN " + mysqldb.Identifier(oldName) + " " + definition
		}
	}
	if _, err := conn.ExecContext(ctx, statement); err != nil {
		writeMySQLError(w, err)
		return
	}
	writeJSON(w, 200, map[string]string{"status": "ok", "sql": statement + ";"})
}

func (a *application) handleMySQLDatabase(w http.ResponseWriter, r *http.Request) {
	var input struct {
		Database string `json:"database"`
		NewName  string `json:"newName"`
	}
	if !readJSON(w, r, &input) {
		return
	}
	if !mysqldb.ValidName(input.Database) {
		writeError(w, 400, "invalid_database", "Choose a database.")
		return
	}
	if rejectMySQLSystemDDL(w, input.Database) {
		return
	}
	if r.Method == http.MethodPatch {
		writeError(w, 400, "database_rename_unsupported", "MySQL cannot rename a database.")
		return
	}
	if r.Method == http.MethodDelete {
		actor, ok := a.requireUser(w, r)
		if !ok {
			return
		}
		session := a.getDatabaseSession(w, r, actor)
		if session == nil {
			return
		}
		if !session.hasDatabase(input.Database) {
			writeError(w, 404, "database_not_available", "This database is not available for this connection.")
			return
		}
	}
	conn, ctx, cleanup := a.openMySQLRequest(w, r, "", false)
	if conn == nil {
		return
	}
	defer cleanup()
	statement := "CREATE DATABASE " + mysqldb.Identifier(input.Database)
	if r.Method == http.MethodDelete {
		statement = "DROP DATABASE " + mysqldb.Identifier(input.Database)
	}
	if _, err := conn.ExecContext(ctx, statement); err != nil {
		writeMySQLError(w, err)
		return
	}
	actor, _ := a.requireUser(w, r)
	session := a.getDatabaseSession(w, r, actor)
	if session != nil {
		if r.Method == http.MethodPost {
			session.addDatabase(input.Database)
		} else {
			session.removeDatabase(input.Database)
		}
	}
	writeJSON(w, 200, map[string]string{"status": "ok", "sql": statement + ";"})
}

func (a *application) handleMySQLIndex(w http.ResponseWriter, r *http.Request) {
	var input schemaChange
	if !readJSON(w, r, &input) {
		return
	}
	if !mysqldb.ValidName(input.Database) || !mysqldb.ValidName(input.Table) || !mysqldb.ValidName(input.Name) || input.Schema != "" || r.Method == http.MethodPatch && !mysqldb.ValidName(input.OldName) {
		writeError(w, 400, "invalid_index", "Choose a valid index and table.")
		return
	}
	if rejectMySQLSystemDDL(w, input.Database) {
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
		writeError(w, 400, "invalid_table", "Indexes can only be changed on tables.")
		return
	}
	oldName := input.Name
	if r.Method == http.MethodPatch {
		oldName = input.OldName
	}
	if r.Method != http.MethodPost && !slices.ContainsFunc(meta.Indexes, func(index tableIndex) bool { return index.Name == oldName && index.Editable && !index.Managed }) {
		writeError(w, 404, "index_not_editable", "This index cannot be changed from the form.")
		return
	}
	table := mysqlTableName(input.Database, input.Table)
	statement := "ALTER TABLE " + table + " "
	if r.Method != http.MethodPost {
		statement += "DROP INDEX " + mysqldb.Identifier(oldName)
	}
	if r.Method != http.MethodDelete {
		columns, colErr := mysqlColumns(input.Columns, meta.Columns)
		if colErr != nil {
			writeError(w, 400, "invalid_index", colErr.Error())
			return
		}
		if r.Method == http.MethodPatch {
			statement += ","
		}
		if input.Unique {
			statement += "ADD UNIQUE INDEX "
		} else {
			statement += "ADD INDEX "
		}
		statement += mysqldb.Identifier(input.Name) + " (" + columns + ")"
	}
	if _, err := conn.ExecContext(ctx, statement); err != nil {
		writeMySQLError(w, err)
		return
	}
	writeJSON(w, 200, map[string]string{"status": "ok", "sql": statement + ";"})
}

func (a *application) handleMySQLConstraint(w http.ResponseWriter, r *http.Request) {
	var input schemaChange
	if !readJSON(w, r, &input) {
		return
	}
	if !mysqldb.ValidName(input.Database) || !mysqldb.ValidName(input.Table) || input.Schema != "" || input.ValidateOnly {
		writeError(w, 400, "invalid_constraint", "Choose a valid table and constraint.")
		return
	}
	if rejectMySQLSystemDDL(w, input.Database) {
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
		writeError(w, 400, "invalid_table", "Constraints can only be changed on tables.")
		return
	}
	mariaDB, _, _, _, err := mysqldb.ReadServerVersion(ctx, conn)
	if err != nil {
		writeMySQLError(w, err)
		return
	}
	statement := "ALTER TABLE " + mysqlTableName(input.Database, input.Table) + " "
	if r.Method != http.MethodPost {
		oldName := input.Name
		if r.Method == http.MethodPatch {
			oldName = input.OldName
		}
		itemIndex := slices.IndexFunc(meta.Constraints, func(c tableConstraint) bool { return c.Name == oldName })
		if itemIndex < 0 {
			writeError(w, 404, "constraint_not_found", "This constraint no longer exists.")
			return
		}
		item := meta.Constraints[itemIndex]
		if r.Method == http.MethodPatch && item.Type != input.Type {
			writeError(w, 400, "constraint_type_change_unsupported", "Keep the constraint type when editing.")
			return
		}
		switch item.Type {
		case "PRIMARY KEY":
			statement += "DROP PRIMARY KEY"
		case "UNIQUE":
			statement += "DROP INDEX " + mysqldb.Identifier(oldName)
		case "FOREIGN KEY":
			statement += "DROP FOREIGN KEY " + mysqldb.Identifier(oldName)
		case "CHECK":
			if mariaDB {
				statement += "DROP CONSTRAINT " + mysqldb.Identifier(oldName)
			} else {
				statement += "DROP CHECK " + mysqldb.Identifier(oldName)
			}
		default:
			writeError(w, 400, "invalid_constraint", "This constraint cannot be changed.")
			return
		}
	}
	if r.Method != http.MethodDelete {
		if r.Method == http.MethodPatch {
			statement += ", "
		}
		if !mysqldb.ValidName(input.Name) && input.Type != "PRIMARY KEY" {
			writeError(w, 400, "invalid_constraint", "Choose a constraint name.")
			return
		}
		switch input.Type {
		case "PRIMARY KEY", "UNIQUE":
			columns, colErr := mysqlColumns(input.Columns, meta.Columns)
			if colErr != nil {
				writeError(w, 400, "invalid_constraint", colErr.Error())
				return
			}
			if input.Type == "PRIMARY KEY" {
				statement += "ADD PRIMARY KEY (" + columns + ")"
			} else {
				statement += "ADD CONSTRAINT " + mysqldb.Identifier(input.Name) + " UNIQUE (" + columns + ")"
			}
		case "FOREIGN KEY":
			actions, actionErr := foreignKeyActions(input, false)
			if actionErr != nil {
				writeError(w, 400, "invalid_foreign_key_action", actionErr.Error())
				return
			}
			if input.ReferenceSchema != "" && input.ReferenceSchema != input.Database {
				writeError(w, 400, "invalid_reference", "Foreign keys must reference the same database.")
				return
			}
			columns, colErr := mysqlColumns(input.Columns, meta.Columns)
			if colErr != nil {
				writeError(w, 400, "invalid_constraint", colErr.Error())
				return
			}
			referenced, metaErr := readMySQLMeta(ctx, conn, input.Database, input.ReferenceTable)
			if metaErr != nil {
				writeMySQLError(w, metaErr)
				return
			}
			reference, refErr := mysqlColumns(input.ReferenceColumns, referenced.Columns)
			if refErr != nil || len(input.Columns) != len(input.ReferenceColumns) {
				writeError(w, 400, "invalid_reference", "Choose matching reference columns.")
				return
			}
			var one int
			checkErr := conn.QueryRowContext(ctx, "SELECT 1 FROM "+mysqlTableName(input.Database, input.Table)+" LIMIT 1").Scan(&one)
			if checkErr == nil {
				writeError(w, 409, "foreign_key_population_requires_manual_sql", "Adding a foreign key to a populated table may copy and lock it. Use SQL console if intended.")
				return
			}
			if !errors.Is(checkErr, sql.ErrNoRows) {
				writeMySQLError(w, checkErr)
				return
			}
			statement += "ADD CONSTRAINT " + mysqldb.Identifier(input.Name) + " FOREIGN KEY (" + columns + ") REFERENCES " + mysqlTableName(input.Database, input.ReferenceTable) + " (" + reference + ")" + actions
		case "CHECK":
			columnNames := make([]string, len(meta.Columns))
			for i, column := range meta.Columns {
				columnNames[i] = column.Name
			}
			expression, checkErr := mysqldb.BuildCheck(input.Expression, columnNames)
			if checkErr != nil {
				writeError(w, 400, "invalid_check", checkErr.Error())
				return
			}
			statement += "ADD CONSTRAINT " + mysqldb.Identifier(input.Name) + " CHECK (" + expression + ")"
		default:
			writeError(w, 400, "invalid_constraint", "Choose a supported constraint type.")
			return
		}
	}
	if _, err := conn.ExecContext(ctx, statement); err != nil {
		writeMySQLError(w, err)
		return
	}
	writeJSON(w, 200, map[string]string{"status": "ok", "sql": statement + ";"})
}
