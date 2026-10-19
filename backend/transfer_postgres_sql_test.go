package main

import (
	"io"
	"os"
	"strings"
	"testing"
)

func TestPostgresSQLPreparation(t *testing.T) {
	for _, test := range []struct{ name, script, want string }{
		{"plain", "SELECT 1;\n", "SELECT 1;\n"},
		{"BOM", "\ufeffSELECT 'é';\r\n", "SELECT 'é';\r\n"},
		{"dump guards", "-- dump\n\n\\restrict OriginalKey\nSELECT 1;\n\\unrestrict OriginalKey\n-- end\n", "-- dump\n\nSELECT 1;\n-- end\n"},
		{"function body", "\\restrict OriginalKey\nCREATE FUNCTION example() RETURNS text LANGUAGE sql AS $body$\nSELECT $text$\n\\restrict OriginalKey\n\\unrestrict OriginalKey\n$text$;\n$body$;\n\\unrestrict OriginalKey\n", "CREATE FUNCTION example() RETURNS text LANGUAGE sql AS $body$\nSELECT $text$\n\\restrict OriginalKey\n\\unrestrict OriginalKey\n$text$;\n$body$;\n"},
		{"COPY data", "\\restrict OriginalKey\nCOPY example FROM stdin;\n\\unrestrict OriginalKey\n\\.\n\\unrestrict OriginalKey\n", "COPY example FROM stdin;\n\\unrestrict OriginalKey\n\\.\n"},
		{"internal unrestrict remains blocked", "\\restrict OriginalKey\n\\unrestrict OriginalKey\nSELECT 1;\n\\unrestrict OriginalKey\n", "\\unrestrict OriginalKey\nSELECT 1;\n"},
	} {
		t.Run(test.name, func(t *testing.T) {
			source, err := os.CreateTemp(t.TempDir(), "sql-*")
			if err != nil {
				t.Fatal(err)
			}
			defer source.Close()
			if _, err := io.WriteString(source, test.script); err != nil {
				t.Fatal(err)
			}
			prepared, err := preparePostgresSQL(source)
			if err != nil {
				t.Fatal(err)
			}
			defer os.Remove(prepared.Name())
			defer prepared.Close()
			data, err := io.ReadAll(prepared)
			if err != nil {
				t.Fatal(err)
			}
			guard, body, ok := strings.Cut(string(data), "\n")
			if !ok || !strings.HasPrefix(guard, `\restrict `) || guard == `\restrict OriginalKey` || body != test.want {
				t.Fatalf("SQL preparation changed content: %q, wanted %q", string(data), test.want)
			}
		})
	}
}
