package main

import (
	"database/sql"
	"encoding/json"
	"path/filepath"
	"testing"
)

func TestSQLiteConsoleCommand(t *testing.T) {
	for _, statement := range []string{
		"SELECT 1; -- trailing comment", "/* initial */ insert into t values ('a;b', 'it''s');",
		"UPDATE [a;b] SET `a;b`='x'; /* end */", "DELETE FROM \"a;b\"", "SELECT ';--/*';",
	} {
		if sqliteConsoleCommand(statement) == "" {
			t.Errorf("rejected %q", statement)
		}
	}
	for _, statement := range []string{
		"", "-- comment", "SELECT 1; DELETE FROM t", "SELECT 1;;", "ATTACH 'x' AS x",
		"PRAGMA foreign_keys=OFF", "CREATE TABLE t(x)", "BEGIN", "WITH t AS (SELECT 1) SELECT * FROM t",
		"SELECT 'unterminated", "SELECT 1 /* unterminated", "SELECT 1; /* comment */ UPDATE t SET x=1",
	} {
		if sqliteConsoleCommand(statement) != "" {
			t.Errorf("accepted %q", statement)
		}
	}
}

func TestSQLiteConsoleWrites(t *testing.T) {
	_, h := testApplication(t)
	cookie, _ := setupAdmin(t, h)
	path := filepath.Join(t.TempDir(), "console.sqlite")
	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	_, err = db.Exec(`CREATE TABLE parent(id INTEGER PRIMARY KEY);
		INSERT INTO parent VALUES(1);
		CREATE TABLE items(id INTEGER PRIMARY KEY, name TEXT NOT NULL UNIQUE,
			score INTEGER NOT NULL DEFAULT 1 CHECK(score>0), parent INTEGER REFERENCES parent(id),
			optional TEXT, derived TEXT GENERATED ALWAYS AS (name || '!') STORED) STRICT;
		CREATE TABLE defaults(id INTEGER PRIMARY KEY, value TEXT DEFAULT 'default');`)
	if err != nil {
		t.Fatal(err)
	}
	fields := connectionFields{Engine: "SQLite", Name: "Console", File: path, Location: "Local file"}
	w := request(t, h, "POST", "/api/connections", queryBody(t, fields), cookie, 201)
	var profile connectionProfile
	if err := json.Unmarshal(w.Body.Bytes(), &profile); err != nil {
		t.Fatal(err)
	}
	base := "/api/connections/" + profile.ID
	request(t, h, "POST", base+"/catalog", `{"password":""}`, cookie, 200)
	query := func(statement string, status int) map[string]any {
		t.Helper()
		w := request(t, h, "POST", base+"/query", queryBody(t, map[string]string{"database": "console", "sql": statement}), cookie, status)
		var result map[string]any
		if err := json.Unmarshal(w.Body.Bytes(), &result); err != nil {
			t.Fatal(err)
		}
		return result
	}
	result := query("/* start */ INSERT INTO items(name,parent) VALUES('a;b',1),('two',1); -- end", 200)
	if result["affectedRows"] != float64(2) || result["command"] != "INSERT" || len(result["columns"].([]any)) != 0 {
		t.Fatalf("insert result: %+v", result)
	}
	result = query("UPDATE items SET score=2 RETURNING name, score", 200)
	if result["affectedRows"] != float64(2) || len(result["rows"].([]any)) != 2 {
		t.Fatalf("returning result: %+v", result)
	}
	query("INSERT INTO defaults DEFAULT VALUES", 200)
	query("INSERT INTO items(name,optional) VALUES('nullable',NULL) RETURNING id, optional", 200)
	query("INSERT INTO items(name) VALUES('new'),('two')", 400)
	query("INSERT INTO items DEFAULT VALUES", 400)
	query("INSERT INTO items(name,score) VALUES('bad-check',0)", 400)
	query("INSERT INTO items(name,parent) VALUES('bad-fk',999)", 400)
	query("INSERT INTO items(name,score) VALUES('bad-type','text')", 400)
	query("UPDATE items SET score=-1", 400)
	query("INSERT INTO items(name) VALUES('too-large') RETURNING zeroblob(9437184)", 400)
	query("INSERT INTO items(name) VALUES('many-results-a'),('many-results-b') RETURNING hex(zeroblob(3000000))", 413)
	query("INSERT INTO items(name) WITH RECURSIVE n(x) AS (SELECT 1 UNION ALL SELECT x+1 FROM n WHERE x<10001) SELECT 'row-'||x FROM n RETURNING name", 413)
	for _, statement := range []string{"ATTACH DATABASE 'other' AS bad", "PRAGMA foreign_keys=OFF", "SELECT 1; DELETE FROM items", "DELETE FROM items; ATTACH 'x' AS x"} {
		query(statement, 400)
	}
	var count int
	if err := db.QueryRow("SELECT count(*) FROM items").Scan(&count); err != nil || count != 3 {
		t.Fatalf("rollback count=%d err=%v", count, err)
	}
	result = query("SELECT name, optional FROM items WHERE name='nullable'", 200)
	if result["rows"].([]any)[0].(map[string]any)["optional"] != nil {
		t.Fatal("NULL was changed")
	}
	result = query("DELETE FROM items WHERE name='nullable'", 200)
	if result["affectedRows"] != float64(1) {
		t.Fatalf("delete result: %+v", result)
	}
	request(t, h, "POST", base+"/rows/insert", `{"database":"console","schema":"","table":"items","values":{"name":"form","optional":""}}`, cookie, 200)
	request(t, h, "POST", base+"/rows/insert", `{"database":"console","schema":"","table":"items","values":{"name":"generated","derived":"bad"}}`, cookie, 400)
	var optional, derived string
	if err := db.QueryRow("SELECT optional,derived FROM items WHERE name='form'").Scan(&optional, &derived); err != nil || optional != "" || derived != "form!" {
		t.Fatalf("form values=%q %q err=%v", optional, derived, err)
	}
}
