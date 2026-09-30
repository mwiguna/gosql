package mysql

import (
	"errors"
	"strings"
	"unicode/utf8"
)

func Identifier(name string) string {
	return "`" + strings.ReplaceAll(name, "`", "``") + "`"
}

func ValidName(name string) bool {
	return name != "" && utf8.ValidString(name) && utf8.RuneCountInString(name) <= 64 && !strings.ContainsRune(name, 0)
}

func Literal(value string) (string, error) {
	if len(value) > 4096 || strings.ContainsAny(value, "\\\x00") {
		return "", errors.New("default value or comment is too long or contains a backslash or null byte")
	}
	return "'" + strings.ReplaceAll(value, "'", "''") + "'", nil
}
