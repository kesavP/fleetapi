package collector

import (
	"fmt"
	"math/rand"
	"strings"
	"testing"
)

func TestNormalize(t *testing.T) {
	cases := []struct{ name, in, want string }{
		{"numbers and strings", `SELECT * FROM users WHERE id = 42 AND name = 'bob'`, `SELECT * FROM users WHERE id = ? AND name = ?`},
		{"escaped quote", `SELECT 'it''s'`, `SELECT ?`},
		{"parameters stay", `SELECT * FROM t WHERE a = $1 AND b = $12`, `SELECT * FROM t WHERE a = $1 AND b = $12`},
		{"white space", "SELECT   a,\n\tb\r\n FROM   t  ", `SELECT a, b FROM t`},
		{"line comment", "SELECT 1 -- the secret is hunter2\nFROM t", `SELECT ? FROM t`},
		{"block comment", `SELECT /* traceparent=00-abc */ 1`, `SELECT ?`},
		{"nested block comment", `SELECT /* a /* b */ still comment */ 1`, `SELECT ?`},
		{"quoted identifier kept", `SELECT "Weird Col", "a""b" FROM "T"`, `SELECT "Weird Col", "a""b" FROM "T"`},
		{"identifier digits kept", `SELECT t1.col2, x_3 FROM t1`, `SELECT t1.col2, x_3 FROM t1`},
		{"decimals and exponents", `SELECT 1.5, .5, 3., 1e10, 2.5E-3, 1_000`, `SELECT ?, ?, ?, ?, ?, ?`},
		{"hex and binary", `SELECT 0x1F, 0b101, 0o17`, `SELECT ?, ?, ?`},
		{"E string with backslash quote", `SELECT E'it\'s a secret', 1`, `SELECT ?, ?`},
		{"bit and hex strings", `SELECT B'1010', X'DEADBEEF', N'text', U&'\0041'`, `SELECT ?, ?, ?, ?`},
		{"dollar quoted", `SELECT $$a'secret'b$$, $tag$ x $$ y $tag$`, `SELECT ?, ?`},
		{"dollar quoted never closed", `SELECT $q$ the rest is secret`, `SELECT ?`},
		{"IN list", `SELECT * FROM t WHERE id IN (1, 2, 3, 4)`, `SELECT * FROM t WHERE id IN (?)`},
		{"IN list of params", `WHERE id IN ($1, $2, $3)`, `WHERE id IN ($1, $2, $3)`[:0] + `WHERE id IN (?)`},
		{"IN with one item stays", `WHERE id IN (7)`, `WHERE id IN (?)`},
		{"IN subquery stays", `WHERE id IN (SELECT id FROM u WHERE x = 1)`, `WHERE id IN (SELECT id FROM u WHERE x = ?)`},
		{"a word ending in in", `SELECT main (1, 2), "in" (3, 4)`, `SELECT main (?, ?), "in" (?, ?)`},
		{"multi row values", `INSERT INTO t (a, b) VALUES (1, 'x'), (2, 'y'), (3, 'z')`, `INSERT INTO t (a, b) VALUES (?, ?)`},
		{"values with a different tuple stays", `INSERT INTO t (a) VALUES (1), (now())`, `INSERT INTO t (a) VALUES (?), (now())`},
		{"values then returning", `INSERT INTO t VALUES (1, 2), (3, 4) RETURNING id`, `INSERT INTO t VALUES (?, ?) RETURNING id`},
		{"casts and operators", `SELECT a::int + 1, b->>'k', c @> '{"x":1}'`, `SELECT a::int + ?, b->>?, c @> ?`},
		{"password in a utility statement", `CREATE ROLE leaky LOGIN PASSWORD 'hunter2'`, `CREATE ROLE leaky LOGIN PASSWORD ?`},
		{"unterminated string, as a cut text is", `SELECT * FROM t WHERE email = 'alice@exa`, `SELECT * FROM t WHERE email = ?`},
		{"empty", ``, ``},
		{"only a comment", `-- nothing`, ``},
		{"unicode kept", `SELECT "naïve", ünï FROM t WHERE x = 'ß'`, `SELECT "naïve", ünï FROM t WHERE x = ?`},
		{"a literal glued to a keyword", `SELECT'a'FROM t`, `SELECT?FROM t`},
		{"an array slice", `SELECT a[1:2], a[n]`, `SELECT a[?:?], a[n]`},
	}
	for _, c := range cases {
		if got := Normalize(c.in, 0); got != c.want {
			t.Errorf("%s:\n  in   %q\n  got  %q\n  want %q", c.name, c.in, got, c.want)
		}
	}
}

func TestNormalizeTruncatesAtACharacterBoundary(t *testing.T) {
	got := Normalize("SELECT "+strings.Repeat("ä", 100)+" FROM t", 20)
	if !strings.HasSuffix(got, "...") {
		t.Fatalf("%q", got)
	}
	for _, r := range got {
		if r == '\uFFFD' {
			t.Fatalf("cut a character in half: %q", got)
		}
	}
	if len(got) > 23 {
		t.Fatalf("%d bytes", len(got))
	}
	if short := Normalize("SELECT 1", 100); short != "SELECT ?" {
		t.Fatal(short)
	}
}

func TestNormalizeIsIdempotent(t *testing.T) {
	for _, q := range []string{`SELECT * FROM t WHERE a = 1 AND b IN (1,2,3)`, `INSERT INTO t VALUES (1,2),(3,4)`, `SELECT $1, "x"`, `SELECT /* c */ 'a'`} {
		once := Normalize(q, 0)
		if twice := Normalize(once, 0); twice != once {
			t.Errorf("%q -> %q -> %q", q, once, twice)
		}
	}
}

// The property that matters: whatever a secret is wrapped in, it is not in the result.
func TestNormalizeNeverLetsASecretThrough(t *testing.T) {
	wraps := []string{
		`SELECT * FROM t WHERE x = '%s'`,
		`SELECT * FROM t WHERE x = E'%s'`,
		`SELECT * FROM t WHERE x = $$%s$$`,
		`SELECT * FROM t WHERE x = $a$%s$a$`,
		`SELECT * FROM t WHERE x IN ('%s', 'other')`,
		`INSERT INTO t VALUES ('%s'), ('%s')`,
		`SELECT 1 -- %s`,
		`SELECT /* %s */ 1`,
		`SELECT /* a /* %s */ b */ 1`,
		`CREATE ROLE r PASSWORD '%s'`,
		`SELECT * FROM t WHERE x = '%s`, // cut off mid-literal
		`SELECT * FROM t WHERE x = '%s' AND y = 1`,
		`SELECT U&'%s'`,
		`SELECT X'%s'`,
		`SELECT $$ %s`,
		`SELECT * FROM t WHERE a = 'x' || '%s' || 'y'`,
		`SELECT * FROM t WHERE a = %s`, // a number
	}
	rng := rand.New(rand.NewSource(1))
	for i := 0; i < 400; i++ {
		var secret string
		if i%5 == 0 {
			secret = fmt.Sprintf("%d%d", rng.Intn(1e9)+1e8, rng.Intn(1e9)) // a number: digits only
		} else {
			secret = fmt.Sprintf("S3cr%dT-%x@x.io", rng.Intn(1e6), rng.Uint64())
		}
		for _, w := range wraps {
			if strings.Contains(w, "= %s") && i%5 != 0 {
				continue // a bare word is an identifier, not a literal
			}
			q := strings.ReplaceAll(w, "%s", secret)
			if got := Normalize(q, 0); strings.Contains(got, secret) || strings.Contains(got, secret[:6]) {
				t.Fatalf("secret %q survived in %q: %q", secret, q, got)
			}
		}
	}
}

func TestNormalizeHandlesHostileInput(t *testing.T) {
	// none of these may panic or loop: an unterminated thing of every kind, and bytes that are not UTF-8
	for _, q := range []string{`'`, `"`, `$`, `$$`, `$a`, `$a$`, `/*`, `/* /*`, `--`, `E'\`, `U&`, `0x`, `1e`, `1e+`, `(`, `)`, `IN (`, `IN (?, `, `VALUES (`, `VALUES ((`, `VALUES (?), (`,
		"\xff\xfe", "SELECT '\xff'", strings.Repeat("(", 1000), strings.Repeat("'", 1001), strings.Repeat("/*", 500), strings.Repeat("a'", 5000)} {
		_ = Normalize(q, 0)
	}
	rng := rand.New(rand.NewSource(2))
	alphabet := []byte("'\"$ -/*()0123456789.eEuU&xXbB,?abc\n\\")
	for i := 0; i < 3000; i++ {
		b := make([]byte, rng.Intn(40))
		for j := range b {
			b[j] = alphabet[rng.Intn(len(alphabet))]
		}
		_ = Normalize(string(b), 64)
	}
}
