package main

import (
	"strings"
	"testing"
)

func TestBuildConstraintSQL(t *testing.T) {
	local := []tableColumn{{Name: "id"}, {Name: "customer_id"}, {Name: "amount"}}
	reference := []tableColumn{{Name: "id"}, {Name: "tenant_id"}}
	base := schemaChange{Schema: "public", Table: "orders", Name: "orders_rule"}
	for _, item := range []struct {
		kind     string
		input    schemaChange
		contains string
	}{
		{"UNIQUE", schemaChange{Columns: []string{"customer_id", "amount"}}, `UNIQUE ("customer_id", "amount")`},
		{"PRIMARY KEY", schemaChange{Columns: []string{"id"}}, `PRIMARY KEY ("id")`},
		{"FOREIGN KEY", schemaChange{Columns: []string{"customer_id"}, ReferenceSchema: "public", ReferenceTable: "customers", ReferenceColumns: []string{"id"}}, `FOREIGN KEY ("customer_id") REFERENCES "public"."customers" ("id")`},
		{"CHECK", schemaChange{Expression: `amount >= 0`}, `CHECK (amount >= 0)`},
	} {
		input := base
		input.Type = item.kind
		input.Columns = item.input.Columns
		input.ReferenceSchema = item.input.ReferenceSchema
		input.ReferenceTable = item.input.ReferenceTable
		input.ReferenceColumns = item.input.ReferenceColumns
		input.Expression = item.input.Expression
		statement, err := buildConstraintSQL(input, local, reference)
		if err != nil || !strings.Contains(statement, item.contains) {
			t.Fatalf("%s: %s %v", item.kind, statement, err)
		}
	}
	bad := base
	bad.Type = "FOREIGN KEY"
	bad.Columns = []string{"customer_id", "amount"}
	bad.ReferenceSchema = "public"
	bad.ReferenceTable = "customers"
	bad.ReferenceColumns = []string{"id", "id"}
	if _, err := buildConstraintSQL(bad, local, reference); err == nil {
		t.Fatal("duplicate reference columns accepted")
	}
	bad.Type = "CHECK"
	bad.Expression = `true) NOT VALID, DROP CONSTRAINT orders_rule, ADD CONSTRAINT x CHECK (true`
	if _, err := buildConstraintSQL(bad, local, reference); err == nil {
		t.Fatal("unsafe CHECK expression accepted")
	}
}
