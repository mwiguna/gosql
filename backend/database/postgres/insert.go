package postgres

import (
	"context"
	"errors"
	"slices"
	"strings"

	"github.com/jackc/pgx/v5"
)

type InsertColumn struct {
	Name       string
	Type       string
	Category   string
	Nullable   bool
	HasDefault bool
	Primary    bool
	ForeignKey bool
}

func ReadInsertColumns(ctx context.Context, conn *pgx.Conn, schema, table string) ([]InsertColumn, error) {
	rows, err := conn.Query(ctx, `SELECT a.attname, t.typname, t.typcategory::text, NOT a.attnotnull,
		d.adbin IS NOT NULL OR a.attidentity <> '' OR a.attgenerated <> '',
		EXISTS (SELECT 1 FROM pg_catalog.pg_index i WHERE i.indrelid=c.oid AND i.indisprimary AND a.attnum=ANY(i.indkey)),
		EXISTS (SELECT 1 FROM pg_catalog.pg_constraint f WHERE f.conrelid=c.oid AND f.contype='f' AND a.attnum=ANY(f.conkey))
		FROM pg_catalog.pg_attribute a
		JOIN pg_catalog.pg_class c ON c.oid=a.attrelid
		JOIN pg_catalog.pg_namespace n ON n.oid=c.relnamespace
		JOIN pg_catalog.pg_type t ON t.oid=a.atttypid
		LEFT JOIN pg_catalog.pg_attrdef d ON d.adrelid=c.oid AND d.adnum=a.attnum
		WHERE n.nspname=$1 AND c.relname=$2 AND a.attnum>0 AND NOT a.attisdropped ORDER BY a.attnum`, schema, table)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	columns := []InsertColumn{}
	for rows.Next() {
		var column InsertColumn
		if err := rows.Scan(&column.Name, &column.Type, &column.Category, &column.Nullable, &column.HasDefault, &column.Primary, &column.ForeignKey); err != nil {
			return nil, err
		}
		columns = append(columns, column)
	}
	return columns, rows.Err()
}

func BuildQuickInsertSQL(table string, columns []InsertColumn) (string, bool, error) {
	names := []string{}
	values := []string{}
	generatedPK := false
	for _, column := range columns {
		if column.HasDefault {
			continue
		}
		if column.Nullable && column.ForeignKey {
			continue
		}
		if column.Nullable && slices.Contains([]string{"date", "time", "timetz", "timestamp", "timestamptz"}, column.Type) {
			continue
		}
		value := ""
		name := pgx.Identifier{column.Name}.Sanitize()
		if column.Primary && slices.Contains([]string{"int2", "int4", "int8", "numeric"}, column.Type) {
			value = "(SELECT COALESCE(MAX(" + name + "), 0) + 1 FROM " + table + ")"
			generatedPK = true
		} else {
			switch {
			case column.Category == "S":
				value = "''"
			case column.Category == "N":
				value = "0"
			case column.Type == "bool":
				value = "false"
			case column.Type == "json" || column.Type == "jsonb":
				value = "'{}'"
			case column.Type == "bytea":
				value = "''"
			case column.Category == "A":
				value = "'{}'"
			case column.Type == "date":
				value = "CURRENT_DATE"
			case column.Type == "time" || column.Type == "timetz":
				value = "CURRENT_TIME"
			case column.Type == "timestamp" || column.Type == "timestamptz":
				value = "CURRENT_TIMESTAMP"
			case column.Type == "interval":
				value = "INTERVAL '0'"
			case column.Type == "uuid":
				value = "gen_random_uuid()"
			case column.Nullable:
				continue
			default:
				return "", false, errors.New("column " + column.Name + " needs a database default or an explicit value")
			}
		}
		names = append(names, name)
		values = append(values, value)
	}
	if len(names) == 0 {
		return "INSERT INTO " + table + " DEFAULT VALUES", false, nil
	}
	return "INSERT INTO " + table + " (" + strings.Join(names, ", ") + ") VALUES (" + strings.Join(values, ", ") + ")", generatedPK, nil
}
