package main

import (
	"context"
	"errors"
	"log"
	"net/http"
	"slices"
	"strconv"
	"strings"
	"unicode/utf8"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"

	postgresdb "gosql/database/postgres"
)

const postgresCellCharacterLimit = 2 << 20
const postgresCellByteLimit = 8 << 20

type postgresTableMeta struct {
	Kind        string
	PrimaryKey  []string
	Editable    bool
	Indexes     []tableIndex
	Constraints []tableConstraint
}

func readPostgresTableMeta(ctx context.Context, conn *pgx.Conn, schema, table string) (postgresTableMeta, error) {
	var meta postgresTableMeta
	var oid uint32
	err := conn.QueryRow(ctx, `SELECT c.oid, c.relkind::text,
		pg_catalog.has_table_privilege(c.oid,'UPDATE') AND pg_catalog.has_table_privilege(c.oid,'DELETE')
		FROM pg_catalog.pg_class c JOIN pg_catalog.pg_namespace n ON n.oid=c.relnamespace
		WHERE n.nspname=$1 AND c.relname=$2 AND c.relkind IN ('r','p','v','m','f')`, schema, table).Scan(&oid, &meta.Kind, &meta.Editable)
	if err != nil {
		return meta, err
	}
	meta.PrimaryKey = []string{}
	meta.Indexes = []tableIndex{}
	meta.Constraints = []tableConstraint{}
	rows, err := conn.Query(ctx, `SELECT a.attname FROM pg_catalog.pg_index i
		JOIN LATERAL unnest(i.indkey) WITH ORDINALITY k(attnum,pos) ON true
		JOIN pg_catalog.pg_attribute a ON a.attrelid=i.indrelid AND a.attnum=k.attnum
		WHERE i.indrelid=$1 AND i.indisprimary AND k.pos<=i.indnkeyatts ORDER BY k.pos`, oid)
	if err != nil {
		return meta, err
	}
	for rows.Next() {
		var key string
		if err = rows.Scan(&key); err != nil {
			break
		}
		meta.PrimaryKey = append(meta.PrimaryKey, key)
	}
	rows.Close()
	if err == nil {
		err = rows.Err()
	}
	if err != nil {
		return meta, err
	}
	meta.Editable = meta.Editable && (meta.Kind == "r" || meta.Kind == "p") && len(meta.PrimaryKey) > 0
	rows, err = conn.Query(ctx, `SELECT ic.relname, pg_catalog.pg_get_indexdef(i.indexrelid), i.indisunique, i.indisprimary, am.amname,
		ARRAY(SELECT a.attname::text FROM unnest(i.indkey) WITH ORDINALITY k(attnum,pos)
		JOIN pg_catalog.pg_attribute a ON a.attrelid=i.indrelid AND a.attnum=k.attnum WHERE k.pos<=i.indnkeyatts ORDER BY k.pos),
		EXISTS(SELECT 1 FROM pg_catalog.pg_constraint con WHERE con.conindid=i.indexrelid),
		i.indpred IS NULL AND i.indexprs IS NULL AND i.indnatts=i.indnkeyatts AND am.amname='btree'
		FROM pg_catalog.pg_index i JOIN pg_catalog.pg_class ic ON ic.oid=i.indexrelid
		JOIN pg_catalog.pg_am am ON am.oid=ic.relam
		WHERE i.indrelid=$1 ORDER BY ic.relname`, oid)
	if err != nil {
		return meta, err
	}
	for rows.Next() {
		var item tableIndex
		if err = rows.Scan(&item.Name, &item.Definition, &item.Unique, &item.Primary, &item.Method, &item.Columns, &item.Managed, &item.Editable); err != nil {
			break
		}
		meta.Indexes = append(meta.Indexes, item)
	}
	rows.Close()
	if err == nil {
		err = rows.Err()
	}
	if err != nil {
		return meta, err
	}
	rows, err = conn.Query(ctx, `SELECT con.conname, con.contype::text, pg_catalog.pg_get_constraintdef(con.oid,true),
		ARRAY(SELECT a.attname::text FROM unnest(con.conkey) WITH ORDINALITY k(attnum,pos)
		JOIN pg_catalog.pg_attribute a ON a.attrelid=con.conrelid AND a.attnum=k.attnum ORDER BY k.pos),
		coalesce(rn.nspname,''), coalesce(rc.relname,''),
		ARRAY(SELECT a.attname::text FROM unnest(con.confkey) WITH ORDINALITY k(attnum,pos)
		JOIN pg_catalog.pg_attribute a ON a.attrelid=con.confrelid AND a.attnum=k.attnum ORDER BY k.pos),
		coalesce(pg_catalog.pg_get_expr(con.conbin,con.conrelid),''), con.convalidated
		FROM pg_catalog.pg_constraint con
		LEFT JOIN pg_catalog.pg_class rc ON rc.oid=con.confrelid
		LEFT JOIN pg_catalog.pg_namespace rn ON rn.oid=rc.relnamespace
		WHERE con.conrelid=$1 ORDER BY con.conname`, oid)
	if err != nil {
		return meta, err
	}
	for rows.Next() {
		var item tableConstraint
		var kind string
		if err = rows.Scan(&item.Name, &kind, &item.Definition, &item.Columns, &item.ReferenceSchema, &item.ReferenceTable, &item.ReferenceColumns, &item.Expression, &item.Validated); err != nil {
			break
		}
		item.Type = map[string]string{"p": "PRIMARY KEY", "u": "UNIQUE", "f": "FOREIGN KEY", "c": "CHECK", "x": "EXCLUDE"}[kind]
		if item.Type == "" {
			item.Type = kind
		}
		meta.Constraints = append(meta.Constraints, item)
	}
	rows.Close()
	if err == nil {
		err = rows.Err()
	}
	return meta, err
}

func (a *application) handlePostgresCell(w http.ResponseWriter, r *http.Request) {
	var input tableMutation
	if !readJSON(w, r, &input) {
		return
	}
	if !validSchemaName(input.Database) || !validSchemaName(input.Schema) || !validSchemaName(input.Table) || !validSchemaName(input.Column) {
		writeError(w, 400, "invalid_cell", "Choose a valid table column.")
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
	if !meta.Editable || len(input.Row.Values) != len(meta.PrimaryKey) || input.Row.Version == "" || len(input.Row.Version) > 20 {
		writeError(w, 400, "invalid_row", "The row changed or cannot be edited. Refresh the table.")
		return
	}
	var columnType string
	if err := conn.QueryRow(ctx, `SELECT pg_catalog.format_type(a.atttypid,a.atttypmod) FROM pg_catalog.pg_attribute a JOIN pg_catalog.pg_class c ON c.oid=a.attrelid JOIN pg_catalog.pg_namespace n ON n.oid=c.relnamespace WHERE n.nspname=$1 AND c.relname=$2 AND a.attname=$3 AND a.attnum>0 AND NOT a.attisdropped`, input.Schema, input.Table, input.Column).Scan(&columnType); err != nil {
		writeError(w, 404, "column_not_found", "This column is no longer available. Refresh the table.")
		return
	}
	where := make([]string, 0, len(meta.PrimaryKey)+1)
	args := make([]any, 0, len(meta.PrimaryKey)+1)
	for i, name := range meta.PrimaryKey {
		where = append(where, pgx.Identifier{name}.Sanitize()+"=$"+strconv.Itoa(i+1))
		args = append(args, input.Row.Values[i])
	}
	where = append(where, "xmin::text=$"+strconv.Itoa(len(args)+1))
	args = append(args, input.Row.Version)
	name := pgx.Identifier{input.Column}.Sanitize()
	selected := "LEFT(" + name + "::text, " + strconv.Itoa(postgresCellCharacterLimit+1) + ")"
	if columnType == "bytea" {
		selected = "CASE WHEN " + name + " IS NULL THEN NULL ELSE chr(92) || 'x' || encode(substring(" + name + " from 1 for " + strconv.Itoa(postgresCellByteLimit/2+1) + "), 'hex') END"
	}
	statement := "SELECT " + selected + " FROM " + pgx.Identifier{input.Schema, input.Table}.Sanitize() + " WHERE " + strings.Join(where, " AND ")
	rows, err := conn.Query(ctx, statement, append([]any{pgx.QueryResultFormats{pgx.TextFormatCode}}, args...)...)
	if err != nil {
		writeMutationError(w, err)
		return
	}
	defer rows.Close()
	if !rows.Next() {
		if err := rows.Err(); err != nil {
			writeMutationError(w, err)
		} else {
			writeError(w, 409, "row_changed", "This row changed. Refresh the table.")
		}
		return
	}
	var value *string
	if raw := rows.RawValues()[0]; raw != nil {
		if len(raw) > postgresCellByteLimit || columnType != "bytea" && utf8.RuneCount(raw) > postgresCellCharacterLimit {
			writeError(w, 413, "cell_too_large", "This cell exceeds the editor limit of 2 million characters or 8 MB.")
			return
		}
		full := string(raw)
		value = &full
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		writeMutationError(w, err)
		return
	}
	writeJSON(w, 200, struct {
		Value *string `json:"value"`
	}{value})
}

func (a *application) handlePostgresInsert(w http.ResponseWriter, r *http.Request) {
	var input struct {
		Database string             `json:"database"`
		Schema   string             `json:"schema"`
		Table    string             `json:"table"`
		Values   map[string]*string `json:"values"`
	}
	if !readJSON(w, r, &input) {
		return
	}
	if !validSchemaName(input.Database) || !validSchemaName(input.Schema) || !validSchemaName(input.Table) || len(input.Values) > 128 {
		writeError(w, 400, "invalid_insert", "Choose a valid table and up to 128 column values.")
		return
	}
	conn, ctx, cleanup := a.openPostgresSchemaConnection(w, r, input.Database)
	if conn == nil {
		return
	}
	defer cleanup()
	var kind string
	err := conn.QueryRow(ctx, `SELECT c.relkind::text FROM pg_catalog.pg_class c JOIN pg_catalog.pg_namespace n ON n.oid=c.relnamespace WHERE n.nspname=$1 AND c.relname=$2`, input.Schema, input.Table).Scan(&kind)
	if errors.Is(err, pgx.ErrNoRows) {
		writeError(w, 404, "table_not_found", "This table is no longer available.")
		return
	}
	if err != nil {
		writeMutationError(w, err)
		return
	}
	if kind != "r" && kind != "p" {
		writeError(w, 400, "invalid_table", "Rows can only be added to tables.")
		return
	}
	columns, err := postgresdb.ReadInsertColumns(ctx, conn, input.Schema, input.Table)
	if err != nil {
		writeMutationError(w, err)
		return
	}
	allowed := make(map[string]bool, len(columns))
	for _, column := range columns {
		allowed[column.Name] = true
	}
	names := make([]string, 0, len(input.Values))
	for name := range input.Values {
		if !allowed[name] {
			writeError(w, 400, "invalid_column", "A column is no longer available. Refresh the table.")
			return
		}
		names = append(names, name)
	}
	slices.Sort(names)
	table := pgx.Identifier{input.Schema, input.Table}.Sanitize()
	statement := "INSERT INTO " + table
	generatedPK := false
	args := make([]any, 0, len(names))
	if len(names) == 0 {
		statement, generatedPK, err = postgresdb.BuildQuickInsertSQL(table, columns)
		if err != nil {
			writeError(w, 400, "insert_default_unavailable", err.Error())
			return
		}
	} else {
		quoted := make([]string, len(names))
		parameters := make([]string, len(names))
		for index, name := range names {
			quoted[index] = pgx.Identifier{name}.Sanitize()
			parameters[index] = "$" + strconv.Itoa(index+1)
			if value := input.Values[name]; value != nil {
				args = append(args, *value)
			} else {
				args = append(args, nil)
			}
		}
		statement += " (" + strings.Join(quoted, ", ") + ") VALUES (" + strings.Join(parameters, ", ") + ")"
	}
	returned := make([]string, len(columns)+1)
	for index, column := range columns {
		returned[index] = pgx.Identifier{column.Name}.Sanitize()
	}
	returned[len(columns)] = "xmin::text"
	query := statement + " RETURNING " + strings.Join(returned, ", ")
	var values []*string
	var version string
	for attempt := 0; attempt < 3; attempt++ {
		rows, queryErr := conn.Query(ctx, query, append([]any{pgx.QueryResultFormats{pgx.TextFormatCode}}, args...)...)
		if queryErr == nil {
			if rows.Next() {
				values = make([]*string, len(columns))
				raw := rows.RawValues()
				for index := range values {
					if raw[index] != nil {
						value := string(raw[index])
						values[index] = &value
					}
				}
				version = string(raw[len(columns)])
			}
			rows.Close()
			queryErr = rows.Err()
		}
		if queryErr == nil && values != nil {
			break
		}
		if queryErr == nil {
			writeError(w, 500, "insert_result_missing", "The inserted row could not be returned. Refresh the table.")
			return
		}
		values = nil
		var pgErr *pgconn.PgError
		if !generatedPK || !errors.As(queryErr, &pgErr) || pgErr.Code != "23505" || attempt == 2 {
			writeMutationError(w, queryErr)
			return
		}
	}
	recordedSQL := ""
	if len(names) == 0 {
		recordedSQL = statement + ";"
	}
	writeJSON(w, 200, struct {
		AffectedRows int64     `json:"affectedRows"`
		Row          []*string `json:"row"`
		Version      string    `json:"version"`
		SQL          string    `json:"sql,omitempty"`
	}{1, values, version, recordedSQL})
}

func (a *application) handlePostgresEdit(w http.ResponseWriter, r *http.Request) {
	a.handlePostgresMutation(w, r, false)
}
func (a *application) handlePostgresDelete(w http.ResponseWriter, r *http.Request) {
	a.handlePostgresMutation(w, r, true)
}

func (a *application) handlePostgresMutation(w http.ResponseWriter, r *http.Request, deleting bool) {
	actor, ok := a.requireUser(w, r)
	if !ok {
		return
	}
	var input tableMutation
	if !readJSON(w, r, &input) {
		return
	}
	if input.Database == "" || input.Schema == "" || input.Table == "" || len(input.Database) > 63 || len(input.Schema) > 63 || len(input.Table) > 63 || strings.ContainsRune(input.Database+input.Schema+input.Table+input.Column, 0) || (!deleting && (input.Column == "" || len(input.Column) > 63)) || (deleting && (len(input.Rows) < 1 || len(input.Rows) > 100)) {
		writeError(w, 400, "invalid_mutation", "Choose a table, column, and up to 100 rows.")
		return
	}
	if !a.beginDatabaseRequest(w) {
		return
	}
	defer func() { <-a.databaseSlots }()
	database := a.getDatabaseSession(w, r, actor)
	if database == nil {
		return
	}
	if !database.hasDatabase(input.Database) {
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
	fields.Database = input.Database
	conn, err := connectPostgresMode(ctx, fields, database.password, false)
	if err != nil {
		writePostgresResult(w, postgresResult{}, err)
		return
	}
	defer conn.Close(context.Background())
	meta, err := readPostgresTableMeta(ctx, conn, input.Schema, input.Table)
	if err != nil {
		writeMutationError(w, err)
		return
	}
	if !meta.Editable {
		writeError(w, 403, "table_read_only", "This table needs a primary key and UPDATE/DELETE privileges before editing.")
		return
	}
	if !deleting {
		var allowed bool
		err = conn.QueryRow(ctx, `SELECT a.attgenerated='' AND a.attidentity='' AND pg_catalog.has_column_privilege(c.oid,a.attname,'UPDATE')
			FROM pg_catalog.pg_attribute a JOIN pg_catalog.pg_class c ON c.oid=a.attrelid
			JOIN pg_catalog.pg_namespace n ON n.oid=c.relnamespace
			WHERE n.nspname=$1 AND c.relname=$2 AND a.attname=$3 AND a.attnum>0 AND NOT a.attisdropped`, input.Schema, input.Table, input.Column).Scan(&allowed)
		if err != nil || !allowed || slices.Contains(meta.PrimaryKey, input.Column) {
			writeError(w, 403, "column_read_only", "This column cannot be edited.")
			return
		}
	}
	keys := input.Rows
	if !deleting {
		keys = []tableRowKey{input.Row}
	}
	for _, key := range keys {
		if len(key.Values) != len(meta.PrimaryKey) || key.Version == "" || len(key.Version) > 20 {
			writeError(w, 400, "invalid_row", "The row identity is invalid. Refresh the table.")
			return
		}
	}
	tx, err := conn.Begin(ctx)
	if err != nil {
		writeMutationError(w, err)
		return
	}
	defer tx.Rollback(context.Background())
	name := pgx.Identifier{input.Schema, input.Table}.Sanitize()
	where := make([]string, len(meta.PrimaryKey))
	for i, key := range meta.PrimaryKey {
		where[i] = pgx.Identifier{key}.Sanitize() + "=$" + strconv.Itoa(i+1)
	}
	where = append(where, "xmin::text=$"+strconv.Itoa(len(meta.PrimaryKey)+1))
	for _, key := range keys {
		args := make([]any, 0, len(key.Values)+2)
		for _, value := range key.Values {
			args = append(args, value)
		}
		args = append(args, key.Version)
		statement := "DELETE FROM " + name + " WHERE " + strings.Join(where, " AND ")
		if !deleting {
			statement = "UPDATE " + name + " SET " + pgx.Identifier{input.Column}.Sanitize() + "=$" + strconv.Itoa(len(args)+1) + " WHERE " + strings.Join(where, " AND ")
			args = append(args, input.Value)
		}
		statement += " RETURNING 1"
		var one int
		if err = tx.QueryRow(ctx, statement, args...).Scan(&one); err != nil {
			writeMutationError(w, err)
			return
		}
	}
	if err = tx.Commit(ctx); err != nil {
		writeMutationError(w, err)
		return
	}
	writeJSON(w, 200, map[string]int{"affectedRows": len(keys)})
}

func writeMutationError(w http.ResponseWriter, err error) {
	log.Printf("PostgreSQL mutation error: %+v", err)
	if errors.Is(err, pgx.ErrNoRows) {
		writeError(w, 409, "row_changed", "A selected row changed or no longer exists. Refresh the table.")
		return
	}
	var pgErr *pgconn.PgError
	if errors.As(err, &pgErr) {
		switch pgErr.Code {
		case "55P03":
			writeError(w, 409, "table_busy", "The table is busy. Try again when other transactions finish.")
			return
		case "23505":
			writeError(w, 409, "unique_violation", "This value conflicts with a UNIQUE or PRIMARY KEY constraint.")
			return
		case "23503":
			writeError(w, 409, "foreign_key_violation", "This change conflicts with a FOREIGN KEY constraint.")
			return
		case "23502":
			writeError(w, 400, "not_null_violation", "This column does not allow NULL.")
			return
		case "23514":
			writeError(w, 400, "check_violation", "This value violates a CHECK constraint.")
			return
		case "42501":
			writeError(w, 403, "database_permission_denied", "Your PostgreSQL account cannot change this table.")
			return
		case "42710", "42P07":
			writeError(w, 409, "constraint_exists", "A constraint or index with this name already exists.")
			return
		case "42830":
			writeError(w, 400, "invalid_reference_key", "Referenced columns must form a PRIMARY KEY or UNIQUE key.")
			return
		case "2BP01":
			writeError(w, 409, "dependent_objects", "Other database objects depend on this index or constraint.")
			return
		case "42P16":
			writeError(w, 400, "invalid_constraint", "This constraint cannot be added to the table.")
			return
		}
		if strings.HasPrefix(pgErr.Code, "22") {
			writeError(w, 400, "invalid_value", "The value is not valid for this column type.")
			return
		}
	}
	writePostgresResult(w, postgresResult{}, err)
}
