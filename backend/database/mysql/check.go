package mysql

import (
	"errors"
	"strings"
	"unicode"
	"unicode/utf8"
)

type mysqlCheckToken struct{ kind, value string }
type mysqlCheckParser struct {
	tokens  []mysqlCheckToken
	index   int
	columns map[string]bool
}

func tokenizeMySQLCheck(source string) ([]mysqlCheckToken, error) {
	if len(source) == 0 || len(source) > 4096 || !utf8.ValidString(source) {
		return nil, errors.New("CHECK expression must contain 1 to 4096 valid UTF-8 bytes")
	}
	tokens := []mysqlCheckToken{}
	for i := 0; i < len(source); {
		char, size := utf8.DecodeRuneInString(source[i:])
		if unicode.IsSpace(char) {
			i += size
			continue
		}
		if char == '\'' {
			i++
			var value strings.Builder
			closed := false
			for i < len(source) {
				if source[i] == '\\' {
					return nil, errors.New("backslashes are not supported in CHECK literals")
				}
				if source[i] == '\'' {
					if i+1 < len(source) && source[i+1] == '\'' {
						value.WriteByte('\'')
						i += 2
						continue
					}
					i++
					closed = true
					break
				}
				value.WriteByte(source[i])
				i++
			}
			if !closed {
				return nil, errors.New("unterminated CHECK literal")
			}
			literal, _ := Literal(value.String())
			tokens = append(tokens, mysqlCheckToken{"literal", literal})
			continue
		}
		if char == '`' {
			i++
			var name strings.Builder
			closed := false
			for i < len(source) {
				if source[i] == '`' {
					if i+1 < len(source) && source[i+1] == '`' {
						name.WriteByte('`')
						i += 2
						continue
					}
					i++
					closed = true
					break
				}
				name.WriteByte(source[i])
				i++
			}
			if !closed || !ValidName(name.String()) {
				return nil, errors.New("invalid quoted CHECK column")
			}
			tokens = append(tokens, mysqlCheckToken{"column", name.String()})
			continue
		}
		if char >= '0' && char <= '9' {
			start := i
			for i < len(source) && source[i] >= '0' && source[i] <= '9' {
				i++
			}
			if i < len(source) && source[i] == '.' {
				i++
				digits := i
				for i < len(source) && source[i] >= '0' && source[i] <= '9' {
					i++
				}
				if digits == i {
					return nil, errors.New("invalid numeric literal")
				}
			}
			tokens = append(tokens, mysqlCheckToken{"literal", source[start:i]})
			continue
		}
		if unicode.IsLetter(char) || char == '_' {
			start := i
			for i < len(source) {
				r, size := utf8.DecodeRuneInString(source[i:])
				if !unicode.IsLetter(r) && !unicode.IsDigit(r) && r != '_' {
					break
				}
				i += size
			}
			word := source[start:i]
			keyword := strings.ToUpper(word)
			if keyword == "AND" || keyword == "OR" || keyword == "NOT" || keyword == "IS" || keyword == "NULL" {
				tokens = append(tokens, mysqlCheckToken{"keyword", keyword})
			} else {
				tokens = append(tokens, mysqlCheckToken{"column", word})
			}
			continue
		}
		if i+1 < len(source) {
			pair := source[i : i+2]
			if pair == "<=" || pair == ">=" || pair == "<>" || pair == "!=" {
				tokens = append(tokens, mysqlCheckToken{"operator", pair})
				i += 2
				continue
			}
		}
		if strings.ContainsRune("()+-*/=<>", char) {
			tokens = append(tokens, mysqlCheckToken{"operator", string(char)})
			i++
			continue
		}
		return nil, errors.New("CHECK expression contains an unsupported token")
	}
	if len(tokens) > 256 {
		return nil, errors.New("CHECK expression has too many tokens")
	}
	return tokens, nil
}

func (p *mysqlCheckParser) peek() string {
	if p.index >= len(p.tokens) {
		return ""
	}
	if p.tokens[p.index].kind == "column" {
		return "\x00"
	}
	return p.tokens[p.index].value
}
func (p *mysqlCheckParser) take(value string) bool {
	if strings.EqualFold(p.peek(), value) {
		p.index++
		return true
	}
	return false
}
func (p *mysqlCheckParser) expression() (string, error) { return p.logicalOr() }
func (p *mysqlCheckParser) logicalOr() (string, error) {
	left, err := p.logicalAnd()
	if err != nil {
		return "", err
	}
	for p.take("OR") {
		right, e := p.logicalAnd()
		if e != nil {
			return "", e
		}
		left += " OR " + right
	}
	return left, nil
}
func (p *mysqlCheckParser) logicalAnd() (string, error) {
	left, err := p.logicalNot()
	if err != nil {
		return "", err
	}
	for p.take("AND") {
		right, e := p.logicalNot()
		if e != nil {
			return "", e
		}
		left += " AND " + right
	}
	return left, nil
}
func (p *mysqlCheckParser) logicalNot() (string, error) {
	if p.take("NOT") {
		operand, err := p.logicalNot()
		if err != nil {
			return "", err
		}
		return "NOT " + operand, nil
	}
	return p.comparison()
}
func (p *mysqlCheckParser) comparison() (string, error) {
	left, err := p.additive()
	if err != nil {
		return "", err
	}
	if p.take("IS") {
		mode := "IS "
		if p.take("NOT") {
			mode += "NOT "
		}
		if !p.take("NULL") {
			return "", errors.New("IS must be followed by NULL")
		}
		return left + " " + mode + "NULL", nil
	}
	operator := p.peek()
	if strings.Contains("|=|<>|!=|<|<=|>|>=|", "|"+operator+"|") && operator != "" {
		p.index++
		right, e := p.additive()
		if e != nil {
			return "", e
		}
		return left + " " + operator + " " + right, nil
	}
	return left, nil
}
func (p *mysqlCheckParser) additive() (string, error) {
	left, err := p.multiplicative()
	if err != nil {
		return "", err
	}
	for p.peek() == "+" || p.peek() == "-" {
		operator := p.peek()
		p.index++
		right, e := p.multiplicative()
		if e != nil {
			return "", e
		}
		left += " " + operator + " " + right
	}
	return left, nil
}
func (p *mysqlCheckParser) multiplicative() (string, error) {
	left, err := p.primary()
	if err != nil {
		return "", err
	}
	for p.peek() == "*" || p.peek() == "/" {
		operator := p.peek()
		p.index++
		right, e := p.primary()
		if e != nil {
			return "", e
		}
		left += " " + operator + " " + right
	}
	return left, nil
}
func (p *mysqlCheckParser) primary() (string, error) {
	if p.take("(") {
		inner, err := p.expression()
		if err != nil {
			return "", err
		}
		if !p.take(")") {
			return "", errors.New("unclosed CHECK parenthesis")
		}
		return "(" + inner + ")", nil
	}
	if p.take("+") {
		operand, err := p.primary()
		if err != nil {
			return "", err
		}
		return "+" + operand, nil
	}
	if p.take("-") {
		operand, err := p.primary()
		if err != nil {
			return "", err
		}
		return "-" + operand, nil
	}
	if p.index >= len(p.tokens) {
		return "", errors.New("CHECK expression is incomplete")
	}
	token := p.tokens[p.index]
	p.index++
	if token.kind == "literal" || token.kind == "keyword" && token.value == "NULL" {
		return token.value, nil
	}
	if token.kind == "column" && p.columns[token.value] {
		return Identifier(token.value), nil
	}
	return "", errors.New("CHECK expression references an unsupported or missing column")
}

func BuildCheck(source string, columns []string) (string, error) {
	tokens, err := tokenizeMySQLCheck(source)
	if err != nil {
		return "", err
	}
	parser := mysqlCheckParser{tokens: tokens, columns: map[string]bool{}}
	for _, column := range columns {
		parser.columns[column] = true
	}
	expression, err := parser.expression()
	if err != nil {
		return "", err
	}
	if parser.index != len(parser.tokens) {
		return "", errors.New("CHECK expression has trailing tokens")
	}
	return expression, nil
}
