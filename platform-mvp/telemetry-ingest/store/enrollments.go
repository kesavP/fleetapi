package store

import (
	"context"
	"database/sql"
	"errors"
	"time"

	"github.com/sojoner/kro-lab/platform-mvp/telemetry-ingest/enroll"
)

// Enrollments is the enrollment log in ClickHouse (see the enroll package). It is one table of events; nothing is updated in place.
type Enrollments struct{ s *Store }

func (s *Store) Enrollments() *Enrollments { return &Enrollments{s} }

func (e *Enrollments) Append(ctx context.Context, ev enroll.Event) error {
	exp := ev.ExpiresAt
	if exp.IsZero() {
		exp = time.Unix(0, 0)
	}
	return insert(ctx, e.s, "INSERT INTO enrollment_events (Project, ID, Time, Type, Actor, Cluster, TokenHash, ExpiresAt, KeyHash)", []enroll.Event{ev},
		func(ev enroll.Event) []any {
			return []any{ev.Project, ev.ID, ev.Time.UTC(), string(ev.Type), ev.Actor, ev.Cluster, ev.TokenHash, exp.UTC(), ev.KeyHash}
		})
}

// the order of events that happen in the same millisecond: a redemption cannot come before its creation
const eventOrder = "Time, multiIf(Type = 'created', 0, Type = 'redeemed', 1, 2)"

func (e *Enrollments) scan(ctx context.Context, query string, args ...any) ([]enroll.Event, error) {
	rows, err := e.s.conn.Query(ctx, query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []enroll.Event
	for rows.Next() {
		var ev enroll.Event
		var typ string
		if err := rows.Scan(&ev.Project, &ev.ID, &ev.Time, &typ, &ev.Actor, &ev.Cluster, &ev.TokenHash, &ev.ExpiresAt, &ev.KeyHash); err != nil {
			return nil, err
		}
		ev.Type = enroll.EventType(typ)
		ev.Time, ev.ExpiresAt = ev.Time.UTC(), ev.ExpiresAt.UTC()
		if ev.ExpiresAt.Unix() == 0 {
			ev.ExpiresAt = time.Time{}
		}
		out = append(out, ev)
	}
	return out, rows.Err()
}

const cols = "Project, ID, Time, Type, Actor, Cluster, TokenHash, ExpiresAt, KeyHash"

func (e *Enrollments) EventsOf(ctx context.Context, project, id string) ([]enroll.Event, error) {
	return e.scan(ctx, "SELECT "+cols+" FROM enrollment_events WHERE Project = ? AND ID = ? ORDER BY "+eventOrder, project, id)
}

func (e *Enrollments) Project(ctx context.Context, project string) ([]enroll.Event, error) {
	return e.scan(ctx, "SELECT "+cols+" FROM enrollment_events WHERE Project = ? ORDER BY "+eventOrder, project)
}

func (e *Enrollments) All(ctx context.Context) ([]enroll.Event, error) {
	return e.scan(ctx, "SELECT "+cols+" FROM enrollment_events ORDER BY "+eventOrder)
}

func (e *Enrollments) FindByToken(ctx context.Context, tokenHash string) (string, string, bool, error) {
	var project, id string
	err := e.s.conn.QueryRow(ctx, "SELECT Project, ID FROM enrollment_events WHERE Type = 'created' AND TokenHash = ? LIMIT 1", tokenHash).Scan(&project, &id)
	if errors.Is(err, sql.ErrNoRows) {
		return "", "", false, nil
	}
	if err != nil {
		return "", "", false, err
	}
	return project, id, true, nil
}
