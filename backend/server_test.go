package main

import (
	"context"
	"encoding/json"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

func testApplication(t *testing.T) (*application, http.Handler) {
	t.Helper()
	s, err := openStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = s.close() })
	a := newApplication(s)
	return a, a.handler(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { _, _ = w.Write([]byte("frontend")) }))
}

func request(t *testing.T, h http.Handler, method, path, body string, cookie *http.Cookie, status int) *httptest.ResponseRecorder {
	t.Helper()
	r := httptest.NewRequest(method, "http://localhost"+path, strings.NewReader(body))
	r.RemoteAddr = "127.0.0.1:12345"
	if body != "" {
		r.Header.Set("Content-Type", "application/json")
	}
	if cookie != nil {
		r.AddCookie(cookie)
	}
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)
	if w.Code != status {
		t.Fatalf("%s %s: got %d, want %d: %s", method, path, w.Code, status, w.Body.String())
	}
	return w
}

func setupAdmin(t *testing.T, h http.Handler) (*http.Cookie, user) {
	t.Helper()
	w := request(t, h, "POST", "/api/setup", `{"username":"admin","password":"test-password"}`, nil, 200)
	var data struct {
		User user `json:"user"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &data); err != nil {
		t.Fatal(err)
	}
	return w.Result().Cookies()[0], data.User
}

func TestAuthenticationAndPersistence(t *testing.T) {
	a, h := testApplication(t)
	w := request(t, h, "GET", "/api/session", "", nil, 200)
	if !strings.Contains(w.Body.String(), `"setupRequired":true`) {
		t.Fatal(w.Body.String())
	}
	request(t, h, "GET", "/api/connections", "", &http.Cookie{Name: sessionCookie, Value: "forged"}, 401)
	cookie, account := setupAdmin(t, h)
	if !cookie.HttpOnly || cookie.SameSite != http.SameSiteStrictMode || cookie.Path != "/api" || cookie.Secure || cookie.MaxAge != 0 {
		t.Fatalf("incorrect cookie: %+v", cookie)
	}
	request(t, h, "POST", "/api/setup", `{"username":"other","password":"test-password"}`, nil, 409)
	w = request(t, h, "GET", "/api/users", "", cookie, 200)
	if strings.Contains(w.Body.String(), "Hash") || strings.Contains(w.Body.String(), "salt") {
		t.Fatal("password hash leaked")
	}
	request(t, h, "POST", "/api/logout", "", cookie, 204)
	request(t, h, "GET", "/api/users", "", cookie, 401)
	request(t, h, "POST", "/api/login", `{"username":"admin","password":"wrong-password"}`, nil, 401)
	a.nextLogin = time.Time{}
	w = request(t, h, "POST", "/api/login", `{"username":"admin","password":"test-password","remember":true}`, nil, 200)
	cookie = w.Result().Cookies()[0]
	if cookie.MaxAge != 30*24*60*60 || cookie.Value == "" {
		t.Fatal("remember cookie missing")
	}
	request(t, h, "POST", "/api/login", `{"username":"admin","password":"test-password"}`, nil, 429)
	request(t, h, "POST", "/api/connections", `{"engine":"PostgreSQL","name":"Persisted","host":"localhost","port":"5432","username":"postgres","database":"app","ssl":"Prefer"}`, cookie, 201)
	var algorithm, hash string
	if err := a.store.db.QueryRow("SELECT password_algorithm,password_hash FROM users WHERE id=?", account.ID).Scan(&algorithm, &hash); err != nil {
		t.Fatal(err)
	}
	if hash == "test-password" || algorithm != "pbkdf2-sha256" {
		t.Fatal("password persistence is incorrect")
	}
	directory := filepath.Dir(a.store.path)
	if err := a.store.close(); err != nil {
		t.Fatal(err)
	}
	reopened, err := openStore(directory)
	if err != nil {
		t.Fatal(err)
	}
	defer reopened.close()
	if len(reopened.data.Users) != 1 || reopened.data.Users[0].ID != account.ID {
		t.Fatal("account lost after restart")
	}
	if len(reopened.data.Connections) != 1 || reopened.data.Connections[0].Name != "Persisted" {
		t.Fatal("profile lost after restart")
	}
	restarted := newApplication(reopened)
	request(t, restarted.handler(http.NotFoundHandler()), "GET", "/api/connections", "", cookie, 401)
}

func TestAccountIsolationAndRevocation(t *testing.T) {
	a, h := testApplication(t)
	adminCookie, admin := setupAdmin(t, h)
	w := request(t, h, "POST", "/api/users", `{"username":"reader","password":"reader-password","role":"User"}`, adminCookie, 201)
	var reader user
	if err := json.Unmarshal(w.Body.Bytes(), &reader); err != nil {
		t.Fatal(err)
	}
	w = request(t, h, "POST", "/api/login", `{"username":"reader","password":"reader-password"}`, nil, 200)
	readerCookie := w.Result().Cookies()[0]
	request(t, h, "GET", "/api/users", "", readerCookie, 403)
	request(t, h, "DELETE", "/api/users/"+admin.ID, "", readerCookie, 403)
	request(t, h, "DELETE", "/api/users/"+admin.ID, "", adminCookie, 409)
	w = request(t, h, "POST", "/api/connections", `{"engine":"PostgreSQL","name":"Primary","host":"localhost","port":"5432","username":"postgres","database":"app","ssl":"Prefer"}`, adminCookie, 201)
	var profile connectionProfile
	if err := json.Unmarshal(w.Body.Bytes(), &profile); err != nil {
		t.Fatal(err)
	}
	w = request(t, h, "GET", "/api/connections", "", readerCookie, 200)
	if strings.TrimSpace(w.Body.String()) != "[]" {
		t.Fatal("another user's profile leaked")
	}
	request(t, h, "PATCH", "/api/connections/"+profile.ID, `{"name":"Stolen"}`, readerCookie, 404)
	request(t, h, "DELETE", "/api/connections/"+profile.ID, "", readerCookie, 404)
	request(t, h, "PATCH", "/api/connections/"+profile.ID, `{"password":"do-not-store"}`, adminCookie, 400)
	request(t, h, "PATCH", "/api/connections/"+profile.ID, `{"ownerId":"other"}`, adminCookie, 400)
	w = request(t, h, "PATCH", "/api/connections/"+profile.ID, `{"name":"Renamed"}`, adminCookie, 200)
	if !strings.Contains(w.Body.String(), `"database":"app"`) {
		t.Fatal("partial update lost settings")
	}
	request(t, h, "PATCH", "/api/users/"+reader.ID, `{}`, adminCookie, 400)
	request(t, h, "PATCH", "/api/users/"+reader.ID, `{"enabled":false}`, adminCookie, 204)
	request(t, h, "GET", "/api/connections", "", readerCookie, 401)
	request(t, h, "PATCH", "/api/users/"+reader.ID, `{"enabled":true}`, adminCookie, 204)
	request(t, h, "GET", "/api/connections", "", readerCookie, 401)
	a.nextLogin = time.Time{}
	w = request(t, h, "POST", "/api/login", `{"username":"reader","password":"reader-password"}`, nil, 200)
	readerCookie = w.Result().Cookies()[0]
	request(t, h, "POST", "/api/users/"+reader.ID+"/password", `{"current":"wrong","password":"new-password"}`, readerCookie, 403)
	request(t, h, "POST", "/api/users/"+reader.ID+"/password", `{"current":"reader-password","password":"new-password"}`, readerCookie, 204)
	request(t, h, "GET", "/api/connections", "", readerCookie, 401)
	a.nextLogin = time.Time{}
	request(t, h, "POST", "/api/login", `{"username":"reader","password":"reader-password"}`, nil, 401)
	a.nextLogin = time.Time{}
	w = request(t, h, "POST", "/api/login", `{"username":"reader","password":"new-password"}`, nil, 200)
	readerCookie = w.Result().Cookies()[0]
	request(t, h, "POST", "/api/connections", `{"engine":"PostgreSQL","name":"Reader","host":"localhost","port":"5432","username":"reader","database":"app","ssl":"Require"}`, readerCookie, 201)
	request(t, h, "DELETE", "/api/users/"+reader.ID, "", adminCookie, 204)
	request(t, h, "GET", "/api/connections", "", readerCookie, 401)
	if len(a.store.data.Connections) != 1 {
		t.Fatal("deleted account profiles were retained")
	}
	request(t, h, "DELETE", "/api/connections/"+profile.ID, "", adminCookie, 204)
}

func TestSessionExpiryAndSecureCookie(t *testing.T) {
	a, h := testApplication(t)
	cookie, _ := setupAdmin(t, h)
	s := a.sessions[cookie.Value]
	s.LastSeen = time.Now().Add(-sessionIdleTimeout)
	a.sessions[cookie.Value] = s
	request(t, h, "GET", "/api/connections", "", cookie, 401)
	s.Remember = true
	a.sessions[cookie.Value] = s
	request(t, h, "GET", "/api/connections", "", cookie, 200)
	s.ExpiresAt = time.Now().Add(-time.Second)
	a.sessions[cookie.Value] = s
	request(t, h, "GET", "/api/connections", "", cookie, 401)
	r := httptest.NewRequest("POST", "https://localhost/api/login", strings.NewReader(`{"username":"admin","password":"test-password"}`))
	r.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)
	if w.Code != 200 || !w.Result().Cookies()[0].Secure {
		t.Fatal("HTTPS session cookie must be secure")
	}
}

func TestHTTPValidation(t *testing.T) {
	_, h := testApplication(t)
	request(t, h, "GET", "/api/missing", "", nil, 404)
	request(t, h, "PUT", "/api/login", "", nil, 405)
	request(t, h, "POST", "/api/setup", "", nil, 415)
	request(t, h, "POST", "/api/setup", `{} {}`, nil, 400)
	request(t, h, "POST", "/api/setup", `null`, nil, 400)
	request(t, h, "POST", "/api/setup", `{"username":"admin","password":"test-password","role":"Super Admin"}`, nil, 400)
	request(t, h, "POST", "/api/setup", `{"username":"`+strings.Repeat("x", maxRequestBytes)+`"}`, nil, 413)
	for _, headers := range []map[string]string{{"Origin": "https://other.example"}, {"Sec-Fetch-Site": "cross-site"}} {
		r := httptest.NewRequest("POST", "http://localhost/api/logout", nil)
		for key, value := range headers {
			r.Header.Set(key, value)
		}
		w := httptest.NewRecorder()
		h.ServeHTTP(w, r)
		if w.Code != 403 {
			t.Fatal("cross-origin mutation was accepted")
		}
	}
	r := httptest.NewRequest("POST", "http://localhost/api/setup", strings.NewReader(`{"username":"admin","password":"test-password"}`))
	r.Header.Set("Content-Type", "application/json")
	r.RemoteAddr = "192.0.2.1:3000"
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)
	if w.Code != 403 {
		t.Fatal("remote bootstrap was allowed")
	}
	w = httptest.NewRecorder()
	protectLocalHost("127.0.0.1:8080", h).ServeHTTP(w, httptest.NewRequest("GET", "http://rebind.example/api/session", nil))
	if w.Code != 403 {
		t.Fatal("DNS rebinding Host was accepted")
	}
	w = request(t, h, "GET", "/", "", nil, 200)
	if w.Body.String() != "frontend" || w.Header().Get("Content-Security-Policy") == "" {
		t.Fatal("static frontend or security headers missing")
	}
}

func TestStoreLockAndWriteFailure(t *testing.T) {
	a, h := testApplication(t)
	cookie, _ := setupAdmin(t, h)
	if other, err := openStore(filepath.Dir(a.store.path)); err == nil {
		other.close()
		t.Fatal("second writer was allowed")
	}
	if _, err := a.store.db.Exec("PRAGMA query_only=ON"); err != nil {
		t.Fatal(err)
	}
	request(t, h, "POST", "/api/connections", `{"engine":"PostgreSQL","name":"Failed","host":"localhost","port":"5432","username":"postgres","database":"app","ssl":"Disable"}`, cookie, 500)
	if len(a.store.data.Connections) != 0 {
		t.Fatal("failed save mutated memory")
	}
	var count int
	if err := a.store.db.QueryRow("SELECT count(*) FROM connections").Scan(&count); err != nil || count != 0 {
		t.Fatal("failed save changed persisted data")
	}
	directory := t.TempDir()
	if err := os.WriteFile(filepath.Join(directory, "store.json"), []byte(`{"version":`), 0600); err != nil {
		t.Fatal(err)
	}
	if s, err := openStore(directory); err == nil {
		s.close()
		t.Fatal("corrupt store was accepted")
	}
}

func TestGracefulShutdownWaitsForRequest(t *testing.T) {
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	entered, release := make(chan struct{}), make(chan struct{})
	server := &http.Server{Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		close(entered)
		<-release
		_, _ = w.Write([]byte("finished"))
	})}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- serve(ctx, server, listener) }()
	response := make(chan string, 1)
	go func() {
		client := &http.Client{Timeout: 3 * time.Second}
		r, err := client.Get("http://" + listener.Addr().String())
		if err != nil {
			response <- err.Error()
			return
		}
		defer r.Body.Close()
		body, _ := io.ReadAll(r.Body)
		response <- string(body)
	}()
	select {
	case <-entered:
	case <-time.After(3 * time.Second):
		t.Fatal("request did not start")
	}
	cancel()
	select {
	case err := <-done:
		t.Fatalf("shutdown did not wait: %v", err)
	case <-time.After(30 * time.Millisecond):
	}
	close(release)
	if got := <-response; got != "finished" {
		t.Fatal(got)
	}
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("shutdown did not finish")
	}
}

type delayedReader struct {
	io.Reader
	started chan struct{}
	resume  chan struct{}
	once    sync.Once
}

func (r *delayedReader) Read(p []byte) (int, error) {
	r.once.Do(func() { close(r.started); <-r.resume })
	return r.Reader.Read(p)
}

func TestRevokedAdministratorCannotFinishPendingMutation(t *testing.T) {
	_, h := testApplication(t)
	firstCookie, first := setupAdmin(t, h)
	w := request(t, h, "POST", "/api/users", `{"username":"second","password":"second-password","role":"Super Admin"}`, firstCookie, 201)
	var second user
	if err := json.Unmarshal(w.Body.Bytes(), &second); err != nil {
		t.Fatal(err)
	}
	w = request(t, h, "POST", "/api/login", `{"username":"second","password":"second-password"}`, nil, 200)
	secondCookie := w.Result().Cookies()[0]
	body := &delayedReader{Reader: strings.NewReader(`{"enabled":false}`), started: make(chan struct{}), resume: make(chan struct{})}
	r := httptest.NewRequest("PATCH", "http://localhost/api/users/"+first.ID, body)
	r.Header.Set("Content-Type", "application/json")
	r.AddCookie(secondCookie)
	result := httptest.NewRecorder()
	done := make(chan struct{})
	go func() { h.ServeHTTP(result, r); close(done) }()
	<-body.started
	request(t, h, "PATCH", "/api/users/"+second.ID, `{"enabled":false}`, firstCookie, 204)
	close(body.resume)
	<-done
	if result.Code != 401 {
		t.Fatalf("revoked admin completed pending mutation: %d", result.Code)
	}
	request(t, h, "GET", "/api/users", "", firstCookie, 200)
}

func TestConcurrentProfileWrites(t *testing.T) {
	a, h := testApplication(t)
	cookie, _ := setupAdmin(t, h)
	var workers sync.WaitGroup
	statuses := make(chan int, 8)
	for range 8 {
		workers.Go(func() {
			r := httptest.NewRequest("POST", "http://localhost/api/connections", strings.NewReader(`{"engine":"PostgreSQL","name":"Concurrent","host":"localhost","port":"5432","username":"postgres","database":"app","ssl":"Prefer"}`))
			r.Header.Set("Content-Type", "application/json")
			r.AddCookie(cookie)
			w := httptest.NewRecorder()
			h.ServeHTTP(w, r)
			statuses <- w.Code
		})
	}
	workers.Wait()
	close(statuses)
	for status := range statuses {
		if status != 201 {
			t.Fatalf("concurrent create returned %d", status)
		}
	}
	var count int
	if err := a.store.db.QueryRow("SELECT count(*) FROM connections").Scan(&count); err != nil {
		t.Fatal(err)
	}
	if count != 8 {
		t.Fatal("concurrent updates were lost")
	}
}
