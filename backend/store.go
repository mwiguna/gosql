package main

import (
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"

	_ "modernc.org/sqlite"
)

// -----------------------------------------------------------------------------
// SQLite storage and bounded account/profile cache
// -----------------------------------------------------------------------------

type storeData struct {
	Version     int                 `json:"version"`
	Users       []storedUser        `json:"users"`
	Connections []connectionProfile `json:"connections"`
}

type store struct {
	mu   sync.Mutex
	path string
	lock *os.File
	db   *sql.DB
	data storeData
}

func openStore(directory string) (*store, error) {
	if err := os.MkdirAll(directory, 0700); err != nil {
		return nil, err
	}
	lock, err := lockStore(filepath.Join(directory, "store.lock"))
	if err != nil {
		return nil, fmt.Errorf("lock data directory: %w", err)
	}
	path, err := filepath.Abs(filepath.Join(directory, "gosql.db"))
	if err != nil {
		lock.Close()
		return nil, err
	}
	uriPath := filepath.ToSlash(path)
	if !strings.HasPrefix(uriPath, "/") {
		uriPath = "/" + uriPath
	}
	options := url.Values{"_pragma": {"foreign_keys(1)", "busy_timeout(5000)", "journal_mode(WAL)", "synchronous(FULL)", "cache_size(-2048)"}}
	uri := url.URL{Scheme: "file", Path: uriPath, RawQuery: options.Encode()}
	db, err := sql.Open("sqlite", uri.String())
	if err != nil {
		lock.Close()
		return nil, err
	}
	db.SetMaxOpenConns(1)
	db.SetMaxIdleConns(1)
	s := &store{path: path, lock: lock, db: db}
	if err = s.initialize(); err == nil {
		err = s.loadAccounts()
	}
	if err == nil {
		err = s.migrateJSON(filepath.Join(directory, "store.json"))
	}
	if err != nil {
		s.close()
		return nil, fmt.Errorf("open application database: %w", err)
	}
	return s, nil
}

func (s *store) close() error {
	return errors.Join(s.db.Close(), s.lock.Close())
}

func (s *store) initialize() error {
	var version int
	if err := s.db.QueryRow("PRAGMA user_version").Scan(&version); err != nil {
		return err
	}
	if version == 1 {
		return nil
	}
	if version != 0 {
		return errors.New("unsupported database version")
	}
	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	_, err = tx.Exec(`
CREATE TABLE users (
 id TEXT PRIMARY KEY, username TEXT NOT NULL UNIQUE,
 role TEXT NOT NULL CHECK(role IN ('User','Super Admin')),
 enabled INTEGER NOT NULL CHECK(enabled IN (0,1)),
 password_algorithm TEXT NOT NULL, password_iterations INTEGER NOT NULL,
 password_salt TEXT NOT NULL, password_hash TEXT NOT NULL
);
CREATE TABLE connections (
 id TEXT PRIMARY KEY, owner_id TEXT NOT NULL REFERENCES users(id) ON DELETE CASCADE,
 engine TEXT NOT NULL, name TEXT NOT NULL, host TEXT NOT NULL, port TEXT NOT NULL,
 username TEXT NOT NULL, database_name TEXT NOT NULL, ssl TEXT NOT NULL, ssh INTEGER NOT NULL,
 ssh_host TEXT NOT NULL, ssh_port TEXT NOT NULL, ssh_user TEXT NOT NULL, ssh_auth TEXT NOT NULL,
 file TEXT NOT NULL, location TEXT NOT NULL
);
CREATE INDEX connections_owner ON connections(owner_id);
CREATE TABLE saved_queries (
 sequence INTEGER PRIMARY KEY AUTOINCREMENT,
 owner_id TEXT NOT NULL REFERENCES users(id) ON DELETE CASCADE,
 id TEXT NOT NULL, name TEXT NOT NULL, sql TEXT NOT NULL,
 connection_id TEXT NOT NULL, database_name TEXT NOT NULL, schema_name TEXT NOT NULL,
 UNIQUE(owner_id,id)
);
CREATE INDEX saved_queries_owner_sequence ON saved_queries(owner_id,sequence DESC);
CREATE TABLE query_history (
 sequence INTEGER PRIMARY KEY AUTOINCREMENT,
 owner_id TEXT NOT NULL REFERENCES users(id) ON DELETE CASCADE,
 id TEXT NOT NULL, sql TEXT NOT NULL, connection_id TEXT NOT NULL,
 database_name TEXT NOT NULL, schema_name TEXT NOT NULL, recorded_at TEXT NOT NULL,
 duration REAL NOT NULL CHECK(duration >= 0), status TEXT NOT NULL, row_count INTEGER NOT NULL CHECK(row_count >= 0),
 UNIQUE(owner_id,id)
);
CREATE INDEX history_owner_sequence ON query_history(owner_id,sequence DESC);
CREATE TABLE workspace_imports (
 owner_id TEXT NOT NULL REFERENCES users(id) ON DELETE CASCADE,
 source TEXT NOT NULL, PRIMARY KEY(owner_id,source)
);
CREATE TABLE store_metadata (key TEXT PRIMARY KEY, value TEXT NOT NULL);
PRAGMA user_version = 1;`)
	if err != nil {
		return err
	}
	return tx.Commit()
}

func (s *store) loadAccounts() error {
	s.data = storeData{Version: 1, Users: []storedUser{}, Connections: []connectionProfile{}}
	rows, err := s.db.Query(`SELECT id,username,role,enabled,password_algorithm,password_iterations,password_salt,password_hash FROM users ORDER BY rowid`)
	if err != nil {
		return err
	}
	for rows.Next() {
		var u storedUser
		if err := rows.Scan(&u.ID, &u.Username, &u.Role, &u.Enabled, &u.Password.Algorithm, &u.Password.Iterations, &u.Password.Salt, &u.Password.Hash); err != nil {
			rows.Close()
			return err
		}
		s.data.Users = append(s.data.Users, u)
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		return err
	}
	rows, err = s.db.Query(`SELECT id,owner_id,engine,name,host,port,username,database_name,ssl,ssh,ssh_host,ssh_port,ssh_user,ssh_auth,file,location FROM connections ORDER BY rowid`)
	if err != nil {
		return err
	}
	defer rows.Close()
	for rows.Next() {
		var c connectionProfile
		if err := rows.Scan(&c.ID, &c.OwnerID, &c.Engine, &c.Name, &c.Host, &c.Port, &c.Username, &c.Database, &c.SSL, &c.SSH, &c.SSHHost, &c.SSHPort, &c.SSHUser, &c.SSHAuth, &c.File, &c.Location); err != nil {
			return err
		}
		s.data.Connections = append(s.data.Connections, c)
	}
	return rows.Err()
}

// Pemanggil memegang mu. Cache diganti hanya setelah transaksi berhasil commit.
func (s *store) save(next storeData) error {
	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if err := s.writeChanges(tx, next); err != nil {
		return err
	}
	if err := tx.Commit(); err != nil {
		return err
	}
	s.data = next
	return nil
}

func (s *store) writeChanges(tx *sql.Tx, next storeData) error {
	users := make(map[string]storedUser, len(s.data.Users))
	profiles := make(map[string]connectionProfile, len(s.data.Connections))
	for _, u := range s.data.Users {
		users[u.ID] = u
	}
	for _, c := range s.data.Connections {
		profiles[c.ID] = c
	}
	for _, u := range next.Users {
		previous, exists := users[u.ID]
		if !exists || previous != u {
			_, err := tx.Exec(`INSERT INTO users VALUES (?,?,?,?,?,?,?,?)
ON CONFLICT(id) DO UPDATE SET username=excluded.username,role=excluded.role,enabled=excluded.enabled,
password_algorithm=excluded.password_algorithm,password_iterations=excluded.password_iterations,
password_salt=excluded.password_salt,password_hash=excluded.password_hash`, u.ID, u.Username, u.Role, u.Enabled, u.Password.Algorithm, u.Password.Iterations, u.Password.Salt, u.Password.Hash)
			if err != nil {
				return err
			}
		}
		delete(users, u.ID)
	}
	for _, c := range next.Connections {
		previous, exists := profiles[c.ID]
		if !exists || previous != c {
			_, err := tx.Exec(`INSERT INTO connections VALUES (?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)
ON CONFLICT(id) DO UPDATE SET owner_id=excluded.owner_id,engine=excluded.engine,name=excluded.name,
host=excluded.host,port=excluded.port,username=excluded.username,database_name=excluded.database_name,
ssl=excluded.ssl,ssh=excluded.ssh,ssh_host=excluded.ssh_host,ssh_port=excluded.ssh_port,
ssh_user=excluded.ssh_user,ssh_auth=excluded.ssh_auth,file=excluded.file,location=excluded.location`, c.ID, c.OwnerID, c.Engine, c.Name, c.Host, c.Port, c.Username, c.Database, c.SSL, c.SSH, c.SSHHost, c.SSHPort, c.SSHUser, c.SSHAuth, c.File, c.Location)
			if err != nil {
				return err
			}
		}
		delete(profiles, c.ID)
	}
	for id := range profiles {
		if _, err := tx.Exec("DELETE FROM connections WHERE id=?", id); err != nil {
			return err
		}
	}
	for id := range users {
		if _, err := tx.Exec("DELETE FROM users WHERE id=?", id); err != nil {
			return err
		}
	}
	return nil
}

func (s *store) snapshot() storeData {
	return storeData{Version: s.data.Version, Users: append([]storedUser{}, s.data.Users...), Connections: append([]connectionProfile{}, s.data.Connections...)}
}

// -----------------------------------------------------------------------------
// One-time legacy migration
// -----------------------------------------------------------------------------

// JSON hanya dibaca untuk migrasi. Sumber dihapus setelah commit; digest mencegah
// impor ganda jika proses berhenti di antara commit dan penghapusan file lama.
func (s *store) migrateJSON(path string) error {
	data, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	digest := sha256.Sum256(data)
	source := hex.EncodeToString(digest[:])
	var imported string
	err = s.db.QueryRow("SELECT value FROM store_metadata WHERE key='legacy_json'").Scan(&imported)
	if err == nil {
		if imported != source {
			return errors.New("legacy JSON changed after migration; refusing to discard data")
		}
		return os.Remove(path)
	}
	if !errors.Is(err, sql.ErrNoRows) {
		return err
	}
	if len(s.data.Users) != 0 || len(s.data.Connections) != 0 {
		return errors.New("both SQLite and unmigrated JSON contain data")
	}
	var legacy storeData
	if err := json.Unmarshal(data, &legacy); err != nil {
		return fmt.Errorf("invalid legacy JSON: %w", err)
	}
	if legacy.Version != 1 {
		return errors.New("unsupported legacy JSON version")
	}
	users, profiles := map[string]bool{}, map[string]bool{}
	activeAdmin := false
	for _, account := range legacy.Users {
		if account.ID == "" || users[account.ID] || account.Username == "" {
			return errors.New("invalid or duplicate legacy account")
		}
		users[account.ID] = true
		activeAdmin = activeAdmin || (account.Enabled && account.Role == "Super Admin")
	}
	if len(users) > 0 && !activeAdmin {
		return errors.New("legacy data has no active administrator")
	}
	for _, profile := range legacy.Connections {
		if profile.ID == "" || profiles[profile.ID] || !users[profile.OwnerID] {
			return errors.New("invalid or duplicate legacy connection")
		}
		profiles[profile.ID] = true
	}
	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if err := s.writeChanges(tx, legacy); err != nil {
		return err
	}
	if _, err := tx.Exec("INSERT INTO store_metadata VALUES ('legacy_json',?)", source); err != nil {
		return err
	}
	if err := tx.Commit(); err != nil {
		return err
	}
	s.data = legacy
	return os.Remove(path)
}
