package main

import (
	"bytes"
	"encoding/json"
	"net/http"
	"os"
	"slices"
	"strconv"
	"strings"
)

// -----------------------------------------------------------------------------
// Connection profiles
// -----------------------------------------------------------------------------

// Hanya konfigurasi profil; password dan material private key tidak diterima API ini.
type connectionFields struct {
	Engine      string `json:"engine"`
	Name        string `json:"name"`
	Host        string `json:"host,omitempty"`
	Port        string `json:"port,omitempty"`
	Username    string `json:"username,omitempty"`
	Database    string `json:"database,omitempty"`
	SSL         string `json:"ssl,omitempty"`
	SSH         bool   `json:"ssh"`
	SSHHost     string `json:"sshHost,omitempty"`
	SSHPort     string `json:"sshPort,omitempty"`
	SSHUser     string `json:"sshUser,omitempty"`
	SSHAuth     string `json:"sshAuth,omitempty"`
	File        string `json:"file,omitempty"`
	Location    string `json:"location,omitempty"`
	PickerToken string `json:"pickerToken,omitempty"`
}

type connectionProfile struct {
	ID      string `json:"id"`
	OwnerID string `json:"ownerId"`
	connectionFields
}

func (c *connectionFields) valid() bool {
	c.Name = strings.TrimSpace(c.Name)
	if len(c.Name) == 0 || len(c.Name) > 80 {
		return false
	}
	for _, value := range []string{c.Host, c.Username, c.Database, c.SSHHost, c.SSHUser, c.File, c.PickerToken} {
		if len(value) > 1024 || strings.ContainsRune(value, 0) {
			return false
		}
	}
	if c.Engine == "SQLite" {
		c.Host, c.Port, c.Username, c.Database, c.SSL = "", "", "", "", ""
		c.SSH, c.SSHHost, c.SSHPort, c.SSHUser, c.SSHAuth = false, "", "", "", ""
		return c.File != "" && slices.Contains([]string{"Local file", "Server upload", "Native file"}, c.Location)
	}
	c.File, c.Location, c.PickerToken = "", "", ""
	if c.Engine != "PostgreSQL" && c.Engine != "MySQL" && c.Engine != "MariaDB" {
		return false
	}
	port, err := strconv.Atoi(c.Port)
	if err != nil || port < 1 || port > 65535 || strings.TrimSpace(c.Host) == "" || strings.TrimSpace(c.Username) == "" {
		return false
	}
	if c.Engine == "PostgreSQL" && strings.TrimSpace(c.Database) == "" {
		return false
	}
	if (c.Engine == "MySQL" || c.Engine == "MariaDB") && c.Database != "" {
		return false
	}
	if !slices.Contains([]string{"Disable", "Prefer", "Require", "Verify CA", "Verify Full"}, c.SSL) {
		return false
	}
	if c.SSH {
		port, err = strconv.Atoi(c.SSHPort)
		if err != nil || port < 1 || port > 65535 || c.SSHHost == "" || c.SSHUser == "" || !slices.Contains([]string{"Password", "Private key"}, c.SSHAuth) {
			return false
		}
	}
	return true
}

func (a *application) handleConnections(w http.ResponseWriter, r *http.Request) {
	actor, ok := a.requireUser(w, r)
	if !ok {
		return
	}
	if r.Method == http.MethodGet {
		a.store.mu.Lock()
		defer a.store.mu.Unlock()
		if !a.requireUserLocked(w, r) {
			return
		}
		profiles := []connectionProfile{}
		for _, profile := range a.store.data.Connections {
			if profile.OwnerID == actor.ID {
				profiles = append(profiles, profile)
			}
		}
		writeJSON(w, 200, profiles)
		return
	}
	var fields connectionFields
	if !readJSON(w, r, &fields) {
		return
	}
	if !fields.valid() {
		writeError(w, 400, "invalid_profile", "Check the connection name, engine, and connection settings.")
		return
	}
	a.store.mu.Lock()
	defer a.store.mu.Unlock()
	if !a.requireUserLocked(w, r) {
		return
	}
	if !a.validateSQLitePicker(actor.ID, fields, connectionProfile{}) {
		writeError(w, 403, "sqlite_file_not_selected", "Choose the SQLite file using the native file dialog.")
		return
	}
	pickerToken := fields.PickerToken
	fields.PickerToken = ""
	count := 0
	for _, profile := range a.store.data.Connections {
		if profile.OwnerID == actor.ID {
			count++
		}
	}
	if count >= 100 {
		writeError(w, 409, "profile_limit", "The connection profile limit has been reached.")
		return
	}
	profile := connectionProfile{newID(), actor.ID, fields}
	next := a.store.snapshot()
	next.Connections = append(next.Connections, profile)
	if err := a.store.save(next); err != nil {
		writeError(w, 500, "storage_failed", "Could not save the connection profile.")
		return
	}
	a.consumeSQLitePickerToken(pickerToken)
	writeJSON(w, 201, profile)
}

func (a *application) handleConnection(w http.ResponseWriter, r *http.Request) {
	actor, ok := a.requireUser(w, r)
	if !ok {
		return
	}
	// Body dibaca sebelum lock agar klien lambat tidak menahan akses seluruh store.
	var patch json.RawMessage
	if r.Method == http.MethodPatch && !readJSON(w, r, &patch) {
		return
	}
	a.store.mu.Lock()
	defer a.store.mu.Unlock()
	if !a.requireUserLocked(w, r) {
		return
	}
	next := a.store.snapshot()
	index := slices.IndexFunc(next.Connections, func(c connectionProfile) bool { return c.ID == r.PathValue("id") && c.OwnerID == actor.ID })
	if index < 0 {
		writeError(w, 404, "profile_not_found", "Connection profile not found.")
		return
	}
	previous := next.Connections[index]
	pickerToken := ""
	if r.Method == http.MethodDelete {
		next.Connections = slices.Delete(next.Connections, index, index+1)
	} else {
		decoder := json.NewDecoder(bytes.NewReader(patch))
		decoder.DisallowUnknownFields()
		if err := decoder.Decode(&next.Connections[index].connectionFields); err != nil {
			writeError(w, 400, "invalid_json", "Invalid JSON body or unsupported fields.")
			return
		}
		if !next.Connections[index].valid() {
			writeError(w, 400, "invalid_profile", "Check the connection settings.")
			return
		}
		if !a.validateSQLitePicker(actor.ID, next.Connections[index].connectionFields, previous) {
			writeError(w, 403, "sqlite_file_not_selected", "Choose the SQLite file using the native file dialog.")
			return
		}
		pickerToken = next.Connections[index].PickerToken
		next.Connections[index].PickerToken = ""
	}
	if err := a.store.save(next); err != nil {
		writeError(w, 500, "storage_failed", "Could not update the connection profile.")
		return
	}
	a.consumeSQLitePickerToken(pickerToken)
	a.mu.Lock()
	for token, session := range a.sessions {
		session.closeDatabase(r.PathValue("id"))
		a.sessions[token] = session
	}
	a.mu.Unlock()
	removeUpload := previous.Engine == "SQLite" && previous.Location == "Server upload" && (r.Method == http.MethodDelete || next.Connections[index].Engine != "SQLite" || next.Connections[index].Location != "Server upload")
	if removeUpload {
		path, _ := a.sqlitePath(previous)
		for _, suffix := range []string{"", "-wal", "-shm"} {
			_ = os.Remove(path + suffix)
		}
	}
	if r.Method == http.MethodDelete {
		w.WriteHeader(204)
	} else {
		writeJSON(w, 200, next.Connections[index])
	}
}
