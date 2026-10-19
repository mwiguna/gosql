package main

import (
	"bytes"
	"context"
	"encoding/json"
	"net/url"
	"os"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5"
)

func TestPostgresTransferIntegration(t *testing.T) {
	address := os.Getenv("GOSQL_TEST_POSTGRES_URL")
	if address == "" {
		t.Skip("GOSQL_TEST_POSTGRES_URL is not configured")
	}
	ctx := context.Background()
	conn, err := pgx.Connect(ctx, address)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close(ctx)
	schema := "transfer_" + strings.ToLower(newID())
	restoredDatabase := "restore_" + strings.ToLower(newID())
	sqlDatabase := "sql_" + strings.ToLower(newID())
	quoted := pgx.Identifier{schema}.Sanitize()
	if _, err := conn.Exec(ctx, "CREATE DATABASE "+pgx.Identifier{restoredDatabase}.Sanitize()); err != nil {
		t.Fatal(err)
	}
	defer conn.Exec(ctx, "DROP DATABASE "+pgx.Identifier{restoredDatabase}.Sanitize()+" WITH (FORCE)")
	if _, err := conn.Exec(ctx, "CREATE DATABASE "+pgx.Identifier{sqlDatabase}.Sanitize()); err != nil {
		t.Fatal(err)
	}
	defer conn.Exec(ctx, "DROP DATABASE "+pgx.Identifier{sqlDatabase}.Sanitize()+" WITH (FORCE)")
	_, err = conn.Exec(ctx, "CREATE SCHEMA "+quoted+`; CREATE TABLE `+quoted+`."a""b" (id bigint GENERATED ALWAYS AS IDENTITY PRIMARY KEY, value text, payload bytea, doubled bigint GENERATED ALWAYS AS (id*2) STORED);
		CREATE TABLE `+quoted+`.destination (LIKE `+quoted+`."a""b" INCLUDING ALL);
		INSERT INTO `+quoted+`."a""b" (id,value,payload) OVERRIDING SYSTEM VALUE VALUES (45, 'literal \N', E'\\x00ff'),(2,NULL,NULL);`)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Exec(ctx, "DROP SCHEMA "+quoted+" CASCADE")
	u, err := url.Parse(address)
	if err != nil {
		t.Fatal(err)
	}
	password, _ := u.User.Password()
	fields := connectionFields{Engine: "PostgreSQL", Name: "Transfer integration", Host: u.Hostname(), Port: u.Port(), Username: u.User.Username(), Database: strings.TrimPrefix(u.Path, "/"), SSL: "Disable"}
	_, handler := testApplication(t)
	cookie, _ := setupAdmin(t, handler)
	created := request(t, handler, "POST", "/api/connections", queryBody(t, fields), cookie, 201)
	var profile connectionProfile
	if err := json.Unmarshal(created.Body.Bytes(), &profile); err != nil {
		t.Fatal(err)
	}
	base := "/api/connections/" + profile.ID
	request(t, handler, "POST", base+"/catalog", queryBody(t, map[string]string{"password": password}), cookie, 200)
	endpoint := func(database, table, format string) string {
		params := url.Values{"database": {database}, "format": {format}}
		if table != "" {
			params.Set("table", table)
			params.Set("schema", schema)
		}
		return base + "/transfer?" + params.Encode()
	}
	csv := request(t, handler, "GET", endpoint(fields.Database, `a"b`, "csv"), "", cookie, 200)
	transferUpload(t, handler, endpoint(fields.Database, "destination", "csv"), csv.Body.Bytes(), cookie, 200)
	var next int64
	if err := conn.QueryRow(ctx, "INSERT INTO "+quoted+".destination DEFAULT VALUES RETURNING id").Scan(&next); err != nil || next <= 45 {
		t.Fatalf("imported identity sequence was not advanced: %d, %v", next, err)
	}
	tableDump := request(t, handler, "GET", endpoint(fields.Database, `a"b`, "dump"), "", cookie, 200)
	if !bytes.HasPrefix(tableDump.Body.Bytes(), []byte("PGDMP")) {
		t.Fatal("invalid PostgreSQL table archive")
	}
	dump := request(t, handler, "GET", endpoint(fields.Database, "", "dump"), "", cookie, 200)
	transferUpload(t, handler, endpoint(restoredDatabase, "", "dump"), dump.Body.Bytes(), cookie, 200)
	u.Path = "/" + restoredDatabase
	restored, err := pgx.Connect(ctx, u.String())
	if err != nil {
		t.Fatal(err)
	}
	defer restored.Close(ctx)
	var count int
	if err := restored.QueryRow(ctx, "SELECT count(*) FROM "+quoted+`."a""b"`).Scan(&count); err != nil || count != 2 {
		t.Fatalf("archive restore failed: %d, %v", count, err)
	}
	sqlDump := request(t, handler, "GET", endpoint(fields.Database, "", "sql"), "", cookie, 200)
	if bytes.HasPrefix(sqlDump.Body.Bytes(), []byte("PGDMP")) || !strings.Contains(sqlDump.Body.String(), "CREATE TABLE") || !strings.Contains(sqlDump.Body.String(), "COPY ") {
		t.Fatal("SQL export is not a plain-text database dump")
	}
	tableSQL := request(t, handler, "GET", endpoint(fields.Database, `a"b`, "sql"), "", cookie, 200)
	if !strings.Contains(tableSQL.Body.String(), "CREATE TABLE") {
		t.Fatal("SQL table export is missing its structure")
	}
	transferUpload(t, handler, endpoint(sqlDatabase, "", "sql"), sqlDump.Body.Bytes(), cookie, 200)
	u.Path = "/" + sqlDatabase
	sqlRestored, err := pgx.Connect(ctx, u.String())
	if err != nil {
		t.Fatal(err)
	}
	defer sqlRestored.Close(ctx)
	if err := sqlRestored.QueryRow(ctx, "SELECT count(*) FROM "+quoted+`."a""b"`).Scan(&count); err != nil || count != 2 {
		t.Fatalf("SQL restore failed: %d, %v", count, err)
	}
	transferUpload(t, handler, endpoint(sqlDatabase, "", "sql"), []byte("CREATE TABLE public.partial_import (id int);\n\\echo unexpected-client-command\n"), cookie, 400)
	var partial *string
	if err := sqlRestored.QueryRow(ctx, "SELECT to_regclass('public.partial_import')::text").Scan(&partial); err != nil || partial != nil {
		t.Fatal("blocked client command did not roll back SQL import")
	}
}
