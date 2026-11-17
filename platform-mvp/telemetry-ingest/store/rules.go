package store

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/sojoner/kro-lab/platform-mvp/telemetry-ingest/rules"
)

// Rules is the saved rules in ClickHouse: what rules.Manager needs from a database. Obtain it with (*Store).Rules.
type Rules struct{ s *Store }

func (s *Store) Rules() *Rules { return &Rules{s} }

// ErrRuleVersionTaken means that version of the rule is already kept. The Manager writes one change at a time, so this is a second writer (a second
// replica of the service) getting in. As with the incident log the check and the insert are not one atomic step, so run a single replica.
var ErrRuleVersionTaken = errors.New("that version of the rule is already recorded")

func (r *Rules) Append(ctx context.Context, project string, rule rules.Rule) error {
	var n uint64
	if err := r.s.conn.QueryRow(ctx, "SELECT count() FROM rule_versions WHERE Project = ? AND Name = ? AND Version = ?", project, rule.Name, rule.Version).Scan(&n); err != nil {
		return err
	}
	if n > 0 {
		return ErrRuleVersionTaken
	}
	data, err := json.Marshal(rule.Settings)
	if err != nil {
		return err
	}
	return insert(ctx, r.s, "INSERT INTO rule_versions (Project, Name, Version, UpdatedAt, UpdatedBy, Settings)", []rules.Rule{rule},
		func(x rules.Rule) []any {
			return []any{project, x.Name, x.Version, x.UpdatedAt, x.UpdatedBy, string(data)}
		})
}

func (r *Rules) Latest(ctx context.Context, project string) ([]rules.Rule, error) {
	rows, err := r.s.conn.Query(ctx, "SELECT Name, Version, UpdatedAt, UpdatedBy, Settings FROM rule_versions WHERE Project = ? ORDER BY Name, Version DESC LIMIT 1 BY Name", project)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []rules.Rule
	for rows.Next() {
		var x rules.Rule
		var at time.Time
		var data string
		if err := rows.Scan(&x.Name, &x.Version, &at, &x.UpdatedBy, &data); err != nil {
			return nil, err
		}
		if err := json.Unmarshal([]byte(data), &x.Settings); err != nil {
			return nil, fmt.Errorf("rule %s version %d does not read: %w", x.Name, x.Version, err)
		}
		x.UpdatedAt = at.UTC()
		out = append(out, x)
	}
	return out, rows.Err()
}
