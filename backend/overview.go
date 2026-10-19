package main

import (
	"context"
	"database/sql"
	"net/http"
	"slices"

	mysqldb "gosql/database/mysql"
	sqlitedb "gosql/database/sqlite"
)

type tableSummary struct {
	Name      string `json:"name"`
	Rows      *int64 `json:"rows"`
	SizeBytes *int64 `json:"sizeBytes"`
	Estimated bool   `json:"estimated"`
	Virtual   bool   `json:"virtual,omitempty"`
}

func (a *application) handlePostgresOverview(w http.ResponseWriter, r *http.Request) {
	database, schema := r.URL.Query().Get("database"), r.URL.Query().Get("schema")
	if !validSchemaName(database) || !validSchemaName(schema) {
		writeError(w, 400, "invalid_schema", "Choose a database and schema.")
		return
	}
	conn, ctx, cleanup := a.openPostgresSchemaConnection(w, r, database)
	if conn == nil {
		return
	}
	defer cleanup()
	rows, err := conn.Query(ctx, `SELECT c.relname,
		CASE WHEN c.reltuples >= 0 THEN c.reltuples::bigint END,
		CASE WHEN c.relkind = 'r' THEN pg_catalog.pg_total_relation_size(c.oid) END
		FROM pg_catalog.pg_class c JOIN pg_catalog.pg_namespace n ON n.oid=c.relnamespace
		WHERE n.nspname=$1 AND c.relkind IN ('r','p','f')
		AND pg_catalog.has_schema_privilege(n.oid,'USAGE') ORDER BY c.relname`, schema)
	if err != nil {
		writePostgresResult(w, postgresResult{}, err)
		return
	}
	defer rows.Close()
	tables := []tableSummary{}
	for rows.Next() {
		table := tableSummary{Estimated: true}
		if err := rows.Scan(&table.Name, &table.Rows, &table.SizeBytes); err != nil {
			writePostgresResult(w, postgresResult{}, err)
			return
		}
		tables = append(tables, table)
	}
	if err := rows.Err(); err != nil {
		writePostgresResult(w, postgresResult{}, err)
		return
	}
	writeJSON(w, 200, tables)
}

func (a *application) handleMySQLOverview(w http.ResponseWriter, r *http.Request) {
	database := r.URL.Query().Get("database")
	if !mysqldb.ValidName(database) {
		writeError(w, 400, "invalid_database", "Choose a database.")
		return
	}
	conn, ctx, cleanup := a.openMySQLRequest(w, r, database, false)
	if conn == nil {
		return
	}
	defer cleanup()
	rows, err := conn.QueryContext(ctx, `SELECT TABLE_NAME,TABLE_ROWS,DATA_LENGTH+INDEX_LENGTH
		FROM INFORMATION_SCHEMA.TABLES WHERE TABLE_SCHEMA=? AND TABLE_TYPE='BASE TABLE' ORDER BY TABLE_NAME`, database)
	if err != nil {
		writeMySQLError(w, err)
		return
	}
	defer rows.Close()
	tables := []tableSummary{}
	for rows.Next() {
		table := tableSummary{Estimated: true}
		if err := rows.Scan(&table.Name, &table.Rows, &table.SizeBytes); err != nil {
			writeMySQLError(w, err)
			return
		}
		tables = append(tables, table)
	}
	if err := rows.Err(); err != nil {
		writeMySQLError(w, err)
		return
	}
	writeJSON(w, 200, tables)
}

func (a *application) handleSQLiteOverview(w http.ResponseWriter, r *http.Request) {
	conn, ctx, cleanup := a.openSQLiteRequest(w, r, r.URL.Query().Get("database"), true)
	if conn == nil {
		return
	}
	defer cleanup()
	tables, err := readSQLiteOverview(ctx, conn)
	if err != nil {
		writeSQLiteError(w, err)
		return
	}
	writeJSON(w, 200, tables)
}

func readSQLiteOverview(ctx context.Context, conn *sql.Conn) ([]tableSummary, error) {
	catalog, err := sqliteCatalogFor(ctx, conn, "main")
	if err != nil {
		return nil, err
	}
	// dbstat is optional in SQLite builds; unavailable sizes remain null.
	sizes := map[string]int64{}
	stats, err := conn.QueryContext(ctx, `SELECT s.tbl_name,SUM(d.pgsize) FROM main.sqlite_schema s
		JOIN dbstat d ON d.name=s.name WHERE s.type IN ('table','index') GROUP BY s.tbl_name`)
	if err == nil {
		for stats.Next() {
			var name string
			var size int64
			if err := stats.Scan(&name, &size); err != nil {
				stats.Close()
				return nil, err
			}
			sizes[name] = size
		}
		err = stats.Err()
		stats.Close()
		if err != nil {
			return nil, err
		}
	}
	tables := make([]tableSummary, 0, len(catalog.Tables))
	for _, name := range catalog.Tables {
		table := tableSummary{Name: name, Virtual: slices.Contains(catalog.VirtualTables, name)}
		if !table.Virtual {
			var count int64
			if err := conn.QueryRowContext(ctx, "SELECT count(*) FROM main."+sqlitedb.Quote(name)).Scan(&count); err != nil {
				return nil, err
			}
			table.Rows = &count
			if size, ok := sizes[name]; ok {
				table.SizeBytes = &size
			}
		}
		tables = append(tables, table)
	}
	return tables, nil
}
