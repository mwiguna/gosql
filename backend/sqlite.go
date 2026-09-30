package main

import (
	"context"
	"database/sql"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"time"

	sqlitedb "gosql/database/sqlite"
)

const sqliteTimeout = 30 * time.Second
const sqliteUploadLimit = 256 << 20

type sqliteCatalog struct {
	Databases     []string `json:"databases"`
	Tables        []string `json:"tables"`
	Views         []string `json:"views"`
	VirtualTables []string `json:"virtualTables"`
}

type sqliteMeta struct {
	Columns        []tableColumn
	PrimaryKey     []string
	Indexes        []tableIndex
	Constraints    []tableConstraint
	Kind           string
	RowID          bool
	RowIDName      string
	Editable       bool
	ReadOnlyReason string
	Strict         bool
	WithoutRowID   bool
	Definition     string
}

func sqliteDatabaseName(fields connectionFields) string {
	name := filepath.Base(fields.File)
	name = strings.TrimSuffix(name, filepath.Ext(name))
	if name == "" {
		return "main"
	}
	return name
}

func (a *application) sqlitePath(profile connectionProfile) (string, error) {
	if profile.Location == "Server upload" {
		return filepath.Join(filepath.Dir(a.store.path), "sqlite-uploads", profile.OwnerID, profile.ID+".sqlite"), nil
	}
	if profile.Location == "Native file" {
		path, err := a.sqliteNativePath(profile.File)
		if err != nil {
			return "", err
		}
		if path != filepath.Clean(profile.File) {
			return "", errors.New("the selected SQLite file path changed; choose it again")
		}
		return path, nil
	}
	return a.sqliteNativePath(profile.File)
}

func (a *application) sqliteProfile(w http.ResponseWriter, r *http.Request) (connectionProfile, bool) {
	actor, ok := a.requireUser(w, r)
	if !ok {
		return connectionProfile{}, false
	}
	a.store.mu.Lock()
	defer a.store.mu.Unlock()
	index := slices.IndexFunc(a.store.data.Connections, func(c connectionProfile) bool {
		return c.ID == r.PathValue("id") && c.OwnerID == actor.ID && c.Engine == "SQLite"
	})
	if index < 0 {
		writeError(w, 404, "profile_not_found", "SQLite connection profile not found.")
		return connectionProfile{}, false
	}
	return a.store.data.Connections[index], true
}

func (a *application) openSQLiteRequest(w http.ResponseWriter, r *http.Request, database string, readOnly bool) (*sql.Conn, context.Context, func()) {
	actor, ok := a.requireUser(w, r)
	if !ok || !a.beginDatabaseRequest(w) {
		return nil, nil, nil
	}
	session := a.getDatabaseSession(w, r, actor)
	if session == nil {
		<-a.databaseSlots
		return nil, nil, nil
	}
	if !session.hasDatabase(database) || database != sqliteDatabaseName(session.fields) {
		<-a.databaseSlots
		writeError(w, 404, "database_not_available", "This SQLite file is not available for this connection.")
		return nil, nil, nil
	}
	select {
	case session.busy <- struct{}{}:
	default:
		<-a.databaseSlots
		writeError(w, 429, "database_busy", "This SQLite connection is busy. Try again shortly.")
		return nil, nil, nil
	}
	ctx, cancel := context.WithTimeout(r.Context(), sqliteTimeout)
	stop := context.AfterFunc(session.ctx, cancel)
	profile, ok := a.sqliteProfile(w, r)
	if !ok {
		stop()
		cancel()
		<-session.busy
		<-a.databaseSlots
		return nil, nil, nil
	}
	path, err := a.sqlitePath(profile)
	var db *sql.DB
	var conn *sql.Conn
	if err == nil {
		db, conn, err = sqlitedb.Open(ctx, path, readOnly)
	}
	if err != nil {
		stop()
		cancel()
		<-session.busy
		<-a.databaseSlots
		writeSQLiteFileError(w, err)
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

func (a *application) handleSQLiteTest(w http.ResponseWriter, r *http.Request) {
	actor, ok := a.requireUser(w, r)
	if !ok || !a.beginDatabaseRequest(w) {
		return
	}
	defer func() { <-a.databaseSlots }()
	if strings.HasPrefix(r.Header.Get("Content-Type"), "multipart/form-data;") {
		temporary, err := sqliteUpload(w, r, filepath.Join(filepath.Dir(a.store.path), "sqlite-upload-tests"), a.sqliteUploadLimit)
		if err != nil {
			if errors.Is(err, errSQLiteUploadTooLarge) {
				writeError(w, 413, "sqlite_upload_too_large", err.Error())
			} else {
				writeError(w, 400, "invalid_sqlite_upload", err.Error())
			}
			return
		}
		defer os.Remove(temporary)
		writeJSON(w, 200, map[string]string{"status": "ok"})
		return
	}
	var fields connectionFields
	if !readJSON(w, r, &fields) {
		return
	}
	if fields.Engine != "SQLite" || !slices.Contains([]string{"Local file", "Native file"}, fields.Location) || !fields.valid() {
		writeError(w, 400, "invalid_profile", "Choose a valid SQLite file.")
		return
	}
	if !a.validateSQLitePicker(actor.ID, fields, connectionProfile{}) {
		writeError(w, 403, "sqlite_file_not_selected", "Choose the SQLite file using the native file dialog.")
		return
	}
	path, err := a.sqlitePath(connectionProfile{connectionFields: fields})
	if err == nil {
		ctx, cancel := context.WithTimeout(r.Context(), sqliteTimeout)
		defer cancel()
		err = sqlitedb.Check(ctx, path)
	}
	if err != nil {
		writeSQLiteFileError(w, err)
		return
	}
	writeJSON(w, 200, map[string]string{"status": "ok"})
}

func sqliteCatalogFor(ctx context.Context, conn *sql.Conn, database string) (sqliteCatalog, error) {
	result := sqliteCatalog{Databases: []string{database}, Tables: []string{}, Views: []string{}, VirtualTables: []string{}}
	rows, err := conn.QueryContext(ctx, "PRAGMA main.table_list")
	if err != nil {
		return result, err
	}
	defer rows.Close()
	for rows.Next() {
		var schema, name, kind string
		var columns, withoutRowID, strict int
		if err := rows.Scan(&schema, &name, &kind, &columns, &withoutRowID, &strict); err != nil {
			return result, err
		}
		if schema != "main" || strings.HasPrefix(name, "sqlite_") || kind == "shadow" {
			continue
		}
		if kind == "view" {
			result.Views = append(result.Views, name)
		} else if kind == "table" || kind == "virtual" {
			result.Tables = append(result.Tables, name)
			if kind == "virtual" {
				result.VirtualTables = append(result.VirtualTables, name)
			}
		}
	}
	if err := rows.Err(); err != nil {
		return result, err
	}
	slices.Sort(result.Tables)
	slices.Sort(result.Views)
	slices.Sort(result.VirtualTables)
	return result, nil
}

func (a *application) handleSQLiteCatalog(w http.ResponseWriter, r *http.Request) {
	profile, ok := a.sqliteProfile(w, r)
	if !ok {
		return
	}
	name := sqliteDatabaseName(profile.connectionFields)
	if r.Method == http.MethodPost {
		var input struct {
			Password string `json:"password"`
		}
		if !readJSON(w, r, &input) {
			return
		}
		ctx, cancel := context.WithTimeout(r.Context(), sqliteTimeout)
		defer cancel()
		path, err := a.sqlitePath(profile)
		if err == nil {
			err = sqlitedb.Check(ctx, path)
		}
		if err != nil {
			writeSQLiteFileError(w, err)
			return
		}
		if !a.beginDatabaseRequest(w) {
			return
		}
		defer func() { <-a.databaseSlots }()
		db, conn, err := sqlitedb.Open(ctx, path, true)
		if err != nil {
			writeSQLiteFileError(w, err)
			return
		}
		defer db.Close()
		defer conn.Close()
		catalog, err := sqliteCatalogFor(ctx, conn, name)
		if err != nil {
			writeSQLiteError(w, err)
			return
		}
		actor, _ := a.requireUser(w, r)
		if !a.saveDatabaseSession(w, r, actor, profile.connectionFields, "", catalog.Databases) {
			return
		}
		writeJSON(w, 200, catalog)
		return
	}
	conn, ctx, cleanup := a.openSQLiteRequest(w, r, name, true)
	if conn == nil {
		return
	}
	defer cleanup()
	catalog, err := sqliteCatalogFor(ctx, conn, name)
	if err != nil {
		writeSQLiteError(w, err)
		return
	}
	writeJSON(w, 200, catalog)
}

func writeSQLiteError(w http.ResponseWriter, err error) {
	if err == nil {
		return
	}
	if errors.Is(err, errSQLiteRowLarge) {
		writeError(w, 413, "row_too_large", err.Error())
		return
	}
	message := err.Error()
	status := 400
	if strings.Contains(message, "locked") || strings.Contains(message, "busy") {
		status = 409
	}
	writeError(w, status, "sqlite_error", message)
}

func writeSQLiteFileError(w http.ResponseWriter, err error) {
	message := err.Error()
	switch {
	case errors.Is(err, os.ErrNotExist):
		writeError(w, 404, "sqlite_file_missing", "SQLite file was not found. Check the connection path or upload a file.")
	case errors.Is(err, os.ErrPermission):
		writeError(w, 403, "sqlite_file_unreadable", "SQLite file cannot be read by the GoSQL server.")
	case strings.Contains(message, "application database"):
		writeError(w, 403, "sqlite_path_forbidden", message)
	case strings.Contains(message, "locked") || strings.Contains(message, "busy"):
		writeError(w, 409, "sqlite_file_locked", "SQLite file is locked. Try again shortly.")
	case strings.Contains(message, "not a SQLite database") || strings.Contains(message, "integrity check") || strings.Contains(message, "malformed"):
		writeError(w, 400, "sqlite_file_corrupt", "The file is not a valid, consistent SQLite database.")
	default:
		writeError(w, 400, "sqlite_file_unavailable", message)
	}
}

func readSQLiteMeta(ctx context.Context, conn *sql.Conn, table string) (sqliteMeta, error) {
	return readSQLiteMetaWithDetails(ctx, conn, table, true)
}

func readSQLiteMetaBasic(ctx context.Context, conn *sql.Conn, table string) (sqliteMeta, error) {
	return readSQLiteMetaWithDetails(ctx, conn, table, false)
}

func readSQLiteMetaWithDetails(ctx context.Context, conn *sql.Conn, table string, withDetails bool) (sqliteMeta, error) {
	meta := sqliteMeta{Columns: []tableColumn{}, PrimaryKey: []string{}, Indexes: []tableIndex{}, Constraints: []tableConstraint{}}
	if !sqlitedb.ValidName(table) {
		return meta, errors.New("invalid table name")
	}
	var sqlText sql.NullString
	if err := conn.QueryRowContext(ctx, "SELECT type,sql FROM main.sqlite_schema WHERE name=? AND type IN ('table','view') AND name NOT GLOB 'sqlite_*'", table).Scan(&meta.Kind, &sqlText); err != nil {
		return meta, err
	}
	meta.Definition = sqlText.String
	objects, err := conn.QueryContext(ctx, "PRAGMA main.table_list("+sqlitedb.Quote(table)+")")
	if err != nil {
		return meta, err
	}
	objectKind := ""
	withoutRowID := 0
	for objects.Next() {
		var schema, name, kind string
		var columns, wr, strict int
		if err = objects.Scan(&schema, &name, &kind, &columns, &wr, &strict); err != nil {
			break
		}
		if schema == "main" && name == table {
			objectKind = kind
			withoutRowID = wr
			meta.Strict = strict != 0
		}
	}
	if err == nil {
		err = objects.Err()
	}
	objects.Close()
	if err != nil {
		return meta, err
	}
	if objectKind == "shadow" || objectKind == "" {
		return meta, errors.New("this SQLite object is not available")
	}
	meta.Kind = objectKind
	meta.RowID = objectKind == "table" && withoutRowID == 0
	meta.WithoutRowID = withoutRowID != 0
	rows, err := conn.QueryContext(ctx, "PRAGMA main.table_xinfo("+sqlitedb.Quote(table)+")")
	if err != nil {
		return meta, err
	}
	keys := map[int]string{}
	for rows.Next() {
		var cid, notNull, pk, hidden int
		var name, kind string
		var defaultValue sql.NullString
		if err = rows.Scan(&cid, &name, &kind, &notNull, &defaultValue, &pk, &hidden); err != nil {
			break
		}
		column := tableColumn{Name: name, Type: kind, Nullable: notNull == 0, Editable: hidden == 0, DefinitionEditable: hidden == 0, Affinity: sqlitedb.Affinity(kind), PrimaryOrder: pk, Hidden: hidden != 0}
		if hidden == 2 {
			column.Generated = "VIRTUAL"
		} else if hidden == 3 {
			column.Generated = "STORED"
		}
		if defaultValue.Valid {
			value := defaultValue.String
			column.Default = &value
			column.DefaultSource = sqlitedb.DefaultSource(value)
		}
		if pk > 0 {
			column.Key = "PRIMARY KEY"
			keys[pk] = name
		}
		meta.Columns = append(meta.Columns, column)
	}
	if err == nil {
		err = rows.Err()
	}
	rows.Close()
	if err != nil {
		return meta, err
	}
	for i := 1; i <= len(keys); i++ {
		if name, ok := keys[i]; ok {
			meta.PrimaryKey = append(meta.PrimaryKey, name)
		}
	}
	if meta.RowID {
		for _, candidate := range []string{"rowid", "_rowid_", "oid"} {
			if !slices.ContainsFunc(meta.Columns, func(c tableColumn) bool { return strings.EqualFold(c.Name, candidate) }) {
				meta.RowIDName = candidate
				break
			}
		}
		if meta.RowIDName != "" {
			meta.PrimaryKey = []string{meta.RowIDName}
			meta.Columns = append([]tableColumn{{Name: meta.RowIDName, Type: "INTEGER", Affinity: "INTEGER", Nullable: false, Key: "ROWID", Editable: false}}, meta.Columns...)
		}
	}
	meta.Editable = meta.Kind == "table" && (meta.RowIDName != "" || !meta.RowID && len(meta.PrimaryKey) > 0)
	if !meta.Editable {
		meta.ReadOnlyReason = "Views and tables without an accessible rowid or primary key are read-only."
	}
	if objectKind == "virtual" {
		meta.Editable = false
		meta.ReadOnlyReason = "Virtual tables are read-only in the general table editor."
	}
	if !withDetails {
		return meta, nil
	}
	idx, err := conn.QueryContext(ctx, "PRAGMA main.index_list("+sqlitedb.Quote(table)+")")
	if err != nil {
		return meta, err
	}
	for idx.Next() {
		var seq, unique, partial int
		var name, origin string
		if err = idx.Scan(&seq, &name, &unique, &origin, &partial); err != nil {
			break
		}
		item := tableIndex{Name: name, Unique: unique != 0, Primary: origin == "pk", Method: "BTREE", Columns: []string{}, Directions: []string{}, Managed: origin != "c", Editable: origin == "c", Partial: partial != 0}
		meta.Indexes = append(meta.Indexes, item)
	}
	if err == nil {
		err = idx.Err()
	}
	idx.Close()
	if err != nil {
		return meta, err
	}
	for i := range meta.Indexes {
		item := &meta.Indexes[i]
		var definition sql.NullString
		if err = conn.QueryRowContext(ctx, "SELECT sql FROM main.sqlite_schema WHERE type='index' AND name=?", item.Name).Scan(&definition); err != nil && !errors.Is(err, sql.ErrNoRows) {
			return meta, err
		}
		err = nil
		item.Definition = definition.String
		parts, queryErr := conn.QueryContext(ctx, "PRAGMA main.index_xinfo("+sqlitedb.Quote(item.Name)+")")
		if queryErr != nil {
			return meta, queryErr
		}
		for parts.Next() {
			var seq, cid, desc, key int
			var name, collation sql.NullString
			if err = parts.Scan(&seq, &cid, &name, &desc, &collation, &key); err != nil {
				break
			}
			if key != 0 {
				if cid < 0 || !name.Valid {
					item.Expression = true
				} else {
					item.Columns = append(item.Columns, name.String)
				}
				if desc != 0 {
					item.Directions = append(item.Directions, "DESC")
				} else {
					item.Directions = append(item.Directions, "ASC")
				}
			}
		}
		if err == nil {
			err = parts.Err()
		}
		parts.Close()
		if err != nil {
			return meta, err
		}
		if item.Definition == "" {
			item.Definition = "(" + strings.Join(item.Columns, ", ") + ")"
		}
	}
	if len(keys) > 0 {
		ordered := make([]string, len(keys))
		for order, name := range keys {
			ordered[order-1] = name
		}
		meta.Constraints = append(meta.Constraints, tableConstraint{Name: "PRIMARY KEY", Type: "PRIMARY KEY", Definition: "PRIMARY KEY (" + strings.Join(ordered, ", ") + ")", Validated: true, Columns: ordered, ReferenceColumns: []string{}})
	}
	for _, index := range meta.Indexes {
		if index.Managed && index.Unique && !index.Primary {
			meta.Constraints = append(meta.Constraints, tableConstraint{Name: index.Name, Type: "UNIQUE", Definition: index.Definition, Validated: true, Columns: index.Columns, ReferenceColumns: []string{}})
		}
	}
	fk, err := conn.QueryContext(ctx, "PRAGMA main.foreign_key_list("+sqlitedb.Quote(table)+")")
	if err != nil {
		return meta, err
	}
	for fk.Next() {
		var id, seq int
		var target, from, to, onUpdate, onDelete, match string
		if err = fk.Scan(&id, &seq, &target, &from, &to, &onUpdate, &onDelete, &match); err != nil {
			break
		}
		name := fmt.Sprintf("fk_%d", id)
		index := slices.IndexFunc(meta.Constraints, func(c tableConstraint) bool { return c.Name == name })
		if index < 0 {
			meta.Constraints = append(meta.Constraints, tableConstraint{Name: name, Type: "FOREIGN KEY", Definition: "REFERENCES " + sqlitedb.Quote(target) + " ON UPDATE " + onUpdate + " ON DELETE " + onDelete, Validated: true, Columns: []string{}, ReferenceTable: target, ReferenceColumns: []string{}})
			index = len(meta.Constraints) - 1
		}
		meta.Constraints[index].Columns = append(meta.Constraints[index].Columns, from)
		meta.Constraints[index].ReferenceColumns = append(meta.Constraints[index].ReferenceColumns, to)
	}
	if err == nil {
		err = fk.Err()
	}
	fk.Close()
	if err == nil && meta.Kind == "table" {
		if parts, _, parseErr := sqlitedb.TableParts(meta.Definition); parseErr == nil {
			checkNumber := 0
			for _, part := range parts {
				tokens, tokenErr := sqlitedb.DDLTokens(part)
				if tokenErr != nil {
					continue
				}
				for i, token := range tokens {
					if !strings.EqualFold(token.Text, "CHECK") || i+1 >= len(tokens) || !strings.HasPrefix(tokens[i+1].Text, "(") {
						continue
					}
					checkNumber++
					name := fmt.Sprintf("check_%d", checkNumber)
					if i >= 2 && strings.EqualFold(tokens[i-2].Text, "CONSTRAINT") {
						name = sqlitedb.DDLName(tokens[i-1].Text)
					}
					meta.Constraints = append(meta.Constraints, tableConstraint{Name: name, Type: "CHECK", Definition: part, Expression: tokens[i+1].Text, Validated: true, Columns: []string{}, ReferenceColumns: []string{}})
				}
			}
		}
	}
	return meta, err
}

func sqliteValue(value any) (*string, bool, int) {
	if value == nil {
		return nil, false, 0
	}
	switch v := value.(type) {
	case []byte:
		length := len(v)
		preview := len(v) > postgresPreviewLength
		if preview {
			v = v[:postgresPreviewLength]
		}
		result := `\x` + strings.ToUpper(hex.EncodeToString(v))
		return &result, preview, length
	case int64:
		result := strconv.FormatInt(v, 10)
		return &result, false, len(result)
	case float64:
		result := strconv.FormatFloat(v, 'g', -1, 64)
		return &result, false, len(result)
	case string:
		length := len(v)
		preview := len(v) > postgresPreviewLength
		if preview {
			v = v[:postgresPreviewLength]
			v = strings.ToValidUTF8(v, "")
		}
		return &v, preview, length
	default:
		result := fmt.Sprint(v)
		return &result, false, len(result)
	}
}

func sqliteKeyValue(value any) (string, string) {
	switch typed := value.(type) {
	case int64:
		return strconv.FormatInt(typed, 10), "integer"
	case float64:
		return strconv.FormatFloat(typed, 'g', -1, 64), "real"
	case []byte:
		return hex.EncodeToString(typed), "blob"
	case string:
		return typed, "text"
	default:
		return "", "null"
	}
}

type sqliteCursor struct {
	Table  string   `json:"t"`
	Size   int      `json:"s"`
	Values []string `json:"v"`
	Types  []string `json:"y"`
}

func (a *application) handleSQLiteRows(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	page, pageErr := strconv.Atoi(q.Get("page"))
	size, sizeErr := strconv.Atoi(q.Get("pageSize"))
	if pageErr != nil || sizeErr != nil || page < 1 || page > 1000000 || size < 1 || size > 100 || !sqlitedb.ValidName(q.Get("table")) || q.Get("schema") != "" {
		writeError(w, 400, "invalid_page", "Choose a table and a page size between 1 and 100.")
		return
	}
	conn, ctx, cleanup := a.openSQLiteRequest(w, r, q.Get("database"), true)
	if conn == nil {
		return
	}
	defer cleanup()
	started := time.Now()
	meta, err := readSQLiteMeta(ctx, conn, q.Get("table"))
	if err != nil {
		writeSQLiteError(w, err)
		return
	}
	result := tablePage{Columns: meta.Columns, Rows: [][]*string{}, Truncated: [][]bool{}, Versions: []string{}, KeyValues: [][]string{}, KeyTypes: [][]string{}, PrimaryKey: meta.PrimaryKey, Editable: meta.Editable, ReadOnlyReason: meta.ReadOnlyReason, Indexes: meta.Indexes, Constraints: meta.Constraints, SQLiteStrict: meta.Strict, SQLiteWithoutRowID: meta.WithoutRowID, SQLiteDDL: meta.Definition, SQLiteVirtual: meta.Kind == "virtual"}
	result.CursorPaging = meta.Editable
	selected := make([]string, len(meta.Columns))
	for i, c := range meta.Columns {
		selected[i] = sqlitedb.Quote(c.Name)
	}
	suffix := " FROM main." + sqlitedb.Quote(q.Get("table"))
	args := []any{}
	if result.CursorPaging {
		if page > 1 {
			var cursor sqliteCursor
			bytes, decodeErr := base64.RawURLEncoding.DecodeString(q.Get("cursor"))
			if decodeErr != nil || len(bytes) > 8192 || json.Unmarshal(bytes, &cursor) != nil || cursor.Table != q.Get("table") || cursor.Size != size {
				writeError(w, 400, "invalid_page_cursor", "This page is no longer available. Return to the first page.")
				return
			}
			keys, keyErr := sqliteRowIdentity(meta, tableRowKey{Values: cursor.Values, Types: cursor.Types, Version: strings.Repeat("0", 64)})
			if keyErr != nil {
				writeError(w, 400, "invalid_page_cursor", "This page is no longer available. Return to the first page.")
				return
			}
			order := make([]string, len(meta.PrimaryKey))
			for i, name := range meta.PrimaryKey {
				order[i] = sqlitedb.Quote(name)
			}
			suffix += " WHERE (" + strings.Join(order, ",") + ") > (" + strings.TrimSuffix(strings.Repeat("?,", len(order)), ",") + ")"
			args = append(args, keys...)
		}
		order := make([]string, len(meta.PrimaryKey))
		for i, name := range meta.PrimaryKey {
			order[i] = sqlitedb.Quote(name)
		}
		suffix += " ORDER BY " + strings.Join(order, ",")
	} else {
		suffix += " ORDER BY "
		order := meta.PrimaryKey
		if len(order) == 0 {
			for _, column := range meta.Columns {
				order = append(order, column.Name)
			}
		}
		for i, name := range order {
			if i > 0 {
				suffix += ","
			}
			suffix += sqlitedb.Quote(name)
		}
	}
	suffix += " LIMIT ?"
	args = append(args, size+1)
	if !result.CursorPaging {
		suffix += " OFFSET ?"
		args = append(args, (page-1)*size)
	}
	if _, err = conn.ExecContext(ctx, "BEGIN"); err != nil {
		writeSQLiteError(w, err)
		return
	}
	defer conn.ExecContext(context.Background(), "ROLLBACK")
	lengths := make([]string, len(selected))
	for i, column := range selected {
		lengths[i] = "COALESCE(length(CAST(" + column + " AS BLOB)),0)"
	}
	preflight := "SELECT COALESCE(SUM(byte_count),0) FROM (SELECT (" + strings.Join(lengths, "+") + ") AS byte_count" + suffix + ")"
	var pageBytes int64
	if err = conn.QueryRowContext(ctx, preflight, args...).Scan(&pageBytes); err != nil {
		writeSQLiteError(w, err)
		return
	}
	if pageBytes > 8<<20 {
		writeError(w, 413, "page_too_large", "This page exceeds 8 MB. Choose a smaller page size.")
		return
	}
	statement := "SELECT " + strings.Join(selected, ",") + suffix
	rows, err := conn.QueryContext(ctx, statement, args...)
	if err != nil {
		writeSQLiteError(w, err)
		return
	}
	defer rows.Close()
	bytesTotal := 0
	var lastValues, lastTypes []string
	for rows.Next() {
		raw := make([]any, len(meta.Columns))
		dest := make([]any, len(raw))
		for i := range raw {
			dest[i] = &raw[i]
		}
		if err = rows.Scan(dest...); err != nil {
			writeSQLiteError(w, err)
			return
		}
		if len(result.Rows) == size {
			result.HasMore = true
			break
		}
		row := make([]*string, len(raw))
		truncated := make([]bool, len(raw))
		for i, value := range raw {
			var length int
			row[i], truncated[i], length = sqliteValue(value)
			bytesTotal += length
		}
		if bytesTotal > 8<<20 {
			writeError(w, 413, "page_too_large", "This page exceeds 8 MB. Choose a smaller page size.")
			return
		}
		result.Rows = append(result.Rows, row)
		result.Truncated = append(result.Truncated, truncated)
		result.Versions = append(result.Versions, sqliteRowVersion(raw))
		keys := make([]string, len(meta.PrimaryKey))
		types := make([]string, len(meta.PrimaryKey))
		for i, name := range meta.PrimaryKey {
			index := slices.IndexFunc(meta.Columns, func(column tableColumn) bool { return column.Name == name })
			keys[i], types[i] = sqliteKeyValue(raw[index])
			bytesTotal += len(keys[i])
		}
		if bytesTotal > 8<<20 {
			writeError(w, 413, "page_too_large", "This page exceeds 8 MB. Choose a smaller page size.")
			return
		}
		result.KeyValues = append(result.KeyValues, keys)
		result.KeyTypes = append(result.KeyTypes, types)
		if result.CursorPaging {
			lastValues, lastTypes = keys, types
		}
	}
	if err = rows.Err(); err != nil {
		writeSQLiteError(w, err)
		return
	}
	if err = rows.Close(); err != nil {
		writeSQLiteError(w, err)
		return
	}
	if _, err = conn.ExecContext(ctx, "COMMIT"); err != nil {
		writeSQLiteError(w, err)
		return
	}
	if result.HasMore && result.CursorPaging {
		encoded, _ := json.Marshal(sqliteCursor{Table: q.Get("table"), Size: size, Values: lastValues, Types: lastTypes})
		result.NextCursor = base64.RawURLEncoding.EncodeToString(encoded)
	}
	result.Duration = time.Since(started).Milliseconds()
	writeJSON(w, 200, result)
}

func (a *application) handleSQLiteCount(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	if !sqlitedb.ValidName(q.Get("table")) || q.Get("schema") != "" {
		writeError(w, 400, "invalid_table", "Choose a table.")
		return
	}
	conn, ctx, cleanup := a.openSQLiteRequest(w, r, q.Get("database"), true)
	if conn == nil {
		return
	}
	defer cleanup()
	if _, err := readSQLiteMetaBasic(ctx, conn, q.Get("table")); err != nil {
		writeSQLiteError(w, err)
		return
	}
	var count int64
	if err := conn.QueryRowContext(ctx, "SELECT count(*) FROM main."+sqlitedb.Quote(q.Get("table"))).Scan(&count); err != nil {
		writeSQLiteError(w, err)
		return
	}
	writeJSON(w, 200, map[string]int64{"totalRows": count})
}

func (a *application) handleSQLiteColumns(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	conn, ctx, cleanup := a.openSQLiteRequest(w, r, q.Get("database"), true)
	if conn == nil {
		return
	}
	defer cleanup()
	meta, err := readSQLiteMetaBasic(ctx, conn, q.Get("table"))
	if err != nil {
		writeSQLiteError(w, err)
		return
	}
	writeJSON(w, 200, meta.Columns)
}
