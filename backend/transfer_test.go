package main

import (
	"bytes"
	"database/sql"
	"encoding/csv"
	"encoding/json"
	"io"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"net/url"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

func transferUpload(t *testing.T, handler http.Handler, path string, data []byte, cookie *http.Cookie, status int) *httptest.ResponseRecorder {
	t.Helper()
	var body bytes.Buffer
	writer := multipart.NewWriter(&body)
	part, err := writer.CreateFormFile("file", "import.dat")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := part.Write(data); err != nil {
		t.Fatal(err)
	}
	if err := writer.Close(); err != nil {
		t.Fatal(err)
	}
	r := httptest.NewRequest("POST", path, &body)
	r.Header.Set("Content-Type", writer.FormDataContentType())
	if cookie != nil {
		r.AddCookie(cookie)
	}
	w := httptest.NewRecorder()
	handler.ServeHTTP(w, r)
	if w.Code != status {
		t.Fatalf("import: expected %d, got %d: %s", status, w.Code, w.Body.String())
	}
	return w
}

func TestSQLiteTransfers(t *testing.T) {
	_, handler := testApplication(t)
	cookie, _ := setupAdmin(t, handler)
	path := filepath.Join(t.TempDir(), "transfer.sqlite")
	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	_, err = db.Exec(`PRAGMA journal_mode=WAL;
		CREATE TABLE "a""b" (id INTEGER PRIMARY KEY, value TEXT, payload BLOB, label TEXT DEFAULT 'default', doubled INT GENERATED ALWAYS AS (id*2) STORED);
		CREATE TABLE destination (id INTEGER PRIMARY KEY, value TEXT, payload BLOB, label TEXT DEFAULT 'default', doubled INT GENERATED ALWAYS AS (id*2) STORED);
		CREATE INDEX value_index ON "a""b"(value);
		CREATE VIEW listing AS SELECT id,value FROM "a""b";
		INSERT INTO "a""b"(id,value,payload) VALUES (1,NULL,X'00FF'),(2,'',X'');`)
	if err != nil {
		t.Fatal(err)
	}
	text := "literal \\N, backslash \\ and \\\\; \"quoted\"\n" + strings.Repeat("é", 700)
	if _, err := db.Exec(`INSERT INTO "a""b"(id,value,payload) VALUES (3,?,NULL)`, text); err != nil {
		t.Fatal(err)
	}
	fields := connectionFields{Engine: "SQLite", Name: "Transfer", File: path, Location: "Local file"}
	created := request(t, handler, "POST", "/api/connections", queryBody(t, fields), cookie, 201)
	var profile connectionProfile
	if err := json.Unmarshal(created.Body.Bytes(), &profile); err != nil {
		t.Fatal(err)
	}
	base := "/api/connections/" + profile.ID
	endpoint := func(table, format string) string {
		return base + "/transfer?" + url.Values{"database": {"transfer"}, "table": {table}, "format": {format}}.Encode()
	}
	request(t, handler, "GET", endpoint(`a"b`, "csv"), "", nil, 401)
	request(t, handler, "GET", endpoint(`a"b`, "csv"), "", cookie, 409)
	request(t, handler, "POST", base+"/catalog", `{"password":""}`, cookie, 200)
	response := request(t, handler, "GET", endpoint(`a"b`, "csv"), "", cookie, 200)
	if !strings.Contains(response.Header().Get("Content-Disposition"), "attachment") {
		t.Fatal("CSV was not a download")
	}
	records, err := csv.NewReader(bytes.NewReader(response.Body.Bytes())).ReadAll()
	if err != nil {
		t.Fatal(err)
	}
	if len(records) != 4 || !slices.Equal(records[0], []string{"id", "value", "payload", "label"}) || records[1][1] != `\N` || records[2][1] != "" || *csvInput(records[3][1]) != text {
		t.Fatalf("CSV lost headers, full values, empty strings, or NULL: %+v", records)
	}
	imported := transferUpload(t, handler, endpoint("destination", "csv"), response.Body.Bytes(), cookie, 200)
	if !strings.Contains(imported.Body.String(), `"rows":3`) {
		t.Fatal(imported.Body.String())
	}
	var gotText string
	var payload []byte
	if err := db.QueryRow(`SELECT value FROM destination WHERE id=3`).Scan(&gotText); err != nil || gotText != text {
		t.Fatalf("text changed: %q, %v", gotText, err)
	}
	if err := db.QueryRow(`SELECT payload FROM destination WHERE id=1`).Scan(&payload); err != nil || !bytes.Equal(payload, []byte{0, 255}) {
		t.Fatalf("binary changed: %x, %v", payload, err)
	}
	transferUpload(t, handler, endpoint("destination", "csv"), []byte("id,value\n999,new\n1,duplicate\n"), cookie, 400)
	var count int
	if err := db.QueryRow("SELECT count(*) FROM destination WHERE id=999").Scan(&count); err != nil || count != 0 {
		t.Fatal("failed CSV import was not rolled back")
	}
	transferUpload(t, handler, endpoint("destination", "csv"), []byte("id,id\n8,9\n"), cookie, 400)
	transferUpload(t, handler, endpoint("destination", "csv"), []byte("missing\nvalue\n"), cookie, 400)
	transferUpload(t, handler, endpoint("listing", "csv"), []byte("id,value\n8,view\n"), cookie, 400)
	transferUpload(t, handler, endpoint("destination", "csv"), nil, cookie, 400)
	request(t, handler, "GET", endpoint("", "csv"), "", cookie, 400)
	request(t, handler, "GET", endpoint("", "sql"), "", cookie, 400)
	request(t, handler, "GET", base+"/transfer?database=other&format=sqlite", "", cookie, 404)
	view := request(t, handler, "GET", endpoint("listing", "csv"), "", cookie, 200)
	if !strings.HasPrefix(view.Body.String(), "id,value\n") {
		t.Fatal("view export failed")
	}
	// A full snapshot includes committed WAL data and can restore a local file.
	backup := request(t, handler, "GET", endpoint("", "sqlite"), "", cookie, 200)
	if !bytes.HasPrefix(backup.Body.Bytes(), []byte("SQLite format 3\x00")) {
		t.Fatal("invalid SQLite snapshot")
	}
	if _, err := db.Exec(`DROP VIEW listing; DROP TABLE "a""b"`); err != nil {
		t.Fatal(err)
	}
	transferUpload(t, handler, endpoint("", "sqlite"), []byte("not a database"), cookie, 400)
	transferUpload(t, handler, endpoint("", "sqlite"), backup.Body.Bytes(), cookie, 200)
	if err := db.QueryRow(`SELECT count(*) FROM "a""b"`).Scan(&count); err != nil || count != 3 {
		t.Fatalf("snapshot restore failed: %d, %v", count, err)
	}
}

func TestCSVHeaderAndNullEncoding(t *testing.T) {
	for _, value := range []string{"", `\N`, `\\N`, `\x00`, "a\nb", `C:\some\file`} {
		encoded := csvValue(&value)
		decoded := csvInput(encoded)
		if decoded == nil || *decoded != value {
			t.Fatalf("CSV value changed: %q -> %q -> %v", value, encoded, decoded)
		}
	}
	if csvInput(csvValue(nil)) != nil {
		t.Fatal("NULL became a string")
	}
	reader, header, err := csvHeader(strings.NewReader("\ufeffvalue\ntext\n"), []string{"value"})
	if err != nil || !slices.Equal(header, []string{"value"}) {
		t.Fatalf("BOM header: %v %v", header, err)
	}
	if _, err := reader.Read(); err != nil && err != io.EOF {
		t.Fatal(err)
	}
}
