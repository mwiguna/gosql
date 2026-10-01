package main

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	sqlitedb "gosql/database/sqlite"
)

func TestSQLiteMetadataAndRowVersion(t *testing.T) {
	path := filepath.Join(t.TempDir(), "sample.sqlite")
	seed, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	_, err = seed.Exec(`CREATE TABLE "notes" ("id" INTEGER PRIMARY KEY, "text" TEXT NOT NULL, "payload" BLOB);
		CREATE INDEX "notes_text" ON "notes"("text");
		INSERT INTO "notes"("text","payload") VALUES ('hello',x'00FF');`)
	if err != nil {
		t.Fatal(err)
	}
	seed.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err = sqlitedb.Check(ctx, path); err != nil {
		t.Fatal(err)
	}
	db, conn, err := sqlitedb.Open(ctx, path, false)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	defer conn.Close()
	catalog, err := sqliteCatalogFor(ctx, conn, "sample")
	if err != nil {
		t.Fatal(err)
	}
	if len(catalog.Tables) != 1 || catalog.Tables[0] != "notes" {
		t.Fatalf("catalog: %+v", catalog)
	}
	meta, err := readSQLiteMeta(ctx, conn, "notes")
	if err != nil {
		t.Fatal(err)
	}
	if !meta.Editable || len(meta.Indexes) != 1 || meta.PrimaryKey[0] != "rowid" {
		t.Fatalf("metadata: %+v", meta)
	}
	raw, err := sqliteReadRow(ctx, conn, meta, "notes", []any{int64(1)})
	if err != nil {
		t.Fatal(err)
	}
	if raw[0].(int64) != 1 || raw[2].(string) != "hello" {
		t.Fatalf("row: %#v", raw)
	}
	version := sqliteRowVersion(raw)
	if _, err = conn.ExecContext(ctx, `UPDATE notes SET text='changed' WHERE id=1`); err != nil {
		t.Fatal(err)
	}
	raw, err = sqliteReadRow(ctx, conn, meta, "notes", []any{int64(1)})
	if err != nil {
		t.Fatal(err)
	}
	if sqliteRowVersion(raw) == version {
		t.Fatal("row version did not change")
	}
}

func TestSingleSQLiteTriggerStatement(t *testing.T) {
	valid := `CREATE
	TRIGGER audit AFTER INSERT ON items BEGIN
		-- A semicolon inside a string is part of the trigger body.
		INSERT INTO log(message) VALUES ('before;after');
		SELECT CASE WHEN NEW.id > 0 THEN 1 ELSE 0 END;
	END;`
	if !singleSQLiteTrigger(valid) {
		t.Fatal("valid trigger body was rejected")
	}
	if singleSQLiteTrigger(valid + " DELETE FROM items;") {
		t.Fatal("second SQL statement was accepted")
	}
}

func TestSQLiteLocalConnectionFlow(t *testing.T) {
	_, h := testApplication(t)
	cookie, _ := setupAdmin(t, h)
	path := filepath.Join(t.TempDir(), "flow.sqlite")
	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = db.Exec(`CREATE TABLE "items" ("id" INTEGER PRIMARY KEY,"name" TEXT NOT NULL); INSERT INTO items(name) VALUES ('first');
		CREATE TRIGGER items_touch AFTER UPDATE ON items BEGIN SELECT NEW.name; END;
		CREATE TABLE "wr" ("code" TEXT,"seq" INTEGER,"value" TEXT,PRIMARY KEY("code","seq")) WITHOUT ROWID, STRICT;
		INSERT INTO wr(code,seq,value) VALUES ('A',9223372036854775807,'original')`); err != nil {
		t.Fatal(err)
	}
	db.Close()
	fields := connectionFields{Engine: "SQLite", Name: "Flow", File: path, Location: "Local file"}
	request(t, h, "POST", "/api/sqlite/test", queryBody(t, fields), cookie, 200)
	w := request(t, h, "POST", "/api/connections", queryBody(t, fields), cookie, 201)
	var profile connectionProfile
	if err = json.Unmarshal(w.Body.Bytes(), &profile); err != nil {
		t.Fatal(err)
	}
	base := "/api/connections/" + profile.ID
	w = request(t, h, "POST", base+"/catalog", `{"password":""}`, cookie, 200)
	var catalog sqliteCatalog
	if err = json.Unmarshal(w.Body.Bytes(), &catalog); err != nil || len(catalog.Triggers) != 1 || catalog.Triggers[0].Name != "items_touch" || catalog.Triggers[0].Table != "items" {
		t.Fatalf("trigger catalog: %+v %v", catalog.Triggers, err)
	}
	w = request(t, h, "GET", base+"/triggers?database=flow&table=items&name=items_touch", "", cookie, 200)
	if !strings.Contains(w.Body.String(), "CREATE TRIGGER items_touch") {
		t.Fatalf("trigger definition: %s", w.Body.String())
	}
	trigger := triggerChange{Database: "flow", Table: "items", Name: "items_insert", Definition: `CREATE TRIGGER "items_insert" AFTER INSERT ON main."items" FOR EACH ROW BEGIN SELECT NEW.name; END;`}
	request(t, h, "POST", base+"/triggers", queryBody(t, trigger), cookie, 200)
	trigger.Definition = `CREATE TRIGGER "items_insert" BEFORE INSERT ON "items" FOR EACH ROW BEGIN SELECT NEW.name; END;`
	request(t, h, "PATCH", base+"/triggers", queryBody(t, trigger), cookie, 200)
	w = request(t, h, "GET", base+"/triggers?database=flow&table=items&name=items_insert", "", cookie, 200)
	if !strings.Contains(w.Body.String(), "BEFORE INSERT") {
		t.Fatalf("updated trigger: %s", w.Body.String())
	}
	trigger.Definition = `CREATE TRIGGER "items_insert" AFTER INSERT ON "items" BEGIN SELECT NEW.name; END; DELETE FROM items;`
	request(t, h, "PATCH", base+"/triggers", queryBody(t, trigger), cookie, 400)
	request(t, h, "DELETE", base+"/triggers", queryBody(t, trigger), cookie, 200)
	request(t, h, "GET", base+"/triggers?database=flow&table=items&name=items_insert", "", cookie, 404)
	w = request(t, h, "GET", base+"/rows?database=flow&schema=&table=items&page=1&pageSize=20", "", cookie, 200)
	var page tablePage
	if err = json.Unmarshal(w.Body.Bytes(), &page); err != nil {
		t.Fatal(err)
	}
	if !page.Editable || len(page.Rows) != 1 || len(page.PrimaryKey) != 1 || page.PrimaryKey[0] != "rowid" {
		t.Fatalf("page: %+v", page)
	}
	key := tableRowKey{Values: []string{*page.Rows[0][0]}, Version: page.Versions[0]}
	mutation := tableMutation{Database: "flow", Table: "items", Column: "name", Value: ptrString("second"), Row: key}
	request(t, h, "POST", base+"/rows/edit", queryBody(t, mutation), cookie, 200)
	request(t, h, "POST", base+"/rows/edit", queryBody(t, mutation), cookie, 409)
	w = request(t, h, "GET", base+"/rows?database=flow&schema=&table=wr&page=1&pageSize=20", "", cookie, 200)
	page = tablePage{}
	if err = json.Unmarshal(w.Body.Bytes(), &page); err != nil {
		t.Fatal(err)
	}
	if !page.Editable || len(page.KeyValues) != 1 || page.KeyValues[0][1] != "9223372036854775807" {
		t.Fatalf("WITHOUT ROWID page: %+v", page)
	}
	request(t, h, "POST", base+"/rows/insert", `{"database":"flow","schema":"","table":"wr","values":{"code":"B","seq":"2","value":"next"}}`, cookie, 200)
	w = request(t, h, "GET", base+"/rows?database=flow&schema=&table=wr&page=1&pageSize=1", "", cookie, 200)
	page = tablePage{}
	if err = json.Unmarshal(w.Body.Bytes(), &page); err != nil {
		t.Fatal(err)
	}
	if !page.CursorPaging || !page.HasMore || page.NextCursor == "" || len(page.Rows) != 1 {
		t.Fatalf("first WITHOUT ROWID page: %+v", page)
	}
	w = request(t, h, "GET", base+"/rows?database=flow&schema=&table=wr&page=2&pageSize=1&cursor="+page.NextCursor, "", cookie, 200)
	var second tablePage
	if err = json.Unmarshal(w.Body.Bytes(), &second); err != nil {
		t.Fatal(err)
	}
	if len(second.Rows) != 1 || second.KeyValues[0][0] != "B" || second.HasMore {
		t.Fatalf("second WITHOUT ROWID page: %+v", second)
	}
	w = request(t, h, "GET", base+"/rows?database=flow&schema=&table=wr&page=1&pageSize=20", "", cookie, 200)
	page = tablePage{}
	if err = json.Unmarshal(w.Body.Bytes(), &page); err != nil {
		t.Fatal(err)
	}
	mutation = tableMutation{Database: "flow", Table: "wr", Column: "value", Value: ptrString("updated"), Row: tableRowKey{Values: page.KeyValues[0], Types: page.KeyTypes[0], Version: page.Versions[0]}}
	request(t, h, "POST", base+"/rows/edit", queryBody(t, mutation), cookie, 200)
	request(t, h, "PATCH", base+"/table-columns", `{"database":"flow","schema":"","table":"wr","oldName":"value","name":"value","changeDefault":true,"default":"upper('x')","defaultExpression":true}`, cookie, 200)
	request(t, h, "POST", base+"/query", `{"database":"flow","sql":"SELECT name FROM items"}`, cookie, 200)
	request(t, h, "POST", base+"/query", `{"database":"flow","sql":"ATTACH DATABASE 'other' AS bad"}`, cookie, 400)
	request(t, h, "POST", base+"/query", `{"database":"flow","sql":"SELECT zeroblob(9437184)"}`, cookie, 400)
	request(t, h, "POST", base+"/tables", `{"database":"flow","schema":"","table":"fresh","columns":[{"name":"id","type":"INTEGER","primary":true,"nullable":false},{"name":"label","type":"TEXT","nullable":true}]}`, cookie, 200)
	request(t, h, "POST", base+"/rows/insert", `{"database":"flow","schema":"","table":"fresh","values":{}}`, cookie, 200)
	request(t, h, "POST", base+"/tables", `{"database":"flow","schema":"","table":"child","columns":[{"name":"id","type":"INTEGER","primary":true,"nullable":false},{"name":"parent","type":"INTEGER","nullable":false,"referenceTable":"fresh","referenceColumn":"id"},{"name":"score","type":"REAL","nullable":false,"check":"score >= 0"},{"name":"code","type":"TEXT","nullable":false,"unique":true}]}`, cookie, 200)
	request(t, h, "POST", base+"/rows/insert", `{"database":"flow","schema":"","table":"child","values":{"parent":"1","score":"-1","code":"one"}}`, cookie, 400)
	request(t, h, "POST", base+"/rows/insert", `{"database":"flow","schema":"","table":"child","values":{"parent":"1","score":"1","code":"one"}}`, cookie, 200)
	request(t, h, "POST", base+"/rows/insert", `{"database":"flow","schema":"","table":"child","values":{"parent":"1","score":"1","code":"one"}}`, cookie, 400)
	request(t, h, "POST", base+"/indexes", `{"database":"flow","schema":"","table":"items","name":"items_name_desc","columns":["name"],"directions":["DESC"]}`, cookie, 200)
	w = request(t, h, "POST", base+"/indexes", `{"database":"flow","schema":"","table":"items","name":"items_name_default","columns":["name"]}`, cookie, 200)
	if strings.Contains(w.Body.String(), "ASC") || strings.Contains(w.Body.String(), "DESC") {
		t.Fatalf("default index order should omit direction: %s", w.Body.String())
	}
	request(t, h, "POST", base+"/indexes", `{"database":"flow","schema":"","table":"items","name":"bad_direction","columns":["name"],"directions":["SIDEWAYS"]}`, cookie, 400)
	request(t, h, "PATCH", base+"/table-columns", `{"database":"flow","schema":"","table":"items","oldName":"name","name":"name","type":"BLOB","changeDefault":true}`, cookie, 200)
	seedDB, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = seedDB.Exec(`CREATE TABLE large_blob (id INTEGER PRIMARY KEY, payload BLOB); INSERT INTO large_blob(payload) VALUES (zeroblob(9437184))`); err != nil {
		t.Fatal(err)
	}
	seedDB.Close()
	request(t, h, "GET", base+"/rows?database=flow&schema=&table=large_blob&page=1&pageSize=20", "", cookie, 413)
	request(t, h, "GET", base+"/catalog?database=flow", "", cookie, 200)
	request(t, h, "DELETE", base, "", cookie, http.StatusNoContent)
	if _, err = os.Stat(path); err != nil {
		t.Fatalf("local database was deleted: %v", err)
	}
}

func TestSQLiteAddConstraintsWithRebuild(t *testing.T) {
	_, h := testApplication(t)
	cookie, _ := setupAdmin(t, h)
	path := filepath.Join(t.TempDir(), "constraints.sqlite")
	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	_, err = db.Exec(`CREATE TABLE parent(id INTEGER PRIMARY KEY, code TEXT UNIQUE);
		INSERT INTO parent(id,code) VALUES(1,'A');
		CREATE TABLE child(id INTEGER, parent_id INTEGER, code TEXT, score INTEGER);
		INSERT INTO child VALUES(1,1,'one',3);
		CREATE INDEX child_score ON child(score DESC);
		CREATE VIEW child_view AS SELECT code FROM child;
		CREATE TRIGGER child_audit AFTER INSERT ON child BEGIN SELECT 1; END;`)
	if err != nil {
		t.Fatal(err)
	}
	db.Close()
	fields := connectionFields{Engine: "SQLite", Name: "Constraints", File: path, Location: "Local file"}
	w := request(t, h, "POST", "/api/connections", queryBody(t, fields), cookie, 201)
	var profile connectionProfile
	if err = json.Unmarshal(w.Body.Bytes(), &profile); err != nil {
		t.Fatal(err)
	}
	base := "/api/connections/" + profile.ID
	request(t, h, "POST", base+"/catalog", `{"password":""}`, cookie, 200)
	changes := []schemaChange{
		{Database: "constraints", Table: "child", Name: "child_score_check", Type: "CHECK", Expression: "score >= 0"},
		{Database: "constraints", Table: "child", Name: "child_code_unique", Type: "UNIQUE", Columns: []string{"code"}},
		{Database: "constraints", Table: "child", Name: "child_parent_fk", Type: "FOREIGN KEY", Columns: []string{"parent_id"}, ReferenceTable: "parent", ReferenceColumns: []string{"id"}, OnUpdate: "CASCADE", OnDelete: "SET NULL"},
		{Database: "constraints", Table: "child", Name: "child_id_pk", Type: "PRIMARY KEY", Columns: []string{"id"}},
	}
	for _, change := range changes {
		change.PreviewOnly = true
		w = request(t, h, "POST", base+"/constraints", queryBody(t, change), cookie, 200)
		var preview struct {
			SQL         string `json:"sql"`
			RebuildName string `json:"rebuildName"`
			PreviewHash string `json:"previewHash"`
		}
		if err = json.Unmarshal(w.Body.Bytes(), &preview); err != nil || preview.SQL == "" || preview.PreviewHash == "" {
			t.Fatalf("preview %s: %s %v", change.Name, w.Body.String(), err)
		}
		change.PreviewOnly = false
		change.RebuildName = preview.RebuildName
		change.PreviewHash = preview.PreviewHash
		request(t, h, "POST", base+"/constraints", queryBody(t, change), cookie, 200)
	}
	check, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	defer check.Close()
	for _, statement := range []string{
		`INSERT INTO child VALUES(2,1,'two',-1)`,
		`INSERT INTO child VALUES(2,1,'one',1)`,
		`INSERT INTO child VALUES(2,99,'two',1)`,
		`INSERT INTO child VALUES(1,1,'two',1)`,
	} {
		if _, err = check.Exec("PRAGMA foreign_keys=ON; " + statement); err == nil {
			t.Fatalf("constraint did not reject %s", statement)
		}
	}
	var count int
	if err = check.QueryRow(`SELECT count(*) FROM child_view`).Scan(&count); err != nil || count != 1 {
		t.Fatalf("view or row lost: %d %v", count, err)
	}
	metaDB, conn, err := sqlitedb.Open(context.Background(), path, true)
	if err != nil {
		t.Fatal(err)
	}
	defer metaDB.Close()
	defer conn.Close()
	meta, err := readSQLiteMeta(context.Background(), conn, "child")
	if err != nil || len(meta.Constraints) < 4 || !slices.ContainsFunc(meta.Constraints, func(c tableConstraint) bool { return c.Name == "child_score_check" && c.Type == "CHECK" }) {
		t.Fatalf("constraint metadata: %+v %v", meta.Constraints, err)
	}
	if !slices.ContainsFunc(meta.Constraints, func(c tableConstraint) bool {
		return c.Type == "FOREIGN KEY" && c.OnUpdate == "CASCADE" && c.OnDelete == "SET NULL"
	}) {
		t.Fatalf("foreign key actions missing: %+v", meta.Constraints)
	}
	if _, err = check.Exec(`PRAGMA foreign_keys=ON; UPDATE parent SET id=2 WHERE id=1`); err != nil {
		t.Fatalf("ON UPDATE CASCADE failed: %v", err)
	}
	var parentID sql.NullInt64
	if err = check.QueryRow(`SELECT parent_id FROM child WHERE id=1`).Scan(&parentID); err != nil || !parentID.Valid || parentID.Int64 != 2 {
		t.Fatalf("child key was not updated: %v %v", parentID, err)
	}
	if _, err = check.Exec(`DELETE FROM parent WHERE id=2`); err != nil {
		t.Fatalf("ON DELETE SET NULL failed: %v", err)
	}
	if err = check.QueryRow(`SELECT parent_id FROM child WHERE id=1`).Scan(&parentID); err != nil || parentID.Valid {
		t.Fatalf("child key was not cleared: %v %v", parentID, err)
	}
	var objects int
	if err = check.QueryRow(`SELECT count(*) FROM sqlite_schema WHERE name IN ('child_score','child_view','child_audit')`).Scan(&objects); err != nil || objects != 3 {
		t.Fatalf("dependent objects lost: %d %v", objects, err)
	}
	if _, err = check.Exec(`CREATE TABLE duplicates(value TEXT); INSERT INTO duplicates VALUES('x'),('x')`); err != nil {
		t.Fatal(err)
	}
	invalid := schemaChange{Database: "constraints", Table: "duplicates", Name: "duplicates_unique", Type: "UNIQUE", Columns: []string{"value"}, PreviewOnly: true}
	w = request(t, h, "POST", base+"/constraints", queryBody(t, invalid), cookie, 200)
	var preview struct {
		RebuildName string `json:"rebuildName"`
		PreviewHash string `json:"previewHash"`
	}
	if err = json.Unmarshal(w.Body.Bytes(), &preview); err != nil {
		t.Fatal(err)
	}
	invalid.PreviewOnly = false
	invalid.RebuildName = preview.RebuildName
	invalid.PreviewHash = preview.PreviewHash
	request(t, h, "POST", base+"/constraints", queryBody(t, invalid), cookie, 400)
	if err = check.QueryRow(`SELECT count(*) FROM duplicates`).Scan(&count); err != nil || count != 2 {
		t.Fatalf("failed rebuild changed data: %d %v", count, err)
	}
	if _, err = check.Exec(`CREATE TABLE external_parent(code TEXT); CREATE UNIQUE INDEX external_parent_code ON external_parent(code);
		CREATE TABLE external_child(code TEXT REFERENCES external_parent(code)); INSERT INTO external_parent VALUES('A'); INSERT INTO external_child VALUES('A')`); err != nil {
		t.Fatal(err)
	}
	request(t, h, "DELETE", base+"/indexes", `{"database":"constraints","schema":"","table":"external_parent","name":"external_parent_code"}`, cookie, 400)
	if err = check.QueryRow(`SELECT count(*) FROM sqlite_schema WHERE type='index' AND name='external_parent_code'`).Scan(&count); err != nil || count != 1 {
		t.Fatalf("foreign key parent index was dropped: %d %v", count, err)
	}
}

func TestSQLiteDetailedMetadata(t *testing.T) {
	path := filepath.Join(t.TempDir(), "metadata.sqlite")
	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	_, err = db.Exec(`CREATE TABLE "odd ""table" ("first key" TEXT, "second key" INTEGER, "value" TEXT DEFAULT (lower('ABC')), "computed" TEXT GENERATED ALWAYS AS ("value" || '!') VIRTUAL, PRIMARY KEY("first key", "second key")) WITHOUT ROWID;
		CREATE UNIQUE INDEX "odd_index" ON "odd ""table" ("value" DESC) WHERE "value" IS NOT NULL;
		CREATE INDEX "expression_index" ON "odd ""table" (lower("value"));
		CREATE VIEW "odd_view" AS SELECT "value" FROM "odd ""table";
		CREATE TRIGGER "odd_trigger" AFTER INSERT ON "odd ""table" BEGIN SELECT 1; END;`)
	if err != nil {
		t.Fatal(err)
	}
	db.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	opened, conn, err := sqlitedb.Open(ctx, path, false)
	if err != nil {
		t.Fatal(err)
	}
	defer opened.Close()
	defer conn.Close()
	meta, err := readSQLiteMeta(ctx, conn, `odd "table`)
	if err != nil {
		t.Fatal(err)
	}
	if !meta.WithoutRowID || !meta.Editable || len(meta.PrimaryKey) != 2 || meta.PrimaryKey[0] != "first key" || meta.PrimaryKey[1] != "second key" {
		t.Fatalf("primary key: %+v", meta)
	}
	if meta.Columns[0].Affinity != "TEXT" || meta.Columns[0].PrimaryOrder != 1 || meta.Columns[2].DefaultSource != "expression" || meta.Columns[3].Generated != "VIRTUAL" {
		t.Fatalf("columns: %+v", meta.Columns)
	}
	if len(meta.Constraints) == 0 || len(meta.Constraints[0].Columns) != 2 {
		t.Fatalf("constraints: %+v", meta.Constraints)
	}
	if !slices.ContainsFunc(meta.Indexes, func(index tableIndex) bool {
		return index.Name == "odd_index" && index.Partial && len(index.Directions) == 1 && index.Directions[0] == "DESC"
	}) || !slices.ContainsFunc(meta.Indexes, func(index tableIndex) bool { return index.Name == "expression_index" && index.Expression }) {
		t.Fatalf("indexes: %+v", meta.Indexes)
	}
	definition, err := sqliteRebuildDefinition(meta, tableColumnChange{OldName: "value", Name: "value", Type: "BLOB"}, "__test_rebuild")
	if err != nil {
		t.Fatal(err)
	}
	if _, err = conn.ExecContext(ctx, definition); err != nil {
		t.Fatalf("rewritten quoted/generated table: %s: %v", definition, err)
	}
	catalog, err := sqliteCatalogFor(ctx, conn, "metadata")
	if err != nil || len(catalog.Views) != 1 || catalog.Views[0] != "odd_view" {
		t.Fatalf("catalog: %+v %v", catalog, err)
	}
}

func TestSQLiteColumnRebuildPreservesDependencies(t *testing.T) {
	_, h := testApplication(t)
	cookie, _ := setupAdmin(t, h)
	path := filepath.Join(t.TempDir(), "rebuild.sqlite")
	seed, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	_, err = seed.Exec(`CREATE TABLE parent(id INTEGER PRIMARY KEY AUTOINCREMENT, value TEXT DEFAULT 'old', optional TEXT);
		INSERT INTO parent(id,value) VALUES (1,'one'),(5,'five'); DELETE FROM parent WHERE id=5;
		CREATE UNIQUE INDEX parent_value ON parent(value DESC);
		CREATE TABLE child(id INTEGER PRIMARY KEY, parent_id INTEGER REFERENCES parent(id));
		INSERT INTO child VALUES(1,1);
		CREATE VIEW parent_view AS SELECT value FROM parent;
		CREATE TABLE audit(note TEXT);
		CREATE TRIGGER parent_audit AFTER INSERT ON parent BEGIN INSERT INTO audit(note) VALUES(NEW.value); END;`)
	if err != nil {
		t.Fatal(err)
	}
	seed.Close()
	fields := connectionFields{Engine: "SQLite", Name: "Rebuild", File: path, Location: "Local file"}
	w := request(t, h, "POST", "/api/connections", queryBody(t, fields), cookie, 201)
	var profile connectionProfile
	if err = json.Unmarshal(w.Body.Bytes(), &profile); err != nil {
		t.Fatal(err)
	}
	base := "/api/connections/" + profile.ID
	request(t, h, "POST", base+"/catalog", `{"password":""}`, cookie, 200)
	change := tableColumnChange{Database: "rebuild", Table: "parent", Name: "value", OldName: "value", Type: "BLOB", Nullable: ptrBool(false), ChangeDefault: true, Default: ptrString("future"), PreviewOnly: true}
	w = request(t, h, "PATCH", base+"/table-columns", queryBody(t, change), cookie, 200)
	var preview struct {
		SQL         string   `json:"sql"`
		RebuildName string   `json:"rebuildName"`
		PreviewHash string   `json:"previewHash"`
		Objects     []string `json:"objects"`
	}
	if err = json.Unmarshal(w.Body.Bytes(), &preview); err != nil {
		t.Fatal(err)
	}
	if preview.RebuildName == "" || preview.PreviewHash == "" || len(preview.Objects) < 3 || !strings.Contains(preview.SQL, "foreign_key_check") {
		t.Fatalf("preview: %+v", preview)
	}
	change.PreviewOnly = false
	change.RebuildName = preview.RebuildName
	change.PreviewHash = preview.PreviewHash
	change.PreviewHash = "stale"
	request(t, h, "PATCH", base+"/table-columns", queryBody(t, change), cookie, 409)
	change.PreviewHash = preview.PreviewHash
	request(t, h, "PATCH", base+"/table-columns", queryBody(t, change), cookie, 200)
	verify, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	defer verify.Close()
	var value string
	if err = verify.QueryRow(`SELECT value FROM parent_view`).Scan(&value); err != nil || value != "one" {
		t.Fatalf("view after rebuild: %q %v", value, err)
	}
	if _, err = verify.Exec(`INSERT INTO parent DEFAULT VALUES`); err != nil {
		t.Fatal(err)
	}
	var id int64
	if err = verify.QueryRow(`SELECT id,value FROM parent WHERE id>1`).Scan(&id, &value); err != nil || id != 6 || value != "future" {
		t.Fatalf("sequence or default: %d %q %v", id, value, err)
	}
	if err = verify.QueryRow(`SELECT note FROM audit`).Scan(&value); err != nil || value != "future" {
		t.Fatalf("trigger: %q %v", value, err)
	}
	var indexCount int
	if err = verify.QueryRow(`SELECT count(*) FROM sqlite_schema WHERE type='index' AND name='parent_value'`).Scan(&indexCount); err != nil || indexCount != 1 {
		t.Fatalf("index: %d %v", indexCount, err)
	}
	var fkViolation int
	if err = verify.QueryRow(`SELECT count(*) FROM pragma_foreign_key_check`).Scan(&fkViolation); err != nil || fkViolation != 0 {
		t.Fatalf("foreign keys: %d %v", fkViolation, err)
	}
	bad := tableColumnChange{Database: "rebuild", Table: "parent", Name: "optional", OldName: "optional", Nullable: ptrBool(false)}
	request(t, h, "PATCH", base+"/table-columns", queryBody(t, bad), cookie, 400)
	var nullable int
	if err = verify.QueryRow(`SELECT "notnull" FROM pragma_table_xinfo('parent') WHERE name='optional'`).Scan(&nullable); err != nil || nullable != 0 {
		t.Fatalf("failed rebuild changed column: %d %v", nullable, err)
	}
	if err = verify.QueryRow(`SELECT value FROM parent_view WHERE value='one'`).Scan(&value); err != nil || value != "one" {
		t.Fatalf("failed rebuild changed view: %q %v", value, err)
	}
}

func ptrBool(value bool) *bool { return &value }

func ptrString(value string) *string { return &value }

func TestSQLiteUploadAndDownload(t *testing.T) {
	a, h := testApplication(t)
	cookie, _ := setupAdmin(t, h)
	source := filepath.Join(t.TempDir(), "source.sqlite")
	db, err := sql.Open("sqlite", source)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = db.Exec(`CREATE TABLE entries (id INTEGER PRIMARY KEY, value TEXT); INSERT INTO entries(value) VALUES ('saved')`); err != nil {
		t.Fatal(err)
	}
	db.Close()
	data, err := os.ReadFile(source)
	if err != nil {
		t.Fatal(err)
	}
	fields := connectionFields{Engine: "SQLite", Name: "Upload", File: "source.sqlite", Location: "Server upload"}
	w := request(t, h, "POST", "/api/connections", queryBody(t, fields), cookie, 201)
	var profile connectionProfile
	if err = json.Unmarshal(w.Body.Bytes(), &profile); err != nil {
		t.Fatal(err)
	}
	base := "/api/connections/" + profile.ID
	request(t, h, "POST", base+"/catalog", `{"password":""}`, cookie, 404)
	var body bytes.Buffer
	writer := multipart.NewWriter(&body)
	part, err := writer.CreateFormFile("file", "source.sqlite")
	if err != nil {
		t.Fatal(err)
	}
	if _, err = part.Write(data); err != nil {
		t.Fatal(err)
	}
	if err = writer.Close(); err != nil {
		t.Fatal(err)
	}
	req := httptest.NewRequest("POST", "http://localhost"+base+"/sqlite-file", &body)
	req.Header.Set("Content-Type", writer.FormDataContentType())
	req.AddCookie(cookie)
	response := httptest.NewRecorder()
	h.ServeHTTP(response, req)
	if response.Code != 200 {
		t.Fatalf("upload: %d %s", response.Code, response.Body.String())
	}
	request(t, h, "POST", base+"/catalog", `{"password":""}`, cookie, 200)
	replacement := filepath.Join(t.TempDir(), "replacement.sqlite")
	replacementDB, err := sql.Open("sqlite", replacement)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = replacementDB.Exec(`CREATE TABLE replaced (value TEXT); INSERT INTO replaced(value) VALUES ('new')`); err != nil {
		t.Fatal(err)
	}
	replacementDB.Close()
	data, err = os.ReadFile(replacement)
	if err != nil {
		t.Fatal(err)
	}
	body.Reset()
	writer = multipart.NewWriter(&body)
	part, err = writer.CreateFormFile("file", "replacement.sqlite")
	if err != nil {
		t.Fatal(err)
	}
	if _, err = part.Write(data); err != nil {
		t.Fatal(err)
	}
	if err = writer.Close(); err != nil {
		t.Fatal(err)
	}
	req = httptest.NewRequest("POST", "http://localhost"+base+"/sqlite-file", &body)
	req.Header.Set("Content-Type", writer.FormDataContentType())
	req.AddCookie(cookie)
	response = httptest.NewRecorder()
	h.ServeHTTP(response, req)
	if response.Code != 200 {
		t.Fatalf("replacement: %d %s", response.Code, response.Body.String())
	}
	body.Reset()
	writer = multipart.NewWriter(&body)
	part, err = writer.CreateFormFile("file", "broken.sqlite")
	if err != nil {
		t.Fatal(err)
	}
	if _, err = part.Write([]byte("not sqlite")); err != nil {
		t.Fatal(err)
	}
	if err = writer.Close(); err != nil {
		t.Fatal(err)
	}
	req = httptest.NewRequest("POST", "http://localhost"+base+"/sqlite-file", &body)
	req.Header.Set("Content-Type", writer.FormDataContentType())
	req.AddCookie(cookie)
	response = httptest.NewRecorder()
	h.ServeHTTP(response, req)
	if response.Code != 400 {
		t.Fatalf("invalid replacement: %d %s", response.Code, response.Body.String())
	}
	request(t, h, "GET", base+"/catalog?database=source", "", cookie, 409)
	w = request(t, h, "POST", base+"/catalog", `{"password":""}`, cookie, 200)
	if !strings.Contains(w.Body.String(), `"replaced"`) {
		t.Fatalf("replacement catalog: %s", w.Body.String())
	}
	req = httptest.NewRequest("GET", "http://localhost"+base+"/sqlite-file", nil)
	req.AddCookie(cookie)
	response = httptest.NewRecorder()
	h.ServeHTTP(response, req)
	if response.Code != 200 {
		t.Fatalf("download: %d %s", response.Code, response.Body.String())
	}
	snapshot := filepath.Join(t.TempDir(), "snapshot.sqlite")
	if err = os.WriteFile(snapshot, response.Body.Bytes(), 0600); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err = sqlitedb.Check(ctx, snapshot); err != nil {
		t.Fatal(err)
	}
	path, _ := a.sqlitePath(profile)
	request(t, h, "DELETE", base, "", cookie, 204)
	if _, err = os.Stat(path); !os.IsNotExist(err) {
		t.Fatalf("uploaded copy retained: %v", err)
	}
}

func TestSQLiteManualPathOutsideDataDirectory(t *testing.T) {
	a, _ := testApplication(t)
	path := filepath.Join(t.TempDir(), "outside.sqlite")
	if err := os.WriteFile(path, []byte("SQLite format 3\x00"), 0600); err != nil {
		t.Fatal(err)
	}
	profile := connectionProfile{connectionFields: connectionFields{Engine: "SQLite", Location: "Local file", File: path}}
	canonical, err := filepath.EvalSymlinks(path)
	if err != nil {
		t.Fatal(err)
	}
	if got, err := a.sqlitePath(profile); err != nil || got != canonical {
		t.Fatalf("path outside data directory: %q, %v", got, err)
	}
	link := filepath.Join(t.TempDir(), "linked.sqlite")
	if err := os.Symlink(path, link); err != nil {
		t.Fatal(err)
	}
	profile.File = link
	if got, err := a.sqlitePath(profile); err != nil || got != canonical {
		t.Fatalf("symlink to SQLite file: %q, %v", got, err)
	}
	for _, invalid := range []string{"relative.sqlite", t.TempDir(), a.store.path} {
		profile.File = invalid
		if _, err := a.sqlitePath(profile); err == nil {
			t.Fatalf("accepted invalid path %q", invalid)
		}
	}
}

func TestSQLiteRemoteManualPath(t *testing.T) {
	_, h := testApplication(t)
	cookie, _ := setupAdmin(t, h)
	path := filepath.Join(t.TempDir(), "remote.sqlite")
	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`CREATE TABLE notes(id INTEGER PRIMARY KEY, body TEXT)`); err != nil {
		t.Fatal(err)
	}
	db.Close()
	remoteRequest := func(method, endpoint, body string, status int) *httptest.ResponseRecorder {
		t.Helper()
		r := httptest.NewRequest(method, "http://localhost"+endpoint, strings.NewReader(body))
		r.RemoteAddr = "192.0.2.1:12345"
		r.AddCookie(cookie)
		if body != "" {
			r.Header.Set("Content-Type", "application/json")
		}
		w := httptest.NewRecorder()
		h.ServeHTTP(w, r)
		if w.Code != status {
			t.Fatalf("%s %s: got %d, want %d: %s", method, endpoint, w.Code, status, w.Body.String())
		}
		return w
	}
	session := remoteRequest("GET", "/api/session", "", 200)
	if !strings.Contains(session.Body.String(), `"nativeFilePickerAllowed":false`) {
		t.Fatalf("remote picker capability: %s", session.Body.String())
	}
	fields := connectionFields{Engine: "SQLite", Name: "Remote", File: path, Location: "Local file"}
	remoteRequest("POST", "/api/sqlite/test", queryBody(t, fields), 200)
	created := remoteRequest("POST", "/api/connections", queryBody(t, fields), 201)
	var profile connectionProfile
	if err := json.Unmarshal(created.Body.Bytes(), &profile); err != nil {
		t.Fatal(err)
	}
	remoteRequest("POST", "/api/connections/"+profile.ID+"/catalog", `{"password":""}`, 200)
}

func TestSQLiteNativeSelectionEditsOriginal(t *testing.T) {
	a, h := testApplication(t)
	cookie, admin := setupAdmin(t, h)
	remote := httptest.NewRequest("POST", "http://localhost/api/sqlite/pick-file", strings.NewReader(`{}`))
	remote.RemoteAddr = "192.0.2.1:12345"
	remote.AddCookie(cookie)
	blocked := httptest.NewRecorder()
	h.ServeHTTP(blocked, remote)
	if blocked.Code != 403 {
		t.Fatalf("remote native picker: %d %s", blocked.Code, blocked.Body.String())
	}
	proxied := httptest.NewRequest("GET", "http://gosql.example/api/session", nil)
	proxied.RemoteAddr = "127.0.0.1:12345"
	proxied.AddCookie(cookie)
	proxiedSession := httptest.NewRecorder()
	h.ServeHTTP(proxiedSession, proxied)
	if proxiedSession.Code != 200 || !strings.Contains(proxiedSession.Body.String(), `"nativeFilePickerAllowed":false`) {
		t.Fatalf("proxied picker capability: %d %s", proxiedSession.Code, proxiedSession.Body.String())
	}
	path := filepath.Join(t.TempDir(), "original.sqlite")
	seed, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = seed.Exec(`CREATE TABLE notes(id INTEGER PRIMARY KEY, body TEXT); INSERT INTO notes VALUES(1,'before')`); err != nil {
		t.Fatal(err)
	}
	seed.Close()
	path, err = a.sqliteNativePath(path)
	if err != nil {
		t.Fatal(err)
	}
	fields := connectionFields{Engine: "SQLite", Name: "Original", File: path, Location: "Native file"}
	request(t, h, "POST", "/api/connections", queryBody(t, fields), cookie, 403)
	token := newID()
	a.mu.Lock()
	a.sqlitePicks[token] = sqlitePickedFile{ownerID: admin.ID, path: path, expires: time.Now().Add(time.Minute)}
	a.mu.Unlock()
	fields.PickerToken = token
	if a.validateSQLitePicker("another-user", fields, connectionProfile{}) {
		t.Fatal("native selection token accepted for another owner")
	}
	if _, err = a.sqliteNativePath(a.store.path); err == nil {
		t.Fatal("internal GoSQL database was accepted")
	}
	w := request(t, h, "POST", "/api/connections", queryBody(t, fields), cookie, 201)
	var profile connectionProfile
	if err = json.Unmarshal(w.Body.Bytes(), &profile); err != nil {
		t.Fatal(err)
	}
	if profile.PickerToken != "" {
		t.Fatal("picker token was saved in profile")
	}
	request(t, h, "POST", "/api/connections", queryBody(t, fields), cookie, 403)
	base := "/api/connections/" + profile.ID
	request(t, h, "POST", base+"/catalog", `{"password":""}`, cookie, 200)
	w = request(t, h, "GET", base+"/rows?database=original&schema=&table=notes&page=1&pageSize=20", "", cookie, 200)
	var page tablePage
	if err = json.Unmarshal(w.Body.Bytes(), &page); err != nil {
		t.Fatal(err)
	}
	change := tableMutation{Database: "original", Table: "notes", Column: "body", Value: ptrString("after"), Row: tableRowKey{Values: page.KeyValues[0], Types: page.KeyTypes[0], Version: page.Versions[0]}}
	request(t, h, "POST", base+"/rows/edit", queryBody(t, change), cookie, 200)
	check, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	defer check.Close()
	var body string
	if err = check.QueryRow(`SELECT body FROM notes WHERE id=1`).Scan(&body); err != nil || body != "after" {
		t.Fatalf("original SQLite file: %q %v", body, err)
	}
	request(t, h, "DELETE", base, "", cookie, 204)
	if _, err = os.Stat(path); err != nil {
		t.Fatalf("original file was removed: %v", err)
	}
}
