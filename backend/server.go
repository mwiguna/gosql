package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"log"
	"mime"
	"net"
	"net/http"
	"slices"
	"strconv"
	"strings"
)

// -----------------------------------------------------------------------------
// Application routes
// -----------------------------------------------------------------------------

func (a *application) handler(files http.Handler) http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("/api/session", methods(a.handleSession, "GET"))
	mux.HandleFunc("/api/session/connections", methods(a.handleSessionConnection, "POST"))
	mux.HandleFunc("/api/setup", methods(a.handleSetup, "POST"))
	mux.HandleFunc("/api/login", methods(a.handleLogin, "POST"))
	mux.HandleFunc("/api/logout", methods(a.handleLogout, "POST"))
	mux.HandleFunc("/api/users", methods(a.handleUsers, "GET", "POST"))
	mux.HandleFunc("/api/users/{id}", methods(a.handleUser, "PATCH", "DELETE"))
	mux.HandleFunc("/api/users/{id}/password", methods(a.handlePassword, "POST"))
	mux.HandleFunc("/api/connections", methods(a.handleConnections, "GET", "POST"))
	mux.HandleFunc("/api/connections/{id}", methods(a.handleConnection, "PATCH", "DELETE"))
	mux.HandleFunc("/api/postgres/test", methods(a.handlePostgresTest, "POST"))
	mux.HandleFunc("/api/mysql/test", methods(a.handleMySQLTest, "POST"))
	mux.HandleFunc("/api/mariadb/test", methods(a.handleMySQLTest, "POST"))
	mux.HandleFunc("/api/sqlite/test", methods(a.handleSQLiteTest, "POST"))
	mux.HandleFunc("/api/sqlite/pick-file", methods(a.handleSQLitePickFile, "POST"))
	mux.HandleFunc("/api/connections/{id}/sqlite-file", methods(a.handleSQLiteFile, "POST", "GET"))
	mux.HandleFunc("/api/connections/{id}/catalog", methods(a.databaseHandler(a.handlePostgresCatalog, a.handleMySQLCatalog, a.handleSQLiteCatalog), "GET", "POST"))
	mux.HandleFunc("/api/connections/{id}/routines", methods(a.databaseHandler(a.handlePostgresRoutine, a.handleMySQLRoutine, a.handleSQLiteUnsupported), "GET"))
	mux.HandleFunc("/api/connections/{id}/triggers", methods(a.databaseHandler(a.handlePostgresTrigger, a.handleMySQLTrigger, a.handleSQLiteTrigger), "GET", "POST", "PATCH", "DELETE"))
	mux.HandleFunc("/api/connections/{id}/rows", methods(a.databaseHandler(a.handlePostgresRows, a.handleMySQLRows, a.handleSQLiteRows), "GET"))
	mux.HandleFunc("/api/connections/{id}/rows/count", methods(a.databaseHandler(a.handlePostgresCount, a.handleMySQLCount, a.handleSQLiteCount), "GET"))
	mux.HandleFunc("/api/connections/{id}/rows/cell", methods(a.databaseHandler(a.handlePostgresCell, a.handleMySQLCell, a.handleSQLiteCell), "POST"))
	mux.HandleFunc("/api/connections/{id}/rows/edit", methods(a.databaseHandler(a.handlePostgresEdit, a.handleMySQLEdit, a.handleSQLiteEdit), "POST"))
	mux.HandleFunc("/api/connections/{id}/rows/insert", methods(a.databaseHandler(a.handlePostgresInsert, a.handleMySQLInsert, a.handleSQLiteInsert), "POST"))
	mux.HandleFunc("/api/connections/{id}/rows/delete", methods(a.databaseHandler(a.handlePostgresDelete, a.handleMySQLDelete, a.handleSQLiteDelete), "POST"))
	mux.HandleFunc("/api/connections/{id}/constraints", methods(a.databaseHandler(a.handlePostgresConstraint, a.handleMySQLConstraint, a.handleSQLiteConstraint), "POST", "PATCH", "DELETE"))
	mux.HandleFunc("/api/connections/{id}/indexes", methods(a.databaseHandler(a.handlePostgresIndex, a.handleMySQLIndex, a.handleSQLiteIndex), "POST", "PATCH", "DELETE"))
	mux.HandleFunc("/api/connections/{id}/columns", methods(a.databaseHandler(a.handlePostgresColumns, a.handleMySQLColumns, a.handleSQLiteColumns), "GET"))
	mux.HandleFunc("/api/connections/{id}/tables", methods(a.databaseHandler(a.handlePostgresTable, a.handleMySQLTable, a.handleSQLiteTable), "POST", "PATCH", "DELETE"))
	mux.HandleFunc("/api/connections/{id}/schemas", methods(a.databaseHandler(a.handlePostgresSchema, func(w http.ResponseWriter, r *http.Request) {
		writeError(w, 404, "schema_unsupported", "MySQL and MariaDB databases do not have a separate schema tree.")
	}, a.handleSQLiteUnsupported), "POST", "PATCH", "DELETE"))
	mux.HandleFunc("/api/connections/{id}/table-columns", methods(a.databaseHandler(a.handlePostgresTableColumn, a.handleMySQLTableColumn, a.handleSQLiteTableColumn), "POST", "PATCH", "DELETE"))
	mux.HandleFunc("/api/connections/{id}/databases", methods(a.databaseHandler(a.handlePostgresDatabase, a.handleMySQLDatabase, a.handleSQLiteUnsupported), "POST", "PATCH", "DELETE"))
	mux.HandleFunc("/api/connections/{id}/query", methods(a.databaseHandler(a.handlePostgresQuery, a.handleMySQLQuery, a.handleSQLiteQuery), "POST"))
	mux.HandleFunc("/api/workspace", methods(a.handleWorkspace, "GET"))
	mux.HandleFunc("/api/workspace/import", methods(a.handleWorkspaceImport, "POST"))
	mux.HandleFunc("/api/saved-queries", methods(a.handleSavedQueries, "GET", "POST"))
	mux.HandleFunc("/api/saved-queries/{id}", methods(a.handleDeleteQuery, "DELETE"))
	mux.HandleFunc("/api/history", methods(a.handleHistory, "GET", "POST", "DELETE"))
	mux.HandleFunc("/api/history/{id}", methods(a.handleDeleteQuery, "DELETE"))
	missing := func(w http.ResponseWriter, r *http.Request) {
		writeError(w, 404, "not_found", "API endpoint not found.")
	}
	mux.HandleFunc("/api", missing)
	mux.HandleFunc("/api/", missing)
	mux.Handle("/", files)
	protection := http.NewCrossOriginProtection()
	protection.SetDenyHandler(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		writeError(w, 403, "cross_origin_request", "Cross-origin requests are not allowed.")
	}))
	return securityHeaders(protection.Handler(mux))
}

func (a *application) databaseHandler(postgres, mysql, sqlite http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		actor, ok := a.requireUser(w, r)
		if !ok {
			return
		}
		a.store.mu.Lock()
		index := slices.IndexFunc(a.store.data.Connections, func(c connectionProfile) bool {
			return c.ID == r.PathValue("id") && c.OwnerID == actor.ID
		})
		engine := ""
		if index >= 0 {
			engine = a.store.data.Connections[index].Engine
		}
		a.store.mu.Unlock()
		switch engine {
		case "PostgreSQL":
			postgres(w, r)
		case "MySQL", "MariaDB":
			mysql(w, r)
		case "SQLite":
			sqlite(w, r)
		default:
			writeError(w, 404, "profile_not_found", "Connection profile not found.")
		}
	}
}

func methods(handler http.HandlerFunc, allowed ...string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if !slices.Contains(allowed, r.Method) {
			w.Header().Set("Allow", strings.Join(allowed, ", "))
			writeError(w, 405, "method_not_allowed", "HTTP method not allowed.")
			return
		}
		handler(w, r)
	}
}

// -----------------------------------------------------------------------------
// HTTP responses and request validation
// -----------------------------------------------------------------------------

const maxRequestBytes = 512 << 10

type apiError struct {
	Code            string `json:"code"`
	Message         string `json:"message"`
	Detail          string `json:"detail,omitempty"`
	SQLState        string `json:"sqlState,omitempty"`
	Hint            string `json:"hint,omitempty"`
	DatabaseMessage string `json:"databaseMessage,omitempty"`
}

func writeJSON(w http.ResponseWriter, status int, value any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(value)
}

func writeError(w http.ResponseWriter, status int, code, message string) {
	log.Printf("API error: status=%d code=%s message=%q", status, code, message)
	writeJSON(w, status, struct {
		Error apiError `json:"error"`
	}{apiError{Code: code, Message: message}})
}

func readJSON(w http.ResponseWriter, r *http.Request, target any) bool {
	return readJSONLimit(w, r, target, maxRequestBytes)
}

func readJSONLimit(w http.ResponseWriter, r *http.Request, target any, limit int64) bool {
	mediaType, _, err := mime.ParseMediaType(r.Header.Get("Content-Type"))
	if err != nil || mediaType != "application/json" {
		writeError(w, http.StatusUnsupportedMediaType, "invalid_content_type", "Use application/json.")
		return false
	}
	r.Body = http.MaxBytesReader(w, r.Body, limit)
	decoder := json.NewDecoder(r.Body)
	var raw json.RawMessage
	err = decoder.Decode(&raw)
	if err == nil {
		var extra any
		if trailingErr := decoder.Decode(&extra); trailingErr != io.EOF {
			err = trailingErr
			if err == nil {
				err = errors.New("multiple JSON values")
			}
		}
	}
	if err == nil {
		if len(raw) == 0 || raw[0] != '{' {
			err = errors.New("expected JSON object")
		} else {
			decoder = json.NewDecoder(bytes.NewReader(raw))
			decoder.DisallowUnknownFields()
			err = decoder.Decode(target)
		}
	}
	if err != nil {
		var sizeError *http.MaxBytesError
		if errors.As(err, &sizeError) {
			writeError(w, http.StatusRequestEntityTooLarge, "request_too_large", "Request body is too large.")
		} else {
			writeError(w, http.StatusBadRequest, "invalid_json", "Invalid JSON body or unsupported fields.")
		}
		return false
	}
	return true
}

func uniqueQueryColumns(names []string) []string {
	columns := make([]string, len(names))
	counts := make(map[string]int, len(names))
	used := make(map[string]bool, len(names))
	for i, name := range names {
		counts[name]++
		candidate := name
		for used[candidate] {
			counts[name]++
			candidate = name + " (" + strconv.Itoa(counts[name]) + ")"
		}
		columns[i] = candidate
		used[candidate] = true
	}
	return columns
}

func securityHeaders(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("X-Content-Type-Options", "nosniff")
		w.Header().Set("Referrer-Policy", "same-origin")
		w.Header().Set("Content-Security-Policy", "default-src 'self'; script-src 'self'; style-src 'self' 'unsafe-inline'; img-src 'self' data:; connect-src 'self'; object-src 'none'; base-uri 'none'; frame-ancestors 'none'")
		w.Header().Set("Cache-Control", "no-store")
		next.ServeHTTP(w, r)
	})
}

// Binding loopback hanya menerima Host lokal untuk mencegah DNS rebinding.
func protectLocalHost(address string, next http.Handler) http.Handler {
	host, _, err := net.SplitHostPort(address)
	if err != nil {
		return next
	}
	ip := net.ParseIP(host)
	if host != "localhost" && (ip == nil || !ip.IsLoopback()) {
		return next
	}
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requestHost := r.Host
		if parsed, _, err := net.SplitHostPort(requestHost); err == nil {
			requestHost = parsed
		}
		requestIP := net.ParseIP(requestHost)
		if !strings.EqualFold(requestHost, "localhost") && (requestIP == nil || !requestIP.IsLoopback()) {
			writeError(w, 403, "invalid_host", "Use the local GoSQL address.")
			return
		}
		next.ServeHTTP(w, r)
	})
}
