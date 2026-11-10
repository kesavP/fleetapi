package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"time"

	"github.com/sojoner/kro-lab/platform-mvp/telemetry-ingest/incident"
)

// Incidents is the incident log in ClickHouse: what incident.Manager needs from a database. Obtain it with (*Store).Incidents.
type Incidents struct{ s *Store }

func (s *Store) Incidents() *Incidents { return &Incidents{s} }

// ErrSequenceTaken means an event with that sequence number is already in the log. The Manager writes one change at a time, so this is a
// second writer (a second replica of the service) getting in: refusing here keeps one history. The check and the insert are not one atomic
// step, so it narrows the window and does not close it: run a single replica.
var ErrSequenceTaken = errors.New("an event with that sequence number is already recorded")

func isNoRows(err error) bool { return errors.Is(err, sql.ErrNoRows) }

func (i *Incidents) Append(ctx context.Context, ev incident.Event) error {
	var n uint64
	if err := i.s.conn.QueryRow(ctx, "SELECT count() FROM incident_events WHERE Project = ? AND IncidentID = ? AND Seq = ?", ev.Project, ev.IncidentID, ev.Seq).Scan(&n); err != nil {
		return err
	}
	if n > 0 {
		return ErrSequenceTaken
	}
	return insert(ctx, i.s, "INSERT INTO incident_events (Project, IncidentID, Seq, EventID, Time, Type, Actor, DedupeKey, Data)", []incident.Event{ev},
		func(e incident.Event) []any {
			return []any{e.Project, e.IncidentID, e.Seq, e.ID, e.Time, string(e.Type), e.Actor, e.DedupeKey, string(e.Data)}
		})
}

const eventColumns = "IncidentID, Seq, EventID, Time, Type, Actor, DedupeKey, Data"

type scanner interface{ Scan(dest ...any) error }

func scanEvent(project string, r scanner) (incident.Event, error) {
	var e incident.Event
	var typ, data string
	var at time.Time
	if err := r.Scan(&e.IncidentID, &e.Seq, &e.ID, &at, &typ, &e.Actor, &e.DedupeKey, &data); err != nil {
		return e, err
	}
	e.Project, e.Time, e.Type, e.Data = project, at.UTC(), incident.EventType(typ), json.RawMessage(data)
	return e, nil
}

func (i *Incidents) Events(ctx context.Context, project, id string) ([]incident.Event, error) {
	rows, err := i.s.conn.Query(ctx, "SELECT "+eventColumns+" FROM incident_events WHERE Project = ? AND IncidentID = ? ORDER BY Seq", project, id)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []incident.Event
	for rows.Next() {
		e, err := scanEvent(project, rows)
		if err != nil {
			return nil, err
		}
		out = append(out, e)
	}
	return out, rows.Err()
}

func (i *Incidents) Recent(ctx context.Context, project string, limit int) ([][]incident.Event, error) {
	ids, err := i.s.conn.Query(ctx, "SELECT IncidentID FROM incident_events WHERE Project = ? AND Seq = 1 ORDER BY Time DESC, IncidentID LIMIT ?", project, limit)
	if err != nil {
		return nil, err
	}
	var order []string
	for ids.Next() {
		var id string
		if err := ids.Scan(&id); err != nil {
			ids.Close()
			return nil, err
		}
		order = append(order, id)
	}
	if err := ids.Err(); err != nil {
		return nil, err
	}
	ids.Close()
	if len(order) == 0 {
		return nil, nil
	}
	rows, err := i.s.conn.Query(ctx, "SELECT "+eventColumns+" FROM incident_events WHERE Project = ? AND IncidentID IN (?) ORDER BY IncidentID, Seq", project, order)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	by := map[string][]incident.Event{}
	for rows.Next() {
		e, err := scanEvent(project, rows)
		if err != nil {
			return nil, err
		}
		by[e.IncidentID] = append(by[e.IncidentID], e)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	out := make([][]incident.Event, 0, len(order))
	for _, id := range order {
		if evs := by[id]; len(evs) > 0 {
			out = append(out, evs)
		}
	}
	return out, nil
}

func (i *Incidents) FindByDedupeKey(ctx context.Context, project, key string) (string, error) {
	var id string
	err := i.s.conn.QueryRow(ctx, "SELECT IncidentID FROM incident_events WHERE Project = ? AND Seq = 1 AND DedupeKey = ? ORDER BY Time LIMIT 1", project, key).Scan(&id)
	if err != nil {
		if isNoRows(err) {
			return "", nil
		}
		return "", err
	}
	return id, nil
}
