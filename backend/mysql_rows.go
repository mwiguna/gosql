package main

import (
	"context"
	"database/sql"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
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

type mysqlColumn struct {
	tableColumn
	DataType          string
	Extra             string
	Collation         string
	Comment           string
	DefaultExpression bool
}

type mysqlTableMeta struct {
	Kind           string
	Engine         string
	Columns        []mysqlColumn
	PrimaryKey     []string
	Editable       bool
	ReadOnlyReason string
	Indexes        []tableIndex
	Constraints    []tableConstraint
}

type mysqlPageCursor struct {
	Version int      `json:"v"`
	DB      string   `json:"d"`
	Table   string   `json:"t"`
	Size    int      `json:"s"`
	Keys    []string `json:"k"`
}

var errMySQLCursor = errors.New("invalid page cursor")
var errMySQLPageLarge = errors.New("database page exceeds response limit")

func readMySQLMeta(ctx context.Context, conn *sql.Conn, database, table string) (mysqlTableMeta, error) {
	meta := mysqlTableMeta{Columns: []mysqlColumn{}, PrimaryKey: []string{}, Indexes: []tableIndex{}, Constraints: []tableConstraint{}}
	mariaDB, major, minor, _, err := mysqldb.ReadServerVersion(ctx, conn)
	if err != nil {
		return meta, err
	}
	if err := conn.QueryRowContext(ctx, "SELECT TABLE_TYPE, COALESCE(ENGINE,'') FROM INFORMATION_SCHEMA.TABLES WHERE TABLE_SCHEMA=? AND TABLE_NAME=?", database, table).Scan(&meta.Kind, &meta.Engine); err != nil {
		return meta, err
	}
	rows, err := conn.QueryContext(ctx, `SELECT COLUMN_NAME,COLUMN_TYPE,DATA_TYPE,IS_NULLABLE,COLUMN_DEFAULT,EXTRA,COLLATION_NAME,COLUMN_COMMENT
		FROM INFORMATION_SCHEMA.COLUMNS WHERE TABLE_SCHEMA=? AND TABLE_NAME=? ORDER BY ORDINAL_POSITION`, database, table)
	if err != nil {
		return meta, err
	}
	for rows.Next() {
		var column mysqlColumn
		var nullable string
		var defaultValue sql.NullString
		var collation sql.NullString
		if err = rows.Scan(&column.Name, &column.Type, &column.DataType, &nullable, &defaultValue, &column.Extra, &collation, &column.Comment); err != nil {
			break
		}
		column.Collation = collation.String
		column.tableColumn.Collation = column.Collation
		column.tableColumn.Comment = column.Comment
		column.Nullable = nullable == "YES"
		if defaultValue.Valid {
			value := defaultValue.String
			if mariaDB {
				if len(value) >= 2 && value[0] == '\'' && value[len(value)-1] == '\'' {
					value = strings.ReplaceAll(value[1:len(value)-1], "''", "'")
				} else if value == "NULL" && column.Nullable {
					value = ""
				} else if _, numberErr := strconv.ParseFloat(value, 64); numberErr != nil {
					column.DefaultExpression = true
				}
			} else if strings.HasPrefix(strings.ToUpper(value), "CURRENT_") || strings.HasPrefix(value, "(") {
				column.DefaultExpression = true
			}
			if value != "" || defaultValue.String != "NULL" {
				column.Default = &value
			}
		}
		column.Editable = !strings.Contains(strings.ToLower(column.Extra), "generated")
		meta.Columns = append(meta.Columns, column)
	}
	if err == nil {
		err = rows.Err()
	}
	rows.Close()
	if err != nil {
		return meta, err
	}
	rows, err = conn.QueryContext(ctx, `SELECT COLUMN_NAME FROM INFORMATION_SCHEMA.KEY_COLUMN_USAGE
		WHERE TABLE_SCHEMA=? AND TABLE_NAME=? AND CONSTRAINT_NAME='PRIMARY' ORDER BY ORDINAL_POSITION`, database, table)
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
	if err == nil {
		err = rows.Err()
	}
	rows.Close()
	if err != nil {
		return meta, err
	}
	for i := range meta.Columns {
		if slices.Contains(meta.PrimaryKey, meta.Columns[i].Name) {
			meta.Columns[i].Key = "PRIMARY KEY"
		}
		column := &meta.Columns[i]
		var defaultErr error
		if column.Default != nil {
			_, defaultErr = mysqldb.Literal(*column.Default)
		}
		_, commentErr := mysqldb.Literal(column.Comment)
		column.DefinitionEditable = column.Extra == "" && !slices.Contains(meta.PrimaryKey, column.Name) && validMySQLType(column.Type) && !column.DefaultExpression && defaultErr == nil && commentErr == nil
	}
	meta.ReadOnlyReason = mysqlReadOnlyReason(meta)
	if mysqlSystemDatabase(database) {
		meta.ReadOnlyReason = "System databases are read-only."
	}
	meta.Editable = meta.ReadOnlyReason == ""
	indexExpression, indexVisible := "COALESCE(EXPRESSION,'')", "IS_VISIBLE"
	if mariaDB {
		indexExpression, indexVisible = "''", "'YES'"
		if major > 10 || major == 10 && minor >= 6 {
			indexVisible = "IF(IGNORED='YES','NO','YES')"
		}
	}
	rows, err = conn.QueryContext(ctx, `SELECT INDEX_NAME,NON_UNIQUE,INDEX_TYPE,COALESCE(COLUMN_NAME,''),COALESCE(SUB_PART,0),`+indexExpression+`,COALESCE(COLLATION,''),`+indexVisible+`
		FROM INFORMATION_SCHEMA.STATISTICS WHERE TABLE_SCHEMA=? AND TABLE_NAME=? ORDER BY INDEX_NAME,SEQ_IN_INDEX`, database, table)
	if err != nil {
		return meta, err
	}
	for rows.Next() {
		var name, method, column, expression, direction, visible string
		var nonUnique, prefix int
		if err = rows.Scan(&name, &nonUnique, &method, &column, &prefix, &expression, &direction, &visible); err != nil {
			break
		}
		index := slices.IndexFunc(meta.Indexes, func(item tableIndex) bool { return item.Name == name })
		if index < 0 {
			meta.Indexes = append(meta.Indexes, tableIndex{Name: name, Unique: nonUnique == 0, Primary: name == "PRIMARY", Method: method, Columns: []string{}})
			index = len(meta.Indexes) - 1
			meta.Indexes[index].Editable = name != "PRIMARY" && method == "BTREE"
		}
		item := &meta.Indexes[index]
		if column != "" {
			item.Columns = append(item.Columns, column)
		}
		if column == "" || prefix != 0 || expression != "" || direction != "A" || visible != "YES" {
			item.Editable = false
		}
	}
	if err == nil {
		err = rows.Err()
	}
	rows.Close()
	if err != nil {
		return meta, err
	}
	for i := range meta.Indexes {
		item := &meta.Indexes[i]
		item.Definition = item.Method + " (" + strings.Join(item.Columns, ", ") + ")"
	}
	enforced, checkJoin := "COALESCE(tc.ENFORCED,'YES')", ""
	if mariaDB {
		enforced = "'YES'"
		checkJoin = " AND cc.TABLE_NAME=tc.TABLE_NAME"
	}
	rows, err = conn.QueryContext(ctx, `SELECT tc.CONSTRAINT_NAME,tc.CONSTRAINT_TYPE,COALESCE(cc.CHECK_CLAUSE,''),`+enforced+`
		FROM INFORMATION_SCHEMA.TABLE_CONSTRAINTS tc
		LEFT JOIN INFORMATION_SCHEMA.CHECK_CONSTRAINTS cc ON cc.CONSTRAINT_SCHEMA=tc.CONSTRAINT_SCHEMA AND cc.CONSTRAINT_NAME=tc.CONSTRAINT_NAME`+checkJoin+`
		WHERE tc.TABLE_SCHEMA=? AND tc.TABLE_NAME=? ORDER BY tc.CONSTRAINT_NAME`, database, table)
	if err != nil {
		return meta, err
	}
	for rows.Next() {
		var item tableConstraint
		var enforced string
		if err = rows.Scan(&item.Name, &item.Type, &item.Expression, &enforced); err != nil {
			break
		}
		item.Validated = enforced == "YES"
		item.Columns = []string{}
		item.ReferenceColumns = []string{}
		meta.Constraints = append(meta.Constraints, item)
	}
	if err == nil {
		err = rows.Err()
	}
	rows.Close()
	if err != nil {
		return meta, err
	}
	for i := range meta.Constraints {
		item := &meta.Constraints[i]
		rows, err = conn.QueryContext(ctx, `SELECT COALESCE(COLUMN_NAME,''),COALESCE(REFERENCED_TABLE_SCHEMA,''),COALESCE(REFERENCED_TABLE_NAME,''),COALESCE(REFERENCED_COLUMN_NAME,'')
			FROM INFORMATION_SCHEMA.KEY_COLUMN_USAGE WHERE TABLE_SCHEMA=? AND TABLE_NAME=? AND CONSTRAINT_NAME=? ORDER BY ORDINAL_POSITION`, database, table, item.Name)
		if err != nil {
			return meta, err
		}
		for rows.Next() {
			var column, refDB, refTable, refColumn string
			if err = rows.Scan(&column, &refDB, &refTable, &refColumn); err != nil {
				break
			}
			if column != "" {
				item.Columns = append(item.Columns, column)
			}
			if refColumn != "" {
				item.ReferenceSchema, item.ReferenceTable = refDB, refTable
				item.ReferenceColumns = append(item.ReferenceColumns, refColumn)
			}
		}
		if err == nil {
			err = rows.Err()
		}
		rows.Close()
		if err != nil {
			return meta, err
		}
		item.Definition = item.Type
		if item.Type == "CHECK" {
			item.Definition += " (" + item.Expression + ")"
		} else {
			item.Definition += " (" + strings.Join(item.Columns, ", ") + ")"
		}
		if item.Type == "FOREIGN KEY" {
			item.Definition += " REFERENCES " + item.ReferenceSchema + "." + item.ReferenceTable + " (" + strings.Join(item.ReferenceColumns, ", ") + ")"
		}
	}
	for i := range meta.Indexes {
		index := &meta.Indexes[i]
		for _, constraint := range meta.Constraints {
			if constraint.Type == "PRIMARY KEY" && index.Primary ||
				constraint.Type == "UNIQUE" && index.Name == constraint.Name ||
				constraint.Type == "FOREIGN KEY" && len(index.Columns) >= len(constraint.Columns) && slices.Equal(index.Columns[:len(constraint.Columns)], constraint.Columns) {
				index.Managed = true
				index.Editable = false
			}
		}
	}
	return meta, nil
}

func mysqlBinaryType(kind string) bool {
	return strings.Contains(kind, "blob") || slices.Contains([]string{"binary", "varbinary"}, kind)
}

func mysqlPreviewType(kind string) bool {
	return mysqlBinaryType(kind) || strings.Contains(kind, "text") || slices.Contains([]string{"varchar", "json", "char"}, kind)
}

func mysqlReadOnlyReason(meta mysqlTableMeta) string {
	if meta.Kind != "BASE TABLE" {
		return "This view is read-only."
	}
	if !strings.EqualFold(meta.Engine, "InnoDB") && !strings.EqualFold(meta.Engine, "MyISAM") && !strings.EqualFold(meta.Engine, "Aria") {
		return "Editing supports InnoDB, MyISAM, and Aria; this table uses " + meta.Engine + "."
	}
	if len(meta.PrimaryKey) == 0 {
		return "Editing requires a primary key."
	}
	for _, column := range meta.Columns {
		if !slices.Contains([]string{"tinyint", "smallint", "mediumint", "int", "bigint", "decimal", "float", "double", "bit", "char", "varchar", "tinytext", "text", "mediumtext", "longtext", "binary", "varbinary", "tinyblob", "blob", "mediumblob", "longblob", "date", "time", "datetime", "timestamp", "year", "json", "enum", "set"}, column.DataType) {
			return "Editing does not support column " + column.Name + " (" + column.Type + ")."
		}
	}
	for _, key := range meta.PrimaryKey {
		column, found := mysqlSelectedColumn(meta, key)
		if !found || mysqlBinaryType(column.DataType) || column.DataType == "json" {
			return "Editing does not support primary key column " + key + "."
		}
	}
	return ""
}

func mysqlVersionSQL(columns []mysqlColumn) string {
	parts := []string{"'v1:'"}
	lengths := []string{}
	for _, column := range columns {
		name := mysqldb.Identifier(column.Name)
		value := "CAST(" + name + " AS BINARY)"
		if column.DataType == "bit" {
			value = "HEX(" + name + ")"
		}
		if strings.Contains(column.DataType, "geometry") || slices.Contains([]string{"point", "polygon", "linestring", "multipoint", "multilinestring", "multipolygon", "geometrycollection"}, column.DataType) {
			value = "ST_AsWKB(" + name + ")"
		}
		parts = append(parts, "0x"+hex.EncodeToString([]byte(column.Name+":"+column.Type+":")), "IF("+name+" IS NULL,'N',CONCAT('V',SHA2("+value+",256)))")
		lengths = append(lengths, "COALESCE(OCTET_LENGTH("+value+"),0)")
	}
	return "CASE WHEN " + strings.Join(lengths, "+") + " > 1048576 THEN '' ELSE SHA2(CONCAT(" + strings.Join(parts, ",") + "),256) END"
}

func readMySQLPage(ctx context.Context, conn *sql.Conn, database, table string, page, size int, cursor string) (tablePage, error) {
	started := time.Now()
	result := tablePage{Columns: []tableColumn{}, Rows: [][]*string{}, Truncated: [][]bool{}, Versions: []string{}, PrimaryKey: []string{}, Indexes: []tableIndex{}, Constraints: []tableConstraint{}}
	meta, err := readMySQLMeta(ctx, conn, database, table)
	if err != nil {
		return result, err
	}
	result.PrimaryKey, result.Indexes, result.Constraints, result.Editable = meta.PrimaryKey, meta.Indexes, meta.Constraints, meta.Editable
	result.ReadOnlyReason = meta.ReadOnlyReason
	for _, column := range meta.Columns {
		result.Columns = append(result.Columns, column.tableColumn)
	}
	result.CursorPaging = len(meta.PrimaryKey) > 0
	for _, key := range meta.PrimaryKey {
		index := slices.IndexFunc(meta.Columns, func(column mysqlColumn) bool { return column.Name == key })
		if index < 0 || !slices.Contains([]string{"tinyint", "smallint", "mediumint", "int", "bigint", "decimal", "bit", "char", "varchar", "date", "time", "datetime", "timestamp", "year", "enum", "set"}, meta.Columns[index].DataType) {
			result.CursorPaging = false
		}
	}
	if result.CursorPaging && page > 1 && cursor == "" {
		return result, errMySQLCursor
	}
	selected := make([]string, len(meta.Columns))
	preview := make([]bool, len(meta.Columns))
	for i, column := range meta.Columns {
		name := mysqldb.Identifier(column.Name)
		selected[i] = name
		if column.DataType == "bit" {
			selected[i] = "CAST(" + name + " AS UNSIGNED)"
		} else if mysqlBinaryType(column.DataType) && !slices.Contains(meta.PrimaryKey, column.Name) {
			selected[i] = "CONCAT(CHAR(92),'x',HEX(SUBSTRING(" + name + ",1,257)))"
			preview[i] = true
		} else if mysqlPreviewType(column.DataType) && !slices.Contains(meta.PrimaryKey, column.Name) {
			selected[i] = "LEFT(" + name + ",513)"
			preview[i] = true
		}
	}
	if meta.Editable {
		selected = append(selected, mysqlVersionSQL(meta.Columns))
	} else {
		selected = append(selected, "''")
	}
	statement := "SELECT " + strings.Join(selected, ",") + " FROM " + mysqldb.Identifier(database) + "." + mysqldb.Identifier(table)
	args := []any{}
	if result.CursorPaging && page > 1 {
		var parsed mysqlPageCursor
		decoded, decodeErr := base64.RawURLEncoding.DecodeString(cursor)
		if len(cursor) > 8192 || decodeErr != nil || json.Unmarshal(decoded, &parsed) != nil || parsed.Version != 1 || parsed.DB != database || parsed.Table != table || parsed.Size != size || len(parsed.Keys) != len(meta.PrimaryKey) {
			return result, errMySQLCursor
		}
		keys := make([]string, len(meta.PrimaryKey))
		marks := make([]string, len(keys))
		for i, key := range meta.PrimaryKey {
			column, _ := mysqlSelectedColumn(meta, key)
			keys[i], marks[i] = mysqldb.Identifier(key), mysqlKeyParameter(column)
			args = append(args, parsed.Keys[i])
		}
		statement += " WHERE (" + strings.Join(keys, ",") + ") > (" + strings.Join(marks, ",") + ")"
	}
	if result.CursorPaging {
		keys := make([]string, len(meta.PrimaryKey))
		for i, key := range meta.PrimaryKey {
			keys[i] = mysqldb.Identifier(key)
		}
		statement += " ORDER BY " + strings.Join(keys, ",")
	}
	statement += " LIMIT ?"
	args = append(args, size+1)
	if !result.CursorPaging {
		statement += " OFFSET ?"
		args = append(args, (page-1)*size)
	}
	rows, err := conn.QueryContext(ctx, statement, args...)
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
		raw := make([]sql.RawBytes, len(selected))
		dest := make([]any, len(raw))
		for i := range raw {
			dest[i] = &raw[i]
		}
		if err = rows.Scan(dest...); err != nil {
			return result, err
		}
		values := make([]*string, len(meta.Columns))
		truncated := make([]bool, len(values))
		for i := range values {
			bytes += len(raw[i])
			if bytes > 8<<20 {
				return result, errMySQLPageLarge
			}
			if raw[i] == nil {
				continue
			}
			value := string(raw[i])
			if preview[i] {
				if mysqlBinaryType(meta.Columns[i].DataType) {
					if len(value) > 514 {
						value = value[:514]
						truncated[i] = true
					}
				} else if utf8.RuneCountInString(value) > 512 {
					value = string([]rune(value)[:512])
					truncated[i] = true
				}
			}
			values[i] = &value
		}
		result.Rows = append(result.Rows, values)
		result.Truncated = append(result.Truncated, truncated)
		result.Versions = append(result.Versions, string(raw[len(raw)-1]))
	}
	if err = rows.Err(); err != nil {
		return result, err
	}
	if result.CursorPaging && result.HasMore && len(result.Rows) > 0 {
		last := result.Rows[len(result.Rows)-1]
		keys := make([]string, len(meta.PrimaryKey))
		for i, key := range meta.PrimaryKey {
			index := slices.IndexFunc(meta.Columns, func(column mysqlColumn) bool { return column.Name == key })
			if index < 0 || last[index] == nil {
				return result, errMySQLCursor
			}
			keys[i] = *last[index]
		}
		encoded, _ := json.Marshal(mysqlPageCursor{Version: 1, DB: database, Table: table, Size: size, Keys: keys})
		result.NextCursor = base64.RawURLEncoding.EncodeToString(encoded)
		if len(result.NextCursor) > 8192 {
			result.CursorPaging = false
			result.NextCursor = ""
		}
	}
	result.Duration = time.Since(started).Milliseconds()
	return result, nil
}

func (a *application) handleMySQLRows(w http.ResponseWriter, r *http.Request) {
	query := r.URL.Query()
	page, pageErr := strconv.Atoi(query.Get("page"))
	size, sizeErr := strconv.Atoi(query.Get("pageSize"))
	database, table := query.Get("database"), query.Get("table")
	if pageErr != nil || sizeErr != nil || page < 1 || page > 1000000 || size < 1 || size > 100 || !mysqldb.ValidName(database) || !mysqldb.ValidName(table) || query.Get("schema") != "" {
		writeError(w, 400, "invalid_page", "Choose a table and a page size between 1 and 100.")
		return
	}
	conn, ctx, cleanup := a.openMySQLRequest(w, r, database, false)
	if conn == nil {
		return
	}
	defer cleanup()
	result, err := readMySQLPage(ctx, conn, database, table, page, size, query.Get("cursor"))
	if errors.Is(err, errMySQLCursor) {
		writeError(w, 400, "invalid_page_cursor", "This page is no longer available. Return to the first page.")
	} else if errors.Is(err, errMySQLPageLarge) {
		writeError(w, 413, "page_too_large", "This page exceeds 8 MB. Choose a smaller page size.")
	} else if err != nil {
		writeMySQLError(w, err)
	} else {
		writeJSON(w, 200, result)
	}
}

func (a *application) handleMySQLCount(w http.ResponseWriter, r *http.Request) {
	database, table := r.URL.Query().Get("database"), r.URL.Query().Get("table")
	if !mysqldb.ValidName(database) || !mysqldb.ValidName(table) || r.URL.Query().Get("schema") != "" {
		writeError(w, 400, "invalid_table", "Choose a table.")
		return
	}
	actor, ok := a.requireUser(w, r)
	if !ok {
		return
	}
	session := a.getDatabaseSession(w, r, actor)
	if session == nil {
		return
	}
	if !session.hasDatabase(database) {
		writeError(w, 404, "database_not_available", "This database is not available for this connection.")
		return
	}
	select {
	case a.countSlot <- struct{}{}:
		defer func() { <-a.countSlot }()
	default:
		log.Printf("MySQL count skipped: connection=%q database=%q table=%q reason=count slot busy", r.PathValue("id"), database, table)
		writeError(w, 429, "count_busy", "Another row count is running. Retry shortly.")
		return
	}
	started := time.Now()
	ctx, cancel := context.WithTimeout(r.Context(), mysqldb.RequestTimeout)
	defer cancel()
	stop := context.AfterFunc(session.ctx, cancel)
	defer stop()
	db, conn, err := openMySQL(ctx, session.fields, session.password, database, false)
	if err != nil {
		log.Printf("MySQL count failed: connection=%q database=%q table=%q duration=%s error=%+v", r.PathValue("id"), database, table, time.Since(started), err)
		writeMySQLError(w, err)
		return
	}
	defer db.Close()
	defer conn.Close()
	var total int64
	if err = conn.QueryRowContext(ctx, "SELECT COUNT(*) FROM "+mysqldb.Identifier(database)+"."+mysqldb.Identifier(table)).Scan(&total); err != nil {
		log.Printf("MySQL count failed: connection=%q database=%q table=%q duration=%s error=%+v", r.PathValue("id"), database, table, time.Since(started), err)
		writeMySQLError(w, err)
		return
	}
	writeJSON(w, 200, map[string]int64{"totalRows": total})
}

func mysqlDecodeBinary(value string) ([]byte, error) {
	if !strings.HasPrefix(value, `\x`) {
		return nil, errors.New("invalid binary value")
	}
	return hex.DecodeString(value[2:])
}
