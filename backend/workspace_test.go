package main

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func testProfile(t *testing.T, h http.Handler, cookie *http.Cookie) connectionProfile {
	t.Helper()
	w := request(t, h, "POST", "/api/connections", `{"engine":"PostgreSQL","name":"Queries","host":"localhost","port":"5432","username":"postgres","database":"app","ssl":"Prefer"}`, cookie, 201)
	var profile connectionProfile
	if err := json.Unmarshal(w.Body.Bytes(), &profile); err != nil {
		t.Fatal(err)
	}
	return profile
}

func queryBody(t *testing.T, value any) string {
	t.Helper()
	data, err := json.Marshal(value)
	if err != nil {
		t.Fatal(err)
	}
	return string(data)
}

func TestSQLHistoryDeduplicatesOnlyLatestInDatabase(t *testing.T) {
	a, h := testApplication(t)
	cookie, owner := setupAdmin(t, h)
	profile := testProfile(t, h, cookie)
	location := queryLocation{ConnectionID: profile.ID, DB: "app", SchemaName: "public", SQL: "SELECT 1"}
	post := func(id, database, query string, status int) {
		location.ID, location.DB, location.SQL = id, database, query
		entry := historyEntry{location, time.Now().UTC().Format(time.RFC3339Nano), 1, "Success", 1}
		request(t, h, "POST", "/api/history?dedupeLatest=1", queryBody(t, entry), cookie, status)
	}
	post("first", "app", "SELECT 1", 204)
	post("duplicate", "app", "SELECT 1", 200)
	post("second", "app", "SELECT 2", 204)
	post("third", "app", "SELECT 1", 204)
	post("other", "other", "SELECT 1", 204)
	post("other-duplicate", "other", "SELECT 1", 200)
	history, err := a.store.listHistory(context.Background(), owner.ID)
	if err != nil || len(history) != 4 || history[0].ID != "other" || history[1].ID != "third" {
		t.Fatalf("latest-query deduplication failed: %+v, %v", history, err)
	}
}

func TestSQLiteMigrationPreservesAccounts(t *testing.T) {
	directory := t.TempDir()
	account := storedUser{user{"existing", "admin", "Super Admin", true}, makePassword("existing-password")}
	legacy := storeData{Version: 1, Users: []storedUser{account}, Connections: []connectionProfile{{ID: "pg", OwnerID: account.ID, connectionFields: connectionFields{Engine: "PostgreSQL", Name: "Existing", Host: "localhost", Port: "5432", Username: "postgres", Database: "app", SSL: "Prefer"}}}}
	source := []byte(queryBody(t, legacy))
	path := filepath.Join(directory, "store.json")
	if err := os.WriteFile(path, source, 0600); err != nil {
		t.Fatal(err)
	}
	s, err := openStore(directory)
	if err != nil {
		t.Fatal(err)
	}
	if len(s.data.Users) != 1 || s.data.Users[0] != account || len(s.data.Connections) != 1 {
		t.Fatal("migration changed existing data")
	}
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Fatal("JSON source retained after successful migration")
	}
	var integrity string
	if err := s.db.QueryRow("PRAGMA integrity_check").Scan(&integrity); err != nil || integrity != "ok" {
		t.Fatal("SQLite integrity check failed")
	}
	a := newApplication(s)
	request(t, a.handler(http.NotFoundHandler()), "POST", "/api/login", `{"username":"admin","password":"existing-password"}`, nil, 200)
	if err := s.close(); err != nil {
		t.Fatal(err)
	}
	// Ulangi kondisi commit berhasil tetapi penghapusan JSON belum sempat berjalan.
	if err := os.WriteFile(path, source, 0600); err != nil {
		t.Fatal(err)
	}
	s, err = openStore(directory)
	if err != nil {
		t.Fatal(err)
	}
	if len(s.data.Users) != 1 {
		t.Fatal("retry duplicated accounts")
	}
	s.close()
	if err := os.WriteFile(path, append(source, ' '), 0600); err != nil {
		t.Fatal(err)
	}
	if s, err := openStore(directory); err == nil {
		s.close()
		t.Fatal("changed legacy data silently discarded")
	}
	if _, err := os.Stat(path); err != nil {
		t.Fatal("conflicting legacy data was deleted")
	}
}

func TestMigrationFailureRollsBack(t *testing.T) {
	directory := t.TempDir()
	first := storedUser{user{"one", "same", "Super Admin", true}, makePassword("old-password")}
	second := first
	second.ID = "two"
	legacy := storeData{Version: 1, Users: []storedUser{first, second}}
	path := filepath.Join(directory, "store.json")
	if err := os.WriteFile(path, []byte(queryBody(t, legacy)), 0600); err != nil {
		t.Fatal(err)
	}
	if s, err := openStore(directory); err == nil {
		s.close()
		t.Fatal("conflicting migration succeeded")
	}
	if _, err := os.Stat(path); err != nil {
		t.Fatal("failed migration deleted the source")
	}
	legacy.Users = legacy.Users[:1]
	if err := os.WriteFile(path, []byte(queryBody(t, legacy)), 0600); err != nil {
		t.Fatal(err)
	}
	s, err := openStore(directory)
	if err != nil {
		t.Fatal(err)
	}
	defer s.close()
	if len(s.data.Users) != 1 {
		t.Fatal("migration retry did not recover")
	}
}

func TestWorkspacePersistenceIsolationAndLimits(t *testing.T) {
	a, h := testApplication(t)
	cookie, owner := setupAdmin(t, h)
	profile := testProfile(t, h, cookie)
	location := queryLocation{ID: "saved", SQL: "SELECT '日本語';", ConnectionID: profile.ID, DB: "app", SchemaName: "public"}
	request(t, h, "POST", "/api/saved-queries", queryBody(t, savedQuery{location, "Example"}), cookie, 204)
	request(t, h, "POST", "/api/saved-queries", queryBody(t, savedQuery{location, "Example"}), cookie, 204)
	for i := range maxHistoryEntries + 3 {
		location.ID = fmt.Sprintf("history-%d", i)
		entry := historyEntry{location, time.Now().UTC().Format(time.RFC3339Nano), 1, "Success", 1}
		request(t, h, "POST", "/api/history", queryBody(t, entry), cookie, 204)
	}
	workspace := request(t, h, "GET", "/api/workspace", "", cookie, 200)
	var data workspaceData
	if err := json.Unmarshal(workspace.Body.Bytes(), &data); err != nil {
		t.Fatal(err)
	}
	if len(data.Saved) != 1 || len(data.History) != maxHistoryEntries || data.History[0].ID != "history-102" {
		t.Fatal("workspace order, deduplication or retention failed")
	}
	location.ID, location.DB = "other-history", "other"
	request(t, h, "POST", "/api/history", queryBody(t, historyEntry{location, time.Now().UTC().Format(time.RFC3339Nano), 1, "Success", 1}), cookie, 204)
	if history, err := a.store.listHistory(context.Background(), owner.ID); err != nil || len(history) != maxHistoryEntries+1 {
		t.Fatal("history for another database was trimmed")
	}
	request(t, h, "DELETE", "/api/history?connectionId="+profile.ID+"&db=other", "", cookie, 204)
	location.DB = "app"
	request(t, h, "POST", "/api/users", `{"username":"other","password":"other-password","role":"User"}`, cookie, 201)
	w := request(t, h, "POST", "/api/login", `{"username":"other","password":"other-password"}`, nil, 200)
	otherCookie := w.Result().Cookies()[0]
	request(t, h, "POST", "/api/saved-queries", queryBody(t, savedQuery{location, "Forbidden"}), otherCookie, 404)
	request(t, h, "DELETE", "/api/saved-queries/saved", "", otherCookie, 404)
	request(t, h, "DELETE", "/api/history/history-102", "", otherCookie, 404)
	w = request(t, h, "GET", "/api/workspace", "", otherCookie, 200)
	if strings.Contains(w.Body.String(), "SELECT") {
		t.Fatal("workspace leaked to another user")
	}
	request(t, h, "DELETE", "/api/history", "", otherCookie, 204)
	if history, err := a.store.listHistory(context.Background(), owner.ID); err != nil || len(history) != maxHistoryEntries {
		t.Fatal("clearing other history affected owner")
	}
	for i := 1; i < maxSavedQueries; i++ {
		location.ID = fmt.Sprintf("saved-%d", i)
		request(t, h, "POST", "/api/saved-queries", queryBody(t, savedQuery{location, "Example"}), cookie, 204)
	}
	location.ID = "over-limit"
	request(t, h, "POST", "/api/saved-queries", queryBody(t, savedQuery{location, "Example"}), cookie, 409)
	location.ID, location.DB = "other-saved", "other"
	request(t, h, "POST", "/api/saved-queries", queryBody(t, savedQuery{location, "Other database"}), cookie, 204)
	request(t, h, "DELETE", "/api/saved-queries/other-saved", "", cookie, 204)
	directory := filepath.Dir(a.store.path)
	a.store.close()
	reopened, err := openStore(directory)
	if err != nil {
		t.Fatal(err)
	}
	defer reopened.close()
	history, err := reopened.listHistory(context.Background(), owner.ID)
	if err != nil || len(history) != maxHistoryEntries {
		t.Fatal("history was not persisted")
	}
	saved, err := reopened.listSaved(context.Background(), owner.ID)
	if err != nil || len(saved) != maxSavedQueries {
		t.Fatal("saved queries were not persisted")
	}
}

func TestBrowserImportIsAtomicAndIdempotent(t *testing.T) {
	a, h := testApplication(t)
	cookie, owner := setupAdmin(t, h)
	profile := testProfile(t, h, cookie)
	location := queryLocation{ID: "legacy", SQL: "SELECT 1", ConnectionID: profile.ID, DB: "app", SchemaName: "public"}
	entry := historyEntry{location, time.Now().UTC().Format(time.RFC3339Nano), 0, "Success", 1}
	input := map[string]any{"source": strings.Repeat("a", 64), "saved": []savedQuery{{location, "Legacy"}}, "history": []historyEntry{entry}}
	request(t, h, "POST", "/api/workspace/import", queryBody(t, input), cookie, 204)
	request(t, h, "DELETE", "/api/saved-queries/legacy", "", cookie, 204)
	request(t, h, "DELETE", "/api/history/legacy", "", cookie, 204)
	request(t, h, "POST", "/api/workspace/import", queryBody(t, input), cookie, 204)
	saved, err := a.store.listSaved(context.Background(), owner.ID)
	if err != nil || len(saved) != 0 {
		t.Fatal("retry resurrected deleted data")
	}
	input["source"] = strings.Repeat("b", 64)
	if _, err := a.store.db.Exec(`CREATE TRIGGER reject_history BEFORE INSERT ON query_history BEGIN SELECT RAISE(ABORT,'test failure'); END`); err != nil {
		t.Fatal(err)
	}
	request(t, h, "POST", "/api/workspace/import", queryBody(t, input), cookie, 500)
	saved, err = a.store.listSaved(context.Background(), owner.ID)
	if err != nil || len(saved) != 0 {
		t.Fatal("failed import partially saved data")
	}
	if _, err := a.store.db.Exec("DROP TRIGGER reject_history"); err != nil {
		t.Fatal(err)
	}
	request(t, h, "POST", "/api/workspace/import", queryBody(t, input), cookie, 204)
	// Penghapusan akun membersihkan seluruh workspace melalui foreign key.
	next := a.store.snapshot()
	next.Users = nil
	next.Connections = nil
	if err := a.store.save(next); err != nil {
		t.Fatal(err)
	}
	for _, table := range []string{"saved_queries", "query_history", "workspace_imports"} {
		var count int
		if err := a.store.db.QueryRow("SELECT count(*) FROM " + table).Scan(&count); err != nil || count != 0 {
			t.Fatalf("orphan rows in %s", table)
		}
	}
}

func TestSessionConnectionsStayInMemory(t *testing.T) {
	_, h := testApplication(t)
	cookie, _ := setupAdmin(t, h)
	profile := testProfile(t, h, cookie)
	request(t, h, "POST", "/api/session/connections", queryBody(t, map[string]any{"id": profile.ID, "enabled": true}), cookie, 204)
	w := request(t, h, "GET", "/api/session", "", cookie, 200)
	if !strings.Contains(w.Body.String(), profile.ID) {
		t.Fatal("connection status missing from session")
	}
	request(t, h, "POST", "/api/session/connections", `{"id":"foreign","enabled":true}`, cookie, 404)
	request(t, h, "POST", "/api/session/connections", queryBody(t, map[string]any{"id": profile.ID, "enabled": false}), cookie, 204)
	w = request(t, h, "GET", "/api/session", "", cookie, 200)
	if strings.Contains(w.Body.String(), profile.ID) {
		t.Fatal("connection status was not cleared")
	}
}
