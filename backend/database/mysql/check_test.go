package mysql

import "testing"

func TestMySQLCheckParserRejectsSQLAndMissingColumns(t *testing.T) {
	columns := []string{"price", "status"}
	sql, err := BuildCheck("price >= 0 AND (status = 'ready' OR status IS NULL)", columns)
	if err != nil || sql != "`price` >= 0 AND (`status` = 'ready' OR `status` IS NULL)" {
		t.Fatalf("unexpected CHECK SQL: %q, %v", sql, err)
	}
	sql, err = BuildCheck("`odd name` IS NOT NULL", append(columns, "odd name"))
	if err != nil || sql != "`odd name` IS NOT NULL" {
		t.Fatalf("quoted CHECK column was rejected: %q, %v", sql, err)
	}
	for _, source := range []string{
		"price >= 0; DROP TABLE users", "price >= 0 -- comment", "unknown > 0",
		"price > 0 OR EXISTS(SELECT 1)", "price = 'a\\b'", "price +",
		"price >= 0 AND", "price = 'unfinished", "price = 1) OR 1=1",
	} {
		if sql, err := BuildCheck(source, columns); err == nil {
			t.Errorf("invalid CHECK accepted: %q => %q", source, sql)
		}
	}
}
