// Package sqlscan splits PostgreSQL migration files into statements and tokens.
package sqlscan

import (
	"errors"
	"fmt"
	"strings"
	"unicode"
)

// MaxInput is the largest migration file accepted, in bytes.
const MaxInput = 8 << 20

// ErrTooLarge is returned for inputs over MaxInput.
var ErrTooLarge = errors.New("sqlscan: input larger than 8 MiB")

// Statement is one SQL statement and the line it starts on.
type Statement struct {
	Text string
	Line int
}

// Kind classifies a token.
type Kind int

// Token kinds.
const (
	Word   Kind = iota // unquoted identifier or keyword
	Ident              // "quoted identifier"
	String             // 'string', E'string' or $tag$string$tag$
	Number             // numeric literal
	Punct              // any other single character
)

// Token is one lexical unit of a statement.
type Token struct {
	Kind Kind
	Text string // identifier value (unquoted, case folded for Word) or raw text
}

// Is reports whether the token is the given keyword (case-insensitive).
func (t Token) Is(word string) bool {
	return t.Kind == Word && t.Text == strings.ToLower(word)
}

// Split cuts src into statements at top-level semicolons, skipping comments.
func Split(src string) ([]Statement, error) {
	if len(src) > MaxInput {
		return nil, ErrTooLarge
	}
	var out []Statement
	var b strings.Builder
	line, start := 1, 0
	flush := func() {
		if text := strings.TrimSpace(b.String()); text != "" {
			out = append(out, Statement{Text: text, Line: start})
		}
		b.Reset()
		start = 0
	}
	for i := 0; i < len(src); {
		c := src[i]
		if start == 0 && !isSpace(c) && !startsComment(src, i) && c != ';' {
			start = line
		}
		switch {
		case c == '\n':
			line++
			b.WriteByte(c)
			i++
		case c == ';':
			flush()
			i++
		case startsComment(src, i):
			end, err := skipComment(src, i)
			if err != nil {
				return nil, fmt.Errorf("line %d: %w", line, err)
			}
			line += strings.Count(src[i:end], "\n")
			b.WriteByte(' ')
			i = end
		case c == '\'' || c == '"' || c == '$':
			end, err := skipQuoted(src, i)
			if err != nil {
				return nil, fmt.Errorf("line %d: %w", line, err)
			}
			line += strings.Count(src[i:end], "\n")
			b.WriteString(src[i:end])
			i = end
		default:
			b.WriteByte(c)
			i++
		}
	}
	flush()
	return out, nil
}

// Tokens lexes one statement; quoted text stays intact.
func Tokens(stmt string) ([]Token, error) {
	var out []Token
	for i := 0; i < len(stmt); {
		c := stmt[i]
		switch {
		case isSpace(c):
			i++
		case startsComment(stmt, i):
			end, err := skipComment(stmt, i)
			if err != nil {
				return nil, err
			}
			i = end
		case c == '"':
			end, err := skipQuoted(stmt, i)
			if err != nil {
				return nil, err
			}
			out = append(out, Token{Kind: Ident, Text: strings.ReplaceAll(stmt[i+1:end-1], `""`, `"`)})
			i = end
		case c == '\'' || (c == '$' && dollarTag(stmt, i) != ""):
			end, err := skipQuoted(stmt, i)
			if err != nil {
				return nil, err
			}
			out = append(out, Token{Kind: String, Text: stmt[i:end]})
			i = end
		case (c == 'e' || c == 'E') && i+1 < len(stmt) && stmt[i+1] == '\'':
			end, err := skipQuoted(stmt, i+1)
			if err != nil {
				return nil, err
			}
			out = append(out, Token{Kind: String, Text: stmt[i:end]})
			i = end
		case isWordStart(c):
			j := i + 1
			for j < len(stmt) && isWordPart(stmt[j]) {
				j++
			}
			out = append(out, Token{Kind: Word, Text: strings.ToLower(stmt[i:j])})
			i = j
		case c >= '0' && c <= '9':
			j := i + 1
			for j < len(stmt) && (stmt[j] >= '0' && stmt[j] <= '9' || stmt[j] == '.') {
				j++
			}
			out = append(out, Token{Kind: Number, Text: stmt[i:j]})
			i = j
		default:
			out = append(out, Token{Kind: Punct, Text: string(c)})
			i++
		}
	}
	return out, nil
}

// isSpace reports ASCII whitespace.
func isSpace(c byte) bool { return c == ' ' || c == '\t' || c == '\n' || c == '\r' || c == '\f' }

// isWordStart reports a character that can begin an unquoted identifier.
func isWordStart(c byte) bool { return c == '_' || c >= 0x80 || unicode.IsLetter(rune(c)) }

// isWordPart reports a character that can continue an unquoted identifier.
func isWordPart(c byte) bool { return isWordStart(c) || c >= '0' && c <= '9' || c == '$' }

// startsComment reports a -- or /* comment at i.
func startsComment(s string, i int) bool {
	return i+1 < len(s) && (s[i] == '-' && s[i+1] == '-' || s[i] == '/' && s[i+1] == '*')
}

// skipComment returns the index just past the comment at i (block comments nest).
func skipComment(s string, i int) (int, error) {
	if s[i] == '-' {
		if j := strings.IndexByte(s[i:], '\n'); j >= 0 {
			return i + j, nil
		}
		return len(s), nil
	}
	depth := 0
	for j := i; j+1 < len(s); j++ {
		switch {
		case s[j] == '/' && s[j+1] == '*':
			depth++
			j++
		case s[j] == '*' && s[j+1] == '/':
			depth--
			j++
			if depth == 0 {
				return j + 1, nil
			}
		}
	}
	return 0, errors.New("unterminated block comment")
}

// dollarTag returns the $tag$ opening at i, or "" if there is none.
func dollarTag(s string, i int) string {
	if i > 0 && isWordPart(s[i-1]) {
		return "" // part of an identifier such as a$b
	}
	j := i + 1
	for j < len(s) && (isWordStart(s[j]) || s[j] >= '0' && s[j] <= '9' && j > i+1) {
		j++
	}
	if j < len(s) && s[j] == '$' {
		return s[i : j+1]
	}
	return ""
}

// skipQuoted returns the index just past the quoted text at i.
func skipQuoted(s string, i int) (int, error) {
	q := s[i]
	if q == '$' {
		tag := dollarTag(s, i)
		if tag == "" {
			return i + 1, nil // a parameter such as $1
		}
		end := strings.Index(s[i+len(tag):], tag)
		if end < 0 {
			return 0, fmt.Errorf("unterminated dollar-quoted string %s", tag)
		}
		return i + len(tag) + end + len(tag), nil
	}
	escapes := q == '\'' && i > 0 && (s[i-1] == 'e' || s[i-1] == 'E') && (i < 2 || !isWordPart(s[i-2]))
	for j := i + 1; j < len(s); j++ {
		switch {
		case escapes && s[j] == '\\':
			j++
		case s[j] == q && j+1 < len(s) && s[j+1] == q:
			j++
		case s[j] == q:
			return j + 1, nil
		}
	}
	if q == '"' {
		return 0, errors.New("unterminated quoted identifier")
	}
	return 0, errors.New("unterminated string literal")
}
