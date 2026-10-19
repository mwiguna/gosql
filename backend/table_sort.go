package main

import (
	"encoding/json"
	"errors"
	"slices"
	"strings"
)

var errInvalidTableSort = errors.New("choose distinct table columns and ascending or descending sorting (maximum 16 rules)")

// Column names are checked against metadata before quoting them for ORDER BY.
func tableSortOrder(raw string, columns []tableColumn, primaryKey []string, quote func(string) string) (string, error) {
	if raw == "" {
		return "", nil
	}
	var sorts []struct {
		Field string `json:"field"`
		Dir   string `json:"dir"`
	}
	if len(raw) > 8192 || json.Unmarshal([]byte(raw), &sorts) != nil || len(sorts) > 16 {
		return "", errInvalidTableSort
	}
	if len(sorts) == 0 {
		return "", nil
	}
	order := []string{}
	seen := map[string]bool{}
	for _, sort := range sorts {
		if seen[sort.Field] || (sort.Dir != "asc" && sort.Dir != "desc") || !slices.ContainsFunc(columns, func(column tableColumn) bool { return column.Name == sort.Field }) {
			return "", errInvalidTableSort
		}
		seen[sort.Field] = true
		order = append(order, quote(sort.Field)+" "+strings.ToUpper(sort.Dir))
	}
	// Primary keys give equal sort values a consistent order across pages.
	for _, key := range primaryKey {
		if !seen[key] {
			order = append(order, quote(key)+" ASC")
		}
	}
	return strings.Join(order, ","), nil
}
