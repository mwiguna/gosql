package mysql

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"database/sql"
	"errors"
	"net"
	"strconv"
	"strings"
	"time"

	driver "github.com/go-sql-driver/mysql"
)

type Config struct{ Host, Port, Username, SSL string }

const RequestTimeout = 60 * time.Second

var ErrMariaDBVersionUnsupported = errors.New("MariaDB 10.5.10 or newer is required")
var ErrMySQLVersionUnsupported = errors.New("MySQL 8.0.16 or newer is required")
var ErrMariaDBProfileRequired = errors.New("This server is MariaDB. Select MariaDB as the connection engine")
var ErrMySQLProfileRequired = errors.New("This server is MySQL. Select MySQL as the connection engine")

func tlsConfig(mode, host string) (*tls.Config, string, error) {
	switch mode {
	case "Disable":
		return nil, "false", nil
	case "Prefer":
		return nil, "preferred", nil
	case "Require":
		return nil, "skip-verify", nil
	case "Verify Full":
		return &tls.Config{MinVersion: tls.VersionTLS12, ServerName: host}, "", nil
	case "Verify CA":
		roots, err := x509.SystemCertPool()
		if err != nil {
			return nil, "", err
		}
		return &tls.Config{MinVersion: tls.VersionTLS12, InsecureSkipVerify: true, VerifyConnection: func(state tls.ConnectionState) error {
			if len(state.PeerCertificates) == 0 {
				return errors.New("server did not provide a certificate")
			}
			intermediate := x509.NewCertPool()
			for _, certificate := range state.PeerCertificates[1:] {
				intermediate.AddCert(certificate)
			}
			_, err := state.PeerCertificates[0].Verify(x509.VerifyOptions{Roots: roots, Intermediates: intermediate})
			return err
		}}, "", nil
	}
	return nil, "", errors.New("unsupported SSL mode")
}

func Open(ctx context.Context, fields Config, password, database string, multiStatements bool) (*sql.DB, *sql.Conn, error) {
	tlsConfig, tlsMode, err := tlsConfig(fields.SSL, fields.Host)
	if err != nil {
		return nil, nil, err
	}
	config := driver.NewConfig()
	config.User = fields.Username
	config.Passwd = password
	config.Net = "tcp"
	config.Addr = net.JoinHostPort(fields.Host, fields.Port)
	config.DBName = database
	config.ParseTime = false
	config.MultiStatements = multiStatements
	config.ClientFoundRows = true
	config.TLSConfig = tlsMode
	config.TLS = tlsConfig
	config.Timeout = RequestTimeout
	config.ReadTimeout = RequestTimeout
	config.WriteTimeout = RequestTimeout
	connector, err := driver.NewConnector(config)
	if err != nil {
		return nil, nil, err
	}
	db := sql.OpenDB(connector)
	db.SetMaxOpenConns(1)
	db.SetMaxIdleConns(0)
	conn, err := db.Conn(ctx)
	if err != nil {
		db.Close()
		return nil, nil, err
	}
	if _, err = conn.ExecContext(ctx, "SET SESSION time_zone = '+00:00', lock_wait_timeout = 60"); err != nil {
		conn.Close()
		db.Close()
		return nil, nil, err
	}
	return db, conn, nil
}

func ReadServerVersion(ctx context.Context, conn *sql.Conn) (bool, int, int, int, error) {
	var version string
	if err := conn.QueryRowContext(ctx, "SELECT VERSION()").Scan(&version); err != nil {
		return false, 0, 0, 0, err
	}
	return ParseServerVersion(version)
}

func ParseServerVersion(version string) (bool, int, int, int, error) {
	mariaDB := strings.Contains(strings.ToLower(version), "mariadb")
	if mariaDB {
		version = strings.TrimPrefix(version, "5.5.5-")
	}
	parts := strings.SplitN(version, ".", 3)
	major, err := strconv.Atoi(parts[0])
	if err != nil || len(parts) < 3 {
		return mariaDB, 0, 0, 0, ErrMySQLVersionUnsupported
	}
	minor, minorErr := strconv.Atoi(parts[1])
	patchDigits := strings.SplitN(parts[2], "-", 2)[0]
	patchDigits = strings.SplitN(patchDigits, ".", 2)[0]
	patch, patchErr := strconv.Atoi(patchDigits)
	if minorErr != nil || patchErr != nil {
		return mariaDB, 0, 0, 0, ErrMySQLVersionUnsupported
	}
	if mariaDB {
		if major < 10 || major == 10 && minor < 5 || major == 10 && minor == 5 && patch < 10 {
			return true, 0, 0, 0, ErrMariaDBVersionUnsupported
		}
	} else if major < 8 || major == 8 && minor == 0 && patch < 16 {
		return false, 0, 0, 0, ErrMySQLVersionUnsupported
	}
	return mariaDB, major, minor, patch, nil
}

func ReadDatabases(ctx context.Context, conn *sql.Conn, engine string) ([]string, error) {
	mariaDB, _, _, _, err := ReadServerVersion(ctx, conn)
	if err != nil {
		return nil, err
	}
	if err = ServerProfileError(mariaDB, engine); err != nil {
		return nil, err
	}
	rows, err := conn.QueryContext(ctx, "SHOW DATABASES")
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	names := []string{}
	for rows.Next() {
		var name string
		if err := rows.Scan(&name); err != nil {
			return nil, err
		}
		names = append(names, name)
	}
	return names, rows.Err()
}

func ServerProfileError(mariaDB bool, engine string) error {
	if mariaDB && engine == "MySQL" {
		return ErrMariaDBProfileRequired
	}
	if !mariaDB && engine == "MariaDB" {
		return ErrMySQLProfileRequired
	}
	return nil
}
