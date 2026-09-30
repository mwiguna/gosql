package main

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"errors"
	"fmt"
	"net/http"
	"slices"
	"strings"

	sqlitedb "gosql/database/sqlite"
)

type sqliteSavedObject struct {
	name string
	kind string
	sql  string
}

type sqliteRebuildRequest struct {
	table       string
	temporary   string
	previewOnly bool
	previewHash string
}

func sqliteRebuildName(requested string) (string, error) {
	if requested == "" {
		return "__gosql_rebuild_" + strings.ToLower(newID()), nil
	}
	if !strings.HasPrefix(requested, "__gosql_rebuild_") || !sqlitedb.ValidName(requested) {
		return "", errors.New("refresh the structure preview and try again")
	}
	return requested, nil
}

func sqliteRewriteColumn(part string, input tableColumnChange, strict bool) (string, error) {
	tokens, err := sqlitedb.DDLTokens(part)
	if err != nil || len(tokens) < 2 {
		return "", errors.New("unsupported column definition")
	}
	if sqlitedb.DDLName(tokens[0].Text) != input.OldName {
		return "", errors.New("column definition changed; refresh the structure")
	}
	if input.Name != input.OldName {
		return "", errors.New("rename the column separately before changing its definition")
	}
	constraint := len(tokens)
	for i := 1; i < len(tokens); i++ {
		switch strings.ToUpper(tokens[i].Text) {
		case "CONSTRAINT", "PRIMARY", "NOT", "NULL", "UNIQUE", "CHECK", "DEFAULT", "COLLATE", "REFERENCES", "GENERATED", "AS":
			constraint = i
		}
		if constraint != len(tokens) {
			break
		}
	}
	if constraint == 1 {
		return "", errors.New("column type cannot be determined safely")
	}
	columnType := strings.TrimSpace(part[tokens[1].Start:tokens[constraint-1].End])
	if input.Type != "" {
		if !sqliteType(input.Type, strict) {
			return "", errors.New("choose a supported SQLite type")
		}
		columnType = strings.ToUpper(input.Type)
	}
	suffix := ""
	if constraint < len(tokens) {
		keep := []string{}
		for i := constraint; i < len(tokens); i++ {
			word := strings.ToUpper(tokens[i].Text)
			if input.Nullable != nil && word == "NOT" && i+1 < len(tokens) && strings.EqualFold(tokens[i+1].Text, "NULL") {
				i++
				continue
			}
			if input.ChangeDefault && word == "DEFAULT" {
				if i+1 >= len(tokens) {
					return "", errors.New("invalid existing default clause")
				}
				i++
				if (tokens[i].Text == "+" || tokens[i].Text == "-") && i+1 < len(tokens) {
					i++
				}
				continue
			}
			keep = append(keep, tokens[i].Text)
		}
		suffix = strings.Join(keep, " ")
	}
	result := tokens[0].Text + " " + columnType
	if input.Nullable != nil && !*input.Nullable {
		result += " NOT NULL"
	}
	if input.ChangeDefault && input.Default != nil {
		defaultType := "TEXT"
		if strings.Contains(strings.ToUpper(columnType), "BLOB") {
			defaultType = "BLOB"
		}
		newColumn := tableColumnInput{Name: input.Name, Type: defaultType, Nullable: true, Default: input.Default, DefaultExpression: input.DefaultExpression}
		definition, err := sqliteColumnDefinition(newColumn, strict, false)
		if err != nil {
			return "", err
		}
		index := strings.Index(definition, " DEFAULT ")
		if index < 0 {
			return "", errors.New("invalid default clause")
		}
		result += definition[index:]
	}
	if suffix != "" {
		result += " " + suffix
	}
	return result, nil
}

func sqliteRebuildDefinition(meta sqliteMeta, input tableColumnChange, temporary string) (string, error) {
	parts, tail, err := sqlitedb.TableParts(meta.Definition)
	if err != nil {
		return "", err
	}
	found := false
	for i, part := range parts {
		tokens, parseErr := sqlitedb.DDLTokens(part)
		if parseErr != nil || len(tokens) == 0 {
			return "", errors.New("unsupported item in table definition")
		}
		if sqlitedb.DDLName(tokens[0].Text) != input.OldName {
			continue
		}
		parts[i], err = sqliteRewriteColumn(part, input, meta.Strict)
		if err != nil {
			return "", err
		}
		found = true
	}
	if !found {
		return "", errors.New("column definition changed; refresh the structure")
	}
	return "CREATE TABLE main." + sqlitedb.Quote(temporary) + " (" + strings.Join(parts, ", ") + ")" + tail, nil
}

func sqliteSavedObjects(ctx context.Context, conn *sql.Conn, table string) ([]sqliteSavedObject, error) {
	// Rename tabel sementara memvalidasi seluruh schema; view dan trigger disimpan agar dapat dibuat ulang setelah nama asli pulih.
	rows, err := conn.QueryContext(ctx, `SELECT name,type,sql FROM main.sqlite_schema WHERE sql IS NOT NULL AND (type IN ('view','trigger') OR (type='index' AND tbl_name=?)) ORDER BY rowid`, table)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	objects := []sqliteSavedObject{}
	for rows.Next() {
		var item sqliteSavedObject
		if err := rows.Scan(&item.name, &item.kind, &item.sql); err != nil {
			return nil, err
		}
		if strings.HasPrefix(item.name, "sqlite_") {
			continue
		}
		objects = append(objects, item)
	}
	return objects, rows.Err()
}

func (a *application) handleSQLiteColumnRebuild(w http.ResponseWriter, ctx context.Context, conn *sql.Conn, meta sqliteMeta, input tableColumnChange) {
	columnIndex := slices.IndexFunc(meta.Columns, func(column tableColumn) bool { return column.Name == input.OldName })
	if columnIndex < 0 || meta.Columns[columnIndex].Key == "ROWID" || meta.Columns[columnIndex].Hidden || meta.Columns[columnIndex].PrimaryOrder > 0 {
		writeError(w, 400, "sqlite_rebuild_unsafe", "Generated, hidden, rowid, and primary key columns need a separate migration.")
		return
	}
	if input.Name != input.OldName || input.Type != "" && !sqliteType(input.Type, meta.Strict) {
		writeError(w, 400, "invalid_column", "Rename separately and choose a supported SQLite type.")
		return
	}
	temporary, err := sqliteRebuildName(input.RebuildName)
	if err != nil {
		writeError(w, 400, "invalid_rebuild_name", err.Error())
		return
	}
	create, err := sqliteRebuildDefinition(meta, input, temporary)
	if err != nil {
		writeError(w, 400, "sqlite_rebuild_unsafe", err.Error())
		return
	}
	a.runSQLiteRebuild(w, ctx, conn, meta, sqliteRebuildRequest{input.Table, temporary, input.PreviewOnly, input.PreviewHash}, create)
}

func (a *application) runSQLiteRebuild(w http.ResponseWriter, ctx context.Context, conn *sql.Conn, meta sqliteMeta, request sqliteRebuildRequest, create string) {
	if meta.RowID && meta.RowIDName == "" {
		writeError(w, 400, "sqlite_rebuild_unsafe", "This table hides every rowid alias, so its row identities cannot be preserved safely.")
		return
	}
	objects, err := sqliteSavedObjects(ctx, conn, request.table)
	if err != nil {
		writeSQLiteError(w, err)
		return
	}
	columns := []string{}
	if meta.RowIDName != "" {
		columns = append(columns, sqlitedb.Quote(meta.RowIDName))
	}
	for _, column := range meta.Columns {
		if column.Key != "ROWID" && !column.Hidden {
			columns = append(columns, sqlitedb.Quote(column.Name))
		}
	}
	if len(columns) == 0 {
		writeError(w, 400, "sqlite_rebuild_unsafe", "No writable columns can be copied.")
		return
	}
	copySQL := "INSERT INTO main." + sqlitedb.Quote(request.temporary) + " (" + strings.Join(columns, ", ") + ") SELECT " + strings.Join(columns, ", ") + " FROM main." + sqlitedb.Quote(request.table)
	steps := []string{"PRAGMA foreign_keys=OFF", "BEGIN IMMEDIATE", create, copySQL}
	for _, item := range objects {
		if item.kind == "trigger" || item.kind == "view" {
			steps = append(steps, "DROP "+strings.ToUpper(item.kind)+" main."+sqlitedb.Quote(item.name))
		}
	}
	steps = append(steps, "DROP TABLE main."+sqlitedb.Quote(request.table), "ALTER TABLE main."+sqlitedb.Quote(request.temporary)+" RENAME TO "+sqlitedb.Quote(request.table))
	for _, item := range objects {
		steps = append(steps, item.sql)
	}
	steps = append(steps, "PRAGMA foreign_key_check", "PRAGMA integrity_check", "COMMIT", "PRAGMA foreign_keys=ON")
	preview := strings.Join(steps, ";\n") + ";"
	previewHash := fmt.Sprintf("%x", sha256.Sum256([]byte(preview)))
	if request.previewOnly {
		names := make([]string, len(objects))
		for i, item := range objects {
			names[i] = item.kind + " " + item.name
		}
		writeJSON(w, 200, map[string]any{"sql": preview, "objects": names, "rebuildName": request.temporary, "previewHash": previewHash})
		return
	}
	if request.previewHash != "" && request.previewHash != previewHash {
		writeError(w, 409, "sqlite_schema_changed", "The table or dependent objects changed since the preview. Review the change again.")
		return
	}
	// SQLite hanya menerima perubahan foreign_keys di luar transaksi. Pemeriksaan ulang dilakukan sebelum commit.
	if _, err = conn.ExecContext(ctx, "PRAGMA foreign_keys=OFF"); err != nil {
		writeSQLiteError(w, err)
		return
	}
	defer conn.ExecContext(context.Background(), "PRAGMA foreign_keys=ON")
	if _, err = conn.ExecContext(ctx, "BEGIN IMMEDIATE"); err != nil {
		writeSQLiteError(w, err)
		return
	}
	defer conn.ExecContext(context.Background(), "ROLLBACK")
	var currentDefinition string
	if err = conn.QueryRowContext(ctx, "SELECT sql FROM main.sqlite_schema WHERE type='table' AND name=?", request.table).Scan(&currentDefinition); err != nil {
		writeSQLiteError(w, err)
		return
	}
	if currentDefinition != meta.Definition {
		writeError(w, 409, "sqlite_schema_changed", "Table definition changed. Refresh the structure and try again.")
		return
	}
	currentObjects, err := sqliteSavedObjects(ctx, conn, request.table)
	if err != nil {
		writeSQLiteError(w, err)
		return
	}
	if !slices.Equal(objects, currentObjects) {
		writeError(w, 409, "sqlite_schema_changed", "Indexes, views, or triggers changed. Refresh the structure and try again.")
		return
	}
	var originalCount int64
	if err = conn.QueryRowContext(ctx, "SELECT count(*) FROM main."+sqlitedb.Quote(request.table)).Scan(&originalCount); err != nil {
		writeSQLiteError(w, err)
		return
	}
	var sequence sql.NullInt64
	sequenceErr := conn.QueryRowContext(ctx, "SELECT seq FROM main.sqlite_sequence WHERE name=?", request.table).Scan(&sequence)
	if sequenceErr != nil && !errors.Is(sequenceErr, sql.ErrNoRows) && !strings.Contains(sequenceErr.Error(), "no such table") {
		writeSQLiteError(w, sequenceErr)
		return
	}
	for _, statement := range steps[2 : len(steps)-4] {
		if _, err = conn.ExecContext(ctx, statement); err != nil {
			writeSQLiteError(w, err)
			return
		}
	}
	var copiedCount int64
	if err = conn.QueryRowContext(ctx, "SELECT count(*) FROM main."+sqlitedb.Quote(request.table)).Scan(&copiedCount); err != nil {
		writeSQLiteError(w, err)
		return
	}
	if copiedCount != originalCount {
		writeError(w, 409, "sqlite_rebuild_conflict", "Row count changed during the rebuild.")
		return
	}
	if sequence.Valid {
		var current sql.NullInt64
		if err = conn.QueryRowContext(ctx, "SELECT seq FROM main.sqlite_sequence WHERE name=?", request.table).Scan(&current); err != nil {
			writeSQLiteError(w, err)
			return
		}
		if sequence.Int64 > current.Int64 {
			if _, err = conn.ExecContext(ctx, "UPDATE main.sqlite_sequence SET seq=? WHERE name=?", sequence.Int64, request.table); err != nil {
				writeSQLiteError(w, err)
				return
			}
		}
	}
	check, err := conn.QueryContext(ctx, "PRAGMA main.foreign_key_check")
	if err != nil {
		writeSQLiteError(w, err)
		return
	}
	broken := check.Next()
	err = check.Err()
	check.Close()
	if err != nil {
		writeSQLiteError(w, err)
		return
	}
	if broken {
		writeError(w, 400, "sqlite_foreign_key_violation", "The rebuild would violate a foreign key.")
		return
	}
	var integrity string
	if err = conn.QueryRowContext(ctx, "PRAGMA main.integrity_check").Scan(&integrity); err != nil {
		writeSQLiteError(w, err)
		return
	}
	if integrity != "ok" {
		writeError(w, 400, "sqlite_integrity_failed", integrity)
		return
	}
	if _, err = conn.ExecContext(ctx, "COMMIT"); err != nil {
		writeSQLiteError(w, err)
		return
	}
	writeJSON(w, 200, map[string]any{"status": "ok", "sql": preview, "rows": copiedCount})
}
