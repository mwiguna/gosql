package sqlite

import (
	"context"
	"database/sql"
	"errors"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"unicode/utf8"
)

func ValidName(name string) bool {
	return name != "" && len(name) <= 255 && utf8.ValidString(name) && !strings.ContainsRune(name, 0)
}

func Quote(name string) string { return `"` + strings.ReplaceAll(name, `"`, `""`) + `"` }

func Affinity(declared string) string {
	typeName := strings.ToUpper(declared)
	switch {
	case strings.Contains(typeName, "INT"):
		return "INTEGER"
	case strings.Contains(typeName, "CHAR"), strings.Contains(typeName, "CLOB"), strings.Contains(typeName, "TEXT"):
		return "TEXT"
	case typeName == "" || strings.Contains(typeName, "BLOB"):
		return "BLOB"
	case strings.Contains(typeName, "REAL"), strings.Contains(typeName, "FLOA"), strings.Contains(typeName, "DOUB"):
		return "REAL"
	default:
		return "NUMERIC"
	}
}

func DefaultSource(value string) string {
	trimmed := strings.TrimSpace(value)
	upper := strings.ToUpper(trimmed)
	if upper == "NULL" || upper == "TRUE" || upper == "FALSE" || strings.HasPrefix(trimmed, "'") || strings.HasPrefix(trimmed, `"`) || strings.HasPrefix(upper, "X'") {
		return "literal"
	}
	if _, err := strconv.ParseFloat(trimmed, 64); err == nil {
		return "literal"
	}
	return "expression"
}

func Open(ctx context.Context, path string, readOnly bool) (*sql.DB, *sql.Conn, error) {
	mode := "rw"
	if readOnly {
		mode = "ro"
	}
	options := url.Values{"mode": {mode}, "_pragma": {"foreign_keys(1)", "busy_timeout(3000)", "trusted_schema(0)"}}
	uri := url.URL{Scheme: "file", Path: filepath.ToSlash(path), RawQuery: options.Encode()}
	db, err := sql.Open("sqlite", uri.String())
	if err != nil {
		return nil, nil, err
	}
	db.SetMaxOpenConns(1)
	db.SetMaxIdleConns(0)
	conn, err := db.Conn(ctx)
	if err != nil {
		db.Close()
		return nil, nil, err
	}
	if err := conn.PingContext(ctx); err != nil {
		conn.Close()
		db.Close()
		return nil, nil, err
	}
	return db, conn, nil
}

func Check(ctx context.Context, path string) error {
	file, err := os.Open(path)
	if err != nil {
		return err
	}
	header := make([]byte, 16)
	_, err = file.Read(header)
	file.Close()
	if err != nil || string(header) != "SQLite format 3\x00" {
		return errors.New("file is not a SQLite database")
	}
	db, conn, err := Open(ctx, path, true)
	if err != nil {
		return err
	}
	defer db.Close()
	defer conn.Close()
	var result string
	if err = conn.QueryRowContext(ctx, "PRAGMA quick_check").Scan(&result); err != nil {
		return err
	}
	if result != "ok" {
		return errors.New("SQLite integrity check failed: " + result)
	}
	return nil
}
