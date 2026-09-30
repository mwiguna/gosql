package postgres

import (
	"encoding/json"
	"errors"
	"strconv"
	"strings"

	"github.com/jackc/pgx/v5"
)

var ErrInvalidPageCursor = errors.New("invalid page cursor")

func PageCursor(keys []string, cursor string) (string, []any, error) {
	if len(cursor) > 8<<10 {
		return "", nil, ErrInvalidPageCursor
	}
	var encoded []json.RawMessage
	if err := json.Unmarshal([]byte(cursor), &encoded); err != nil || len(encoded) != len(keys) {
		return "", nil, ErrInvalidPageCursor
	}
	columns := make([]string, len(keys))
	parameters := make([]string, len(keys))
	args := make([]any, len(keys))
	for index, key := range keys {
		var value string
		if len(encoded[index]) == 0 || encoded[index][0] != '"' || json.Unmarshal(encoded[index], &value) != nil {
			return "", nil, ErrInvalidPageCursor
		}
		columns[index] = pgx.Identifier{key}.Sanitize()
		parameters[index] = "$" + strconv.Itoa(index+2)
		args[index] = value
	}
	if len(keys) == 1 {
		return columns[0] + " > " + parameters[0], args, nil
	}
	return "(" + strings.Join(columns, ",") + ") > (" + strings.Join(parameters, ",") + ")", args, nil
}

func PreviewSQL(name, kind string) string {
	if kind == "bytea" {
		return "CASE WHEN " + name + " IS NULL THEN NULL ELSE chr(92) || 'x' || encode(substring(" + name + " from 1 for 257), 'hex') END"
	}
	return "LEFT(" + name + "::text, 513)"
}

func PreviewColumn(kind string) bool {
	return kind == "text" || kind == "bytea" || kind == "json" || kind == "jsonb" || kind == "xml" ||
		strings.HasPrefix(kind, "character varying") || strings.HasPrefix(kind, "character(") ||
		strings.HasPrefix(kind, "bit varying") || strings.HasSuffix(kind, "[]")
}
