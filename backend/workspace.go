package main

import (
	"context"
	"database/sql"
	"encoding/hex"
	"errors"
	"net/http"
	"slices"
	"strings"
	"time"
	"unicode/utf8"
)

// -----------------------------------------------------------------------------
// Saved queries and SQL history
// -----------------------------------------------------------------------------

const maxHistoryEntries = 100
const maxSavedQueries = 100
const maxWorkspaceEntries = 1000
const maxSQLLength = 50000

type queryLocation struct {
	ID           string `json:"id"`
	SQL          string `json:"sql"`
	ConnectionID string `json:"connectionId"`
	DB           string `json:"db"`
	SchemaName   string `json:"schemaName"`
}

type savedQuery struct {
	queryLocation
	Name string `json:"name"`
}

type historyEntry struct {
	queryLocation
	Time     string  `json:"time"`
	Duration float64 `json:"duration"`
	Status   string  `json:"status"`
	RowCount int64   `json:"rowCount"`
}

type workspaceData struct {
	Saved   []savedQuery   `json:"saved"`
	History []historyEntry `json:"history"`
}

var errSavedQueryLimit = errors.New("saved query limit reached")

func (q queryLocation) valid() bool {
	return q.ID != "" && len(q.ID) <= 128 && q.ConnectionID != "" && len(q.ConnectionID) <= 128 &&
		strings.TrimSpace(q.SQL) != "" && utf8.RuneCountInString(q.SQL) <= maxSQLLength &&
		q.DB != "" && len(q.DB) <= 1024 && len(q.SchemaName) <= 1024
}

func (q savedQuery) valid() bool {
	return q.queryLocation.valid() && strings.TrimSpace(q.Name) != "" && utf8.RuneCountInString(q.Name) <= 100
}

func (q historyEntry) valid() bool {
	_, err := time.Parse(time.RFC3339Nano, q.Time)
	return q.queryLocation.valid() && err == nil && q.Duration >= 0 && q.RowCount >= 0 && slices.Contains([]string{"Success", "Error", "Cancelled"}, q.Status)
}

func (s *store) ownsConnection(owner, id string, legacy bool) bool {
	for _, profile := range s.data.Connections {
		if profile.ID == id {
			return profile.OwnerID == owner
		}
	}
	// History lama tetap dipertahankan ketika profil asal sudah dihapus.
	return legacy
}

func (s *store) listSaved(ctx context.Context, owner string) ([]savedQuery, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT id,sql,connection_id,database_name,schema_name,name FROM saved_queries WHERE owner_id=? ORDER BY sequence DESC LIMIT ?`, owner, maxWorkspaceEntries)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	items := []savedQuery{}
	for rows.Next() {
		var q savedQuery
		if err := rows.Scan(&q.ID, &q.SQL, &q.ConnectionID, &q.DB, &q.SchemaName, &q.Name); err != nil {
			return nil, err
		}
		items = append(items, q)
	}
	return items, rows.Err()
}

func (s *store) listHistory(ctx context.Context, owner string) ([]historyEntry, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT id,sql,connection_id,database_name,schema_name,recorded_at,duration,status,row_count FROM query_history WHERE owner_id=? ORDER BY sequence DESC LIMIT ?`, owner, maxWorkspaceEntries)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	items := []historyEntry{}
	for rows.Next() {
		var q historyEntry
		if err := rows.Scan(&q.ID, &q.SQL, &q.ConnectionID, &q.DB, &q.SchemaName, &q.Time, &q.Duration, &q.Status, &q.RowCount); err != nil {
			return nil, err
		}
		items = append(items, q)
	}
	return items, rows.Err()
}

func insertSaved(ctx context.Context, tx *sql.Tx, owner string, q savedQuery) error {
	var count, existing int
	if err := tx.QueryRowContext(ctx, "SELECT count(*) FROM saved_queries WHERE owner_id=? AND id=?", owner, q.ID).Scan(&existing); err != nil {
		return err
	}
	if existing != 0 {
		return nil
	}
	if err := tx.QueryRowContext(ctx, "SELECT count(*) FROM saved_queries WHERE owner_id=? AND connection_id=? AND database_name=?", owner, q.ConnectionID, q.DB).Scan(&count); err != nil {
		return err
	}
	if count >= maxSavedQueries {
		return errSavedQueryLimit
	}
	_, err := tx.ExecContext(ctx, `INSERT INTO saved_queries(owner_id,id,name,sql,connection_id,database_name,schema_name) VALUES (?,?,?,?,?,?,?)`, owner, q.ID, q.Name, q.SQL, q.ConnectionID, q.DB, q.SchemaName)
	return err
}

func insertHistory(ctx context.Context, tx *sql.Tx, owner string, q historyEntry) error {
	_, err := tx.ExecContext(ctx, `INSERT INTO query_history(owner_id,id,sql,connection_id,database_name,schema_name,recorded_at,duration,status,row_count) VALUES (?,?,?,?,?,?,?,?,?,?) ON CONFLICT(owner_id,id) DO NOTHING`, owner, q.ID, q.SQL, q.ConnectionID, q.DB, q.SchemaName, q.Time, q.Duration, q.Status, q.RowCount)
	return err
}

func trimHistory(ctx context.Context, tx *sql.Tx, owner, connectionID, database string) error {
	_, err := tx.ExecContext(ctx, `DELETE FROM query_history WHERE owner_id=? AND connection_id=? AND database_name=? AND sequence NOT IN (SELECT sequence FROM query_history WHERE owner_id=? AND connection_id=? AND database_name=? ORDER BY sequence DESC LIMIT ?)`, owner, connectionID, database, owner, connectionID, database, maxHistoryEntries)
	return err
}

func workspaceError(w http.ResponseWriter, err error) {
	if errors.Is(err, errSavedQueryLimit) {
		writeError(w, 409, "saved_query_limit", "Saved query limit reached. Delete a saved query first.")
		return
	}
	writeError(w, 500, "storage_failed", "Could not access the saved queries or SQL history.")
}

// -----------------------------------------------------------------------------
// Workspace API
// -----------------------------------------------------------------------------

func (a *application) handleWorkspace(w http.ResponseWriter, r *http.Request) {
	actor, ok := a.requireUser(w, r)
	if !ok {
		return
	}
	a.store.mu.Lock()
	defer a.store.mu.Unlock()
	if !a.requireUserLocked(w, r) {
		return
	}
	saved, err := a.store.listSaved(r.Context(), actor.ID)
	if err != nil {
		workspaceError(w, err)
		return
	}
	history, err := a.store.listHistory(r.Context(), actor.ID)
	if err != nil {
		workspaceError(w, err)
		return
	}
	writeJSON(w, 200, workspaceData{saved, history})
}

func (a *application) handleSavedQueries(w http.ResponseWriter, r *http.Request) {
	actor, ok := a.requireUser(w, r)
	if !ok {
		return
	}
	var entry savedQuery
	if r.Method == http.MethodPost {
		if !readJSON(w, r, &entry) {
			return
		}
		if !entry.valid() {
			writeError(w, 400, "invalid_query", "Check the query name, SQL, and destination.")
			return
		}
	}
	a.store.mu.Lock()
	defer a.store.mu.Unlock()
	if !a.requireUserLocked(w, r) {
		return
	}
	if r.Method == http.MethodPost {
		if !a.store.ownsConnection(actor.ID, entry.ConnectionID, false) {
			writeError(w, 404, "profile_not_found", "Connection profile not found.")
			return
		}
		tx, err := a.store.db.BeginTx(r.Context(), nil)
		if err != nil {
			workspaceError(w, err)
			return
		}
		defer tx.Rollback()
		if err := insertSaved(r.Context(), tx, actor.ID, entry); err != nil {
			workspaceError(w, err)
			return
		}
		if err := tx.Commit(); err != nil {
			workspaceError(w, err)
			return
		}
		w.WriteHeader(204)
		return
	}
	items, err := a.store.listSaved(r.Context(), actor.ID)
	if err != nil {
		workspaceError(w, err)
		return
	}
	writeJSON(w, 200, items)
}

func (a *application) handleHistory(w http.ResponseWriter, r *http.Request) {
	actor, ok := a.requireUser(w, r)
	if !ok {
		return
	}
	var entry historyEntry
	if r.Method == http.MethodPost {
		if !readJSON(w, r, &entry) {
			return
		}
		if !entry.valid() {
			writeError(w, 400, "invalid_history", "Invalid SQL history entry.")
			return
		}
	}
	a.store.mu.Lock()
	defer a.store.mu.Unlock()
	if !a.requireUserLocked(w, r) {
		return
	}
	if r.Method == http.MethodDelete {
		connectionID, database := r.URL.Query().Get("connectionId"), r.URL.Query().Get("db")
		statement := "DELETE FROM query_history WHERE owner_id=?"
		args := []any{actor.ID}
		if connectionID != "" && database != "" {
			statement += " AND connection_id=? AND database_name=?"
			args = append(args, connectionID, database)
		}
		if _, err := a.store.db.ExecContext(r.Context(), statement, args...); err != nil {
			workspaceError(w, err)
			return
		}
		w.WriteHeader(204)
		return
	}
	if r.Method == http.MethodPost {
		if !a.store.ownsConnection(actor.ID, entry.ConnectionID, false) {
			writeError(w, 404, "profile_not_found", "Connection profile not found.")
			return
		}
		if r.URL.Query().Get("dedupeLatest") == "1" {
			var previous string
			err := a.store.db.QueryRowContext(r.Context(), `SELECT sql FROM query_history WHERE owner_id=? AND connection_id=? AND database_name=? ORDER BY sequence DESC LIMIT 1`, actor.ID, entry.ConnectionID, entry.DB).Scan(&previous)
			if err != nil && !errors.Is(err, sql.ErrNoRows) {
				workspaceError(w, err)
				return
			}
			if err == nil && previous == entry.SQL {
				writeJSON(w, 200, map[string]bool{"stored": false})
				return
			}
		}
		tx, err := a.store.db.BeginTx(r.Context(), nil)
		if err != nil {
			workspaceError(w, err)
			return
		}
		defer tx.Rollback()
		if err := insertHistory(r.Context(), tx, actor.ID, entry); err != nil {
			workspaceError(w, err)
			return
		}
		if err := trimHistory(r.Context(), tx, actor.ID, entry.ConnectionID, entry.DB); err != nil {
			workspaceError(w, err)
			return
		}
		if err := tx.Commit(); err != nil {
			workspaceError(w, err)
			return
		}
		w.WriteHeader(204)
		return
	}
	items, err := a.store.listHistory(r.Context(), actor.ID)
	if err != nil {
		workspaceError(w, err)
		return
	}
	writeJSON(w, 200, items)
}

func (a *application) handleDeleteQuery(w http.ResponseWriter, r *http.Request) {
	actor, ok := a.requireUser(w, r)
	if !ok {
		return
	}
	a.store.mu.Lock()
	defer a.store.mu.Unlock()
	if !a.requireUserLocked(w, r) {
		return
	}
	statement := "DELETE FROM saved_queries WHERE owner_id=? AND id=?"
	if strings.HasPrefix(r.URL.Path, "/api/history/") {
		statement = "DELETE FROM query_history WHERE owner_id=? AND id=?"
	}
	result, err := a.store.db.ExecContext(r.Context(), statement, actor.ID, r.PathValue("id"))
	if err != nil {
		workspaceError(w, err)
		return
	}
	count, err := result.RowsAffected()
	if err != nil {
		workspaceError(w, err)
		return
	}
	if count == 0 {
		writeError(w, 404, "query_not_found", "Query not found.")
		return
	}
	w.WriteHeader(204)
}

// -----------------------------------------------------------------------------
// One-time browser workspace import
// -----------------------------------------------------------------------------

func (a *application) handleWorkspaceImport(w http.ResponseWriter, r *http.Request) {
	actor, ok := a.requireUser(w, r)
	if !ok {
		return
	}
	var input struct {
		Source string `json:"source"`
		workspaceData
	}
	if !readJSONLimit(w, r, &input, 64<<20) {
		return
	}
	digest, err := hex.DecodeString(input.Source)
	if err != nil || len(digest) != 32 || len(input.Saved) > maxSavedQueries || len(input.History) > maxHistoryEntries {
		writeError(w, 400, "invalid_import", "Invalid workspace import.")
		return
	}
	for _, q := range input.Saved {
		if !q.valid() {
			writeError(w, 400, "invalid_import", "Invalid saved query in the legacy workspace.")
			return
		}
	}
	for _, q := range input.History {
		if !q.valid() {
			writeError(w, 400, "invalid_import", "Invalid history entry in the legacy workspace.")
			return
		}
	}
	a.store.mu.Lock()
	defer a.store.mu.Unlock()
	if !a.requireUserLocked(w, r) {
		return
	}
	var imported int
	if err := a.store.db.QueryRowContext(r.Context(), "SELECT count(*) FROM workspace_imports WHERE owner_id=? AND source=?", actor.ID, input.Source).Scan(&imported); err != nil {
		workspaceError(w, err)
		return
	}
	if imported != 0 {
		w.WriteHeader(204)
		return
	}
	for _, q := range input.Saved {
		if !a.store.ownsConnection(actor.ID, q.ConnectionID, true) {
			writeError(w, 403, "forbidden", "A saved query belongs to another account's connection.")
			return
		}
	}
	for _, q := range input.History {
		if !a.store.ownsConnection(actor.ID, q.ConnectionID, true) {
			writeError(w, 403, "forbidden", "A history entry belongs to another account's connection.")
			return
		}
	}
	tx, err := a.store.db.BeginTx(r.Context(), nil)
	if err != nil {
		workspaceError(w, err)
		return
	}
	defer tx.Rollback()
	// Browser menyimpan urutan terbaru dahulu, sedangkan sequence SQLite bertambah.
	for i := len(input.Saved) - 1; i >= 0; i-- {
		if err := insertSaved(r.Context(), tx, actor.ID, input.Saved[i]); err != nil {
			workspaceError(w, err)
			return
		}
	}
	for i := len(input.History) - 1; i >= 0; i-- {
		if err := insertHistory(r.Context(), tx, actor.ID, input.History[i]); err != nil {
			workspaceError(w, err)
			return
		}
		if err := trimHistory(r.Context(), tx, actor.ID, input.History[i].ConnectionID, input.History[i].DB); err != nil {
			workspaceError(w, err)
			return
		}
	}
	if _, err := tx.ExecContext(r.Context(), "INSERT INTO workspace_imports VALUES (?,?)", actor.ID, input.Source); err != nil {
		workspaceError(w, err)
		return
	}
	if err := tx.Commit(); err != nil {
		workspaceError(w, err)
		return
	}
	w.WriteHeader(204)
}
