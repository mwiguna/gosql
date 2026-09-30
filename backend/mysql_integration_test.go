package main

import (
	"context"
	"database/sql"
	"encoding/json"
	"net"
	"os"
	"strings"
	"testing"
	"time"

	mysql "github.com/go-sql-driver/mysql"

	mysqldb "gosql/database/mysql"
)

func TestMySQLIntegration(t *testing.T) {
	dsn := os.Getenv("GOSQL_TEST_MYSQL_DSN")
	if dsn == "" {
		t.Skip("set GOSQL_TEST_MYSQL_DSN to a dedicated MySQL protocol test database")
	}
	config, err := mysql.ParseDSN(dsn)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasSuffix(config.DBName, "_gosql_test") {
		t.Fatal("integration DSN database must end with _gosql_test")
	}
	config.ParseTime = false
	config.MultiStatements = false
	connector, err := mysql.NewConnector(config)
	if err != nil {
		t.Fatal(err)
	}
	db := sql.OpenDB(connector)
	defer db.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	conn, err := db.Conn(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	if _, err = conn.ExecContext(ctx, "SET SESSION time_zone = '+00:00', lock_wait_timeout = 60"); err != nil {
		t.Fatal(err)
	}
	if _, err = mysqldb.ReadDatabases(ctx, conn, ""); err != nil {
		t.Fatal(err)
	}
	name := "gosql_probe_" + time.Now().Format("150405")
	table := mysqlTableName(config.DBName, name)
	if _, err = conn.ExecContext(ctx, "CREATE TABLE "+table+" (`id` BIGINT NOT NULL AUTO_INCREMENT PRIMARY KEY,`amount` DECIMAL(30,10) NOT NULL,`note` TEXT NULL,`bytes` BLOB NULL,`payload` JSON NULL,`optional_date` DATE NULL,`updated` TIMESTAMP NOT NULL DEFAULT CURRENT_TIMESTAMP,`score` DOUBLE NULL,`flags` BIT(8) NULL) ENGINE=InnoDB"); err != nil {
		t.Fatal(err)
	}
	defer conn.ExecContext(context.Background(), "DROP TABLE "+table)
	if _, err = conn.ExecContext(ctx, "INSERT INTO "+table+" (amount,note,bytes,payload,optional_date) VALUES (?,?,?,?,?)", "9007199254740993.1234567890", strings.Repeat("a", 600), []byte{0, 255}, `{"key":"value"}`, nil); err != nil {
		t.Fatal(err)
	}
	page, err := readMySQLPage(ctx, conn, config.DBName, name, 1, 20, "")
	if err != nil {
		t.Fatal(err)
	}
	if len(page.Rows) != 1 || len(page.Versions) != 1 || len(page.Versions[0]) != 64 || !page.Editable || !page.CursorPaging {
		t.Fatalf("unexpected MySQL page metadata: %+v", page)
	}
	var amount, blob, date *string
	for i, column := range page.Columns {
		switch column.Name {
		case "amount":
			amount = page.Rows[0][i]
		case "bytes":
			blob = page.Rows[0][i]
		case "optional_date":
			date = page.Rows[0][i]
		}
	}
	if amount == nil || *amount != "9007199254740993.1234567890" || blob == nil || *blob != `\x00FF` || date != nil {
		t.Fatalf("value conversion changed: amount=%v blob=%v date=%v", amount, blob, date)
	}
	if _, err = conn.ExecContext(ctx, "UPDATE "+table+" SET `note`='changed' WHERE `id`=1"); err != nil {
		t.Fatal(err)
	}
	changed, err := readMySQLPage(ctx, conn, config.DBName, name, 1, 20, "")
	if err != nil || changed.Versions[0] == page.Versions[0] {
		t.Fatalf("non-edited column did not change row token: %v", err)
	}
	if _, err = conn.ExecContext(ctx, "UPDATE "+table+" SET `id`=9007199254740993 WHERE `id`=1"); err != nil {
		t.Fatal(err)
	}
	if _, err = conn.ExecContext(ctx, "INSERT INTO "+table+" (id,amount) VALUES (?,?)", "9007199254740994", "1.0000000000"); err != nil {
		t.Fatal(err)
	}
	first, err := readMySQLPage(ctx, conn, config.DBName, name, 1, 1, "")
	if err != nil || !first.HasMore || first.NextCursor == "" {
		t.Fatalf("first BIGINT page: %+v, %v", first, err)
	}
	second, err := readMySQLPage(ctx, conn, config.DBName, name, 2, 1, first.NextCursor)
	if err != nil || len(second.Rows) != 1 || *second.Rows[0][0] != "9007199254740994" {
		t.Fatalf("BIGINT keyset lost precision: %+v, %v", second, err)
	}
}

func TestMariaDBAPIIntegration(t *testing.T) {
	dsn := os.Getenv("GOSQL_TEST_MARIADB_DSN")
	if dsn == "" {
		t.Skip("set GOSQL_TEST_MARIADB_DSN to a dedicated MariaDB test database")
	}
	config, err := mysql.ParseDSN(dsn)
	if err != nil || !strings.HasSuffix(config.DBName, "_gosql_test") {
		t.Fatal("MariaDB integration DSN must use a database ending in _gosql_test")
	}
	host, port, err := net.SplitHostPort(config.Addr)
	if err != nil {
		t.Fatal(err)
	}
	db, err := sql.Open("mysql", dsn)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	var version string
	if err = db.QueryRow("SELECT VERSION()").Scan(&version); err != nil || !strings.Contains(strings.ToLower(version), "mariadb") {
		t.Fatalf("dedicated test server must be MariaDB: %q %v", version, err)
	}
	table := "gosql_maria_" + time.Now().Format("150405")
	child := table + "_child"
	t.Cleanup(func() { _, _ = db.Exec("DROP TABLE IF EXISTS " + mysqlTableName(config.DBName, table)) })
	t.Cleanup(func() { _, _ = db.Exec("DROP TABLE IF EXISTS " + mysqlTableName(config.DBName, child)) })
	_, handler := testApplication(t)
	cookie, _ := setupAdmin(t, handler)
	profileBody, _ := json.Marshal(connectionFields{Engine: "MariaDB", Name: "MariaDB test", Host: host, Port: port, Username: config.User, SSL: "Disable"})
	created := request(t, handler, "POST", "/api/connections", string(profileBody), cookie, 201)
	var profile connectionProfile
	if err = json.Unmarshal(created.Body.Bytes(), &profile); err != nil {
		t.Fatal(err)
	}
	base := "/api/connections/" + profile.ID
	password, _ := json.Marshal(map[string]string{"password": config.Passwd})
	request(t, handler, "POST", base+"/catalog", string(password), cookie, 200)
	marshal := func(value any) string { data, _ := json.Marshal(value); return string(data) }
	pointer := func(value string) *string { return &value }
	boolPointer := func(value bool) *bool { return &value }
	location := map[string]any{"database": config.DBName, "table": table}
	request(t, handler, "POST", base+"/tables", marshal(tableChange{Database: config.DBName, Table: table, Columns: []tableColumnInput{
		{Name: "id", Type: "bigint", Primary: true, AutoIncrement: true},
		{Name: "label", Type: "varchar(30)", Default: pointer("hello")},
		{Name: "amount", Type: "decimal(10,2)", Default: pointer("1.50")},
		{Name: "note", Type: "text", Nullable: true},
	}}), cookie, 200)
	pagePath := base + "/rows?database=" + config.DBName + "&table=" + table + "&page=1&pageSize=20"
	pageResponse := request(t, handler, "GET", pagePath, "", cookie, 200)
	var page tablePage
	if err = json.Unmarshal(pageResponse.Body.Bytes(), &page); err != nil {
		t.Fatal(err)
	}
	if !page.Editable || page.Columns[1].Default == nil || *page.Columns[1].Default != "hello" || !page.Columns[1].DefinitionEditable {
		t.Fatalf("MariaDB column metadata lost a literal default: %+v", page.Columns)
	}
	request(t, handler, "POST", base+"/indexes", marshal(map[string]any{"database": config.DBName, "table": table, "name": "by_label", "columns": []string{"label"}}), cookie, 200)
	request(t, handler, "POST", base+"/constraints", marshal(map[string]any{"database": config.DBName, "table": table, "name": "amount_nonnegative", "type": "CHECK", "expression": "amount >= 0"}), cookie, 200)
	query := request(t, handler, "POST", base+"/query", marshal(map[string]string{"database": config.DBName, "sql": "SELECT 1 AS first_value; SELECT 2 AS last_value"}), cookie, 200)
	if !strings.Contains(query.Body.String(), `"last_value":"2"`) {
		t.Fatalf("MariaDB console did not return the final result set: %s", query.Body.String())
	}
	insert := request(t, handler, "POST", base+"/rows/insert", marshal(map[string]any{"database": config.DBName, "table": table, "values": map[string]any{}}), cookie, 200)
	var inserted struct {
		Row     []*string `json:"row"`
		Version string    `json:"version"`
	}
	if err = json.Unmarshal(insert.Body.Bytes(), &inserted); err != nil || len(inserted.Row) != 4 || inserted.Row[1] == nil || *inserted.Row[1] != "hello" || len(inserted.Version) != 64 {
		t.Fatalf("MariaDB quick insert returned unexpected row: %s %v", insert.Body.String(), err)
	}
	key := tableRowKey{Values: []string{*inserted.Row[0]}, Version: inserted.Version}
	request(t, handler, "POST", base+"/rows/edit", marshal(tableMutation{Database: config.DBName, Table: table, Column: "label", Value: pointer("updated"), Row: key}), cookie, 200)
	pageResponse = request(t, handler, "GET", pagePath, "", cookie, 200)
	if err = json.Unmarshal(pageResponse.Body.Bytes(), &page); err != nil || len(page.Rows) != 1 || len(page.Versions[0]) != 64 {
		t.Fatalf("could not refresh row identity before primary key edit: %s %v", pageResponse.Body.String(), err)
	}
	key = tableRowKey{Values: []string{*page.Rows[0][0]}, Version: page.Versions[0]}
	request(t, handler, "POST", base+"/rows/edit", marshal(tableMutation{Database: config.DBName, Table: table, Column: "id", Value: pointer("1000"), Row: key}), cookie, 200)
	pageResponse = request(t, handler, "GET", pagePath, "", cookie, 200)
	if err = json.Unmarshal(pageResponse.Body.Bytes(), &page); err != nil || len(page.Rows) != 1 || page.Rows[0][0] == nil || *page.Rows[0][0] != "1000" {
		t.Fatalf("primary key edit did not persist: %s %v", pageResponse.Body.String(), err)
	}
	request(t, handler, "PATCH", base+"/table-columns", marshal(tableColumnChange{Database: config.DBName, Table: table, Name: "label", OldName: "label", Nullable: boolPointer(false), Default: pointer("new"), ChangeDefault: true}), cookie, 200)
	request(t, handler, "PATCH", base+"/indexes", marshal(map[string]any{"database": config.DBName, "table": table, "oldName": "by_label", "name": "by_amount", "columns": []string{"amount"}}), cookie, 200)
	request(t, handler, "PATCH", base+"/constraints", marshal(map[string]any{"database": config.DBName, "table": table, "oldName": "amount_nonnegative", "name": "amount_nonnegative", "type": "CHECK", "expression": "amount > -1"}), cookie, 200)
	request(t, handler, "POST", base+"/tables", marshal(tableChange{Database: config.DBName, Table: child, Columns: []tableColumnInput{
		{Name: "id", Type: "bigint", Primary: true, AutoIncrement: true},
		{Name: "parent_id", Type: "bigint", Nullable: true},
	}}), cookie, 200)
	request(t, handler, "POST", base+"/constraints", marshal(map[string]any{"database": config.DBName, "table": child, "name": "parent_fk", "type": "FOREIGN KEY", "columns": []string{"parent_id"}, "referenceTable": table, "referenceColumns": []string{"id"}}), cookie, 200)
	request(t, handler, "DELETE", base+"/constraints", marshal(map[string]any{"database": config.DBName, "table": child, "name": "parent_fk"}), cookie, 200)
	request(t, handler, "DELETE", base+"/tables", marshal(map[string]any{"database": config.DBName, "table": child}), cookie, 200)
	request(t, handler, "DELETE", base+"/constraints", marshal(map[string]any{"database": config.DBName, "table": table, "name": "amount_nonnegative"}), cookie, 200)
	request(t, handler, "DELETE", base+"/indexes", marshal(map[string]any{"database": config.DBName, "table": table, "name": "by_amount"}), cookie, 200)
	myisam := table + "_myisam"
	moved := table + "_moved"
	t.Cleanup(func() { _, _ = db.Exec("DROP TABLE IF EXISTS " + mysqlTableName(config.DBName, myisam)) })
	t.Cleanup(func() { _, _ = db.Exec("DROP TABLE IF EXISTS " + mysqlTableName(config.DBName, moved)) })
	request(t, handler, "POST", base+"/tables", marshal(tableChange{Database: config.DBName, Table: myisam, Engine: "MyISAM", Columns: []tableColumnInput{
		{Name: "id", Type: "bigint", Primary: true, AutoIncrement: true},
		{Name: "score", Type: "double"},
		{Name: "rating", Type: "float"},
		{Name: "flags", Type: "bit(8)"},
	}}), cookie, 200)
	myisamPath := base + "/rows?database=" + config.DBName + "&table=" + myisam + "&page=1&pageSize=20"
	request(t, handler, "POST", base+"/rows/insert", marshal(map[string]any{"database": config.DBName, "table": myisam, "values": map[string]any{}}), cookie, 200)
	readMyISAM := func() tablePage {
		response := request(t, handler, "GET", myisamPath, "", cookie, 200)
		var current tablePage
		if err := json.Unmarshal(response.Body.Bytes(), &current); err != nil || len(current.Rows) == 0 {
			t.Fatalf("MyISAM row missing: %s %v", response.Body.String(), err)
		}
		return current
	}
	myisamPage := readMyISAM()
	if !myisamPage.Editable || myisamPage.Rows[0][3] == nil || *myisamPage.Rows[0][3] != "0" {
		t.Fatalf("MyISAM with numeric columns is read-only or BIT unreadable: %+v", myisamPage)
	}
	for _, edit := range []struct{ column, value string }{{"score", "1.000000000000001"}, {"rating", "1.25"}, {"flags", "255"}} {
		key := tableRowKey{Values: []string{*myisamPage.Rows[0][0]}, Version: myisamPage.Versions[0]}
		request(t, handler, "POST", base+"/rows/edit", marshal(tableMutation{Database: config.DBName, Table: myisam, Column: edit.column, Value: pointer(edit.value), Row: key}), cookie, 200)
		myisamPage = readMyISAM()
	}
	if *myisamPage.Rows[0][3] != "255" {
		t.Fatalf("BIT edit did not persist: %+v", myisamPage.Rows[0])
	}
	stale := tableRowKey{Values: []string{*myisamPage.Rows[0][0]}, Version: myisamPage.Versions[0]}
	if _, err := db.Exec("UPDATE "+mysqlTableName(config.DBName, myisam)+" SET score=1.000000000000002 WHERE id=?", stale.Values[0]); err != nil {
		t.Fatal(err)
	}
	request(t, handler, "POST", base+"/rows/edit", marshal(tableMutation{Database: config.DBName, Table: myisam, Column: "rating", Value: pointer("2"), Row: stale}), cookie, 409)
	myisamPage = readMyISAM()
	request(t, handler, "POST", base+"/rows/insert", marshal(map[string]any{"database": config.DBName, "table": myisam, "values": map[string]any{}}), cookie, 200)
	myisamPage = readMyISAM()
	keys := make([]tableRowKey, len(myisamPage.Rows))
	for i := range myisamPage.Rows {
		keys[i] = tableRowKey{Values: []string{*myisamPage.Rows[i][0]}, Version: myisamPage.Versions[i]}
	}
	request(t, handler, "POST", base+"/rows/delete", marshal(tableMutation{Database: config.DBName, Table: myisam, Rows: keys}), cookie, 200)
	request(t, handler, "PATCH", base+"/tables", marshal(tableChange{Database: config.DBName, Table: myisam, NewName: moved, Engine: "InnoDB"}), cookie, 200)
	var movedEngine string
	if err := db.QueryRow("SELECT ENGINE FROM INFORMATION_SCHEMA.TABLES WHERE TABLE_SCHEMA=? AND TABLE_NAME=?", config.DBName, moved).Scan(&movedEngine); err != nil || movedEngine != "InnoDB" {
		t.Fatalf("table rename and engine change failed: %q %v", movedEngine, err)
	}
	aria := table + "_aria"
	t.Cleanup(func() { _, _ = db.Exec("DROP TABLE IF EXISTS " + mysqlTableName(config.DBName, aria)) })
	request(t, handler, "POST", base+"/tables", marshal(tableChange{Database: config.DBName, Table: aria, Engine: "Aria", Columns: []tableColumnInput{
		{Name: "id", Type: "bigint", Primary: true, AutoIncrement: true},
		{Name: "label", Type: "varchar(30)", Nullable: true},
	}}), cookie, 200)
	ariaPath := base + "/rows?database=" + config.DBName + "&table=" + aria + "&page=1&pageSize=20"
	request(t, handler, "POST", base+"/rows/insert", marshal(map[string]any{"database": config.DBName, "table": aria, "values": map[string]any{}}), cookie, 200)
	ariaResponse := request(t, handler, "GET", ariaPath, "", cookie, 200)
	var ariaPage tablePage
	if err := json.Unmarshal(ariaResponse.Body.Bytes(), &ariaPage); err != nil || !ariaPage.Editable || len(ariaPage.Rows) != 1 {
		t.Fatalf("Aria table should be editable: %s %v", ariaResponse.Body.String(), err)
	}
	ariaKey := tableRowKey{Values: []string{*ariaPage.Rows[0][0]}, Version: ariaPage.Versions[0]}
	request(t, handler, "POST", base+"/rows/edit", marshal(tableMutation{Database: config.DBName, Table: aria, Column: "label", Value: pointer("updated"), Row: ariaKey}), cookie, 200)
	ariaResponse = request(t, handler, "GET", ariaPath, "", cookie, 200)
	if err := json.Unmarshal(ariaResponse.Body.Bytes(), &ariaPage); err != nil || ariaPage.Rows[0][1] == nil || *ariaPage.Rows[0][1] != "updated" {
		t.Fatalf("Aria edit did not persist: %s %v", ariaResponse.Body.String(), err)
	}
	ariaKey = tableRowKey{Values: []string{*ariaPage.Rows[0][0]}, Version: ariaPage.Versions[0]}
	request(t, handler, "POST", base+"/rows/delete", marshal(tableMutation{Database: config.DBName, Table: aria, Rows: []tableRowKey{ariaKey}}), cookie, 200)
	request(t, handler, "PATCH", base+"/tables", marshal(tableChange{Database: config.DBName, Table: aria, NewName: aria, Engine: "InnoDB"}), cookie, 200)
	request(t, handler, "DELETE", base+"/tables", marshal(location), cookie, 200)
}
