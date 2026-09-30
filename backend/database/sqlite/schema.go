package sqlite

import (
	"errors"
	"slices"
	"strings"
	"unicode"
)

type DDLToken struct {
	Text  string
	Start int
	End   int
}

func quotedEnd(value string, start int) (int, error) {
	open := value[start]
	close := open
	if open == '[' {
		close = ']'
	}
	for i := start + 1; i < len(value); i++ {
		if value[i] == close {
			if i+1 < len(value) && value[i+1] == close {
				i++
				continue
			}
			return i + 1, nil
		}
	}
	return 0, errors.New("unclosed quote in table definition")
}

func groupEnd(value string, start int) (int, error) {
	depth := 0
	for i := start; i < len(value); i++ {
		switch value[i] {
		case '\'', '"', '`', '[':
			end, err := quotedEnd(value, i)
			if err != nil {
				return 0, err
			}
			i = end - 1
		case '(':
			depth++
		case ')':
			depth--
			if depth == 0 {
				return i + 1, nil
			}
		}
	}
	return 0, errors.New("unclosed parentheses in table definition")
}

func DDLTokens(value string) ([]DDLToken, error) {
	result := []DDLToken{}
	for i := 0; i < len(value); {
		if unicode.IsSpace(rune(value[i])) {
			i++
			continue
		}
		start := i
		var err error
		switch value[i] {
		case '\'', '"', '`', '[':
			i, err = quotedEnd(value, i)
		case '(':
			i, err = groupEnd(value, i)
		default:
			for i < len(value) && !unicode.IsSpace(rune(value[i])) && !strings.ContainsRune("'\"`[()", rune(value[i])) {
				i++
			}
			if i == start {
				return nil, errors.New("unsupported table definition")
			}
		}
		if err != nil {
			return nil, err
		}
		result = append(result, DDLToken{value[start:i], start, i})
	}
	return result, nil
}

func DDLName(token string) string {
	if len(token) < 2 {
		return token
	}
	switch token[0] {
	case '"':
		return strings.ReplaceAll(token[1:len(token)-1], `""`, `"`)
	case '`':
		return strings.ReplaceAll(token[1:len(token)-1], "``", "`")
	case '[':
		return strings.ReplaceAll(token[1:len(token)-1], "]]", "]")
	}
	return token
}

func TableParts(definition string) ([]string, string, error) {
	if strings.Contains(definition, "--") || strings.Contains(definition, "/*") {
		return nil, "", errors.New("table definitions containing comments need manual SQL migration")
	}
	open := -1
	for i := 0; i < len(definition); i++ {
		switch definition[i] {
		case '\'', '"', '`', '[':
			end, err := quotedEnd(definition, i)
			if err != nil {
				return nil, "", err
			}
			i = end - 1
		case '(':
			open = i
		}
		if open >= 0 {
			break
		}
	}
	if open < 0 {
		return nil, "", errors.New("table definition has no column list")
	}
	close, err := groupEnd(definition, open)
	if err != nil {
		return nil, "", err
	}
	parts := []string{}
	start := open + 1
	for i := start; i < close-1; i++ {
		switch definition[i] {
		case '\'', '"', '`', '[':
			end, err := quotedEnd(definition, i)
			if err != nil {
				return nil, "", err
			}
			i = end - 1
		case '(':
			end, err := groupEnd(definition, i)
			if err != nil {
				return nil, "", err
			}
			i = end - 1
		case ',':
			parts = append(parts, strings.TrimSpace(definition[start:i]))
			start = i + 1
		}
	}
	parts = append(parts, strings.TrimSpace(definition[start:close-1]))
	if slices.Contains(parts, "") {
		return nil, "", errors.New("empty item in table definition")
	}
	return parts, definition[close:], nil
}
