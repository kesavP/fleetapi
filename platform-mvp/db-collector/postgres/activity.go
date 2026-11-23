package postgres

import (
	"sort"
	"time"

	"github.com/sojoner/kro-lab/platform-mvp/db-collector/collector"
)

// ActivityQuery reads the client sessions. now() comes from the server, so how long something has been running does not depend on the
// collector's clock. The query text is cut here and normalized in BuildActivity; sessions the role may not see show "<insufficient privilege>".
func ActivityQuery(textBytes int) string {
	return `/* db-collector */ SELECT now(), a.pid::bigint, COALESCE(a.datname, ''), COALESCE(a.usename, ''), COALESCE(a.application_name, ''), COALESCE(a.state, ''),
COALESCE(a.wait_event_type, ''), COALESCE(a.wait_event, ''), a.query_start, a.xact_start, a.state_change, left(COALESCE(a.query, ''), ` + itoa(textBytes) + `), pg_blocking_pids(a.pid)::bigint[]
FROM pg_stat_activity a WHERE a.pid <> pg_backend_pid() AND a.backend_type = 'client backend'`
}

func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	var b []byte
	for ; n > 0; n /= 10 {
		b = append([]byte{byte('0' + n%10)}, b...)
	}
	return string(b)
}

// activityRow is one session, as it is scanned.
type activityRow struct {
	Now                         time.Time
	PID                         int64
	Database, User, Application string
	State, WaitType, Wait       string
	QueryStart, XactStart       *time.Time
	StateChange                 *time.Time
	Query                       string
	BlockedBy                   []int64
}

// slowSession is how long a statement must have been running, or a session idle in a transaction, to be listed by name. Counts include
// everything; the list is for the few that someone would want to look at.
const slowSession = time.Second

const insufficient = "<insufficient privilege>"

// BuildActivity reduces sessions to a sample: counts, the longest of each kind of wait, and a short list of the sessions worth naming (the
// longest running, the idle in transaction for long, the blocked, and the sessions that block others even if they are quick).
func BuildActivity(rows []activityRow, maxConnections, top int) collector.Activity {
	a := collector.Activity{ByState: map[string]int{}, Waiting: map[string]int{}, MaxConnections: maxConnections}
	blockers := map[int64]bool{}
	for _, r := range rows {
		for _, b := range r.BlockedBy {
			blockers[b] = true
		}
	}
	var listed []collector.Session
	for _, r := range rows {
		a.Total++
		state := r.State
		if state == "" {
			state = "unknown"
		}
		a.ByState[state]++

		s := collector.Session{PID: r.PID, Database: r.Database, User: r.User, Application: r.Application, State: state, WaitType: r.WaitType, Wait: r.Wait, BlockedBy: r.BlockedBy}
		if r.XactStart != nil {
			s.XactMs = ms(r.Now.Sub(*r.XactStart))
			if s.XactMs > a.LongestXactMs {
				a.LongestXactMs = s.XactMs
			}
		}
		switch {
		case state == "active":
			if r.QueryStart != nil {
				s.QueryMs = ms(r.Now.Sub(*r.QueryStart))
			}
			if s.QueryMs > a.LongestQueryMs {
				a.LongestQueryMs = s.QueryMs
			}
			if r.WaitType != "" {
				a.Waiting[r.WaitType+":"+r.Wait]++
			}
		case state == "idle in transaction" || state == "idle in transaction (aborted)":
			if r.StateChange != nil {
				s.QueryMs = ms(r.Now.Sub(*r.StateChange)) // how long it has sat idle in the transaction
			}
			if s.QueryMs > a.LongestIdleXMs {
				a.LongestIdleXMs = s.QueryMs
			}
		}
		if len(r.BlockedBy) > 0 {
			a.Blocked++
		}
		interesting := len(r.BlockedBy) > 0 || blockers[r.PID] ||
			(state == "active" || state == "idle in transaction" || state == "idle in transaction (aborted)") && s.QueryMs >= ms(slowSession)
		if interesting {
			if r.Query != insufficient {
				s.Query = collector.Normalize(r.Query, collector.MaxQueryBytes)
			}
			listed = append(listed, s)
		}
	}
	// the sessions that block others first (they are the cause), then the longest
	sort.Slice(listed, func(i, j int) bool {
		bi, bj := blockers[listed[i].PID], blockers[listed[j].PID]
		if bi != bj {
			return bi
		}
		if listed[i].QueryMs != listed[j].QueryMs {
			return listed[i].QueryMs > listed[j].QueryMs
		}
		return listed[i].PID < listed[j].PID
	})
	if top <= 0 || top > collector.MaxSessions {
		top = collector.MaxSessions
	}
	if len(listed) > top {
		listed = listed[:top]
	}
	a.Sessions = listed
	return a
}

func ms(d time.Duration) float64 { return float64(d) / float64(time.Millisecond) }
