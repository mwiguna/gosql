package main

import (
	"context"
	"errors"
	"os"

	sqlitedb "gosql/database/sqlite"
	sqlite "modernc.org/sqlite"
)

func transferSQLite(ctx context.Context, path string, file *os.File, importing bool) error {
	var err error
	if importing {
		if err := sqlitedb.Check(ctx, file.Name()); err != nil {
			return err
		}
	}
	db, conn, err := sqlitedb.Open(ctx, path, !importing)
	if err != nil {
		return err
	}
	defer db.Close()
	defer conn.Close()
	return conn.Raw(func(raw any) (result error) {
		driver, ok := raw.(interface {
			NewBackup(string) (*sqlite.Backup, error)
			NewRestore(string) (*sqlite.Backup, error)
		})
		if !ok {
			return errors.New("SQLite backup is not supported by this driver")
		}
		var backup *sqlite.Backup
		if importing {
			backup, err = driver.NewRestore(file.Name())
		} else {
			backup, err = driver.NewBackup(file.Name())
		}
		if err != nil {
			return err
		}
		defer func() {
			if err := backup.Finish(); result == nil {
				result = err
			}
		}()
		for more := true; more; {
			if err := ctx.Err(); err != nil {
				return err
			}
			more, err = backup.Step(128)
			if err != nil {
				return err
			}
		}
		return nil
	})
}
