package events

import (
	"fmt"
	"regexp"
	"strings"
)

// quote quotes an SQL identifier.
func quote(name string) string { return `"` + strings.ReplaceAll(name, `"`, `""`) + `"` }

// token is a word, a quoted identifier or a character of an SQL text, and
// where it is.
type token struct {
	text       string // a word as written; a quoted identifier unquoted
	start, end int
	ident      bool // a word or a quoted identifier
	quoted     bool
}

// tokens splits sql into tokens up to max of them, skipping whitespace,
// comments and string literals.
func tokens(sql string, max int) []token {
	var ts []token
	for i := 0; i < len(sql) && len(ts) < max; {
		c := sql[i]
		switch {
		case c == ' ' || c == '\t' || c == '\n' || c == '\r' || c == '\f':
			i++
		case strings.HasPrefix(sql[i:], "--"):
			if j := strings.IndexByte(sql[i:], '\n'); j >= 0 {
				i += j + 1
			} else {
				i = len(sql)
			}
		case strings.HasPrefix(sql[i:], "/*"):
			if j := strings.Index(sql[i+2:], "*/"); j >= 0 {
				i += j + 4
			} else {
				i = len(sql)
			}
		case c == '"' || c == '`' || c == '[' || c == '\'':
			end := c
			if c == '[' {
				end = ']'
			}
			var b strings.Builder
			j := i + 1
			for j < len(sql) {
				if sql[j] == end {
					if end != ']' && j+1 < len(sql) && sql[j+1] == end {
						b.WriteByte(end)
						j += 2
						continue
					}
					break
				}
				b.WriteByte(sql[j])
				j++
			}
			if c != '\'' {
				ts = append(ts, token{text: b.String(), start: i, end: min(j+1, len(sql)), ident: true, quoted: true})
			}
			i = j + 1
		case isWord(c):
			j := i
			for j < len(sql) && isWord(sql[j]) {
				j++
			}
			ts = append(ts, token{text: sql[i:j], start: i, end: j, ident: true})
			i = j
		default:
			ts = append(ts, token{text: sql[i : i+1], start: i, end: i + 1})
			i++
		}
	}
	return ts
}

func isWord(c byte) bool {
	return c == '_' || c == '$' || c >= '0' && c <= '9' || c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= 0x80
}

// keyword reports whether t is the bare word w, in any case.
func (t token) keyword(w string) bool { return t.ident && !t.quoted && strings.EqualFold(t.text, w) }

// created finds in a CREATE TABLE or CREATE INDEX statement the object's
// name and, for an index, its table's: each as the span of a name that may
// carry a schema ("main"."x").
func created(sql string) (kind string, name, table [2]int, err error) {
	ts := tokens(sql, 16)
	i := 0
	next := func(w string) bool {
		if i < len(ts) && ts[i].keyword(w) {
			i++
			return true
		}
		return false
	}
	qualified := func() ([2]int, bool) {
		if i >= len(ts) || !ts[i].ident {
			return [2]int{}, false
		}
		span := [2]int{ts[i].start, ts[i].end}
		i++
		if i+1 < len(ts) && ts[i].text == "." && !ts[i].ident && ts[i+1].ident {
			span[1] = ts[i+1].end
			i += 2
		}
		return span, true
	}
	if !next("CREATE") {
		return "", name, table, fmt.Errorf("events: not a CREATE statement: %.60s", sql)
	}
	next("TEMP")
	next("TEMPORARY")
	switch {
	case next("TABLE"):
		kind = "table"
	case next("VIRTUAL") && next("TABLE"):
		kind = "table"
	case next("UNIQUE") && next("INDEX"), next("INDEX"):
		kind = "index"
	default:
		return "", name, table, fmt.Errorf("events: a projection's tables have only tables and indexes, not %.60s", sql)
	}
	if next("IF") && !(next("NOT") && next("EXISTS")) {
		return "", name, table, fmt.Errorf("events: cannot read %.60s", sql)
	}
	var ok bool
	if name, ok = qualified(); !ok {
		return "", name, table, fmt.Errorf("events: no name in %.60s", sql)
	}
	if kind == "index" {
		if !next("ON") {
			return "", name, table, fmt.Errorf("events: no table in %.60s", sql)
		}
		if table, ok = qualified(); !ok {
			return "", name, table, fmt.Errorf("events: no table in %.60s", sql)
		}
	}
	return kind, name, table, nil
}

// versioned matches the suffix a rebuild gives the names of its copies.
var versioned = regexp.MustCompile(`__v[0-9]+$`)

// shadowName is the name of the copy of table or index name that a
// rebuild to version fills.
func shadowName(name string, version int) string {
	return versioned.ReplaceAllString(name, "") + fmt.Sprintf("__v%d", version)
}

// shadowDDL rewrites the CREATE statement of a projection's table or of
// one of its indexes to create its copy for a rebuild: the table named
// copy, an index renamed for version and put on the copy.
func shadowDDL(sql, copy string, version int) (string, error) {
	kind, name, table, err := created(sql)
	if err != nil {
		return "", err
	}
	var b strings.Builder
	b.WriteString(sql[:name[0]])
	if kind == "table" {
		b.WriteString(quote(copy))
		b.WriteString(sql[name[1]:])
		return b.String(), nil
	}
	ts := tokens(sql[name[0]:name[1]], 3)
	b.WriteString(quote(shadowName(ts[len(ts)-1].text, version))) // the name without its schema
	b.WriteString(sql[name[1]:table[0]])
	b.WriteString(quote(copy))
	b.WriteString(sql[table[1]:])
	return b.String(), nil
}
