package postgres

import (
	"errors"
	"testing"
)

func TestPostgresPageCursor(t *testing.T) {
	predicate, args, err := PageCursor([]string{`first"key`, "second"}, `["9007199254740993","a,b"]`)
	if err != nil || predicate != `("first""key","second") > ($2,$3)` || len(args) != 2 || args[0] != "9007199254740993" || args[1] != "a,b" {
		t.Fatalf("composite page cursor changed: %q, %v, %v", predicate, args, err)
	}
	predicate, args, err = PageCursor([]string{"id"}, `["41"]`)
	if err != nil || predicate != `"id" > $2` || len(args) != 1 || args[0] != "41" {
		t.Fatalf("single-column page cursor changed: %q, %v, %v", predicate, args, err)
	}
	for _, cursor := range []string{"invalid", `[]`, `[null]`, `["1","2"]`} {
		if _, _, err := PageCursor([]string{"id"}, cursor); !errors.Is(err, ErrInvalidPageCursor) {
			t.Fatalf("invalid cursor %q was accepted: %v", cursor, err)
		}
	}
}
