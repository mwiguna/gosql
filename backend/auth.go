package main

import (
	"crypto/pbkdf2"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/hex"
	"net"
	"net/http"
	"slices"
	"strings"
	"sync"
	"time"
)

// -----------------------------------------------------------------------------
// Accounts and runtime sessions
// -----------------------------------------------------------------------------

const sessionCookie = "gosql-session"
const passwordIterations = 600000
const sessionIdleTimeout = 5 * time.Minute

type user struct {
	ID       string `json:"id"`
	Username string `json:"username"`
	Role     string `json:"role"`
	Enabled  bool   `json:"enabled"`
}

type storedUser struct {
	user
	Password passwordHash `json:"passwordHash"`
}

type passwordHash struct {
	Algorithm  string `json:"algorithm"`
	Iterations int    `json:"iterations"`
	Salt       string `json:"salt"`
	Hash       string `json:"hash"`
}

type session struct {
	UserID      string
	ExpiresAt   time.Time
	LastSeen    time.Time
	Remember    bool
	Connections []string
	Databases   map[string]*databaseSession
}

type application struct {
	store             *store
	sqliteUploadLimit int64
	sqlitePicks       map[string]sqlitePickedFile
	sqlitePickerSlot  chan struct{}
	mu                sync.Mutex
	sessions          map[string]session
	authSlot          chan struct{}
	nextLogin         time.Time
	databaseSlots     chan struct{}
	countSlot         chan struct{}
}

func newApplication(s *store) *application {
	return &application{store: s, sqliteUploadLimit: sqliteUploadLimit, sqlitePicks: make(map[string]sqlitePickedFile), sqlitePickerSlot: make(chan struct{}, 1), sessions: make(map[string]session), authSlot: make(chan struct{}, 1), databaseSlots: make(chan struct{}, 8), countSlot: make(chan struct{}, 1)}
}

func newID() string { return rand.Text() }

func makePassword(password string) passwordHash {
	salt := make([]byte, 16)
	_, _ = rand.Read(salt)
	key, err := pbkdf2.Key(sha256.New, password, salt, passwordIterations, 32)
	if err != nil {
		panic(err)
	}
	return passwordHash{"pbkdf2-sha256", passwordIterations, hex.EncodeToString(salt), hex.EncodeToString(key)}
}

func verifyPassword(password string, stored passwordHash) bool {
	salt, saltErr := hex.DecodeString(stored.Salt)
	want, hashErr := hex.DecodeString(stored.Hash)
	if stored.Algorithm != "pbkdf2-sha256" || stored.Iterations != passwordIterations || saltErr != nil || hashErr != nil || len(salt) != 16 || len(want) != 32 {
		return false
	}
	got, err := pbkdf2.Key(sha256.New, password, salt, stored.Iterations, 32)
	return err == nil && subtle.ConstantTimeCompare(got, want) == 1
}

func validCredentials(username, password string) bool {
	return len(username) >= 1 && len(username) <= 64 && !strings.ContainsAny(username, "\r\n\t") && len(password) >= 8 && len(password) <= 1024
}

func (a *application) currentUser(r *http.Request) (user, bool) {
	a.store.mu.Lock()
	defer a.store.mu.Unlock()
	return a.currentUserLocked(r)
}

// Pemanggil memegang store.mu agar otorisasi dan perubahan data tidak terpisah.
func (a *application) currentUserLocked(r *http.Request) (user, bool) {
	cookie, err := r.Cookie(sessionCookie)
	if err != nil {
		return user{}, false
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	now := time.Now()
	s, ok := a.sessions[cookie.Value]
	if !ok {
		return user{}, false
	}
	if !sessionValid(s, now) {
		a.removeSessionLocked(cookie.Value)
		return user{}, false
	}
	for _, account := range a.store.data.Users {
		if account.ID == s.UserID && account.Enabled {
			if now.Sub(s.LastSeen) >= sessionIdleTimeout {
				for id := range s.Databases {
					s.closeDatabase(id)
				}
				s.Connections = nil
			} else {
				// Heartbeat tab aktif menjaga kredensial; sesudah semua tab tutup timer tetap berakhir.
				for _, database := range s.Databases {
					database.expiresAt = now.Add(sessionIdleTimeout)
					database.timer.Reset(sessionIdleTimeout)
				}
			}
			s.LastSeen = now
			a.sessions[cookie.Value] = s
			return account.user, true
		}
	}
	a.removeSessionLocked(cookie.Value)
	return user{}, false
}

func sessionValid(s session, now time.Time) bool {
	return now.Before(s.ExpiresAt) && (s.Remember || now.Sub(s.LastSeen) < sessionIdleTimeout)
}

func (a *application) requireUser(w http.ResponseWriter, r *http.Request) (user, bool) {
	account, ok := a.currentUser(r)
	if !ok {
		writeError(w, 401, "unauthorized", "Sign in to continue.")
	}
	return account, ok
}

func (a *application) requireUserLocked(w http.ResponseWriter, r *http.Request) bool {
	_, ok := a.currentUserLocked(r)
	if !ok {
		writeError(w, 401, "unauthorized", "Sign in to continue.")
	}
	return ok
}

func (a *application) startSession(w http.ResponseWriter, r *http.Request, account user, remember bool) {
	now := time.Now()
	duration := 24 * time.Hour
	if remember {
		duration = 30 * 24 * time.Hour
	}
	token := newID()
	a.mu.Lock()
	for id, existing := range a.sessions {
		if !sessionValid(existing, now) {
			a.removeSessionLocked(id)
		}
	}
	// Batasi sesi per akun agar login berulang tidak menambah memori tanpa batas.
	for {
		count, oldestID := 0, ""
		var oldest time.Time
		for id, existing := range a.sessions {
			if existing.UserID != account.ID {
				continue
			}
			count++
			if oldestID == "" || existing.LastSeen.Before(oldest) {
				oldestID, oldest = id, existing.LastSeen
			}
		}
		if count < 10 {
			break
		}
		a.removeSessionLocked(oldestID)
	}
	if old, err := r.Cookie(sessionCookie); err == nil {
		a.removeSessionLocked(old.Value)
	}
	a.sessions[token] = session{UserID: account.ID, ExpiresAt: now.Add(duration), LastSeen: now, Remember: remember}
	a.mu.Unlock()
	cookie := &http.Cookie{Name: sessionCookie, Value: token, Path: "/api", HttpOnly: true, Secure: r.TLS != nil, SameSite: http.SameSiteStrictMode}
	if remember {
		cookie.MaxAge = int(duration.Seconds())
		cookie.Expires = now.Add(duration)
	}
	http.SetCookie(w, cookie)
	writeJSON(w, 200, map[string]any{"user": account, "remember": remember, "expiresAt": now.Add(duration), "nativeFilePickerAllowed": sqliteNativePickerAllowed(r)})
}

// Hashing dijalankan satu per satu agar percobaan login tidak menghabiskan CPU.
func (a *application) beginAuthentication(w http.ResponseWriter) bool {
	select {
	case a.authSlot <- struct{}{}:
		return true
	default:
		w.Header().Set("Retry-After", "1")
		writeError(w, 429, "authentication_busy", "Try again shortly.")
		return false
	}
}

// -----------------------------------------------------------------------------
// Authentication handlers
// -----------------------------------------------------------------------------

func (a *application) handleSession(w http.ResponseWriter, r *http.Request) {
	account, ok := a.currentUser(r)
	a.store.mu.Lock()
	setup := len(a.store.data.Users) == 0
	a.store.mu.Unlock()
	if !ok {
		writeJSON(w, 200, map[string]any{"user": nil, "setupRequired": setup})
		return
	}
	cookie, _ := r.Cookie(sessionCookie)
	a.mu.Lock()
	s := a.sessions[cookie.Value]
	s.Connections = slices.Clone(s.Connections)
	a.mu.Unlock()
	writeJSON(w, 200, map[string]any{"user": account, "setupRequired": false, "remember": s.Remember, "expiresAt": s.ExpiresAt, "connections": s.Connections, "nativeFilePickerAllowed": sqliteNativePickerAllowed(r)})
}

// Status koneksi mengikuti sesi server, tanpa localStorage atau file.
func (a *application) handleSessionConnection(w http.ResponseWriter, r *http.Request) {
	actor, ok := a.requireUser(w, r)
	if !ok {
		return
	}
	var input struct {
		ID      string `json:"id"`
		Enabled *bool  `json:"enabled"`
	}
	if !readJSON(w, r, &input) {
		return
	}
	if input.ID == "" || len(input.ID) > 128 || input.Enabled == nil {
		writeError(w, 400, "invalid_connection", "Connection ID and enabled are required.")
		return
	}
	a.store.mu.Lock()
	defer a.store.mu.Unlock()
	if !a.requireUserLocked(w, r) {
		return
	}
	if *input.Enabled && !a.store.ownsConnection(actor.ID, input.ID, false) {
		writeError(w, 404, "profile_not_found", "Connection profile not found.")
		return
	}
	cookie, _ := r.Cookie(sessionCookie)
	a.mu.Lock()
	s, exists := a.sessions[cookie.Value]
	if exists {
		if *input.Enabled && !slices.Contains(s.Connections, input.ID) {
			s.Connections = append(s.Connections, input.ID)
		}
		if !*input.Enabled {
			s.closeDatabase(input.ID)
		}
		a.sessions[cookie.Value] = s
	}
	a.mu.Unlock()
	if !exists {
		writeError(w, 401, "unauthorized", "Sign in to continue.")
		return
	}
	w.WriteHeader(204)
}

func (a *application) handleSetup(w http.ResponseWriter, r *http.Request) {
	host, _, _ := net.SplitHostPort(r.RemoteAddr)
	if ip := net.ParseIP(host); ip == nil || !ip.IsLoopback() {
		writeError(w, 403, "local_setup_required", "Create the first administrator from this computer.")
		return
	}
	var input struct {
		Username string `json:"username"`
		Password string `json:"password"`
	}
	if !readJSON(w, r, &input) {
		return
	}
	input.Username = strings.TrimSpace(input.Username)
	if !validCredentials(input.Username, input.Password) {
		writeError(w, 400, "invalid_credentials", "Use a username of 1–64 bytes and a password of 8–1024 bytes.")
		return
	}
	if !a.beginAuthentication(w) {
		return
	}
	defer func() { <-a.authSlot }()
	a.store.mu.Lock()
	if len(a.store.data.Users) != 0 {
		a.store.mu.Unlock()
		writeError(w, 409, "already_initialized", "An administrator already exists.")
		return
	}
	account := storedUser{user{newID(), input.Username, "Super Admin", true}, makePassword(input.Password)}
	next := a.store.snapshot()
	next.Users = append(next.Users, account)
	err := a.store.save(next)
	a.store.mu.Unlock()
	if err != nil {
		writeError(w, 500, "storage_failed", "Could not save the administrator.")
		return
	}
	a.startSession(w, r, account.user, false)
}

func (a *application) handleLogin(w http.ResponseWriter, r *http.Request) {
	var input struct {
		Username string `json:"username"`
		Password string `json:"password"`
		Remember bool   `json:"remember"`
	}
	if !readJSON(w, r, &input) {
		return
	}
	input.Username = strings.TrimSpace(input.Username)
	if !validCredentials(input.Username, input.Password) {
		writeError(w, 401, "invalid_credentials", "Incorrect username or password, or account disabled.")
		return
	}
	if !a.beginAuthentication(w) {
		return
	}
	defer func() { <-a.authSlot }()
	if time.Now().Before(a.nextLogin) {
		w.Header().Set("Retry-After", "1")
		writeError(w, 429, "login_throttled", "Try again shortly.")
		return
	}
	a.nextLogin = time.Now().Add(time.Second)
	a.store.mu.Lock()
	var candidate storedUser
	for _, account := range a.store.data.Users {
		if account.Username == input.Username {
			candidate = account
			break
		}
	}
	valid := false
	if candidate.ID == "" {
		_ = makePassword(input.Password)
	} else {
		valid = verifyPassword(input.Password, candidate.Password)
	}
	if !valid || !candidate.Enabled {
		a.store.mu.Unlock()
		writeError(w, 401, "invalid_credentials", "Incorrect username or password, or account disabled.")
		return
	}
	a.startSession(w, r, candidate.user, input.Remember)
	a.store.mu.Unlock()
}

func (a *application) handleLogout(w http.ResponseWriter, r *http.Request) {
	if cookie, err := r.Cookie(sessionCookie); err == nil {
		a.mu.Lock()
		a.removeSessionLocked(cookie.Value)
		a.mu.Unlock()
	}
	http.SetCookie(w, &http.Cookie{Name: sessionCookie, Path: "/api", MaxAge: -1, HttpOnly: true, Secure: r.TLS != nil, SameSite: http.SameSiteStrictMode})
	w.WriteHeader(http.StatusNoContent)
}

// -----------------------------------------------------------------------------
// Account management
// -----------------------------------------------------------------------------

func (a *application) handleUsers(w http.ResponseWriter, r *http.Request) {
	actor, ok := a.requireUser(w, r)
	if !ok {
		return
	}
	if actor.Role != "Super Admin" {
		writeError(w, 403, "forbidden", "Administrator access required.")
		return
	}
	if r.Method == http.MethodGet {
		a.store.mu.Lock()
		defer a.store.mu.Unlock()
		if !a.requireUserLocked(w, r) {
			return
		}
		users := make([]user, 0, len(a.store.data.Users))
		for _, account := range a.store.data.Users {
			users = append(users, account.user)
		}
		writeJSON(w, 200, users)
		return
	}
	var input struct {
		Username string `json:"username"`
		Password string `json:"password"`
		Role     string `json:"role"`
	}
	if !readJSON(w, r, &input) {
		return
	}
	input.Username = strings.TrimSpace(input.Username)
	if !validCredentials(input.Username, input.Password) || (input.Role != "User" && input.Role != "Super Admin") {
		writeError(w, 400, "invalid_user", "Invalid username, password, or role.")
		return
	}
	if !a.beginAuthentication(w) {
		return
	}
	defer func() { <-a.authSlot }()
	a.store.mu.Lock()
	defer a.store.mu.Unlock()
	if !a.requireUserLocked(w, r) {
		return
	}
	if len(a.store.data.Users) >= 100 {
		writeError(w, 409, "user_limit", "The account limit has been reached.")
		return
	}
	for _, account := range a.store.data.Users {
		if account.Username == input.Username {
			writeError(w, 409, "username_exists", "This username already exists.")
			return
		}
	}
	account := storedUser{user{newID(), input.Username, input.Role, true}, makePassword(input.Password)}
	next := a.store.snapshot()
	next.Users = append(next.Users, account)
	if err := a.store.save(next); err != nil {
		writeError(w, 500, "storage_failed", "Could not save the account.")
		return
	}
	writeJSON(w, 201, account.user)
}

func (a *application) handleUser(w http.ResponseWriter, r *http.Request) {
	actor, ok := a.requireUser(w, r)
	if !ok {
		return
	}
	id := r.PathValue("id")
	if actor.Role != "Super Admin" {
		writeError(w, 403, "forbidden", "Administrator access required.")
		return
	}
	if actor.ID == id {
		writeError(w, 409, "own_account", "You cannot disable or delete your own account.")
		return
	}
	var input struct {
		Enabled *bool `json:"enabled"`
	}
	if r.Method == http.MethodPatch {
		if !readJSON(w, r, &input) {
			return
		}
		if input.Enabled == nil {
			writeError(w, 400, "invalid_user", "The enabled field is required.")
			return
		}
	}
	a.store.mu.Lock()
	defer a.store.mu.Unlock()
	if !a.requireUserLocked(w, r) {
		return
	}
	next := a.store.snapshot()
	index := slices.IndexFunc(next.Users, func(u storedUser) bool { return u.ID == id })
	if index < 0 {
		writeError(w, 404, "user_not_found", "Account not found.")
		return
	}
	if r.Method == http.MethodDelete {
		next.Users = slices.Delete(next.Users, index, index+1)
		next.Connections = slices.DeleteFunc(next.Connections, func(c connectionProfile) bool { return c.OwnerID == id })
	} else {
		next.Users[index].Enabled = *input.Enabled
	}
	if err := a.store.save(next); err != nil {
		writeError(w, 500, "storage_failed", "Could not update the account.")
		return
	}
	a.revokeSessions(id)
	w.WriteHeader(http.StatusNoContent)
}

func (a *application) handlePassword(w http.ResponseWriter, r *http.Request) {
	actor, ok := a.requireUser(w, r)
	if !ok {
		return
	}
	id := r.PathValue("id")
	if actor.ID != id && actor.Role != "Super Admin" {
		writeError(w, 403, "forbidden", "You cannot update this account.")
		return
	}
	var input struct {
		Current  string `json:"current"`
		Password string `json:"password"`
	}
	if !readJSON(w, r, &input) {
		return
	}
	if len(input.Password) < 8 || len(input.Password) > 1024 || len(input.Current) > 1024 {
		writeError(w, 400, "invalid_password", "Use a password of 8–1024 bytes.")
		return
	}
	if !a.beginAuthentication(w) {
		return
	}
	defer func() { <-a.authSlot }()
	a.store.mu.Lock()
	defer a.store.mu.Unlock()
	if !a.requireUserLocked(w, r) {
		return
	}
	next := a.store.snapshot()
	index := slices.IndexFunc(next.Users, func(u storedUser) bool { return u.ID == id })
	if index < 0 {
		writeError(w, 404, "user_not_found", "Account not found.")
		return
	}
	if actor.ID == id && !verifyPassword(input.Current, next.Users[index].Password) {
		writeError(w, 403, "wrong_password", "Current password is incorrect.")
		return
	}
	next.Users[index].Password = makePassword(input.Password)
	if err := a.store.save(next); err != nil {
		writeError(w, 500, "storage_failed", "Could not save the password.")
		return
	}
	a.revokeSessions(id)
	w.WriteHeader(http.StatusNoContent)
}

func (a *application) revokeSessions(userID string) {
	a.mu.Lock()
	defer a.mu.Unlock()
	for token, s := range a.sessions {
		if s.UserID == userID {
			a.removeSessionLocked(token)
		}
	}
}
