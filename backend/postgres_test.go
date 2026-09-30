package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5/pgconn"
)

func TestDeleteDatabaseErrorIncludesPostgresDetail(t *testing.T) {
	w := httptest.NewRecorder()
	writeDatabaseDeleteError(w, &pgconn.PgError{Code: "42501", Message: "must be owner of database shop", Detail: "Owner is another role.", Hint: "Ask the owner to drop it."})
	if w.Code != 403 {
		t.Fatalf("expected permission error, got %d", w.Code)
	}
	var response struct {
		Error apiError `json:"error"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &response); err != nil {
		t.Fatal(err)
	}
	if response.Error.Code != "database_permission_denied" || response.Error.SQLState != "42501" || response.Error.Detail != "Owner is another role." || response.Error.Hint == "" || response.Error.DatabaseMessage != "must be owner of database shop" {
		t.Fatalf("PostgreSQL error detail was lost: %+v", response.Error)
	}
	w = httptest.NewRecorder()
	writeDatabaseDeleteError(w, &pgconn.PgError{Code: "55006", Message: "database is being accessed by other users"})
	if err := json.Unmarshal(w.Body.Bytes(), &response); err != nil || w.Code != 409 || response.Error.Code != "database_in_use" {
		t.Fatalf("database-in-use error was not explained: %+v, %v", response.Error, err)
	}
}

func TestCreateDatabaseErrorExplainsMissingPrivilege(t *testing.T) {
	w := httptest.NewRecorder()
	writeDatabaseCreateError(w, &pgconn.PgError{Code: "42501", Message: "permission denied to create database"})
	var response struct {
		Error apiError `json:"error"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &response); err != nil || w.Code != 403 || response.Error.Code != "database_permission_denied" || response.Error.SQLState != "42501" {
		t.Fatalf("CREATE DATABASE permission error was not explained: %+v, %v", response.Error, err)
	}
}

func TestPostgresEndpoints(t *testing.T) {
	_, handler := testApplication(t)
	cookie, _ := setupAdmin(t, handler)
	request(t, handler, "POST", "/api/postgres/test", `{"engine":"PostgreSQL","name":"Test","host":"127.0.0.1","port":"1","username":"postgres","database":"app","ssl":"Prefer","password":"secret"}`, nil, 401)
	request(t, handler, "POST", "/api/postgres/test", `{"engine":"MySQL","name":"Test","host":"127.0.0.1","port":"1","username":"root","ssl":"Prefer"}`, cookie, 400)
	request(t, handler, "POST", "/api/postgres/test", `{"engine":"PostgreSQL","name":"Test","host":"127.0.0.1","port":"1","username":"postgres","database":"app","ssl":"Prefer","ssh":true,"sshHost":"host","sshPort":"22","sshUser":"user","sshAuth":"Password"}`, cookie, 400)
	request(t, handler, "POST", "/api/connections/missing/catalog", `{"password":"secret"}`, cookie, 404)
	profileResponse := request(t, handler, "POST", "/api/connections", `{"engine":"PostgreSQL","name":"Local","host":"127.0.0.1","port":"1","username":"postgres","database":"app","ssl":"Disable"}`, cookie, 201)
	var profile connectionProfile
	if err := json.Unmarshal(profileResponse.Body.Bytes(), &profile); err != nil {
		t.Fatal(err)
	}
	request(t, handler, "GET", "/api/connections/"+profile.ID+"/catalog?database=app", "", cookie, 409)
	request(t, handler, "GET", "/api/connections/"+profile.ID+"/catalog", "", cookie, 400)
	request(t, handler, "POST", "/api/connections/"+profile.ID+"/tables", `{"database":"app","schema":"public","table":"test","columns":[{"name":"id","type":"text); DROP TABLE users; --"}]}`, cookie, 400)
	request(t, handler, "POST", "/api/connections/"+profile.ID+"/tables", `{"database":"app","schema":"public","table":"test","columns":[{"name":"id","type":"bigint"}]}`, cookie, 409)
	request(t, handler, "POST", "/api/connections/"+profile.ID+"/table-columns", `{"database":"app","schema":"public","table":"test","name":"extra","type":"text"}`, cookie, 409)
	request(t, handler, "DELETE", "/api/connections/"+profile.ID+"/databases", `{"database":"app"}`, cookie, 409)
	response := request(t, handler, "POST", "/api/connections/"+profile.ID+"/catalog", `{"password":"secret"}`, cookie, 502)
	if strings.Contains(response.Body.String(), "secret") {
		t.Fatal("database password leaked in error response")
	}
	response = request(t, handler, "GET", "/api/connections", "", cookie, 200)
	if strings.Contains(response.Body.String(), "secret") {
		t.Fatal("database password was stored in profile")
	}
	request(t, handler, "GET", "/api/postgres/test", "", cookie, http.StatusMethodNotAllowed)
}
