package main

import (
	"context"
	"net/http"
	"slices"
	"sync"
	"time"
)

// Kredensial hanya hidup di memori sesi. Socket dibuka per permintaan dan selalu ditutup.
type databaseSession struct {
	mu        sync.RWMutex
	fields    connectionFields
	password  string
	databases []string
	ctx       context.Context
	cancel    context.CancelFunc
	timer     *time.Timer
	busy      chan struct{}
	expiresAt time.Time
}

func (s *databaseSession) hasDatabase(name string) bool {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return slices.Contains(s.databases, name)
}

func (s *databaseSession) databaseList() []string {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return slices.Clone(s.databases)
}

func (s *databaseSession) removeDatabase(name string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.databases = slices.DeleteFunc(s.databases, func(value string) bool { return value == name })
}

func (s *databaseSession) addDatabase(name string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if !slices.Contains(s.databases, name) {
		s.databases = append(s.databases, name)
		slices.Sort(s.databases)
	}
}

func (a *application) beginDatabaseRequest(w http.ResponseWriter) bool {
	select {
	case a.databaseSlots <- struct{}{}:
		return true
	default:
		writeError(w, 429, "database_busy", "Database requests are busy. Try again shortly.")
		return false
	}
}

func (s *session) closeDatabase(id string) {
	if database := s.Databases[id]; database != nil {
		database.timer.Stop()
		database.cancel()
		delete(s.Databases, id)
	}
	s.Connections = slices.DeleteFunc(s.Connections, func(value string) bool { return value == id })
}

// Pemanggil memegang application.mu, termasuk saat logout atau pencabutan akses akun.
func (a *application) removeSessionLocked(token string) {
	s := a.sessions[token]
	for id := range s.Databases {
		s.closeDatabase(id)
	}
	delete(a.sessions, token)
}

func (a *application) saveDatabaseSession(w http.ResponseWriter, r *http.Request, actor user, fields connectionFields, password string, names []string) bool {
	a.store.mu.Lock()
	defer a.store.mu.Unlock()
	if !a.requireUserLocked(w, r) {
		return false
	}
	if !slices.ContainsFunc(a.store.data.Connections, func(c connectionProfile) bool {
		return c.ID == r.PathValue("id") && c.OwnerID == actor.ID && c.connectionFields == fields
	}) {
		writeError(w, 409, "profile_changed", "The connection profile changed. Reconnect.")
		return false
	}
	cookie, _ := r.Cookie(sessionCookie)
	a.mu.Lock()
	s := a.sessions[cookie.Value]
	id := r.PathValue("id")
	s.closeDatabase(id)
	ctx, cancel := context.WithCancel(context.Background())
	database := &databaseSession{fields: fields, password: password, databases: names, ctx: ctx, cancel: cancel, busy: make(chan struct{}, 2), expiresAt: time.Now().Add(sessionIdleTimeout)}
	database.timer = time.AfterFunc(sessionIdleTimeout, func() {
		a.mu.Lock()
		defer a.mu.Unlock()
		current, exists := a.sessions[cookie.Value]
		if exists && current.Databases[id] == database {
			if remaining := time.Until(database.expiresAt); remaining > 0 {
				database.timer.Reset(remaining)
				return
			}
			current.closeDatabase(id)
			a.sessions[cookie.Value] = current
		}
	})
	if s.Databases == nil {
		s.Databases = make(map[string]*databaseSession)
	}
	s.Databases[id] = database
	s.Connections = append(s.Connections, id)
	a.sessions[cookie.Value] = s
	a.mu.Unlock()
	return true
}

func (a *application) getDatabaseSession(w http.ResponseWriter, r *http.Request, actor user) *databaseSession {
	a.store.mu.Lock()
	if !a.requireUserLocked(w, r) {
		a.store.mu.Unlock()
		return nil
	}
	cookie, _ := r.Cookie(sessionCookie)
	a.mu.Lock()
	s := a.sessions[cookie.Value]
	database := s.Databases[r.PathValue("id")]
	if database != nil && (!time.Now().Before(database.expiresAt) || !slices.ContainsFunc(a.store.data.Connections, func(c connectionProfile) bool {
		return c.ID == r.PathValue("id") && c.OwnerID == actor.ID && c.connectionFields == database.fields
	})) {
		s.closeDatabase(r.PathValue("id"))
		a.sessions[cookie.Value] = s
		database = nil
	}
	if database != nil {
		database.expiresAt = time.Now().Add(sessionIdleTimeout)
		database.timer.Reset(sessionIdleTimeout)
	}
	a.mu.Unlock()
	a.store.mu.Unlock()
	if database == nil {
		writeError(w, 409, "connection_required", "Database session expired. Reconnect to continue.")
		return nil
	}
	return database
}
