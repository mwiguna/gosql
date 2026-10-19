package main

import (
	"context"
	"database/sql"
	"encoding/csv"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"slices"
	"strconv"
	"strings"

	"github.com/jackc/pgx/v5"
	mysqldb "gosql/database/mysql"
	sqlitedb "gosql/database/sqlite"
)

// CSV uses \N for NULL; non-null backslashes are doubled to preserve literal \N.
func csvValue(value *string) string {
	if value == nil {
		return `\N`
	}
	return strings.ReplaceAll(*value, `\`, `\\`)
}

func csvInput(value string) *string {
	if value == `\N` {
		return nil
	}
	value = strings.ReplaceAll(value, `\\`, `\`)
	return &value
}

func csvHeader(file io.Reader, allowed []string) (*csv.Reader, []string, error) {
	reader := csv.NewReader(file)
	reader.ReuseRecord = true
	header, err := reader.Read()
	if err != nil {
		return nil, nil, fmt.Errorf("read CSV header: %w", err)
	}
	header = append([]string(nil), header...)
	header[0] = strings.TrimPrefix(header[0], "\ufeff")
	seen := map[string]bool{}
	for _, name := range header {
		if seen[name] || !slices.Contains(allowed, name) {
			return nil, nil, fmt.Errorf("CSV column %q is missing, duplicated, or generated", name)
		}
		seen[name] = true
	}
	return reader, header, nil
}

func importCSVRows(reader *csv.Reader, insert func([]*string) error) (int64, error) {
	var count int64
	for {
		record, err := reader.Read()
		if err == io.EOF {
			return count, nil
		}
		if err != nil {
			return count, transferError(count, err)
		}
		values := make([]*string, len(record))
		for i, value := range record {
			values[i] = csvInput(value)
		}
		if err := insert(values); err != nil {
			return count, transferError(count, err)
		}
		count++
	}
}

func transferCSV(ctx context.Context, session *databaseSession, target transferTarget, sqlitePath string, file *os.File, importing bool) (int64, error) {
	fields := session.fields
	fields.Database = target.Database
	if fields.Engine == "PostgreSQL" {
		conn, err := connectPostgresMode(ctx, fields, session.password, !importing)
		if err != nil {
			return 0, err
		}
		defer conn.Close(context.Background())
		return postgresCSV(ctx, conn, target, file, importing)
	}
	var db *sql.DB
	var conn *sql.Conn
	var err error
	if fields.Engine == "SQLite" {
		db, conn, err = sqlitedb.Open(ctx, sqlitePath, !importing)
	} else {
		db, conn, err = openMySQL(ctx, fields, session.password, target.Database, false)
	}
	if err != nil {
		return 0, err
	}
	defer db.Close()
	defer conn.Close()
	return sqlCSV(ctx, conn, fields.Engine, target, file, importing)
}

func postgresCSV(ctx context.Context, conn *pgx.Conn, target transferTarget, file *os.File, importing bool) (int64, error) {
	metadata, err := conn.Query(ctx, `SELECT a.attname,pg_catalog.format_type(a.atttypid,a.atttypmod),c.relkind::text
		FROM pg_catalog.pg_attribute a JOIN pg_catalog.pg_class c ON c.oid=a.attrelid
		JOIN pg_catalog.pg_namespace n ON n.oid=c.relnamespace
		WHERE n.nspname=$1 AND c.relname=$2 AND a.attnum>0 AND NOT a.attisdropped AND a.attgenerated=''
		AND c.relkind IN ('r','p','v','m','f') ORDER BY a.attnum`, target.Schema, target.Table)
	if err != nil {
		return 0, err
	}
	names, types := []string{}, []string{}
	kind := ""
	for metadata.Next() {
		var name, columnType string
		if err := metadata.Scan(&name, &columnType, &kind); err != nil {
			metadata.Close()
			return 0, err
		}
		names, types = append(names, name), append(types, columnType)
	}
	err = metadata.Err()
	metadata.Close()
	if err != nil {
		return 0, err
	}
	if len(names) == 0 {
		return 0, errors.New("table has no exportable columns or is unavailable")
	}
	quote := func(name string) string { return pgx.Identifier{name}.Sanitize() }
	table := pgx.Identifier{target.Schema, target.Table}.Sanitize()
	if !importing {
		selected := make([]string, len(names))
		for i, name := range names {
			selected[i] = quote(name)
		}
		rows, err := conn.Query(ctx, "SELECT "+strings.Join(selected, ",")+" FROM "+table, pgx.QueryResultFormats{pgx.TextFormatCode})
		if err != nil {
			return 0, err
		}
		defer rows.Close()
		writer := csv.NewWriter(file)
		if err := writer.Write(names); err != nil {
			return 0, err
		}
		for rows.Next() {
			raw := rows.RawValues()
			record := make([]string, len(raw))
			for i, value := range raw {
				if value == nil {
					record[i] = csvValue(nil)
				} else {
					text := string(value)
					record[i] = csvValue(&text)
				}
			}
			if err := writer.Write(record); err != nil {
				return 0, err
			}
		}
		writer.Flush()
		if err := rows.Err(); err != nil {
			return 0, err
		}
		return 0, writer.Error()
	}
	if kind != "r" && kind != "p" {
		return 0, errors.New("CSV import requires a regular or partitioned table")
	}
	reader, header, err := csvHeader(file, names)
	if err != nil {
		return 0, err
	}
	columns, marks := make([]string, len(header)), make([]string, len(header))
	for i, name := range header {
		columns[i] = quote(name)
		marks[i] = fmt.Sprintf("$%d::text::%s", i+1, types[slices.Index(names, name)])
	}
	tx, err := conn.Begin(ctx)
	if err != nil {
		return 0, err
	}
	defer tx.Rollback(context.Background())
	statement := "INSERT INTO " + table + " (" + strings.Join(columns, ",") + ") OVERRIDING SYSTEM VALUE VALUES (" + strings.Join(marks, ",") + ")"
	if _, err = tx.Prepare(ctx, "gosql_csv", statement); err != nil {
		return 0, err
	}
	count, err := importCSVRows(reader, func(values []*string) error {
		args := make([]any, len(values))
		for i, value := range values {
			if value != nil {
				args[i] = *value
			}
		}
		_, err := tx.Exec(ctx, "gosql_csv", args...)
		return err
	})
	if err != nil {
		return 0, err
	}
	// Advance sequences for imported serial/identity columns without moving them backward.
	for _, name := range header {
		var sequence *string
		if err := tx.QueryRow(ctx, "SELECT pg_get_serial_sequence($1,$2)", table, name).Scan(&sequence); err != nil {
			return 0, err
		}
		if sequence != nil {
			var last, maximum int64
			if err := tx.QueryRow(ctx, "SELECT last_value FROM "+*sequence).Scan(&last); err != nil {
				return 0, err
			}
			if err := tx.QueryRow(ctx, "SELECT COALESCE(MAX("+quote(name)+"),0) FROM "+table).Scan(&maximum); err != nil {
				return 0, err
			}
			if maximum >= last {
				if _, err := tx.Exec(ctx, "SELECT setval($1::regclass,$2,true)", *sequence, maximum); err != nil {
					return 0, err
				}
			}
		}
	}
	if err = tx.Commit(ctx); err != nil {
		return 0, err
	}
	return count, nil
}

func sqlCSV(ctx context.Context, conn *sql.Conn, engine string, target transferTarget, file *os.File, importing bool) (int64, error) {
	columns := []tableColumn{}
	var mysqlColumns []mysqlColumn
	kind := ""
	quote := sqlitedb.Quote
	table := "main." + quote(target.Table)
	if engine == "SQLite" {
		meta, err := readSQLiteMetaBasic(ctx, conn, target.Table)
		if err != nil {
			return 0, err
		}
		kind = meta.Kind
		for _, column := range meta.Columns {
			if column.Key != "ROWID" && !column.Hidden && column.Generated == "" {
				columns = append(columns, column)
			}
		}
	} else {
		meta, err := readMySQLMeta(ctx, conn, target.Database, target.Table)
		if err != nil {
			return 0, err
		}
		kind = meta.Kind
		quote = mysqldb.Identifier
		table = quote(target.Database) + "." + quote(target.Table)
		for _, column := range meta.Columns {
			if !strings.Contains(strings.ToLower(column.Extra), "virtual generated") && !strings.Contains(strings.ToLower(column.Extra), "stored generated") {
				columns = append(columns, column.tableColumn)
				mysqlColumns = append(mysqlColumns, column)
			}
		}
	}
	if len(columns) == 0 {
		return 0, errors.New("table has no exportable columns")
	}
	names, selected := make([]string, len(columns)), make([]string, len(columns))
	for i, column := range columns {
		names[i], selected[i] = column.Name, quote(column.Name)
	}
	if !importing {
		rows, err := conn.QueryContext(ctx, "SELECT "+strings.Join(selected, ",")+" FROM "+table)
		if err != nil {
			return 0, err
		}
		defer rows.Close()
		writer := csv.NewWriter(file)
		if err := writer.Write(names); err != nil {
			return 0, err
		}
		for rows.Next() {
			raw := make([]any, len(names))
			dest := make([]any, len(names))
			for i := range raw {
				dest[i] = &raw[i]
			}
			if err := rows.Scan(dest...); err != nil {
				return 0, err
			}
			record := make([]string, len(names))
			for i, value := range raw {
				if value == nil {
					record[i] = csvValue(nil)
					continue
				}
				text := fmt.Sprint(value)
				if data, ok := value.([]byte); ok {
					if engine == "SQLite" || mysqlBinaryType(mysqlColumns[i].DataType) {
						text = `\x` + hex.EncodeToString(data)
					} else if mysqlColumns[i].DataType == "bit" {
						var number uint64
						for _, b := range data {
							number = number<<8 | uint64(b)
						}
						text = strconv.FormatUint(number, 10)
					} else {
						text = string(data)
					}
				}
				record[i] = csvValue(&text)
			}
			if err := writer.Write(record); err != nil {
				return 0, err
			}
		}
		writer.Flush()
		if err := rows.Err(); err != nil {
			return 0, err
		}
		return 0, writer.Error()
	}
	if kind != "table" && kind != "BASE TABLE" {
		return 0, errors.New("CSV import requires a regular table")
	}
	reader, header, err := csvHeader(file, names)
	if err != nil {
		return 0, err
	}
	selected = make([]string, len(header))
	for i, name := range header {
		selected[i] = quote(name)
	}
	tx, err := conn.BeginTx(ctx, nil)
	if err != nil {
		return 0, err
	}
	defer tx.Rollback()
	statement := "INSERT INTO " + table + " (" + strings.Join(selected, ",") + ") VALUES (" + strings.TrimSuffix(strings.Repeat("?,", len(header)), ",") + ")"
	prepared, err := tx.PrepareContext(ctx, statement)
	if err != nil {
		return 0, err
	}
	defer prepared.Close()
	count, err := importCSVRows(reader, func(values []*string) error {
		args := make([]any, len(values))
		for i, value := range values {
			index := slices.Index(names, header[i])
			var err error
			if engine == "SQLite" {
				if columns[index].Affinity == "BLOB" && value != nil && strings.HasPrefix(*value, `\x`) {
					args[i], err = hex.DecodeString((*value)[2:])
				} else {
					args[i], err = sqliteInputValue(columns[index], value)
				}
			} else {
				args[i], err = mysqlValue(mysqlColumns[index], value)
			}
			if err != nil {
				return err
			}
		}
		_, err := prepared.ExecContext(ctx, args...)
		return err
	})
	if err != nil {
		return 0, err
	}
	if err = tx.Commit(); err != nil {
		return 0, err
	}
	return count, nil
}
