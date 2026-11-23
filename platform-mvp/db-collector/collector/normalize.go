package collector

import (
	"strings"
	"unicode/utf8"
)

// Normalize turns SQL text into the shape of the statement: every literal (strings, numbers, dollar-quoted bodies, bit and hex strings) becomes
// "?", comments are dropped, runs of white space become one space, "IN (?, ?, ?)" becomes "IN (?)" and repeated "VALUES" tuples become one.
// Parameter markers ($1) and quoted identifiers are kept. The text is cut to maxBytes (at a character boundary, with "..." after it).
//
// This is the privacy boundary of the collector: a statement's text can hold anything an application put in it (a password in CREATE ROLE, an
// e-mail address in a WHERE clause), so NOTHING leaves the collector except through this function. It errs towards removing too much: text
// that stops in the middle of a literal (as a truncated pg_stat_activity query does) has the whole of the rest removed, a dollar-quoted body
// with no end is removed to the end, and a comment is always removed, since comments carry things too (trace ids, user ids, tags).
//
// The rules are those of PostgreSQL's own lexer; for another engine, a dialect with its own quoting rules belongs here, not a flag.
func Normalize(sql string, maxBytes int) string {
	var out strings.Builder
	out.Grow(len(sql))
	n := len(sql)
	i := 0
	space := func() {
		if out.Len() > 0 && out.String()[out.Len()-1] != ' ' {
			out.WriteByte(' ')
		}
	}
	for i < n {
		c := sql[i]
		switch {
		case c == ' ' || c == '\t' || c == '\n' || c == '\r' || c == '\f' || c == '\v':
			space()
			i++
		case c == '-' && i+1 < n && sql[i+1] == '-': // line comment
			for i < n && sql[i] != '\n' {
				i++
			}
			space()
		case c == '/' && i+1 < n && sql[i+1] == '*': // block comment, which nests in PostgreSQL
			depth := 1
			i += 2
			for i < n && depth > 0 {
				switch {
				case sql[i] == '/' && i+1 < n && sql[i+1] == '*':
					depth++
					i += 2
				case sql[i] == '*' && i+1 < n && sql[i+1] == '/':
					depth--
					i += 2
				default:
					i++
				}
			}
			space()
		case c == '\'':
			i = skipString(sql, i, false)
			out.WriteByte('?')
		case c == '"': // a quoted identifier: kept as it is
			j := i + 1
			for j < n {
				if sql[j] == '"' {
					if j+1 < n && sql[j+1] == '"' {
						j += 2
						continue
					}
					break
				}
				j++
			}
			if j < n {
				j++
			}
			out.WriteString(sql[i:j])
			i = j
		case c == '$':
			if i+1 < n && sql[i+1] >= '0' && sql[i+1] <= '9' { // a parameter: $1
				j := i + 1
				for j < n && sql[j] >= '0' && sql[j] <= '9' {
					j++
				}
				out.WriteString(sql[i:j])
				i = j
			} else if end, ok := dollarQuoted(sql, i); ok {
				out.WriteByte('?')
				i = end
			} else {
				out.WriteByte(c)
				i++
			}
		case c >= '0' && c <= '9', c == '.' && i+1 < n && sql[i+1] >= '0' && sql[i+1] <= '9':
			i = skipNumber(sql, i)
			out.WriteByte('?')
		case isWordStart(c):
			j := i
			for j < n && isWordByte(sql[j]) {
				j++
			}
			word := sql[i:j]
			switch {
			case j < n && sql[j] == '\'' && len(word) == 1 && strings.ContainsRune("eE", rune(word[0])):
				i = skipString(sql, j, true) // E'...' takes backslash escapes
				out.WriteByte('?')
			case j < n && sql[j] == '\'' && len(word) == 1 && strings.ContainsRune("bBxXnN", rune(word[0])):
				i = skipString(sql, j, false) // B'101', X'1F', N'national'
				out.WriteByte('?')
			case j+1 < n && sql[j] == '&' && sql[j+1] == '\'' && len(word) == 1 && (word[0] == 'u' || word[0] == 'U'):
				i = skipString(sql, j+1, false) // U&'\0041'
				out.WriteByte('?')
			default:
				out.WriteString(word)
				i = j
			}
		default:
			out.WriteByte(c)
			i++
		}
	}
	s := strings.TrimSpace(out.String())
	s = collapseIn(s)
	s = collapseValues(s)
	return truncate(s, maxBytes)
}

func isWordStart(c byte) bool {
	return c == '_' || c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= 0x80
}

func isWordByte(c byte) bool { return isWordStart(c) || c >= '0' && c <= '9' || c == '$' }

// skipString returns the index after the quoted string that starts at sql[at] (a quote). A string that never ends runs to the end of the text.
func skipString(sql string, at int, backslash bool) int {
	n := len(sql)
	j := at + 1
	for j < n {
		switch {
		case backslash && sql[j] == '\\':
			j += 2
		case sql[j] == '\'':
			if j+1 < n && sql[j+1] == '\'' {
				j += 2
				continue
			}
			return j + 1
		default:
			j++
		}
	}
	return n
}

// dollarQuoted recognises $$...$$ and $tag$...$tag$ at sql[at]; ok is false when it is not one.
func dollarQuoted(sql string, at int) (end int, ok bool) {
	n := len(sql)
	j := at + 1
	if j < n && (sql[j] == '_' || sql[j] >= 'a' && sql[j] <= 'z' || sql[j] >= 'A' && sql[j] <= 'Z' || sql[j] >= 0x80) {
		for j < n && (isWordByte(sql[j]) && sql[j] != '$') {
			j++
		}
	}
	if j >= n || sql[j] != '$' {
		return 0, false
	}
	tag := sql[at : j+1]
	if k := strings.Index(sql[j+1:], tag); k >= 0 {
		return j + 1 + k + len(tag), true
	}
	return n, true // never closed: the rest is the body
}

func skipNumber(sql string, at int) int {
	n := len(sql)
	j := at
	if sql[j] == '0' && j+1 < n && (sql[j+1] == 'x' || sql[j+1] == 'X' || sql[j+1] == 'b' || sql[j+1] == 'B' || sql[j+1] == 'o' || sql[j+1] == 'O') {
		j += 2
		for j < n && (isWordByte(sql[j])) {
			j++
		}
		return j
	}
	for j < n && (sql[j] >= '0' && sql[j] <= '9' || sql[j] == '_') {
		j++
	}
	if j < n && sql[j] == '.' && !(j+1 < n && sql[j+1] == '.') {
		j++
		for j < n && (sql[j] >= '0' && sql[j] <= '9' || sql[j] == '_') {
			j++
		}
	}
	if j < n && (sql[j] == 'e' || sql[j] == 'E') {
		k := j + 1
		if k < n && (sql[k] == '+' || sql[k] == '-') {
			k++
		}
		if k < n && sql[k] >= '0' && sql[k] <= '9' {
			for k < n && sql[k] >= '0' && sql[k] <= '9' {
				k++
			}
			j = k
		}
	}
	return j
}

// collapseIn: IN (?, ?, ?) -> IN (?), so that a list of any length is one statement.
func collapseIn(s string) string {
	var out strings.Builder
	low := strings.ToLower(s)
	i := 0
	for i < len(s) {
		k := strings.Index(low[i:], "in")
		if k < 0 {
			out.WriteString(s[i:])
			break
		}
		k += i
		out.WriteString(s[i:k])
		j := k + 2
		boundary := k == 0 || !isWordByte(s[k-1])
		for j < len(s) && s[j] == ' ' {
			j++
		}
		if boundary && j < len(s) && s[j] == '(' {
			if end, ok := onlyPlaceholders(s, j); ok {
				out.WriteString(s[k : k+2])
				out.WriteString(" (?)")
				i = end
				continue
			}
		}
		out.WriteString(s[k : k+2])
		i = k + 2
	}
	return out.String()
}

// onlyPlaceholders reports whether the parenthesised list at s[at] is made only of ? and $n separated by commas, and where it ends.
func onlyPlaceholders(s string, at int) (int, bool) {
	j := at + 1
	count := 0
	for {
		for j < len(s) && s[j] == ' ' {
			j++
		}
		switch {
		case j < len(s) && s[j] == '?':
			j++
		case j < len(s) && s[j] == '$':
			j++
			d := j
			for j < len(s) && s[j] >= '0' && s[j] <= '9' {
				j++
			}
			if j == d {
				return 0, false
			}
		default:
			return 0, false
		}
		count++
		for j < len(s) && s[j] == ' ' {
			j++
		}
		if j < len(s) && s[j] == ',' {
			j++
			continue
		}
		if j < len(s) && s[j] == ')' {
			return j + 1, count > 1
		}
		return 0, false
	}
}

// collapseValues: VALUES (?, ?), (?, ?), (?, ?) -> VALUES (?, ?). A multi-row insert is one statement whatever the number of rows.
func collapseValues(s string) string {
	low := strings.ToLower(s)
	k := strings.Index(low, "values")
	if k < 0 {
		return s
	}
	i := k + len("values")
	var out strings.Builder
	out.WriteString(s[:i])
	prev := ""
	for {
		j := i
		for j < len(s) && s[j] == ' ' {
			j++
		}
		if j >= len(s) || s[j] != '(' {
			out.WriteString(s[i:])
			return out.String()
		}
		depth := 0
		e := j
		for e < len(s) {
			if s[e] == '(' {
				depth++
			} else if s[e] == ')' {
				depth--
				if depth == 0 {
					e++
					break
				}
			}
			e++
		}
		tuple := s[j:e]
		if tuple != prev {
			if prev != "" {
				out.WriteString(",")
			}
			out.WriteString(" " + tuple)
		}
		prev = tuple
		i = e
		for i < len(s) && s[i] == ' ' {
			i++
		}
		if i < len(s) && s[i] == ',' {
			i++
			continue
		}
		out.WriteString(" ")
		out.WriteString(s[i:])
		return strings.TrimRight(out.String(), " ")
	}
}

func truncate(s string, max int) string {
	if max <= 0 || len(s) <= max {
		return s
	}
	cut := max
	for cut > 0 && !utf8.RuneStart(s[cut]) {
		cut--
	}
	return s[:cut] + "..."
}
