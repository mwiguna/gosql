package main

import (
	"context"
	"errors"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"

	sqlitedb "gosql/database/sqlite"
)

type sqlitePickedFile struct {
	ownerID string
	path    string
	expires time.Time
}

func (a *application) sqliteNativePath(path string) (string, error) {
	if !filepath.IsAbs(path) {
		return "", errors.New("SQLite file path must be absolute")
	}
	canonical, err := filepath.EvalSymlinks(path)
	if err != nil {
		return "", err
	}
	canonical, err = filepath.Abs(canonical)
	if err != nil {
		return "", err
	}
	info, err := os.Stat(canonical)
	if err != nil {
		return "", err
	}
	if !info.Mode().IsRegular() {
		return "", errors.New("choose a regular SQLite database file")
	}
	if internal, err := os.Stat(a.store.path); err == nil && os.SameFile(info, internal) {
		return "", errors.New("the GoSQL application database cannot be used as a connection")
	}
	return canonical, nil
}

func (a *application) validateSQLitePicker(ownerID string, fields connectionFields, previous connectionProfile) bool {
	if fields.Engine != "SQLite" || fields.Location != "Native file" {
		return fields.PickerToken == ""
	}
	if fields.PickerToken == "" {
		return previous.OwnerID == ownerID && previous.Engine == "SQLite" && previous.Location == "Native file" && previous.File == fields.File
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	pick, ok := a.sqlitePicks[fields.PickerToken]
	return ok && pick.ownerID == ownerID && pick.path == fields.File && time.Now().Before(pick.expires)
}

func (a *application) consumeSQLitePickerToken(token string) {
	if token == "" {
		return
	}
	a.mu.Lock()
	delete(a.sqlitePicks, token)
	a.mu.Unlock()
}

func sqliteNativePickerAllowed(r *http.Request) bool {
	if r.Header.Get("Forwarded") != "" || r.Header.Get("X-Forwarded-For") != "" {
		return false
	}
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		return false
	}
	ip := net.ParseIP(host)
	if ip == nil || !ip.IsLoopback() {
		return false
	}
	host = r.Host
	if parsed, _, err := net.SplitHostPort(host); err == nil {
		host = parsed
	}
	ip = net.ParseIP(host)
	return strings.EqualFold(host, "localhost") || ip != nil && ip.IsLoopback()
}

func (a *application) handleSQLitePickFile(w http.ResponseWriter, r *http.Request) {
	actor, ok := a.requireUser(w, r)
	if !ok {
		return
	}
	if !sqliteNativePickerAllowed(r) {
		writeError(w, 403, "sqlite_picker_local_only", "The native file picker is available only from the GoSQL machine.")
		return
	}
	select {
	case a.sqlitePickerSlot <- struct{}{}:
		defer func() { <-a.sqlitePickerSlot }()
	default:
		writeError(w, 409, "sqlite_picker_busy", "A file picker is already open.")
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 60*time.Second)
	defer cancel()
	selected, canceled, err := sqlitedb.NativeDialog(ctx)
	if canceled {
		writeJSON(w, 200, map[string]bool{"canceled": true})
		return
	}
	if err != nil {
		writeError(w, 503, "sqlite_picker_unavailable", err.Error())
		return
	}
	path, err := a.sqliteNativePath(selected)
	if err == nil {
		err = sqlitedb.Check(ctx, path)
	}
	if err != nil {
		writeSQLiteFileError(w, err)
		return
	}
	token := newID()
	a.mu.Lock()
	for id, pick := range a.sqlitePicks {
		if time.Now().After(pick.expires) {
			delete(a.sqlitePicks, id)
		}
	}
	a.sqlitePicks[token] = sqlitePickedFile{actor.ID, path, time.Now().Add(5 * time.Minute)}
	a.mu.Unlock()
	writeJSON(w, 200, map[string]string{"file": path, "token": token})
}
