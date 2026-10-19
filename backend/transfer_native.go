package main

import (
	"context"
	"fmt"
	"io"
	"net"
	"net/url"
	"os"
	"os/exec"
	"strings"
)

type transferDiagnostics struct{ text []byte }

func (b *transferDiagnostics) Write(p []byte) (int, error) {
	n := len(p)
	if remaining := (32 << 10) - len(b.text); remaining > 0 {
		b.text = append(b.text, p[:min(len(p), remaining)]...)
	}
	return n, nil
}

func nativeTransfer(ctx context.Context, session *databaseSession, target transferTarget, file *os.File, importing bool) error {
	fields := session.fields
	var tool string
	var args, env []string
	if fields.Engine == "PostgreSQL" {
		address := (&url.URL{Scheme: "postgresql", Host: net.JoinHostPort(fields.Host, fields.Port), Path: "/" + target.Database}).String()
		tool = "pg_dump"
		format := "plain"
		if target.Format == "dump" {
			format = "custom"
		}
		args = []string{"--dbname=" + address, "--format=" + format, "--no-owner", "--no-privileges", "--no-password"}
		if target.Table != "" {
			args = append(args, "--strict-names", "--table="+`"`+strings.ReplaceAll(target.Schema, `"`, `""`)+`"."`+strings.ReplaceAll(target.Table, `"`, `""`)+`"`)
		}
		if importing {
			tool = "pg_restore"
			args = []string{"--dbname=" + address, "--no-owner", "--no-privileges", "--no-password", "--single-transaction", "--exit-on-error"}
			if target.Format == "sql" {
				tool = "psql"
				args = []string{"--dbname=" + address, "--no-psqlrc", "--no-password", "--single-transaction", "--set=ON_ERROR_STOP=on", "--file=-"}
				prepared, err := preparePostgresSQL(file)
				if err != nil {
					return err
				}
				defer os.Remove(prepared.Name())
				defer prepared.Close()
				file = prepared
			}
		}
		ssl := map[string]string{"Disable": "disable", "Prefer": "prefer", "Require": "require", "Verify CA": "verify-ca", "Verify Full": "verify-full"}[fields.SSL]
		env = []string{"PGHOST=" + fields.Host, "PGPORT=" + fields.Port, "PGUSER=" + fields.Username, "PGPASSWORD=" + session.password, "PGDATABASE=" + target.Database, "PGSSLMODE=" + ssl, "PGCONNECT_TIMEOUT=30"}
	} else {
		tool = "mysqldump"
		if fields.Engine == "MariaDB" {
			tool = "mariadb-dump"
		}
		args = []string{"--no-defaults", "--host=" + fields.Host, "--port=" + fields.Port, "--user=" + fields.Username, "--default-character-set=utf8mb4"}
		if fields.Engine == "MariaDB" {
			if fields.SSL == "Disable" {
				args = append(args, "--skip-ssl")
			} else {
				args = append(args, "--ssl")
			}
			if fields.SSL == "Verify Full" || fields.SSL == "Verify CA" {
				args = append(args, "--ssl-verify-server-cert")
			}
		} else {
			ssl := map[string]string{"Disable": "DISABLED", "Prefer": "PREFERRED", "Require": "REQUIRED", "Verify CA": "VERIFY_CA", "Verify Full": "VERIFY_IDENTITY"}[fields.SSL]
			args = append(args, "--ssl-mode="+ssl)
		}
		if importing {
			tool = "mysql"
			if fields.Engine == "MariaDB" {
				tool = "mariadb"
			}
			// Binary batch mode disables local commands such as system/source.
			args = append(args, "--binary-mode", "--batch", "--local-infile=0", "--database="+target.Database)
		} else {
			args = append(args, "--single-transaction", "--quick", "--hex-blob", "--no-tablespaces", "--skip-lock-tables", "--skip-add-drop-table")
			if fields.Engine == "MySQL" {
				args = append(args, "--set-gtid-purged=OFF", "--column-statistics=0")
			}
			if target.Table == "" {
				args = append(args, "--routines", "--events")
			}
			args = append(args, "--", target.Database)
			if target.Table != "" {
				args = append(args, target.Table)
			}
		}
		env = []string{"MYSQL_PWD=" + session.password}
	}
	path, err := exec.LookPath(tool)
	if err != nil {
		return fmt.Errorf("%s is required on the GoSQL machine. Install the database client tools and add their bin folder to PATH", tool)
	}
	cmd := exec.CommandContext(ctx, path, args...)
	configureTransferCommand(cmd)
	cmd.Env = append(os.Environ(), env...)
	var diagnostics transferDiagnostics
	cmd.Stderr = &diagnostics
	if importing {
		cmd.Stdin = file
		cmd.Stdout = io.Discard
	} else {
		cmd.Stdout = file
	}
	if err = cmd.Run(); err != nil {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		return fmt.Errorf("%s failed: %s", tool, strings.TrimSpace(string(diagnostics.text)))
	}
	return nil
}
