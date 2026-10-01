package main

import (
	"context"
	"database/sql"
	"errors"
	"log"
	"net/http"
	"slices"
	"strconv"
	"strings"

	mysql "github.com/go-sql-driver/mysql"

	mysqldb "gosql/database/mysql"
)

type mysqlCatalog struct {
	Databases    []string          `json:"databases"`
	Tables       []string          `json:"tables,omitempty"`
	Views        []string          `json:"views,omitempty"`
	Functions    []string          `json:"functions,omitempty"`
	Procedures   []string          `json:"procedures,omitempty"`
	Triggers     []schemaTrigger   `json:"triggers,omitempty"`
	TableEngines map[string]string `json:"tableEngines,omitempty"`
}

func validMySQLRequest(w http.ResponseWriter, fields connectionFields, password string) bool {
	if (fields.Engine != "MySQL" && fields.Engine != "MariaDB") || !fields.valid() || fields.Database != "" {
		writeError(w, 400, "invalid_profile", "Enter valid MySQL or MariaDB connection settings.")
		return false
	}
	if fields.SSH {
		writeError(w, 400, "ssh_unsupported", "SSH tunnels are not available for this database yet.")
		return false
	}
	if len(password) > 4096 || strings.ContainsRune(password, 0) {
		writeError(w, 400, "invalid_password", "Invalid database password.")
		return false
	}
	return true
}

func openMySQL(ctx context.Context, fields connectionFields, password, database string, multiStatements bool) (*sql.DB, *sql.Conn, error) {
	return mysqldb.Open(ctx, mysqldb.Config{Host: fields.Host, Port: fields.Port, Username: fields.Username, SSL: fields.SSL}, password, database, multiStatements)
}

func (a *application) handleMySQLTest(w http.ResponseWriter, r *http.Request) {
	if _, ok := a.requireUser(w, r); !ok || !a.beginDatabaseRequest(w) {
		return
	}
	defer func() { <-a.databaseSlots }()
	var input struct {
		connectionFields
		Password string `json:"password"`
	}
	if !readJSON(w, r, &input) || !validMySQLRequest(w, input.connectionFields, input.Password) {
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), mysqldb.RequestTimeout)
	defer cancel()
	db, conn, err := openMySQL(ctx, input.connectionFields, input.Password, "", false)
	if err != nil {
		writeMySQLError(w, err)
		return
	}
	defer db.Close()
	defer conn.Close()
	names, err := mysqldb.ReadDatabases(ctx, conn, input.Engine)
	if err != nil {
		writeMySQLError(w, err)
		return
	}
	writeJSON(w, 200, mysqlCatalog{Databases: names})
}

func (a *application) handleMySQLCatalog(w http.ResponseWriter, r *http.Request) {
	actor, ok := a.requireUser(w, r)
	if !ok || !a.beginDatabaseRequest(w) {
		return
	}
	defer func() { <-a.databaseSlots }()
	if r.Method == http.MethodGet {
		a.handleMySQLDatabaseCatalog(w, r, actor)
		return
	}
	var input struct {
		Password string `json:"password"`
	}
	if !readJSON(w, r, &input) {
		return
	}
	a.store.mu.Lock()
	index := slices.IndexFunc(a.store.data.Connections, func(c connectionProfile) bool {
		return c.ID == r.PathValue("id") && c.OwnerID == actor.ID
	})
	var fields connectionFields
	if index >= 0 {
		fields = a.store.data.Connections[index].connectionFields
	}
	a.store.mu.Unlock()
	if index < 0 {
		writeError(w, 404, "profile_not_found", "Connection profile not found.")
		return
	}
	if !validMySQLRequest(w, fields, input.Password) {
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), mysqldb.RequestTimeout)
	defer cancel()
	db, conn, err := openMySQL(ctx, fields, input.Password, "", false)
	if err != nil {
		writeMySQLError(w, err)
		return
	}
	defer db.Close()
	defer conn.Close()
	names, err := mysqldb.ReadDatabases(ctx, conn, fields.Engine)
	if err != nil {
		writeMySQLError(w, err)
		return
	}
	if !a.saveDatabaseSession(w, r, actor, fields, input.Password, names) {
		return
	}
	writeJSON(w, 200, mysqlCatalog{Databases: names})
}

func (a *application) handleMySQLDatabaseCatalog(w http.ResponseWriter, r *http.Request, actor user) {
	name := r.URL.Query().Get("database")
	session := a.getDatabaseSession(w, r, actor)
	if session == nil {
		return
	}
	if name == "" {
		writeJSON(w, 200, mysqlCatalog{Databases: session.databaseList()})
		return
	}
	if !mysqldb.ValidName(name) {
		writeError(w, 400, "invalid_database", "Choose a database.")
		return
	}
	if !session.hasDatabase(name) {
		writeError(w, 404, "database_not_available", "This database is not available for this connection.")
		return
	}
	select {
	case session.busy <- struct{}{}:
		defer func() { <-session.busy }()
	default:
		writeError(w, 429, "database_busy", "Two database requests are already running. Try again shortly.")
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), mysqldb.RequestTimeout)
	defer cancel()
	stop := context.AfterFunc(session.ctx, cancel)
	defer stop()
	db, conn, err := openMySQL(ctx, session.fields, session.password, name, false)
	if err != nil {
		writeMySQLError(w, err)
		return
	}
	defer db.Close()
	defer conn.Close()
	rows, err := conn.QueryContext(ctx, "SELECT TABLE_NAME,TABLE_TYPE,COALESCE(ENGINE,'') FROM INFORMATION_SCHEMA.TABLES WHERE TABLE_SCHEMA=? ORDER BY TABLE_NAME", name)
	if err != nil {
		writeMySQLError(w, err)
		return
	}
	result := mysqlCatalog{Databases: session.databaseList(), Tables: []string{}, Views: []string{}, Functions: []string{}, Procedures: []string{}, Triggers: []schemaTrigger{}, TableEngines: map[string]string{}}
	for rows.Next() {
		var table, kind, engine string
		if err = rows.Scan(&table, &kind, &engine); err != nil {
			break
		}
		if kind == "VIEW" {
			result.Views = append(result.Views, table)
		} else if kind == "BASE TABLE" {
			result.Tables = append(result.Tables, table)
			result.TableEngines[table] = engine
		}
	}
	if err == nil {
		err = rows.Err()
	}
	rows.Close()
	if err != nil {
		writeMySQLError(w, err)
		return
	}
	routines, err := conn.QueryContext(ctx, "SELECT ROUTINE_NAME,ROUTINE_TYPE FROM INFORMATION_SCHEMA.ROUTINES WHERE ROUTINE_SCHEMA=? ORDER BY ROUTINE_NAME", name)
	if err != nil {
		writeMySQLError(w, err)
		return
	}
	for routines.Next() {
		var routine, kind string
		if err = routines.Scan(&routine, &kind); err != nil {
			break
		}
		if kind == "FUNCTION" {
			result.Functions = append(result.Functions, routine)
		}
		if kind == "PROCEDURE" {
			result.Procedures = append(result.Procedures, routine)
		}
	}
	if err == nil {
		err = routines.Err()
	}
	routines.Close()
	if err != nil {
		writeMySQLError(w, err)
		return
	}
	triggers, err := conn.QueryContext(ctx, "SELECT TRIGGER_NAME,EVENT_OBJECT_TABLE FROM INFORMATION_SCHEMA.TRIGGERS WHERE TRIGGER_SCHEMA=? ORDER BY EVENT_OBJECT_TABLE,TRIGGER_NAME", name)
	if err != nil {
		writeMySQLError(w, err)
		return
	}
	for triggers.Next() {
		var trigger schemaTrigger
		if err = triggers.Scan(&trigger.Name, &trigger.Table); err != nil {
			break
		}
		result.Triggers = append(result.Triggers, trigger)
	}
	if err == nil {
		err = triggers.Err()
	}
	triggers.Close()
	if err != nil {
		writeMySQLError(w, err)
		return
	}
	writeJSON(w, 200, result)
}

func (a *application) handleMySQLRoutine(w http.ResponseWriter, r *http.Request) {
	query := r.URL.Query()
	database, name, kind := query.Get("database"), query.Get("name"), query.Get("kind")
	if !mysqldb.ValidName(database) || !mysqldb.ValidName(name) || (kind != "function" && kind != "procedure") {
		writeError(w, 400, "invalid_routine", "Choose a function or procedure.")
		return
	}
	conn, ctx, cleanup := a.openMySQLRequest(w, r, database, false)
	if conn == nil {
		return
	}
	defer cleanup()
	statement := "SHOW CREATE " + strings.ToUpper(kind) + " " + mysqldb.Identifier(database) + "." + mysqldb.Identifier(name)
	definition, err := readMySQLCreate(ctx, conn, statement, "Create "+kind)
	if err != nil {
		writeMySQLError(w, err)
		return
	}
	if definition == "" {
		writeError(w, 403, "routine_definition_unavailable", "The database account cannot read this routine definition.")
		return
	}
	var returns, deterministic, access, security, comment sql.NullString
	err = conn.QueryRowContext(ctx, `SELECT DTD_IDENTIFIER,IS_DETERMINISTIC,SQL_DATA_ACCESS,SECURITY_TYPE,ROUTINE_COMMENT
		FROM INFORMATION_SCHEMA.ROUTINES WHERE ROUTINE_SCHEMA=? AND ROUTINE_NAME=? AND ROUTINE_TYPE=?`, database, name, strings.ToUpper(kind)).Scan(&returns, &deterministic, &access, &security, &comment)
	if err != nil {
		writeMySQLError(w, err)
		return
	}
	metadata := map[string]string{"Security": security.String, "Deterministic": deterministic.String, "SQL access": access.String}
	if kind == "function" {
		metadata["Returns"] = returns.String
	}
	if comment.String != "" {
		metadata["Comment"] = comment.String
	}
	writeJSON(w, 200, map[string]any{"definition": definition, "metadata": metadata})
}

func (a *application) handleMySQLTrigger(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		a.changeMySQLTrigger(w, r)
		return
	}
	query := r.URL.Query()
	database, table, name := query.Get("database"), query.Get("table"), query.Get("name")
	if !mysqldb.ValidName(database) || !mysqldb.ValidName(table) || !mysqldb.ValidName(name) {
		writeError(w, 400, "invalid_trigger", "Choose a trigger.")
		return
	}
	conn, ctx, cleanup := a.openMySQLRequest(w, r, database, false)
	if conn == nil {
		return
	}
	defer cleanup()
	var timing, event, actualTable string
	err := conn.QueryRowContext(ctx, `SELECT ACTION_TIMING,EVENT_MANIPULATION,EVENT_OBJECT_TABLE FROM INFORMATION_SCHEMA.TRIGGERS
		WHERE TRIGGER_SCHEMA=? AND TRIGGER_NAME=? AND EVENT_OBJECT_TABLE=?`, database, name, table).Scan(&timing, &event, &actualTable)
	if errors.Is(err, sql.ErrNoRows) {
		writeError(w, 404, "trigger_not_found", "This trigger no longer exists. Refresh the sidebar.")
		return
	}
	if err != nil {
		writeMySQLError(w, err)
		return
	}
	definition, err := readMySQLCreate(ctx, conn, "SHOW CREATE TRIGGER "+mysqldb.Identifier(database)+"."+mysqldb.Identifier(name), "SQL Original Statement")
	if err != nil {
		writeMySQLError(w, err)
		return
	}
	if definition == "" {
		writeError(w, 403, "trigger_definition_unavailable", "The database account cannot read this trigger definition.")
		return
	}
	writeJSON(w, 200, map[string]any{"definition": definition, "metadata": map[string]string{"Table": actualTable, "Timing": timing, "Event": event}})
}

func readMySQLCreate(ctx context.Context, conn *sql.Conn, statement, columnName string) (string, error) {
	rows, err := conn.QueryContext(ctx, statement)
	if err != nil {
		return "", err
	}
	defer rows.Close()
	columns, err := rows.Columns()
	if err != nil {
		return "", err
	}
	values := make([]sql.NullString, len(columns))
	fields := make([]any, len(columns))
	for index := range values {
		fields[index] = &values[index]
	}
	if !rows.Next() {
		if err := rows.Err(); err != nil {
			return "", err
		}
		return "", sql.ErrNoRows
	}
	if err := rows.Scan(fields...); err != nil {
		return "", err
	}
	for index, column := range columns {
		if strings.EqualFold(column, columnName) && values[index].Valid {
			return values[index].String, nil
		}
	}
	return "", nil
}

func (a *application) openMySQLRequest(w http.ResponseWriter, r *http.Request, database string, multiStatements bool) (*sql.Conn, context.Context, func()) {
	actor, ok := a.requireUser(w, r)
	if !ok || !a.beginDatabaseRequest(w) {
		return nil, nil, nil
	}
	session := a.getDatabaseSession(w, r, actor)
	if session == nil {
		<-a.databaseSlots
		return nil, nil, nil
	}
	if database != "" && !session.hasDatabase(database) {
		<-a.databaseSlots
		writeError(w, 404, "database_not_available", "This database is not available for this connection.")
		return nil, nil, nil
	}
	select {
	case session.busy <- struct{}{}:
	default:
		<-a.databaseSlots
		writeError(w, 429, "database_busy", "Two database requests are already running. Try again shortly.")
		return nil, nil, nil
	}
	ctx, cancel := context.WithTimeout(r.Context(), mysqldb.RequestTimeout)
	stop := context.AfterFunc(session.ctx, cancel)
	db, conn, err := openMySQL(ctx, session.fields, session.password, database, multiStatements)
	if err != nil {
		stop()
		cancel()
		<-session.busy
		<-a.databaseSlots
		writeMySQLError(w, err)
		return nil, nil, nil
	}
	return conn, ctx, func() {
		conn.Close()
		db.Close()
		stop()
		cancel()
		<-session.busy
		<-a.databaseSlots
	}
}

func writeMySQLError(w http.ResponseWriter, err error) {
	log.Printf("MySQL error: %+v", err)
	if errors.Is(err, mysqldb.ErrMariaDBVersionUnsupported) {
		writeError(w, 400, "mariadb_version_unsupported", mysqldb.ErrMariaDBVersionUnsupported.Error())
		return
	}
	if errors.Is(err, mysqldb.ErrMariaDBProfileRequired) {
		writeError(w, 400, "mariadb_profile_required", mysqldb.ErrMariaDBProfileRequired.Error())
		return
	}
	if errors.Is(err, mysqldb.ErrMySQLProfileRequired) {
		writeError(w, 400, "mysql_profile_required", mysqldb.ErrMySQLProfileRequired.Error())
		return
	}
	if errors.Is(err, mysqldb.ErrMySQLVersionUnsupported) {
		writeError(w, 400, "mysql_version_unsupported", mysqldb.ErrMySQLVersionUnsupported.Error())
		return
	}
	if errors.Is(err, sql.ErrNoRows) {
		writeError(w, 404, "table_not_found", "The table or column is no longer available. Refresh the sidebar.")
		return
	}
	var mysqlError *mysql.MySQLError
	if errors.As(err, &mysqlError) {
		status, code, message := 502, "database_error", "Database error "+strconv.Itoa(int(mysqlError.Number))+": "+mysqlError.Message
		switch mysqlError.Number {
		case 1045:
			status, code, message = 422, "database_auth_failed", "The database server rejected the username or password."
		case 1044, 1142, 1143, 1227:
			status, code, message = 403, "database_permission_denied", "Your database account does not have permission for this operation."
		case 1049:
			status, code, message = 404, "database_not_found", "The database does not exist."
		case 1064:
			status, code, message = 400, "invalid_sql", "SQL syntax error: "+mysqlError.Message
		case 1146:
			status, code, message = 404, "table_not_found", "This table no longer exists. Refresh the sidebar."
		case 1007, 1050, 1060, 1061, 1110:
			status, code, message = 409, "object_exists", "A database, table, column, or index with this name already exists."
		case 1008, 1091:
			status, code, message = 404, "object_not_found", "The database object no longer exists. Refresh the sidebar."
		case 1062:
			status, code, message = 409, "unique_violation", "This value conflicts with a UNIQUE or PRIMARY KEY constraint."
		case 1451, 1452:
			status, code, message = 409, "foreign_key_violation", "This change conflicts with a FOREIGN KEY constraint."
		case 1215, 1822, 3780:
			status, code, message = 400, "invalid_reference", "The foreign key columns, types, or referenced index do not match."
		case 1048:
			status, code, message = 400, "not_null_violation", "This column does not allow NULL."
		case 3819, 4025:
			status, code, message = 400, "check_violation", "This value violates a CHECK constraint."
		case 1205, 3572:
			status, code, message = 409, "table_busy", "The table is busy. Try again when other transactions finish."
		case 1213:
			status, code, message = 409, "deadlock", "This operation conflicted with another transaction. Retry."
		case 1317, 3024:
			status, code, message = 504, "database_timeout", "The database query timed out. Refresh to check the result."
		}
		if len(message) > 500 {
			message = message[:500]
		}
		writeJSON(w, status, struct {
			Error apiError `json:"error"`
		}{apiError{
			Code: code, Message: message, SQLState: string(mysqlError.SQLState[:]), DatabaseMessage: mysqlError.Message,
		}})
		return
	}
	if errors.Is(err, context.DeadlineExceeded) || errors.Is(err, context.Canceled) {
		writeError(w, 504, "database_timeout", "The database request timed out or was cancelled. Refresh to check the result.")
		return
	}
	writeError(w, 502, "database_unavailable", "Could not connect to the database server. Check the host, port, and SSL settings.")
}
