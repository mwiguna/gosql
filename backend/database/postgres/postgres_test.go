package postgres

import (
	"strings"
	"testing"
)

func TestColumnTypeModifiers(t *testing.T) {
	for _, value := range []string{"varchar(1)", "varchar(120)", "varchar(10485760)", "character(20)", "bit varying(64)", "numeric(18,2)", "numeric(1000,-1000)", "decimal(12,0)", "smallint", "jsonpath", "tsvector", "text[]", "numeric(18,2)[]", `"public"."mood"`, `"public"."mood"[]`, `"quoted""schema"."custom type"`} {
		if !ValidColumnType(value) {
			t.Fatalf("valid type was rejected: %s", value)
		}
	}
	for _, value := range []string{"varchar(0)", "varchar(10485761)", "varchar(1); DROP TABLE users", "varchar(x)", "numeric(0,2)", "numeric(1001,0)", "numeric(18,1001)", "numeric(18,2); DROP TABLE users", "serial[]", "text[][]", `"public"."mood"; DROP TABLE users`, `"public".mood`} {
		if ValidColumnType(value) {
			t.Fatalf("invalid type was accepted: %s", value)
		}
	}
}

func TestQuickInsertUsesTypedValuesAndNextPrimaryKey(t *testing.T) {
	sql, nextPK, err := BuildQuickInsertSQL(`"public"."items"`, []InsertColumn{
		{Name: "id", Type: "int8", Category: "N", Primary: true},
		{Name: "title", Type: "text", Category: "S"},
		{Name: "quantity", Type: "int4", Category: "N"},
		{Name: "active", Type: "bool", Category: "B"},
		{Name: "created_at", Type: "timestamptz", HasDefault: true},
	})
	if err != nil || !nextPK || sql != `INSERT INTO "public"."items" ("id", "title", "quantity", "active") VALUES ((SELECT COALESCE(MAX("id"), 0) + 1 FROM "public"."items"), '', 0, false)` {
		t.Fatalf("unexpected quick insert SQL: %q, nextPK=%v, err=%v", sql, nextPK, err)
	}
	_, _, err = BuildQuickInsertSQL(`"public"."items"`, []InsertColumn{{Name: "mood", Type: "mood", Category: "E"}})
	if err == nil {
		t.Fatal("required unsupported column was silently assigned a value")
	}
	sql, _, err = BuildQuickInsertSQL(`"public"."children"`, []InsertColumn{
		{Name: "id", Type: "int4", Category: "N", Primary: true},
		{Name: "parent_id", Type: "int4", Category: "N", Nullable: true, ForeignKey: true},
		{Name: "title", Type: "text", Category: "S"},
	})
	if err != nil || strings.Contains(sql, `"parent_id"`) || !strings.Contains(sql, `"title"`) {
		t.Fatalf("nullable foreign key must be left NULL: %q, err=%v", sql, err)
	}
}

func TestQuickInsertDateUsesDefaultNullOrCurrentDate(t *testing.T) {
	sql, _, err := BuildQuickInsertSQL(`"public"."events"`, []InsertColumn{
		{Name: "default_date", Type: "date", Nullable: true, HasDefault: true},
		{Name: "optional_date", Type: "date", Nullable: true},
		{Name: "required_date", Type: "date"},
	})
	if err != nil || sql != `INSERT INTO "public"."events" ("required_date") VALUES (CURRENT_DATE)` {
		t.Fatalf("date columns ignored their default, NULL, or required initial value: %q, %v", sql, err)
	}
	sql, _, err = BuildQuickInsertSQL(`"public"."events"`, []InsertColumn{{Name: "optional_date", Type: "date", Nullable: true}})
	if err != nil || sql != `INSERT INTO "public"."events" DEFAULT VALUES` {
		t.Fatalf("nullable date should use NULL: %q, %v", sql, err)
	}
}
