package main

import (
	"strings"
	"testing"
)

func TestBuildCreateTableSQL(t *testing.T) {
	value := "O'Reilly\\archive"
	statement, err := buildCreateTableSQL(tableChange{Schema: "odd\"schema", Table: "records", Columns: []tableColumnInput{
		{Name: "id", Type: "bigint", Primary: true},
		{Name: "payload", Type: "text", Nullable: true, Default: &value},
	}})
	if err != nil {
		t.Fatal(err)
	}
	for _, part := range []string{`"odd""schema"."records"`, `"id" bigint NOT NULL`, `"payload" text DEFAULT E'O''Reilly\\archive'`, `PRIMARY KEY ("id")`} {
		if !strings.Contains(statement, part) {
			t.Fatalf("missing %q in %s", part, statement)
		}
	}
	for _, columns := range [][]tableColumnInput{
		{{Name: "value", Type: "text"}, {Name: "value", Type: "text"}},
		{{Name: "value", Type: "text); DROP TABLE users; --"}},
	} {
		if _, err := buildCreateTableSQL(tableChange{Schema: "public", Table: "test", Columns: columns}); err == nil {
			t.Fatal("invalid column definition was accepted")
		}
	}
}
