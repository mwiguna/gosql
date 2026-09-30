package main

import (
	"context"
	"errors"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"

	sqlitedb "gosql/database/sqlite"
)

var errSQLiteUploadTooLarge = errors.New("SQLite upload exceeds the configured size limit")

func sqliteUpload(w http.ResponseWriter, r *http.Request, directory string, limit int64) (string, error) {
	r.Body = http.MaxBytesReader(w, r.Body, limit+(1<<20))
	reader, err := r.MultipartReader()
	if err != nil {
		return "", errors.New("choose a SQLite database file")
	}
	if err = os.MkdirAll(directory, 0700); err != nil {
		return "", err
	}
	part, err := reader.NextPart()
	if err != nil || part.FormName() != "file" || part.FileName() == "" {
		return "", errors.New("choose a SQLite database file")
	}
	if len(part.FileName()) > 255 || strings.ContainsRune(part.FileName(), 0) {
		return "", errors.New("invalid file name")
	}
	temporary, err := os.CreateTemp(directory, ".sqlite-upload-*")
	if err != nil {
		return "", err
	}
	name := temporary.Name()
	defer func() {
		temporary.Close()
		if err != nil {
			os.Remove(name)
		}
	}()
	var size int64
	size, err = io.Copy(temporary, io.LimitReader(part, limit+1))
	if err != nil {
		var limit *http.MaxBytesError
		if errors.As(err, &limit) {
			err = errSQLiteUploadTooLarge
		}
		return "", err
	}
	if size == 0 {
		err = errors.New("SQLite upload is empty")
		return "", err
	}
	if size > limit {
		err = errSQLiteUploadTooLarge
		return "", err
	}
	if extra, extraErr := reader.NextPart(); extraErr != io.EOF || extra != nil {
		err = errors.New("upload one SQLite file only")
		return "", err
	}
	if err = temporary.Sync(); err != nil {
		return "", err
	}
	if err = temporary.Close(); err != nil {
		return "", err
	}
	ctx, cancel := context.WithTimeout(r.Context(), sqliteTimeout)
	defer cancel()
	if err = sqlitedb.Check(ctx, name); err != nil {
		return "", err
	}
	return name, nil
}

func (a *application) handleSQLiteFile(w http.ResponseWriter, r *http.Request) {
	profile, ok := a.sqliteProfile(w, r)
	if !ok {
		return
	}
	if profile.Location != "Server upload" {
		writeError(w, 400, "sqlite_upload_required", "This connection uses a local server file.")
		return
	}
	path, _ := a.sqlitePath(profile)
	if r.Method == http.MethodPost {
		if !a.beginDatabaseRequest(w) {
			return
		}
		defer func() { <-a.databaseSlots }()
		temporary, err := sqliteUpload(w, r, filepath.Dir(path), a.sqliteUploadLimit)
		if err != nil {
			if errors.Is(err, errSQLiteUploadTooLarge) {
				writeError(w, 413, "sqlite_upload_too_large", err.Error())
			} else {
				writeError(w, 400, "invalid_sqlite_upload", err.Error())
			}
			return
		}
		defer os.Remove(temporary)
		a.mu.Lock()
		for _, session := range a.sessions {
			if active := session.Databases[profile.ID]; active != nil && len(active.busy) > 0 {
				a.mu.Unlock()
				writeError(w, 409, "sqlite_file_busy", "Wait for active SQLite requests before replacing this file.")
				return
			}
		}
		for token, session := range a.sessions {
			session.closeDatabase(profile.ID)
			a.sessions[token] = session
		}
		a.mu.Unlock()
		if _, statErr := os.Stat(path); statErr == nil {
			ctx, cancel := context.WithTimeout(r.Context(), sqliteTimeout)
			oldDB, oldConn, openErr := sqlitedb.Open(ctx, path, false)
			if openErr == nil {
				_, openErr = oldConn.ExecContext(ctx, "PRAGMA wal_checkpoint(TRUNCATE)")
				oldConn.Close()
				oldDB.Close()
			}
			cancel()
			if openErr != nil {
				writeSQLiteError(w, openErr)
				return
			}
		}
		for _, suffix := range []string{"-wal", "-shm"} {
			if removeErr := os.Remove(path + suffix); removeErr != nil && !os.IsNotExist(removeErr) {
				writeSQLiteError(w, removeErr)
				return
			}
		}
		if err = os.Rename(temporary, path); err != nil {
			writeError(w, 500, "sqlite_upload_failed", err.Error())
			return
		}
		writeJSON(w, 200, map[string]string{"status": "ok"})
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), sqliteTimeout)
	defer cancel()
	if err := sqlitedb.Check(ctx, path); err != nil {
		writeSQLiteFileError(w, err)
		return
	}
	db, conn, err := sqlitedb.Open(ctx, path, false)
	if err != nil {
		writeSQLiteError(w, err)
		return
	}
	defer db.Close()
	defer conn.Close()
	file, err := os.CreateTemp(filepath.Dir(path), ".sqlite-snapshot-*")
	if err != nil {
		writeSQLiteError(w, err)
		return
	}
	snapshot := file.Name()
	file.Close()
	os.Remove(snapshot)
	defer os.Remove(snapshot)
	quoted := "'" + strings.ReplaceAll(snapshot, "'", "''") + "'"
	if _, err = conn.ExecContext(ctx, "VACUUM main INTO "+quoted); err != nil {
		writeSQLiteError(w, err)
		return
	}
	result, err := os.Open(snapshot)
	if err != nil {
		writeSQLiteError(w, err)
		return
	}
	defer result.Close()
	filename := strings.Map(func(char rune) rune {
		if char < 32 || char == 127 || char == '"' || char == '\\' {
			return '_'
		}
		return char
	}, filepath.Base(profile.File))
	if filename == "" || filename == "." {
		filename = "database.sqlite"
	}
	w.Header().Set("Content-Type", "application/vnd.sqlite3")
	w.Header().Set("Content-Disposition", "attachment; filename=\""+filename+"\"")
	http.ServeContent(w, r, filename, time.Now(), result)
}
