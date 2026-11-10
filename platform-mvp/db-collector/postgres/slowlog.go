package postgres

import (
	"bufio"
	"context"
	"errors"
	"io"
	"os"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/sojoner/kro-lab/platform-mvp/db-collector/collector"
)

// What a slow statement looks like in PostgreSQL's server log (log_destination = stderr, the default), with log_min_duration_statement set:
//
//	2026-11-10 06:18:07.627 UTC [107] LOG:  duration: 301.604 ms  statement: SELECT pg_sleep(0.15), 'x'
//	<TAB>  FROM generate_series(1,2)
//	2026-11-10 06:19:01.100 UTC [118] LOG:  duration: 5.0 ms  execute <unnamed>: SELECT * FROM t WHERE id = $1
//	2026-11-10 06:19:01.100 UTC [118] DETAIL:  parameters: $1 = '42'
//
// A record begins at a line that does not start with a tab (a statement of several lines continues on lines that do). Only "statement" and
// "execute" lines are slow statements: "parse" and "bind" are phases of one, "plan:" lines belong to auto_explain, and a line that says only
// "duration: 1.2 ms" (log_duration) has no statement. DETAIL lines (the parameters of a statement) are never read: they are where the
// values are.
var slowRE = regexp.MustCompile(`^(?P<prefix>.*?)\b(?:LOG|DEBUG[1-5]?|INFO|NOTICE):\s+duration:\s+(?P<ms>[0-9]+(?:\.[0-9]+)?)\s+ms\s+(?P<kind>statement|execute\s[^:]*|parse\s[^:]*|bind\s[^:]*):\s?(?P<sql>.*)$`)

var (
	pidRE       = regexp.MustCompile(`\[(\d+)\]`)
	userKVRE    = regexp.MustCompile(`\buser=([^\s,]+)`)
	dbKVRE      = regexp.MustCompile(`\bdb=([^\s,]+)`)
	userAtDBRE  = regexp.MustCompile(`(?:^|\s)([^\s@\[\]]+)@([^\s@\[\]]+)\s*$`)
	timestampRE = regexp.MustCompile(`^(\d{4}-\d{2}-\d{2} \d{2}:\d{2}:\d{2}(?:\.\d+)?) (\S+)`)
)

const (
	maxRecordBytes = 64 << 10 // a statement longer than this is cut: the normalizer makes it shorter still
	maxLineBytes   = 64 << 10 // a longer line is cut here (the rest is read and ignored): a statement that long is already cut by the normalizer
	// marker is the comment every query of the collector starts with, so that the collector's own statements are not reported as slow.
	marker = "/* db-collector */"
)

// LogParser assembles records from lines, in order, and reports the slow statements among them. It is not safe for concurrent use.
type LogParser struct {
	// Now is the time to give a statement whose line has no usable time (a log_timezone that is not UTC or a numeric offset); defaults to time.Now.
	Now func() time.Time

	pending []string
	size    int
}

func (p *LogParser) now() time.Time {
	if p.Now != nil {
		return p.Now()
	}
	return time.Now()
}

// Feed adds one line (without its newline). The record before it is complete when this line starts a new one, and is reported then.
func (p *LogParser) Feed(line string, emit func(collector.SlowQuery)) {
	if len(line) > 0 && (line[0] == '\t') {
		if len(p.pending) > 0 && p.size < maxRecordBytes {
			p.pending = append(p.pending, line[1:])
			p.size += len(line)
		}
		return
	}
	p.Flush(emit)
	p.pending = append(p.pending[:0], line)
	p.size = len(line)
}

// Flush reports the record being assembled, if any: at the end of the input, or when the log has been quiet long enough to be sure it is whole.
func (p *LogParser) Flush(emit func(collector.SlowQuery)) {
	if len(p.pending) == 0 {
		return
	}
	text := strings.Join(p.pending, "\n")
	p.pending = p.pending[:0]
	p.size = 0
	if q, ok := p.parse(text); ok {
		emit(q)
	}
}

func (p *LogParser) parse(record string) (collector.SlowQuery, bool) {
	first := record
	if i := strings.IndexByte(record, '\n'); i >= 0 {
		first = record[:i]
	}
	m := slowRE.FindStringSubmatch(first)
	if m == nil {
		return collector.SlowQuery{}, false
	}
	prefix, ms, kind, sql := m[1], m[2], m[3], m[4]
	switch {
	case kind == "statement":
	case strings.HasPrefix(kind, "execute"):
		kind = "execute"
	default:
		return collector.SlowQuery{}, false // parse and bind are phases of a statement, not statements
	}
	if i := strings.IndexByte(record, '\n'); i >= 0 {
		sql += record[i:]
	}
	if strings.HasPrefix(strings.TrimSpace(sql), marker) {
		return collector.SlowQuery{}, false // the collector's own query
	}
	dur, err := strconv.ParseFloat(ms, 64)
	if err != nil {
		return collector.SlowQuery{}, false
	}
	q := collector.SlowQuery{DurationMs: dur, Kind: kind, Query: collector.Normalize(sql, collector.MaxQueryBytes), Time: p.timeOf(prefix)}
	if q.Query == "" {
		return collector.SlowQuery{}, false
	}
	if pm := pidRE.FindStringSubmatch(prefix); pm != nil {
		q.PID, _ = strconv.ParseInt(pm[1], 10, 64)
	}
	q.User, q.Database = userAndDB(prefix)
	return q, true
}

func userAndDB(prefix string) (user, db string) {
	if m := userKVRE.FindStringSubmatch(prefix); m != nil {
		user = m[1]
	}
	if m := dbKVRE.FindStringSubmatch(prefix); m != nil {
		db = m[1]
	}
	if user == "" && db == "" {
		if m := userAtDBRE.FindStringSubmatch(prefix); m != nil {
			user, db = m[1], m[2]
		}
	}
	if user == "[unknown]" {
		user = ""
	}
	if db == "[unknown]" {
		db = ""
	}
	return user, db
}

// timeOf reads the time at the front of a line's prefix (%m or %t). Only UTC and numeric offsets can be read exactly: another zone's name
// ("CEST") does not say what offset it has, and guessing would put the statement hours away from when it ran, so a line in one of those has
// the time it was read.
func (p *LogParser) timeOf(prefix string) time.Time {
	m := timestampRE.FindStringSubmatch(prefix)
	if m == nil {
		return p.now().UTC()
	}
	zone := m[2]
	layout := "2006-01-02 15:04:05"
	if strings.Contains(m[1], ".") {
		layout = "2006-01-02 15:04:05.999999999"
	}
	var t time.Time
	var err error
	switch {
	case zone == "UTC" || zone == "GMT":
		t, err = time.ParseInLocation(layout, m[1], time.UTC)
	case len(zone) >= 3 && (zone[0] == '+' || zone[0] == '-'):
		off, ok := parseOffset(zone)
		if !ok {
			return p.now().UTC()
		}
		t, err = time.ParseInLocation(layout, m[1], time.FixedZone("", off))
	default:
		return p.now().UTC()
	}
	if err != nil {
		return p.now().UTC()
	}
	return t.UTC()
}

func parseOffset(z string) (int, bool) {
	sign := 1
	if z[0] == '-' {
		sign = -1
	}
	digits := z[1:]
	var h, m int
	var err error
	switch len(digits) {
	case 2:
		h, err = strconv.Atoi(digits)
	case 4:
		if h, err = strconv.Atoi(digits[:2]); err == nil {
			m, err = strconv.Atoi(digits[2:])
		}
	default:
		return 0, false
	}
	if err != nil || h > 14 || m > 59 {
		return 0, false
	}
	return sign * (h*3600 + m*60), true
}

// Tailer follows a log file the way tail -F does: from the end (or the start), across rotation (the path now names another file) and
// truncation (the file is shorter than where it was read to), waiting for the file to appear. It polls, which keeps it free of platform
// specifics and is plenty for a log.
type Tailer struct {
	Path      string
	FromStart bool
	Poll      time.Duration // how often to look for more; default 500 ms
	// Quiet is how long the log must be silent before the last record is taken as complete; default one second.
	Quiet time.Duration
	Now   func() time.Time
	// OnError is told about what could not be read, once per kind of trouble until it clears (so a missing file is one message, not one a poll).
	OnError func(error)
}

// Follow reads lines, passes them to a parser and emits what it finds, until ctx ends.
func (t *Tailer) Follow(ctx context.Context, emit func(collector.SlowQuery)) error {
	poll, quiet := t.Poll, t.Quiet
	if poll <= 0 {
		poll = 500 * time.Millisecond
	}
	if quiet <= 0 {
		quiet = time.Second
	}
	parser := &LogParser{Now: t.Now}
	var (
		f        *os.File
		info     os.FileInfo
		offset   int64
		partial  []byte
		cut      bool // the line being read is longer than maxLineBytes: the rest of it is read and thrown away
		lastRead = time.Now()
		first    = true
		failing  bool
	)
	// A file that is not there yet when the follower starts is new whenever it appears, and all of it is read: only a file that was already
	// there is followed from its end.
	if _, err := os.Stat(t.Path); err != nil {
		first = false
	}
	closeFile := func() {
		if f != nil {
			f.Close()
			f = nil
		}
	}
	defer closeFile()
	note := func(err error) {
		if err != nil && !failing && t.OnError != nil {
			t.OnError(err)
		}
		failing = err != nil
	}
	tick := time.NewTicker(poll)
	defer tick.Stop()
	for {
		select {
		case <-ctx.Done():
			parser.Flush(emit)
			return nil
		case <-tick.C:
		}
		cur, err := os.Stat(t.Path)
		if err != nil {
			note(err)
			closeFile()
			continue
		}
		if f == nil || !os.SameFile(info, cur) || cur.Size() < offset {
			closeFile()
			nf, err := os.Open(t.Path)
			if err != nil {
				note(err)
				continue
			}
			f, info, partial, cut = nf, cur, nil, false
			switch {
			case first && !t.FromStart:
				offset, err = f.Seek(0, io.SeekEnd) // follow from here
				if err != nil {
					note(err)
					closeFile()
					continue
				}
			default:
				offset = 0 // a new file after a rotation is read from its start
			}
			first = false
		}
		note(nil)
		if cur.Size() == offset {
			if time.Since(lastRead) >= quiet {
				parser.Flush(emit)
			}
			continue
		}
		if _, err := f.Seek(offset, io.SeekStart); err != nil {
			note(err)
			closeFile()
			continue
		}
		r := bufio.NewReaderSize(io.LimitReader(f, cur.Size()-offset), 64<<10)
		for {
			// ReadSlice never holds more than the buffer, so a huge line is never all in memory at once
			chunk, err := r.ReadSlice('\n')
			offset += int64(len(chunk))
			if len(chunk) > 0 {
				lastRead = time.Now()
				endsLine := chunk[len(chunk)-1] == '\n'
				body := chunk
				if endsLine {
					body = chunk[:len(chunk)-1]
				}
				if !cut {
					if room := maxLineBytes - len(partial); len(body) > room {
						body, cut = body[:room], true
					}
					partial = append(partial, body...)
				}
				if endsLine {
					parser.Feed(strings.TrimSuffix(string(partial), "\r"), emit)
					partial, cut = nil, false
				}
			}
			if err != nil && !errors.Is(err, bufio.ErrBufferFull) {
				if !errors.Is(err, io.EOF) {
					note(err)
				}
				break
			}
		}
	}
}
