package postgres

import (
	"errors"
	"regexp"
	"strconv"
	"strings"
)

var columnTypes = map[string]bool{
	"smallint": true, "integer": true, "bigint": true, "numeric": true, "decimal": true,
	"real": true, "double precision": true, "money": true,
	"smallserial": true, "serial": true, "bigserial": true,
	"boolean": true, "text": true, "varchar": true, "character": true,
	"bit": true, "bit varying": true, "bytea": true,
	"date": true, "time": true, "time with time zone": true,
	"timestamp": true, "timestamp with time zone": true, "interval": true,
	"uuid": true, "json": true, "jsonb": true, "jsonpath": true, "xml": true,
	"inet": true, "cidr": true, "macaddr": true, "macaddr8": true,
	"point": true, "line": true, "lseg": true, "box": true, "path": true, "polygon": true, "circle": true,
	"tsvector": true, "tsquery": true,
	"int4range": true, "int8range": true, "numrange": true,
	"tsrange": true, "tstzrange": true, "daterange": true,
}

var qualifiedColumnType = regexp.MustCompile(`^"(?:[^"]|"")+"\."(?:[^"]|"")+"$`)

func ValidColumnType(value string) bool {
	if strings.HasSuffix(value, "[]") {
		base := strings.TrimSuffix(value, "[]")
		return base != "smallserial" && base != "serial" && base != "bigserial" && !strings.HasSuffix(base, "[]") && ValidColumnType(base)
	}
	if columnTypes[value] {
		return true
	}
	if !strings.ContainsRune(value, 0) && qualifiedColumnType.MatchString(value) {
		return true
	}
	base, parameters, ok := strings.Cut(value, "(")
	if !ok || !strings.HasSuffix(parameters, ")") {
		return false
	}
	parameters = strings.TrimSuffix(parameters, ")")
	if base == "varchar" || base == "character" || base == "bit" || base == "bit varying" {
		length, err := strconv.Atoi(parameters)
		return err == nil && length >= 1 && length <= 10485760
	}
	if base != "numeric" && base != "decimal" {
		return false
	}
	parts := strings.Split(parameters, ",")
	if len(parts) < 1 || len(parts) > 2 {
		return false
	}
	precision, err := strconv.Atoi(parts[0])
	if err != nil || precision < 1 || precision > 1000 {
		return false
	}
	if len(parts) == 1 {
		return true
	}
	scale, err := strconv.Atoi(parts[1])
	return err == nil && scale >= -1000 && scale <= 1000
}

func QuotedLiteral(value string) (string, error) {
	if len(value) > 4096 || strings.ContainsRune(value, 0) {
		return "", errors.New("default value is too long or contains a null byte")
	}
	return "E'" + strings.ReplaceAll(strings.ReplaceAll(value, "\\", "\\\\"), "'", "''") + "'", nil
}
