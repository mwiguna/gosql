package main

import (
	"database/sql"
	"encoding/json"
	"path/filepath"
	"testing"
)

func TestSQLiteOverview(t *testing.T) {
	_, handler := testApplication(t)
	cookie, _ := setupAdmin(t, handler)
	path := filepath.Join(t.TempDir(), "overview.sqlite")
	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	_, err = db.Exec(`CREATE TABLE "a""b" (id INTEGER PRIMARY KEY, value TEXT);
		CREATE INDEX value_index ON "a""b"(value);
		INSERT INTO "a""b"(value) VALUES ('first'), ('second');
		CREATE TABLE empty (id INTEGER);
		CREATE VIEW listing AS SELECT * FROM "a""b";`)
	if err != nil {
		t.Fatal(err)
	}
	fields := connectionFields{Engine: "SQLite", Name: "Overview", File: path, Location: "Local file"}
	response := request(t, handler, "POST", "/api/connections", queryBody(t, fields), cookie, 201)
	var profile connectionProfile
	if err := json.Unmarshal(response.Body.Bytes(), &profile); err != nil {
		t.Fatal(err)
	}
	base := "/api/connections/" + profile.ID
	request(t, handler, "GET", base+"/overview?database=overview", "", nil, 401)
	request(t, handler, "GET", base+"/overview?database=overview", "", cookie, 409)
	request(t, handler, "POST", base+"/catalog", `{"password":""}`, cookie, 200)
	request(t, handler, "GET", base+"/overview?database=other", "", cookie, 404)
	read := func() []tableSummary {
		t.Helper()
		response := request(t, handler, "GET", base+"/overview?database=overview", "", cookie, 200)
		var tables []tableSummary
		if err := json.Unmarshal(response.Body.Bytes(), &tables); err != nil {
			t.Fatal(err)
		}
		return tables
	}
	tables := read()
	if len(tables) != 2 || tables[0].Name != `a"b` || tables[0].Rows == nil || *tables[0].Rows != 2 || tables[0].Estimated || tables[1].Rows == nil || *tables[1].Rows != 0 {
		t.Fatalf("unexpected table list/counts: %+v", tables)
	}
	if tables[0].SizeBytes == nil || tables[1].SizeBytes == nil || *tables[0].SizeBytes <= *tables[1].SizeBytes {
		t.Fatal("table size including its index must be available")
	}
	response = request(t, handler, "GET", base+"/rows?database=overview&table=a%22b&page=1&pageSize=20&includeSize=1", "", cookie, 200)
	var page tablePage
	if err := json.Unmarshal(response.Body.Bytes(), &page); err != nil || page.SizeBytes == nil || *page.SizeBytes != *tables[0].SizeBytes {
		t.Fatalf("data page size differs from overview: %+v, %v", page.SizeBytes, err)
	}
	request(t, handler, "DELETE", base+"/tables", queryBody(t, tableChange{Database: "overview", Table: `a"b`}), cookie, 200)
	if tables = read(); len(tables) != 1 || tables[0].Name != "empty" {
		t.Fatalf("deleted table remained in overview: %+v", tables)
	}
}
