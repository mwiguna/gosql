package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"mime"
	"net/http"
	"os"
	"strings"
	"time"
)

const transferLimit = 256 << 20
const transferTimeout = 10 * time.Minute

type transferTarget struct {
	Database string
	Schema   string
	Table    string
	Format   string
}

func (a *application) handleTransfer(w http.ResponseWriter, r *http.Request) {
	actor, ok := a.requireUser(w, r)
	if !ok || !a.beginDatabaseRequest(w) {
		return
	}
	defer func() { <-a.databaseSlots }()
	session := a.getDatabaseSession(w, r, actor)
	if session == nil {
		return
	}
	q := r.URL.Query()
	target := transferTarget{q.Get("database"), q.Get("schema"), q.Get("table"), q.Get("format")}
	if !session.hasDatabase(target.Database) {
		writeError(w, 404, "database_not_available", "Choose an available database.")
		return
	}
	engine := session.fields.Engine
	if strings.ContainsRune(target.Schema+target.Table, 0) || len(target.Schema) > 255 || len(target.Table) > 255 ||
		(target.Format != "csv" && target.Format != "dump" && target.Format != "sql" && target.Format != "sqlite") ||
		(target.Format == "csv" && target.Table == "") || (target.Table != "" && engine == "PostgreSQL" && target.Schema == "") ||
		(engine != "PostgreSQL" && target.Schema != "") ||
		(target.Format == "dump" && engine != "PostgreSQL") ||
		(target.Format == "sql" && engine != "PostgreSQL" && engine != "MySQL" && engine != "MariaDB") ||
		(target.Format == "sqlite" && (engine != "SQLite" || target.Table != "")) {
		writeError(w, 400, "invalid_transfer", "Choose a supported format and target table or database.")
		return
	}
	if r.Method == http.MethodPost && target.Format != "csv" && target.Table != "" {
		writeError(w, 400, "invalid_transfer", "Restore native table backups from the database menu; use CSV to append data to a table.")
		return
	}
	if r.Method == http.MethodPost && (engine == "MySQL" || engine == "MariaDB") && rejectMySQLSystemDDL(w, target.Database) {
		return
	}
	select {
	case session.busy <- struct{}{}:
		defer func() { <-session.busy }()
	default:
		writeError(w, 429, "database_busy", "Wait for active database requests, then try again.")
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), transferTimeout)
	defer cancel()
	stop := context.AfterFunc(session.ctx, cancel)
	defer stop()
	sqlitePath := ""
	if engine == "SQLite" {
		var err error
		sqlitePath, err = a.sqlitePath(connectionProfile{ID: r.PathValue("id"), OwnerID: actor.ID, connectionFields: session.fields})
		if err != nil {
			writeSQLiteFileError(w, err)
			return
		}
	}
	controller := http.NewResponseController(w)
	_ = controller.SetReadDeadline(time.Now().Add(transferTimeout))
	_ = controller.SetWriteDeadline(time.Now().Add(transferTimeout))
	file, err := os.CreateTemp("", "gosql-transfer-*")
	if err != nil {
		writeError(w, 500, "transfer_failed", "Could not create a temporary transfer file.")
		return
	}
	defer os.Remove(file.Name())
	defer file.Close()
	if r.Method == http.MethodPost {
		if err = readTransferUpload(w, r, file); err != nil {
			var tooLarge *http.MaxBytesError
			status := 400
			if errors.As(err, &tooLarge) || errors.Is(err, errTransferTooLarge) {
				status = 413
			}
			writeError(w, status, "invalid_import", err.Error())
			return
		}
	}
	var imported int64
	if target.Format == "csv" {
		imported, err = transferCSV(ctx, session, target, sqlitePath, file, r.Method == http.MethodPost)
	} else if engine == "SQLite" {
		err = transferSQLite(ctx, sqlitePath, file, r.Method == http.MethodPost)
	} else {
		err = nativeTransfer(ctx, session, target, file, r.Method == http.MethodPost)
	}
	if err != nil {
		// Native tools must never return a password in their diagnostics.
		message := err.Error()
		if session.password != "" {
			message = strings.ReplaceAll(message, session.password, "[redacted]")
		}
		writeError(w, 400, "transfer_failed", message)
		return
	}
	if r.Method == http.MethodPost {
		writeJSON(w, 200, map[string]any{"status": "ok", "rows": imported})
		return
	}
	filename := target.Database
	if target.Table != "" {
		filename = target.Table
	}
	filename = strings.Map(func(c rune) rune {
		if c < 32 || strings.ContainsRune(`/\\:"`, c) {
			return '_'
		}
		return c
	}, filename) + "." + target.Format
	w.Header().Set("Content-Type", "application/octet-stream")
	if target.Format == "csv" {
		w.Header().Set("Content-Type", "text/csv; charset=utf-8")
	}
	w.Header().Set("Content-Disposition", mime.FormatMediaType("attachment", map[string]string{"filename": filename}))
	http.ServeContent(w, r, filename, time.Now(), file)
}

var errTransferTooLarge = errors.New("import file exceeds 256 MB")

func readTransferUpload(w http.ResponseWriter, r *http.Request, file *os.File) error {
	r.Body = http.MaxBytesReader(w, r.Body, transferLimit+(1<<20))
	reader, err := r.MultipartReader()
	if err != nil {
		return errors.New("choose one import file")
	}
	part, err := reader.NextPart()
	if err != nil || part.FormName() != "file" || part.FileName() == "" {
		return errors.New("choose one import file")
	}
	size, err := io.Copy(file, io.LimitReader(part, transferLimit+1))
	if err != nil {
		return err
	}
	if size > transferLimit {
		return errTransferTooLarge
	}
	if size == 0 {
		return errors.New("import file is empty")
	}
	if _, err = reader.NextPart(); err != io.EOF {
		return errors.New("upload one file only")
	}
	_, err = file.Seek(0, io.SeekStart)
	return err
}

func transferError(row int64, err error) error {
	return fmt.Errorf("import stopped at CSV row %d: %w", row+2, err)
}
