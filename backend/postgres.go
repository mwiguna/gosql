package main

import (
	"context"
	"encoding/json"
	"errors"
	"log"
	"net"
	"net/http"
	"net/url"
	"slices"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"

	postgresdb "gosql/database/postgres"
)

type postgresSchema struct {
	Name   string   `json:"name"`
	Tables []string `json:"tables"`
	Views  []string `json:"views"`
}

type postgresResult struct {
	Databases []string         `json:"databases,omitempty"`
	Schemas   []postgresSchema `json:"schemas"`
	Types     []string         `json:"types"`
}

var errPostgresPageTooLarge = errors.New("database page exceeds response limit")

const postgresRequestTimeout = 60 * time.Second
const postgresPreviewLength = 512

func (a *application) handlePostgresTest(w http.ResponseWriter, r *http.Request) {
	if _, ok := a.requireUser(w, r); !ok {
		return
	}
	if !a.beginDatabaseRequest(w) {
		return
	}
	defer func() { <-a.databaseSlots }()
	var input struct {
		connectionFields
		Password string `json:"password"`
	}
	if !readJSON(w, r, &input) {
		return
	}
	if !validPostgresRequest(w, input.connectionFields, input.Password) {
		return
	}
	result, err := loadPostgresCatalog(r.Context(), input.connectionFields, input.Password)
	writePostgresResult(w, result, err)
}

func (a *application) handlePostgresCatalog(w http.ResponseWriter, r *http.Request) {
	actor, ok := a.requireUser(w, r)
	if !ok {
		return
	}
	if !a.beginDatabaseRequest(w) {
		return
	}
	defer func() { <-a.databaseSlots }()
	if r.Method == http.MethodGet {
		a.handlePostgresDatabaseCatalog(w, r, actor)
		return
	}
	var input struct {
		Password string `json:"password"`
	}
	if !readJSON(w, r, &input) {
		return
	}
	a.store.mu.Lock()
	if !a.requireUserLocked(w, r) {
		a.store.mu.Unlock()
		return
	}
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
	if !validPostgresRequest(w, fields, input.Password) {
		return
	}
	result, err := loadPostgresCatalog(r.Context(), fields, input.Password)
	if err == nil {
		if !a.saveDatabaseSession(w, r, actor, fields, input.Password, result.Databases) {
			return
		}
	}
	writePostgresResult(w, result, err)
}

func validPostgresRequest(w http.ResponseWriter, fields connectionFields, password string) bool {
	if fields.Engine != "PostgreSQL" || !fields.valid() {
		writeError(w, 400, "invalid_profile", "Enter valid PostgreSQL connection settings.")
		return false
	}
	if fields.SSH {
		writeError(w, 400, "ssh_unsupported", "SSH tunnels are not available for PostgreSQL yet.")
		return false
	}
	if len(password) > 4096 || strings.ContainsRune(password, 0) {
		writeError(w, 400, "invalid_password", "Invalid database password.")
		return false
	}
	return true
}

func loadPostgresCatalog(parent context.Context, fields connectionFields, password string) (postgresResult, error) {
	ctx, cancel := context.WithTimeout(parent, postgresRequestTimeout)
	defer cancel()
	conn, err := connectPostgres(ctx, fields, password)
	if err != nil {
		return postgresResult{}, err
	}
	defer conn.Close(context.Background())
	result, err := readPostgresCatalog(ctx, conn)
	if err != nil {
		return postgresResult{}, err
	}
	result.Databases, err = readPostgresDatabases(ctx, conn)
	return result, err
}

func readPostgresDatabases(ctx context.Context, conn *pgx.Conn) ([]string, error) {
	rows, err := conn.Query(ctx, `SELECT datname FROM pg_catalog.pg_database
		WHERE datallowconn AND NOT datistemplate
		AND pg_catalog.has_database_privilege(oid, 'CONNECT')
		ORDER BY datname`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	databases := []string{}
	for rows.Next() {
		var name string
		if err := rows.Scan(&name); err != nil {
			return nil, err
		}
		databases = append(databases, name)
	}
	return databases, rows.Err()
}

func connectPostgres(ctx context.Context, fields connectionFields, password string) (*pgx.Conn, error) {
	return connectPostgresMode(ctx, fields, password, true)
}

func connectPostgresMode(ctx context.Context, fields connectionFields, password string, readOnly bool) (*pgx.Conn, error) {
	mode := map[string]string{"Disable": "disable", "Prefer": "prefer", "Require": "require", "Verify CA": "verify-ca", "Verify Full": "verify-full"}[fields.SSL]
	address := &url.URL{Scheme: "postgres", User: url.UserPassword(fields.Username, password), Host: net.JoinHostPort(fields.Host, fields.Port), Path: "/" + fields.Database}
	query := address.Query()
	query.Set("sslmode", mode)
	address.RawQuery = query.Encode()
	config, err := pgx.ParseConfig(address.String())
	if err != nil {
		return nil, err
	}
	config.Password = password
	statementTimeout := postgresRequestTimeout
	if deadline, ok := ctx.Deadline(); ok {
		statementTimeout = time.Until(deadline)
	}
	config.RuntimeParams = map[string]string{
		"application_name":  "GoSQL",
		"statement_timeout": strconv.FormatInt(max(1, statementTimeout.Milliseconds()), 10),
	}
	if readOnly {
		config.RuntimeParams["default_transaction_read_only"] = "on"
	}
	return pgx.ConnectConfig(ctx, config)
}

func readPostgresCatalog(ctx context.Context, conn *pgx.Conn) (postgresResult, error) {
	rows, err := conn.Query(ctx, `SELECT n.nspname, c.relname, c.relkind::text
		FROM pg_catalog.pg_namespace n
		LEFT JOIN pg_catalog.pg_class c ON c.relnamespace = n.oid
			AND c.relkind IN ('r', 'p', 'v', 'm', 'f')
		WHERE n.nspname NOT IN ('pg_catalog', 'information_schema')
			AND n.nspname NOT LIKE 'pg_toast%' AND n.nspname NOT LIKE 'pg_temp_%'
			AND pg_catalog.has_schema_privilege(n.oid, 'USAGE')
		ORDER BY n.nspname, c.relname`)
	if err != nil {
		return postgresResult{}, err
	}
	defer rows.Close()
	result := postgresResult{Schemas: []postgresSchema{}, Types: []string{}}
	for rows.Next() {
		var schema string
		var name, kind *string
		if err := rows.Scan(&schema, &name, &kind); err != nil {
			return postgresResult{}, err
		}
		if len(result.Schemas) == 0 || result.Schemas[len(result.Schemas)-1].Name != schema {
			result.Schemas = append(result.Schemas, postgresSchema{Name: schema, Tables: []string{}, Views: []string{}})
		}
		if name == nil || kind == nil {
			continue
		}
		current := &result.Schemas[len(result.Schemas)-1]
		if *kind == "v" || *kind == "m" {
			current.Views = append(current.Views, *name)
		} else {
			current.Tables = append(current.Tables, *name)
		}
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return postgresResult{}, err
	}
	types, err := conn.Query(ctx, `SELECT n.nspname, t.typname FROM pg_catalog.pg_type t
		JOIN pg_catalog.pg_namespace n ON n.oid=t.typnamespace
		WHERE n.nspname NOT IN ('pg_catalog', 'information_schema')
		AND n.nspname NOT LIKE 'pg_toast%' AND n.nspname NOT LIKE 'pg_temp_%'
		AND pg_catalog.has_schema_privilege(n.oid, 'USAGE')
		AND t.typisdefined AND t.typtype IN ('b', 'c', 'd', 'e', 'r', 'm')
		AND t.typname NOT IN ('gtrgm', 'pg_lsn')
		AND left(t.typname,1) <> '_'
		AND (t.typtype <> 'c' OR EXISTS (SELECT 1 FROM pg_catalog.pg_class c WHERE c.oid=t.typrelid AND c.relkind='c'))
		ORDER BY n.nspname, t.typname`)
	if err != nil {
		return postgresResult{}, err
	}
	defer types.Close()
	for types.Next() {
		var schema, name string
		if err := types.Scan(&schema, &name); err != nil {
			return postgresResult{}, err
		}
		result.Types = append(result.Types, pgx.Identifier{schema, name}.Sanitize())
	}
	return result, types.Err()
}

func (a *application) handlePostgresDatabaseCatalog(w http.ResponseWriter, r *http.Request, actor user) {
	name := r.URL.Query().Get("database")
	if name == "" || len(name) > 63 || strings.ContainsRune(name, 0) {
		writeError(w, 400, "invalid_database", "Choose a database from this connection.")
		return
	}
	database := a.getDatabaseSession(w, r, actor)
	if database == nil {
		return
	}
	if !database.hasDatabase(name) {
		writeError(w, 404, "database_not_available", "This database is not available for this connection.")
		return
	}
	select {
	case database.busy <- struct{}{}:
		defer func() { <-database.busy }()
	default:
		writeError(w, 429, "database_busy", "Two database requests are already running. Try again shortly.")
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), postgresRequestTimeout)
	defer cancel()
	stop := context.AfterFunc(database.ctx, cancel)
	defer stop()
	fields := database.fields
	fields.Database = name
	conn, err := connectPostgres(ctx, fields, database.password)
	if err != nil {
		writePostgresResult(w, postgresResult{}, err)
		return
	}
	defer conn.Close(context.Background())
	result, err := readPostgresCatalog(ctx, conn)
	result.Databases = database.databaseList()
	writePostgresResult(w, result, err)
}

func (a *application) handlePostgresRows(w http.ResponseWriter, r *http.Request) {
	actor, ok := a.requireUser(w, r)
	if !ok {
		return
	}
	if !a.beginDatabaseRequest(w) {
		return
	}
	defer func() { <-a.databaseSlots }()
	query := r.URL.Query()
	page, pageErr := strconv.Atoi(query.Get("page"))
	size, sizeErr := strconv.Atoi(query.Get("pageSize"))
	name, schema, table := query.Get("database"), query.Get("schema"), query.Get("table")
	if pageErr != nil || sizeErr != nil || page < 1 || page > 1000000 || size < 1 || size > 100 || name == "" || schema == "" || table == "" || len(name) > 63 || len(schema) > 63 || len(table) > 63 || strings.ContainsRune(name+schema+table, 0) {
		writeError(w, 400, "invalid_page", "Choose a table and a page size between 1 and 100.")
		return
	}
	database := a.getDatabaseSession(w, r, actor)
	if database == nil {
		return
	}
	if !database.hasDatabase(name) {
		writeError(w, 404, "database_not_available", "This database is not available for this connection.")
		return
	}
	select {
	case database.busy <- struct{}{}:
		defer func() { <-database.busy }()
	default:
		writeError(w, 429, "database_busy", "Two database requests are already running. Try again shortly.")
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), postgresRequestTimeout)
	defer cancel()
	stop := context.AfterFunc(database.ctx, cancel)
	defer stop()
	fields := database.fields
	fields.Database = name
	conn, err := connectPostgres(ctx, fields, database.password)
	if err != nil {
		writePostgresResult(w, postgresResult{}, err)
		return
	}
	defer conn.Close(context.Background())
	result, err := readPostgresPage(ctx, conn, schema, table, page, size, query.Get("cursor"))
	if err != nil {
		if errors.Is(err, postgresdb.ErrInvalidPageCursor) {
			writeError(w, 400, "invalid_page_cursor", "This page is no longer available. Return to the first page.")
			return
		}
		writePostgresResult(w, postgresResult{}, err)
		return
	}
	writeJSON(w, 200, result)
}

func (a *application) handlePostgresCount(w http.ResponseWriter, r *http.Request) {
	query := r.URL.Query()
	name, schema, table := query.Get("database"), query.Get("schema"), query.Get("table")
	if !validSchemaName(name) || !validSchemaName(schema) || !validSchemaName(table) {
		writeError(w, 400, "invalid_table", "Choose a table to count.")
		return
	}
	actor, ok := a.requireUser(w, r)
	if !ok {
		return
	}
	database := a.getDatabaseSession(w, r, actor)
	if database == nil {
		return
	}
	if !database.hasDatabase(name) {
		writeError(w, 404, "database_not_available", "This database is not available for this connection.")
		return
	}
	select {
	case a.countSlot <- struct{}{}:
		defer func() { <-a.countSlot }()
	default:
		log.Printf("PostgreSQL row count skipped: connection=%q database=%q schema=%q table=%q reason=count slot busy", r.PathValue("id"), name, schema, table)
		writeError(w, 429, "count_busy", "Another row count is running. Retry shortly.")
		return
	}
	started := time.Now()
	ctx, cancel := context.WithTimeout(r.Context(), postgresRequestTimeout)
	defer cancel()
	stop := context.AfterFunc(database.ctx, cancel)
	defer stop()
	fields := database.fields
	fields.Database = name
	conn, err := connectPostgres(ctx, fields, database.password)
	if err != nil {
		log.Printf("PostgreSQL row count failed: connection=%q database=%q schema=%q table=%q duration=%s error=%+v", r.PathValue("id"), name, schema, table, time.Since(started), err)
		writePostgresResult(w, postgresResult{}, err)
		return
	}
	defer conn.Close(context.Background())
	var total int64
	if err := conn.QueryRow(ctx, "SELECT count(*) FROM "+pgx.Identifier{schema, table}.Sanitize()).Scan(&total); err != nil {
		log.Printf("PostgreSQL row count failed: connection=%q database=%q schema=%q table=%q duration=%s error=%+v", r.PathValue("id"), name, schema, table, time.Since(started), err)
		writePostgresResult(w, postgresResult{}, err)
		return
	}
	writeJSON(w, 200, map[string]int64{"totalRows": total})
}

func readPostgresPage(ctx context.Context, conn *pgx.Conn, schema, table string, page, size int, cursor string) (tablePage, error) {
	started := time.Now()
	result := tablePage{Columns: []tableColumn{}, Rows: [][]*string{}, Truncated: [][]bool{}, Versions: []string{}, PrimaryKey: []string{}, Indexes: []tableIndex{}, Constraints: []tableConstraint{}}
	meta, err := readPostgresTableMeta(ctx, conn, schema, table)
	if err != nil {
		return result, err
	}
	result.PrimaryKey, result.Editable, result.Indexes, result.Constraints = meta.PrimaryKey, meta.Editable, meta.Indexes, meta.Constraints
	result.CursorPaging = len(meta.PrimaryKey) > 0
	if result.CursorPaging && page > 1 && cursor == "" {
		return result, postgresdb.ErrInvalidPageCursor
	}
	columns, err := conn.Query(ctx, `SELECT a.attname, pg_catalog.format_type(a.atttypid,a.atttypmod), NOT a.attnotnull,
		pg_catalog.pg_get_expr(d.adbin,d.adrelid),
		(a.attgenerated = '' AND a.attidentity = '' AND pg_catalog.has_column_privilege(c.oid,a.attname,'UPDATE'))
		FROM pg_catalog.pg_attribute a
		JOIN pg_catalog.pg_class c ON c.oid=a.attrelid
		JOIN pg_catalog.pg_namespace n ON n.oid=c.relnamespace
		LEFT JOIN pg_catalog.pg_attrdef d ON d.adrelid=c.oid AND d.adnum=a.attnum
		WHERE n.nspname=$1 AND c.relname=$2 AND c.relkind IN ('r','p','v','m','f')
		AND a.attnum>0 AND NOT a.attisdropped ORDER BY a.attnum`, schema, table)
	if err != nil {
		return result, err
	}
	var order []string
	for columns.Next() {
		var column tableColumn
		if err := columns.Scan(&column.Name, &column.Type, &column.Nullable, &column.Default, &column.Editable); err != nil {
			columns.Close()
			return result, err
		}
		if slices.Contains(result.PrimaryKey, column.Name) {
			column.Key = "PRIMARY KEY"
		}
		result.Columns = append(result.Columns, column)
	}
	columns.Close()
	if err := columns.Err(); err != nil {
		return result, err
	}
	for _, key := range result.PrimaryKey {
		order = append(order, pgx.Identifier{key}.Sanitize())
	}
	// LIMIT + 1 memberi status halaman berikutnya tanpa COUNT(*) terhadap seluruh tabel.
	selected := make([]string, len(result.Columns))
	previewColumns := make([]bool, len(result.Columns))
	for index, column := range result.Columns {
		name := pgx.Identifier{column.Name}.Sanitize()
		selected[index] = name
		if postgresdb.PreviewColumn(column.Type) && !slices.Contains(result.PrimaryKey, column.Name) {
			selected[index] = postgresdb.PreviewSQL(name, column.Type)
			previewColumns[index] = true
		}
	}
	if meta.Kind == "r" || meta.Kind == "p" {
		selected = append(selected, "xmin::text")
	}
	statement := "SELECT " + strings.Join(selected, ",") + " FROM " + pgx.Identifier{schema, table}.Sanitize()
	args := []any{pgx.QueryResultFormats{pgx.TextFormatCode}, size + 1}
	if result.CursorPaging && cursor != "" {
		predicate, values, err := postgresdb.PageCursor(result.PrimaryKey, cursor)
		if err != nil {
			return result, err
		}
		statement += " WHERE " + predicate
		args = append(args, values...)
	}
	if len(order) > 0 {
		statement += " ORDER BY " + strings.Join(order, ",")
	}
	statement += " LIMIT $1"
	if !result.CursorPaging {
		statement += " OFFSET $2"
		args = append(args, (page-1)*size)
	}
	rows, err := conn.Query(ctx, statement, args...)
	if err != nil {
		return result, err
	}
	defer rows.Close()
	bytes := 0
	for rows.Next() {
		if len(result.Rows) == size {
			result.HasMore = true
			break
		}
		values := make([]*string, len(result.Columns))
		truncated := make([]bool, len(values))
		for index, raw := range rows.RawValues() {
			bytes += len(raw)
			if bytes > 8<<20 {
				return result, errPostgresPageTooLarge
			}
			if index >= len(values) {
				result.Versions = append(result.Versions, string(raw))
				continue
			}
			if raw != nil {
				value := string(raw)
				if previewColumns[index] && utf8.RuneCountInString(value) > postgresPreviewLength {
					value = string([]rune(value)[:postgresPreviewLength])
					truncated[index] = true
				}
				values[index] = &value
			}
		}
		result.Rows = append(result.Rows, values)
		result.Truncated = append(result.Truncated, truncated)
	}
	rowErr := rows.Err()
	rows.Close()
	if rowErr != nil {
		return result, rowErr
	}
	if result.CursorPaging && result.HasMore && len(result.Rows) > 0 {
		values := make([]string, len(result.PrimaryKey))
		last := result.Rows[len(result.Rows)-1]
		for index, key := range result.PrimaryKey {
			column := slices.IndexFunc(result.Columns, func(item tableColumn) bool { return item.Name == key })
			if column < 0 || last[column] == nil {
				return result, postgresdb.ErrInvalidPageCursor
			}
			values[index] = *last[column]
		}
		encoded, err := json.Marshal(values)
		if err != nil {
			return result, err
		}
		result.NextCursor = string(encoded)
	}
	result.Duration = time.Since(started).Milliseconds()
	return result, nil
}

func writePostgresResult(w http.ResponseWriter, result postgresResult, err error) {
	if err == nil {
		writeJSON(w, 200, result)
		return
	}
	log.Printf("PostgreSQL error: %+v", err)
	if errors.Is(err, errPostgresPageTooLarge) {
		writeError(w, 413, "page_too_large", "This page exceeds 8 MB. Choose a smaller page size.")
		return
	}
	var pgErr *pgconn.PgError
	if errors.As(err, &pgErr) {
		if pgErr.Code == "42501" {
			writeError(w, 403, "database_permission_denied", "Your PostgreSQL account does not have permission to read this table.")
			return
		}
		if pgErr.Code == "57014" {
			writeError(w, 504, "database_timeout", "PostgreSQL query timed out.")
			return
		}
		if strings.HasPrefix(pgErr.Code, "28") {
			writeError(w, 422, "database_auth_failed", "PostgreSQL rejected the username or password.")
			return
		}
		if pgErr.Code == "3D000" {
			writeError(w, 400, "database_not_found", "PostgreSQL database does not exist.")
			return
		}
		writeError(w, 502, "database_error", "PostgreSQL returned an error (SQLSTATE "+pgErr.Code+").")
		return
	}
	if errors.Is(err, context.DeadlineExceeded) {
		writeError(w, 504, "database_timeout", "PostgreSQL connection timed out.")
		return
	}
	writeError(w, 502, "database_unavailable", "Could not connect to PostgreSQL. Check the host, port, and SSL settings.")
}
