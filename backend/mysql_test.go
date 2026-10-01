package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	mysqldb "gosql/database/mysql"
)

func TestMySQLProfileAndSQLValidation(t *testing.T) {
	fields := connectionFields{Engine: "MySQL", Name: "Local MySQL", Host: "localhost", Port: "3306", Username: "root", SSL: "Prefer"}
	if !fields.valid() {
		t.Fatal("MySQL profile without initial database or password was rejected")
	}
	if mysqldb.Identifier("a`b") != "`a``b`" {
		t.Fatal("MySQL identifier quoting changed")
	}
	for _, kind := range []string{"INT", "int", "int(11)", "int(11) unsigned", "varchar(255)", "DECIMAL(65,30)", "decimal(18,2) unsigned", "datetime", "json"} {
		if !validMySQLType(kind) {
			t.Errorf("valid type rejected: %s", kind)
		}
	}
	for _, kind := range []string{"bit", "bit(1)", "bit(64)"} {
		if !validMySQLType(kind) {
			t.Errorf("valid BIT type rejected: %s", kind)
		}
	}
	for _, kind := range []string{"varchar(0)", "decimal(10,30)", "int;DROP TABLE users", "numeric", "varchar(99999)"} {
		if validMySQLType(kind) {
			t.Errorf("unsafe or invalid type accepted: %s", kind)
		}
	}
	if validMySQLType("bit(65)") {
		t.Fatal("BIT width above 64 accepted")
	}
	definition, err := mysqlColumnDefinition(tableColumnInput{Name: "id", Type: "bigint(20) unsigned", Primary: true, AutoIncrement: true})
	if err != nil || definition != "`id` bigint(20) unsigned NOT NULL AUTO_INCREMENT" {
		t.Fatalf("unsigned auto increment column rejected: %q, %v", definition, err)
	}
	if _, err := mysqlDecodeBinary(`\x00ff`); err != nil {
		t.Fatal(err)
	}
	if _, err := mysqlDecodeBinary("0xff"); err == nil {
		t.Fatal("binary value without prefix accepted")
	}
	if _, err := mysqldb.Literal("a\\b"); err == nil {
		t.Fatal("DDL literal with backslash accepted despite SQL mode ambiguity")
	}
}

func TestMySQLBitValueBounds(t *testing.T) {
	if !validMySQLTableEngine("InnoDB") || !validMySQLTableEngine("MyISAM") || !validMySQLTableEngine("Aria") || validMySQLTableEngine("MyISAM; DROP TABLE t") {
		t.Fatal("table engine validation is incorrect")
	}
	column := mysqlColumn{tableColumn: tableColumn{Type: "bit(8)"}, DataType: "bit"}
	value := "255"
	converted, err := mysqlValue(column, &value)
	if err != nil || converted != uint64(255) {
		t.Fatalf("BIT value conversion failed: %v %v", converted, err)
	}
	for _, invalid := range []string{"256", "-1", "abc"} {
		if _, err := mysqlValue(column, &invalid); err == nil {
			t.Errorf("invalid BIT value accepted: %q", invalid)
		}
	}
}

func TestMySQLQuickInsertDateAndNullRules(t *testing.T) {
	if expression, ok := mysqlQuickValue(mysqlColumn{DataType: "date"}); !ok || expression != "CURRENT_DATE" {
		t.Fatalf("nonnullable date default: %q %v", expression, ok)
	}
	if _, ok := mysqlQuickValue(mysqlColumn{DataType: "enum"}); ok {
		t.Fatal("ENUM got an unsafe default")
	}
	token := mysqlVersionSQL([]mysqlColumn{{tableColumn: tableColumn{Name: "a`b", Type: "varchar(255)"}, DataType: "varchar"}})
	if !strings.Contains(token, "`a``b`") || !strings.Contains(token, "1048576") {
		t.Fatalf("row version SQL lost quoting or size limit: %s", token)
	}
}

func TestMySQLReadOnlyReasonWithPrimaryKey(t *testing.T) {
	meta := mysqlTableMeta{Kind: "BASE TABLE", Engine: "Aria", PrimaryKey: []string{"id"}, Columns: []mysqlColumn{{tableColumn: tableColumn{Name: "id", Type: "int"}, DataType: "int"}}}
	if reason := mysqlReadOnlyReason(meta); reason != "" {
		t.Fatalf("Aria table is read-only: %q", reason)
	}
	meta.Engine = "MEMORY"
	if reason := mysqlReadOnlyReason(meta); !strings.Contains(reason, "MEMORY") {
		t.Fatalf("unsupported engine reason missing: %q", reason)
	}
	meta.Engine = "MyISAM"
	if reason := mysqlReadOnlyReason(meta); reason != "" {
		t.Fatalf("MyISAM table is read-only: %q", reason)
	}
	meta.Engine = "InnoDB"
	meta.Columns = append(meta.Columns, mysqlColumn{tableColumn: tableColumn{Name: "location", Type: "geometry"}, DataType: "geometry"})
	if reason := mysqlReadOnlyReason(meta); !strings.Contains(reason, "location (geometry)") {
		t.Fatalf("unsupported column reason missing: %q", reason)
	}
	meta.Columns = meta.Columns[:1]
	meta.Columns = append(meta.Columns, mysqlColumn{tableColumn: tableColumn{Name: "score", Type: "double"}, DataType: "double"}, mysqlColumn{tableColumn: tableColumn{Name: "flags", Type: "bit(8)"}, DataType: "bit"})
	if reason := mysqlReadOnlyReason(meta); reason != "" {
		t.Fatalf("supported table is read-only: %q", reason)
	}
}

func TestMySQLKeyParametersPreserveNumericPrecision(t *testing.T) {
	meta := mysqlTableMeta{PrimaryKey: []string{"signed", "unsigned", "amount"}, Columns: []mysqlColumn{
		{tableColumn: tableColumn{Name: "signed", Type: "bigint"}, DataType: "bigint"},
		{tableColumn: tableColumn{Name: "unsigned", Type: "bigint unsigned"}, DataType: "bigint"},
		{tableColumn: tableColumn{Name: "amount", Type: "decimal(30,10)"}, DataType: "decimal"},
	}}
	want := "`signed` <=> CAST(? AS SIGNED) AND `unsigned` <=> CAST(? AS UNSIGNED) AND `amount` <=> CAST(? AS DECIMAL(30,10))"
	if got := mysqlKeyWhere(meta); got != want {
		t.Fatalf("key predicate may lose numeric precision: %q", got)
	}
}

func TestMySQLUnsupportedServerErrorsAreVisible(t *testing.T) {
	for _, test := range []struct {
		err  error
		code string
	}{
		{mysqldb.ErrMariaDBVersionUnsupported, "mariadb_version_unsupported"},
		{mysqldb.ErrMySQLVersionUnsupported, "mysql_version_unsupported"},
		{mysqldb.ErrMariaDBProfileRequired, "mariadb_profile_required"},
		{mysqldb.ErrMySQLProfileRequired, "mysql_profile_required"},
	} {
		response := httptest.NewRecorder()
		writeMySQLError(response, test.err)
		if response.Code != 400 || !strings.Contains(response.Body.String(), test.code) {
			t.Fatalf("unsupported server error hidden from user: %d %s", response.Code, response.Body.String())
		}
	}
}

func TestMySQLAndMariaDBVersionBounds(t *testing.T) {
	for _, test := range []struct {
		version string
		mariaDB bool
		allowed bool
	}{
		{"8.0.15", false, false}, {"8.0.16", false, true}, {"8.4.0", false, true},
		{"10.5.9-MariaDB", true, false}, {"10.5.10-MariaDB", true, true},
		{"5.5.5-10.11.8-MariaDB", true, true}, {"13.0.2-MariaDB", true, true},
	} {
		mariaDB, _, _, _, err := mysqldb.ParseServerVersion(test.version)
		if mariaDB != test.mariaDB || (err == nil) != test.allowed {
			t.Errorf("version %q: MariaDB=%v, error=%v", test.version, mariaDB, err)
		}
	}
	if mysqldb.ServerProfileError(true, "MySQL") != mysqldb.ErrMariaDBProfileRequired || mysqldb.ServerProfileError(false, "MariaDB") != mysqldb.ErrMySQLProfileRequired || mysqldb.ServerProfileError(true, "MariaDB") != nil || mysqldb.ServerProfileError(false, "MySQL") != nil {
		t.Fatal("server and profile engines must match to keep DDL previews accurate")
	}
}

func TestMySQLEndpointsDispatchAndPasswordPrivacy(t *testing.T) {
	_, handler := testApplication(t)
	cookie, _ := setupAdmin(t, handler)
	request(t, handler, "POST", "/api/mysql/test", `{"engine":"MySQL","name":"Test","host":"127.0.0.1","port":"1","username":"root","ssl":"Prefer"}`, nil, 401)
	request(t, handler, "POST", "/api/mariadb/test", `{"engine":"MariaDB","name":"Test","host":"127.0.0.1","port":"1","username":"root","ssl":"Prefer"}`, cookie, 502)
	request(t, handler, "POST", "/api/connections", `{"engine":"MariaDB","name":"Local MariaDB","host":"127.0.0.1","port":"1","username":"root","ssl":"Disable"}`, cookie, 201)
	created := request(t, handler, "POST", "/api/connections", `{"engine":"MySQL","name":"Local","host":"127.0.0.1","port":"1","username":"root","ssl":"Disable"}`, cookie, 201)
	var profile connectionProfile
	if err := json.Unmarshal(created.Body.Bytes(), &profile); err != nil {
		t.Fatal(err)
	}
	request(t, handler, "GET", "/api/connections/"+profile.ID+"/rows?database=app&table=items&page=1&pageSize=20", "", cookie, 409)
	request(t, handler, "POST", "/api/connections/"+profile.ID+"/schemas", `{"database":"app","schema":"public"}`, cookie, 404)
	response := request(t, handler, "POST", "/api/connections/"+profile.ID+"/catalog", `{"password":"private-test-password"}`, cookie, 502)
	if strings.Contains(response.Body.String(), "private-test-password") {
		t.Fatal("MySQL password leaked in error response")
	}
	request(t, handler, "GET", "/api/mysql/test", "", cookie, http.StatusMethodNotAllowed)
}

func TestMySQLTriggerTarget(t *testing.T) {
	definition := "CREATE TRIGGER `audit` AFTER INSERT ON `sales`.`items` FOR EACH ROW BEGIN INSERT INTO log VALUES (NEW.id); END"
	if !mysqlTriggerTarget(definition, "sales", "items", "audit") {
		t.Fatal("matching trigger definition was rejected")
	}
	if mysqlTriggerTarget(definition, "sales", "other", "audit") || mysqlTriggerTarget(definition, "sales", "items", "other") {
		t.Fatal("trigger definition for another object was accepted")
	}
	if !mysqlTriggerTarget("CREATE TRIGGER `işlem` AFTER INSERT ON `sales`.`ürün` FOR EACH ROW SET @n=NEW.id", "sales", "ürün", "işlem") {
		t.Fatal("Unicode trigger names were rejected")
	}
}
