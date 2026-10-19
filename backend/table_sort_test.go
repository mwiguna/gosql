package main

import (
	"database/sql"
	"encoding/json"
	"errors"
	"net/url"
	"path/filepath"
	"strings"
	"testing"

	mysqldb "gosql/database/mysql"
	sqlitedb "gosql/database/sqlite"
)

func TestTableSortValidation(t *testing.T) {
	columns := []tableColumn{{Name: "id"}, {Name: `a"b`}, {Name: "amount"}}
	for _, quote := range []func(string) string{sqlitedb.Quote, mysqldb.Identifier} {
		order, err := tableSortOrder(`[{"field":"a\"b","dir":"desc"},{"field":"amount","dir":"asc"}]`, columns, []string{"id"}, quote)
		if err != nil || order != quote(`a"b`)+" DESC,"+quote("amount")+" ASC,"+quote("id")+" ASC" {
			t.Fatalf("sorting lost quoting or stable key order: %q, %v", order, err)
		}
		order, err = tableSortOrder(`[{"field":"id","dir":"desc"}]`, columns, []string{"id"}, quote)
		if err != nil || order != quote("id")+" DESC" {
			t.Fatalf("primary key was duplicated: %q, %v", order, err)
		}
	}
	for _, raw := range []string{
		`broken`, `{}`, `[{"field":"missing","dir":"asc"}]`,
		`[{"field":"id","dir":"asc; DROP TABLE items"}]`,
		`[{"field":"id","dir":"asc"},{"field":"id","dir":"desc"}]`,
		strings.Repeat(" ", 8193),
	} {
		if _, err := tableSortOrder(raw, columns, nil, sqlitedb.Quote); !errors.Is(err, errInvalidTableSort) {
			t.Errorf("unsafe/invalid sort accepted: %q, %v", raw, err)
		}
	}
}

func TestSQLiteSortedPages(t *testing.T) {
	_, handler := testApplication(t)
	cookie, _ := setupAdmin(t, handler)
	path := filepath.Join(t.TempDir(), "sorted.sqlite")
	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	_, err = db.Exec(`CREATE TABLE items (id INTEGER PRIMARY KEY, "a""b" NUMERIC, label TEXT);
		INSERT INTO items VALUES (1,10,'z'),(2,2,'a'),(3,NULL,'n'),(4,2,'b'),(5,-5,'x');
		CREATE VIEW listing AS SELECT * FROM items;`)
	if err != nil {
		t.Fatal(err)
	}
	fields := connectionFields{Engine: "SQLite", Name: "Sorted", File: path, Location: "Local file"}
	created := request(t, handler, "POST", "/api/connections", queryBody(t, fields), cookie, 201)
	var profile connectionProfile
	if err = json.Unmarshal(created.Body.Bytes(), &profile); err != nil {
		t.Fatal(err)
	}
	base := "/api/connections/" + profile.ID
	request(t, handler, "POST", base+"/catalog", `{"password":""}`, cookie, 200)
	read := func(table, sorting, page string) tablePage {
		t.Helper()
		params := url.Values{"database": {"sorted"}, "table": {table}, "page": {page}, "pageSize": {"2"}, "sort": {sorting}}
		response := request(t, handler, "GET", base+"/rows?"+params.Encode(), "", cookie, 200)
		var result tablePage
		if err := json.Unmarshal(response.Body.Bytes(), &result); err != nil {
			t.Fatal(err)
		}
		return result
	}
	sorting := `[{"field":"a\"b","dir":"asc"}]`
	first, second := read("items", sorting, "1"), read("items", sorting, "2")
	if first.CursorPaging || first.NextCursor != "" || !first.HasMore || first.Rows[0][2] != nil || *first.Rows[1][2] != "-5" || *second.Rows[0][1] != "2" || *second.Rows[1][1] != "4" {
		t.Fatalf("sorted pages lost numeric/NULL order or stable identity: %+v %+v", first.Rows, second.Rows)
	}
	if !first.Editable || len(second.Versions) != 2 || len(second.KeyValues) != 2 {
		t.Fatal("sorting lost editable row metadata")
	}
	descending := read("items", `[{"field":"a\"b","dir":"desc"}]`, "1")
	if *descending.Rows[0][2] != "10" || *descending.Rows[1][2] != "2" {
		t.Fatal("descending numeric sorting failed")
	}
	view := read("listing", `[{"field":"label","dir":"desc"}]`, "1")
	if view.Editable || *view.Rows[0][2] != "z" {
		t.Fatal("view sorting failed")
	}
	if original := read("items", "", "1"); !original.CursorPaging {
		t.Fatal("clearing sorting did not restore cursor pagination")
	}
	params := url.Values{"database": {"sorted"}, "table": {"items"}, "page": {"1"}, "pageSize": {"2"}, "sort": {`[{"field":"missing","dir":"asc"}]`}}
	request(t, handler, "GET", base+"/rows?"+params.Encode(), "", cookie, 400)
}
