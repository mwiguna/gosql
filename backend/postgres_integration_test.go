package main

import (
	"context"
	"encoding/json"
	"net/http/httptest"
	"net/url"
	"os"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
)

// Server pengujian harus terpisah dari database pengguna; URL hanya diberikan saat tes integrasi.
func TestPostgresIntegration(t *testing.T) {
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
	schema := "test_" + strings.ToLower(newID())
	quoted := pgx.Identifier{schema}.Sanitize()
	_, err = conn.Exec(ctx, "CREATE SCHEMA "+quoted+"; CREATE TABLE "+quoted+`.items (id bigint PRIMARY KEY, "a.b" text, amount numeric(30,4), optional text); INSERT INTO `+quoted+`.items SELECT n, '<b>row</b>', 9007199254740993.1234, NULL FROM generate_series(1,45) n; CREATE TABLE `+quoted+`.empty (value text); CREATE TABLE `+quoted+`.parents (id bigint PRIMARY KEY); CREATE TABLE `+quoted+`.children (parent_id bigint); CREATE VIEW `+quoted+`.listing AS SELECT * FROM `+quoted+`.items`)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Exec(ctx, "DROP SCHEMA "+quoted+" CASCADE")
	u, _ := url.Parse(address)
	password, _ := u.User.Password()
	fields := connectionFields{Engine: "PostgreSQL", Name: "Integration", Host: u.Hostname(), Port: u.Port(), Username: u.User.Username(), Database: strings.TrimPrefix(u.Path, "/"), SSL: "Disable"}
	if _, err := loadPostgresCatalog(ctx, fields, password); err != nil {
		t.Fatal(err)
	}
	a, handler := testApplication(t)
	cookie, account := setupAdmin(t, handler)
	profileJSON, _ := json.Marshal(fields)
	created := request(t, handler, "POST", "/api/connections", string(profileJSON), cookie, 201)
	var profile connectionProfile
	json.Unmarshal(created.Body.Bytes(), &profile)
	path := "/api/connections/" + profile.ID
	credentials, _ := json.Marshal(map[string]string{"password": password})
	request(t, handler, "POST", path+"/catalog", `{"password":"wrong-password"}`, cookie, 422)
	catalog := request(t, handler, "POST", path+"/catalog", string(credentials), cookie, 200)
	if !strings.Contains(catalog.Body.String(), schema) {
		t.Fatal("schema is missing from catalog")
	}
	var initialCatalog postgresResult
	if err := json.Unmarshal(catalog.Body.Bytes(), &initialCatalog); err != nil || !slices.Contains(initialCatalog.Databases, fields.Database) {
		t.Fatal("initial database is missing from available databases")
	}
	var templateName string
	if err := conn.QueryRow(ctx, `SELECT datname FROM pg_catalog.pg_database WHERE datistemplate LIMIT 1`).Scan(&templateName); err != nil {
		t.Fatal(err)
	}
	if slices.Contains(initialCatalog.Databases, templateName) {
		t.Fatal("a template database was exposed in the sidebar catalog")
	}
	queryInput, _ := json.Marshal(map[string]string{"database": fields.Database, "sql": "SELECT 1 AS first; SELECT id, optional FROM " + quoted + ".items WHERE id = 1"})
	var queryResult struct {
		Rows    []map[string]any `json:"rows"`
		Columns []string         `json:"columns"`
	}
	if err := json.Unmarshal(request(t, handler, "POST", path+"/query", string(queryInput), cookie, 200).Body.Bytes(), &queryResult); err != nil {
		t.Fatal(err)
	}
	if len(queryResult.Rows) != 1 || queryResult.Rows[0]["id"] != "1" || queryResult.Rows[0]["optional"] != nil || !slices.Equal(queryResult.Columns, []string{"id", "optional"}) {
		t.Fatalf("query console returned an unexpected final result: %+v", queryResult)
	}
	queryInput, _ = json.Marshal(map[string]string{"database": fields.Database, "sql": "SELECT generate_series(1,1001) AS value"})
	if err := json.Unmarshal(request(t, handler, "POST", path+"/query", string(queryInput), cookie, 200).Body.Bytes(), &queryResult); err != nil || len(queryResult.Rows) != 1001 {
		t.Fatalf("query console truncated rows: %d, %v", len(queryResult.Rows), err)
	}
	readCursor := func(table string, page, size, cursor string, status int) *httptest.ResponseRecorder {
		return request(t, handler, "GET", path+"/rows?"+url.Values{"database": {fields.Database}, "schema": {schema}, "table": {table}, "page": {page}, "pageSize": {size}, "cursor": {cursor}}.Encode(), "", cookie, status)
	}
	read := func(table string, page, size string, status int) *httptest.ResponseRecorder {
		return readCursor(table, page, size, "", status)
	}
	var first, last tablePage
	json.Unmarshal(read("items", "1", "20", 200).Body.Bytes(), &first)
	var middle tablePage
	json.Unmarshal(readCursor("items", "2", "20", first.NextCursor, 200).Body.Bytes(), &middle)
	json.Unmarshal(readCursor("items", "3", "20", middle.NextCursor, 200).Body.Bytes(), &last)
	read("items", "2", "20", 400)
	readCursor("items", "2", "20", "[null]", 400)
	countedPath := path + "/rows/count?" + url.Values{"database": {fields.Database}, "schema": {schema}, "table": {"items"}}.Encode()
	a.countSlot <- struct{}{}
	request(t, handler, "GET", countedPath, "", cookie, 429)
	<-a.countSlot
	var counted struct {
		TotalRows int64 `json:"totalRows"`
	}
	json.Unmarshal(request(t, handler, "GET", countedPath, "", cookie, 200).Body.Bytes(), &counted)
	if counted.TotalRows != 45 || first.TotalRows != nil {
		t.Fatal("on-demand row count is incorrect")
	}
	if len(first.Rows) != 20 || !first.HasMore || len(last.Rows) != 5 || last.HasMore || *last.Rows[0][0] != "41" {
		t.Fatalf("incorrect pagination: %+v %+v", first, last)
	}
	if *first.Rows[0][2] != "9007199254740993.1234" || first.Rows[0][3] != nil || first.Columns[1].Name != "a.b" {
		t.Fatal("value precision, NULL or column names changed")
	}
	if !first.Editable || !slices.Equal(first.PrimaryKey, []string{"id"}) || len(first.Versions) != 20 || len(first.Indexes) == 0 || len(first.Constraints) == 0 {
		t.Fatal("writable metadata, row versions, indexes or constraints are missing")
	}
	fullText := strings.Repeat("é", 600)
	if _, err := conn.Exec(ctx, "UPDATE "+quoted+".items SET optional=$1 WHERE id=45", fullText); err != nil {
		t.Fatal(err)
	}
	var preview tablePage
	json.Unmarshal(readCursor("items", "3", "20", middle.NextCursor, 200).Body.Bytes(), &preview)
	if !preview.Truncated[4][3] || *preview.Rows[4][3] != strings.Repeat("é", 512) {
		t.Fatal("large text was not bounded in the table preview")
	}
	cellInput, _ := json.Marshal(map[string]any{"database": fields.Database, "schema": schema, "table": "items", "column": "optional", "row": tableRowKey{Values: []string{"45"}, Version: preview.Versions[4]}})
	var cell struct {
		Value string `json:"value"`
	}
	json.Unmarshal(request(t, handler, "POST", path+"/rows/cell", string(cellInput), cookie, 200).Body.Bytes(), &cell)
	if cell.Value != fullText {
		t.Fatal("editor did not reload the complete text value")
	}
	mutate := func(endpoint string, value any, status int) *httptest.ResponseRecorder {
		body, err := json.Marshal(value)
		if err != nil {
			t.Fatal(err)
		}
		return request(t, handler, "POST", path+"/rows/"+endpoint, string(body), cookie, status)
	}
	location := map[string]any{"database": fields.Database, "schema": schema, "table": "items"}
	key := tableRowKey{Values: []string{"1"}, Version: first.Versions[0]}
	location["row"], location["column"], location["value"] = key, "optional", "updated"
	mutate("edit", location, 200)
	mutate("edit", location, 409)
	var afterEdit tablePage
	json.Unmarshal(read("items", "1", "20", 200).Body.Bytes(), &afterEdit)
	if *afterEdit.Rows[0][3] != "updated" {
		t.Fatal("edited value was not stored")
	}
	location["row"] = tableRowKey{Values: []string{"1"}, Version: afterEdit.Versions[0]}
	location["column"], location["value"] = "amount", "not-a-number"
	mutate("edit", location, 400)
	deleteInput := map[string]any{"database": fields.Database, "schema": schema, "table": "items", "rows": []tableRowKey{{Values: []string{"2"}, Version: first.Versions[1]}}}
	mutate("delete", deleteInput, 200)
	json.Unmarshal(request(t, handler, "GET", countedPath, "", cookie, 200).Body.Bytes(), &counted)
	if counted.TotalRows != 44 {
		t.Fatal("row count did not reflect deletion")
	}
	mutate("delete", deleteInput, 409)
	deleteInput["rows"] = []tableRowKey{{Values: []string{"4"}, Version: first.Versions[3]}, key}
	mutate("delete", deleteInput, 409)
	var afterRollback tablePage
	json.Unmarshal(read("items", "1", "20", 200).Body.Bytes(), &afterRollback)
	if !slices.ContainsFunc(afterRollback.Rows, func(row []*string) bool { return row[0] != nil && *row[0] == "4" }) {
		t.Fatal("bulk delete was not rolled back after a row conflict")
	}
	if *first.Rows[1][0] != "2" {
		t.Fatal("test fixture changed unexpectedly")
	}
	constraint := map[string]any{"database": fields.Database, "schema": schema, "table": "items", "name": "optional_unique", "type": "UNIQUE", "columns": []string{"optional"}}
	constraintJSON, _ := json.Marshal(constraint)
	request(t, handler, "POST", path+"/constraints", string(constraintJSON), cookie, 200)
	var constrained tablePage
	json.Unmarshal(read("items", "1", "20", 200).Body.Bytes(), &constrained)
	if !slices.ContainsFunc(constrained.Constraints, func(item tableConstraint) bool { return item.Name == "optional_unique" && item.Type == "UNIQUE" }) {
		t.Fatal("UNIQUE constraint is missing from metadata")
	}
	location["row"], location["column"], location["value"] = tableRowKey{Values: []string{"3"}, Version: first.Versions[2]}, "optional", "updated"
	mutate("edit", location, 409)
	request(t, handler, "DELETE", path+"/constraints", string(constraintJSON), cookie, 200)
	mutate("edit", location, 200)
	index := map[string]any{"database": fields.Database, "schema": schema, "table": "items", "name": "amount_index", "columns": []string{"amount"}, "unique": false}
	indexJSON, _ := json.Marshal(index)
	request(t, handler, "POST", path+"/indexes", string(indexJSON), cookie, 200)
	index["oldName"], index["name"], index["columns"], index["unique"] = "amount_index", "id_unique_index", []string{"id"}, true
	indexJSON, _ = json.Marshal(index)
	request(t, handler, "PATCH", path+"/indexes", string(indexJSON), cookie, 200)
	var indexed tablePage
	json.Unmarshal(read("items", "1", "20", 200).Body.Bytes(), &indexed)
	if !slices.ContainsFunc(indexed.Indexes, func(item tableIndex) bool { return item.Name == "id_unique_index" && item.Unique && !item.Managed }) {
		t.Fatal("unique index was not saved independently from constraints")
	}
	request(t, handler, "DELETE", path+"/indexes", string(indexJSON), cookie, 200)
	check := map[string]any{"database": fields.Database, "schema": schema, "table": "items", "name": "amount_check", "type": "CHECK", "expression": "amount >= 0"}
	checkJSON, _ := json.Marshal(check)
	request(t, handler, "POST", path+"/constraints", string(checkJSON), cookie, 200)
	location["row"], location["column"], location["value"] = tableRowKey{Values: []string{"3"}, Version: indexed.Versions[1]}, "amount", "-1"
	mutate("edit", location, 400)
	request(t, handler, "DELETE", path+"/constraints", string(checkJSON), cookie, 200)
	var empty tablePage
	json.Unmarshal(read("empty", "1", "20", 200).Body.Bytes(), &empty)
	if len(empty.Columns) != 1 || len(empty.Rows) != 0 {
		t.Fatal("empty table lost column metadata")
	}
	primary := map[string]any{"database": fields.Database, "schema": schema, "table": "empty", "name": "empty_pkey", "type": "PRIMARY KEY", "columns": []string{"value"}}
	primaryJSON, _ := json.Marshal(primary)
	request(t, handler, "POST", path+"/constraints", string(primaryJSON), cookie, 200)
	json.Unmarshal(read("empty", "1", "20", 200).Body.Bytes(), &empty)
	if !slices.Equal(empty.PrimaryKey, []string{"value"}) {
		t.Fatal("primary key was not reflected in metadata")
	}
	request(t, handler, "DELETE", path+"/constraints", string(primaryJSON), cookie, 200)
	foreign := map[string]any{"database": fields.Database, "schema": schema, "table": "children", "name": "children_parent_fkey", "type": "FOREIGN KEY", "columns": []string{"parent_id"}, "referenceSchema": schema, "referenceTable": "parents", "referenceColumns": []string{"id"}}
	foreignJSON, _ := json.Marshal(foreign)
	request(t, handler, "POST", path+"/constraints", string(foreignJSON), cookie, 200)
	var children tablePage
	json.Unmarshal(read("children", "1", "20", 200).Body.Bytes(), &children)
	if !slices.ContainsFunc(children.Constraints, func(item tableConstraint) bool {
		return item.Type == "FOREIGN KEY" && item.Validated && item.ReferenceTable == "parents" && slices.Equal(item.ReferenceColumns, []string{"id"})
	}) {
		t.Fatal("foreign key metadata is incomplete")
	}
	addedChild := request(t, handler, "POST", path+"/rows/insert", `{"database":"`+fields.Database+`","schema":"`+schema+`","table":"children","values":{}}`, cookie, 200)
	var child struct {
		Row []*string `json:"row"`
	}
	if err := json.Unmarshal(addedChild.Body.Bytes(), &child); err != nil || len(child.Row) != 1 || child.Row[0] != nil {
		t.Fatalf("quick insert should leave nullable FK as NULL: %+v, %v", child, err)
	}
	request(t, handler, "DELETE", path+"/constraints", string(foreignJSON), cookie, 200)
	if _, err := conn.Exec(ctx, "INSERT INTO "+quoted+".children(parent_id) VALUES (9999)"); err != nil {
		t.Fatal(err)
	}
	request(t, handler, "POST", path+"/constraints", string(foreignJSON), cookie, 409)
	json.Unmarshal(read("children", "1", "20", 200).Body.Bytes(), &children)
	if !slices.ContainsFunc(children.Constraints, func(item tableConstraint) bool {
		return item.Name == "children_parent_fkey" && !item.Validated
	}) {
		t.Fatal("failed validation did not expose the remaining foreign key")
	}
	if _, err := conn.Exec(ctx, "DELETE FROM "+quoted+".children WHERE parent_id=9999"); err != nil {
		t.Fatal(err)
	}
	request(t, handler, "PATCH", path+"/constraints", `{"database":"`+fields.Database+`","schema":"`+schema+`","table":"children","name":"children_parent_fkey","validateOnly":true}`, cookie, 200)
	request(t, handler, "DELETE", path+"/constraints", string(foreignJSON), cookie, 200)
	read("listing", "1", "20", 200)
	_, err = conn.Exec(ctx, "CREATE TABLE "+pgx.Identifier{schema, `quoted"table`}.Sanitize()+` ("<column>" text); INSERT INTO `+pgx.Identifier{schema, `quoted"table`}.Sanitize()+` VALUES ('quoted name')`)
	if err != nil {
		t.Fatal(err)
	}
	read(`quoted"table`, "1", "20", 200)
	ddl := func(method, endpoint string, value any, status int) {
		body, err := json.Marshal(value)
		if err != nil {
			t.Fatal(err)
		}
		request(t, handler, method, path+endpoint, string(body), cookie, status)
	}
	newSchema := "api_" + strings.ToLower(newID())
	defer conn.Exec(ctx, "DROP SCHEMA IF EXISTS "+pgx.Identifier{newSchema}.Sanitize()+" CASCADE")
	ddl("POST", "/schemas", map[string]string{"database": fields.Database, "schema": newSchema}, 200)
	ddl("POST", "/schemas", map[string]string{"database": fields.Database, "schema": newSchema}, 409)
	emptySchema := "rename_" + strings.ToLower(newID())
	renamedSchema := "renamed_" + strings.ToLower(newID())
	defer conn.Exec(ctx, "DROP SCHEMA IF EXISTS "+pgx.Identifier{emptySchema}.Sanitize()+" CASCADE; DROP SCHEMA IF EXISTS "+pgx.Identifier{renamedSchema}.Sanitize()+" CASCADE")
	ddl("POST", "/schemas", map[string]string{"database": fields.Database, "schema": emptySchema}, 200)
	ddl("PATCH", "/schemas", map[string]string{"database": fields.Database, "schema": emptySchema, "newName": renamedSchema}, 200)
	ddl("DELETE", "/schemas", map[string]string{"database": fields.Database, "schema": renamedSchema}, 200)
	ddl("POST", "/tables", map[string]any{"database": fields.Database, "schema": newSchema, "table": "created_here", "columns": []map[string]any{{"name": "id", "type": "bigint", "primary": true}}}, 200)
	ddl("DELETE", "/schemas", map[string]string{"database": fields.Database, "schema": newSchema}, 409)
	if _, err := conn.Exec(ctx, "CREATE TYPE "+pgx.Identifier{newSchema, "mood"}.Sanitize()+" AS ENUM ('happy', 'sad')"); err != nil {
		t.Fatal(err)
	}
	ddl("POST", "/tables", map[string]any{"database": fields.Database, "schema": newSchema, "table": "enum_here", "columns": []map[string]any{{"name": "mood", "type": pgx.Identifier{newSchema, "mood"}.Sanitize(), "nullable": true}, {"name": "moods", "type": pgx.Identifier{newSchema, "mood"}.Sanitize() + "[]", "nullable": true}}}, 200)
	ddl("POST", "/tables", map[string]any{"database": fields.Database, "schema": newSchema, "table": "defaults_here", "columns": []map[string]any{{"name": "id", "type": "bigserial", "primary": true}, {"name": "note", "type": "text", "nullable": true}}}, 200)
	inserted := request(t, handler, "POST", path+"/rows/insert", `{"database":"`+fields.Database+`","schema":"`+newSchema+`","table":"defaults_here","values":{}}`, cookie, 200)
	var insertedRow struct {
		Row     []*string `json:"row"`
		Version string    `json:"version"`
	}
	if err := json.Unmarshal(inserted.Body.Bytes(), &insertedRow); err != nil || len(insertedRow.Row) != 2 || insertedRow.Row[0] == nil || *insertedRow.Row[0] != "1" || insertedRow.Row[1] != nil || insertedRow.Version == "" {
		t.Fatalf("default insert did not return the new row: %+v, %v", insertedRow, err)
	}
	ddl("POST", "/tables", map[string]any{"database": fields.Database, "schema": newSchema, "table": "no_defaults", "columns": []map[string]any{{"name": "id", "type": "integer", "primary": true}, {"name": "title", "type": "text"}, {"name": "quantity", "type": "integer"}}}, 200)
	quickBody := `{"database":"` + fields.Database + `","schema":"` + newSchema + `","table":"no_defaults","values":{}}`
	firstQuick := request(t, handler, "POST", path+"/rows/insert", quickBody, cookie, 200)
	var quickRow struct {
		Row []*string `json:"row"`
		SQL string    `json:"sql"`
	}
	if err := json.Unmarshal(firstQuick.Body.Bytes(), &quickRow); err != nil || len(quickRow.Row) != 3 || *quickRow.Row[0] != "1" || *quickRow.Row[1] != "" || *quickRow.Row[2] != "0" || !strings.Contains(quickRow.SQL, "MAX") {
		t.Fatalf("quick insert did not use typed values: %+v, %v", quickRow, err)
	}
	secondQuick := request(t, handler, "POST", path+"/rows/insert", quickBody, cookie, 200)
	if err := json.Unmarshal(secondQuick.Body.Bytes(), &quickRow); err != nil || *quickRow.Row[0] != "2" {
		t.Fatalf("quick insert did not advance the primary key: %+v, %v", quickRow, err)
	}
	var newCatalog postgresResult
	json.Unmarshal(request(t, handler, "GET", path+"/catalog?database="+url.QueryEscape(fields.Database), "", cookie, 200).Body.Bytes(), &newCatalog)
	if !slices.Contains(newCatalog.Types, pgx.Identifier{newSchema, "mood"}.Sanitize()) || !slices.ContainsFunc(newCatalog.Schemas, func(item postgresSchema) bool {
		return item.Name == newSchema && slices.Contains(item.Tables, "created_here")
	}) {
		t.Fatal("new schema or its table is missing from catalog")
	}
	ddl("POST", "/tables", map[string]any{"database": fields.Database, "schema": schema, "table": "ddl_table", "columns": []map[string]any{{"name": "id", "type": "bigint", "primary": true}, {"name": "note", "type": "text", "nullable": true}}}, 200)
	ddl("POST", "/tables", map[string]any{"database": fields.Database, "schema": schema, "table": "injected", "columns": []map[string]any{{"name": "id", "type": "text); DROP TABLE users; --"}}}, 400)
	ddl("POST", "/table-columns", map[string]any{"database": fields.Database, "schema": schema, "table": "ddl_table", "name": "extra", "type": "text", "nullable": true, "changeDefault": true, "default": "hello"}, 200)
	ddl("PATCH", "/table-columns", map[string]any{"database": fields.Database, "schema": schema, "table": "ddl_table", "oldName": "extra", "name": "details", "type": "varchar(120)", "nullable": false, "changeDefault": true, "default": "world"}, 200)
	ddl("DELETE", "/table-columns", map[string]any{"database": fields.Database, "schema": schema, "table": "ddl_table", "name": "note"}, 200)
	var ddlPage tablePage
	json.Unmarshal(read("ddl_table", "1", "20", 200).Body.Bytes(), &ddlPage)
	if len(ddlPage.Columns) != 2 || ddlPage.Columns[1].Name != "details" || ddlPage.Columns[1].Nullable || ddlPage.Columns[1].Default == nil {
		t.Fatal("column changes were not reflected in table metadata")
	}
	ddl("POST", "/rows/insert", map[string]any{"database": fields.Database, "schema": schema, "table": "ddl_table", "values": map[string]any{"id": "9001", "details": "from form"}}, 200)
	ddl("POST", "/rows/insert", map[string]any{"database": fields.Database, "schema": schema, "table": "ddl_table", "values": map[string]any{"id": "9002"}}, 200)
	ddl("POST", "/rows/insert", map[string]any{"database": fields.Database, "schema": schema, "table": "ddl_table", "values": map[string]any{"unknown": "value"}}, 400)
	ddl("PATCH", "/tables", map[string]any{"database": fields.Database, "schema": schema, "table": "ddl_table", "newName": "renamed_table"}, 200)
	ddl("DELETE", "/tables", map[string]any{"database": fields.Database, "schema": schema, "table": "renamed_table"}, 200)
	ddl("DELETE", "/tables", map[string]any{"database": fields.Database, "schema": schema, "table": "items"}, 409)
	var ddlCatalog postgresResult
	json.Unmarshal(request(t, handler, "GET", path+"/catalog?database="+url.QueryEscape(fields.Database), "", cookie, 200).Body.Bytes(), &ddlCatalog)
	if slices.ContainsFunc(ddlCatalog.Schemas, func(item postgresSchema) bool {
		return item.Name == schema && slices.Contains(item.Tables, "renamed_table")
	}) {
		t.Fatal("deleted table remains in catalog")
	}
	role := "role_" + strings.ToLower(newID())
	_, err = conn.Exec(ctx, "CREATE ROLE "+pgx.Identifier{role}.Sanitize()+" LOGIN PASSWORD 'stage4-role-only'; GRANT USAGE ON SCHEMA "+quoted+" TO "+pgx.Identifier{role}.Sanitize()+"; GRANT SELECT ON "+quoted+".items TO "+pgx.Identifier{role}.Sanitize())
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Exec(ctx, "DROP OWNED BY "+pgx.Identifier{role}.Sanitize()+"; DROP ROLE "+pgx.Identifier{role}.Sanitize())
	limitedFields := fields
	limitedFields.Username = role
	limitedJSON, _ := json.Marshal(limitedFields)
	limitedResponse := request(t, handler, "POST", "/api/connections", string(limitedJSON), cookie, 201)
	var limited connectionProfile
	json.Unmarshal(limitedResponse.Body.Bytes(), &limited)
	limitedPath := "/api/connections/" + limited.ID
	request(t, handler, "POST", limitedPath+"/catalog", `{"password":"stage4-role-only"}`, cookie, 200)
	request(t, handler, "GET", limitedPath+"/rows?database="+fields.Database+"&schema="+schema+"&table=items&page=1&pageSize=20", "", cookie, 200)
	request(t, handler, "GET", limitedPath+"/rows?database="+fields.Database+"&schema="+schema+"&table=empty&page=1&pageSize=20", "", cookie, 403)
	readonly, err := connectPostgres(ctx, fields, password)
	if err != nil {
		t.Fatal(err)
	}
	_, err = readonly.Exec(ctx, "INSERT INTO "+quoted+".items(id) VALUES (100)")
	readonly.Close(ctx)
	if err == nil {
		t.Fatal("connection allowed writes")
	}
	emptyDatabase := "empty_" + strings.ToLower(newID())
	if _, err := conn.Exec(ctx, "CREATE DATABASE "+pgx.Identifier{emptyDatabase}.Sanitize()); err != nil {
		t.Fatal(err)
	}
	defer conn.Exec(ctx, "DROP DATABASE "+pgx.Identifier{emptyDatabase}.Sanitize())
	emptyFields := fields
	emptyFields.Database = emptyDatabase
	emptyCatalog, err := loadPostgresCatalog(ctx, emptyFields, password)
	if err != nil {
		t.Fatal(err)
	}
	for _, schema := range emptyCatalog.Schemas {
		if len(schema.Tables) != 0 || len(schema.Views) != 0 {
			t.Fatal("empty database contains unexpected objects")
		}
	}
	if _, err := conn.Exec(ctx, "REVOKE CONNECT ON DATABASE "+pgx.Identifier{emptyDatabase}.Sanitize()+" FROM PUBLIC"); err != nil {
		t.Fatal(err)
	}
	updated := request(t, handler, "POST", path+"/catalog", string(credentials), cookie, 200)
	var databases postgresResult
	if err := json.Unmarshal(updated.Body.Bytes(), &databases); err != nil || !slices.Contains(databases.Databases, emptyDatabase) {
		t.Fatal("accessible database is missing from connection")
	}
	limitedCatalog := request(t, handler, "POST", limitedPath+"/catalog", `{"password":"stage4-role-only"}`, cookie, 200)
	var limitedDatabases postgresResult
	if err := json.Unmarshal(limitedCatalog.Body.Bytes(), &limitedDatabases); err != nil || slices.Contains(limitedDatabases.Databases, emptyDatabase) {
		t.Fatal("database without CONNECT privilege was exposed")
	}
	otherPath := path + "/catalog?database=" + url.QueryEscape(emptyDatabase)
	request(t, handler, "GET", otherPath, "", cookie, 200)
	request(t, handler, "GET", limitedPath+"/catalog?database="+url.QueryEscape(emptyDatabase), "", cookie, 404)
	emptyURL := *u
	emptyURL.Path = "/" + emptyDatabase
	emptyConn, err := pgx.Connect(ctx, emptyURL.String())
	if err != nil {
		t.Fatal(err)
	}
	if _, err := emptyConn.Exec(ctx, "CREATE TABLE public.other_database (value text); INSERT INTO public.other_database VALUES ('from another database')"); err != nil {
		emptyConn.Close(ctx)
		t.Fatal(err)
	}
	emptyConn.Close(ctx)
	otherRows := request(t, handler, "GET", path+"/rows?"+url.Values{"database": {emptyDatabase}, "schema": {"public"}, "table": {"other_database"}, "page": {"1"}, "pageSize": {"20"}}.Encode(), "", cookie, 200)
	if !strings.Contains(otherRows.Body.String(), "from another database") {
		t.Fatal("rows from another database were not returned")
	}
	ddl("DELETE", "/databases", map[string]string{"database": fields.Database}, 409)
	ddl("DELETE", "/databases", map[string]string{"database": emptyDatabase}, 200)
	request(t, handler, "GET", otherPath, "", cookie, 404)
	createdDatabase := "created_" + strings.ToLower(newID())
	defer conn.Exec(ctx, "DROP DATABASE IF EXISTS "+pgx.Identifier{createdDatabase}.Sanitize())
	ddl("POST", "/databases", map[string]string{"database": createdDatabase}, 200)
	request(t, handler, "GET", path+"/catalog?database="+url.QueryEscape(createdDatabase), "", cookie, 200)
	ddl("POST", "/databases", map[string]string{"database": createdDatabase}, 409)
	renamedDatabase := "renamed_" + strings.ToLower(newID())
	defer conn.Exec(ctx, "DROP DATABASE IF EXISTS "+pgx.Identifier{renamedDatabase}.Sanitize())
	ddl("PATCH", "/databases", map[string]string{"database": createdDatabase, "newName": renamedDatabase}, 200)
	request(t, handler, "GET", path+"/catalog?database="+url.QueryEscape(renamedDatabase), "", cookie, 200)
	ddl("DELETE", "/databases", map[string]string{"database": renamedDatabase}, 200)
	read("items", "1", "101", 400)
	read(`items"; DROP SCHEMA public CASCADE; --`, "1", "20", 502)
	read("items", "1", "20", 200)
	// Sesi lain milik akun yang sama tidak boleh memakai kredensial sesi pertama.
	second := httptest.NewRecorder()
	a.startSession(second, httptest.NewRequest("POST", "/", nil), account, false)
	request(t, handler, "GET", path+"/rows?database="+fields.Database+"&schema="+schema+"&table=items&page=1&pageSize=20", "", second.Result().Cookies()[0], 409)
	a.mu.Lock()
	database := a.sessions[cookie.Value].Databases[profile.ID]
	database.expiresAt = time.Now()
	database.timer.Reset(time.Millisecond)
	a.mu.Unlock()
	select {
	case <-database.ctx.Done():
	case <-time.After(time.Second):
		t.Fatal("database session did not expire")
	}
	read("items", "1", "20", 409)
	request(t, handler, "POST", path+"/catalog", string(credentials), cookie, 200)
	a.mu.Lock()
	database = a.sessions[cookie.Value].Databases[profile.ID]
	a.mu.Unlock()
	request(t, handler, "POST", "/api/session/connections", `{"id":"`+profile.ID+`","enabled":false}`, cookie, 204)
	if database.ctx.Err() == nil {
		t.Fatal("disconnect did not cancel database requests")
	}
	read("items", "1", "20", 409)
	request(t, handler, "POST", path+"/catalog", string(credentials), cookie, 200)
	a.mu.Lock()
	database = a.sessions[cookie.Value].Databases[profile.ID]
	a.mu.Unlock()
	request(t, handler, "POST", "/api/logout", "", cookie, 204)
	if database.ctx.Err() == nil {
		t.Fatal("logout did not clear database session")
	}
}
