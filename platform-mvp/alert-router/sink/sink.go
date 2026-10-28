// Package sink holds the destinations an alert can be delivered to. Today
// there is one, the text log; the triage agent will be another Sink.
package sink

import (
	"context"
	"log/slog"
	"time"

	"github.com/sojoner/kro-lab/platform-mvp/alert-router/model"
)

// Sink delivers a batch of alerts somewhere.
type Sink interface {
	Name() string
	Send(ctx context.Context, alerts []model.Alert) error
}

// Log writes one text line per alert. Values pass through slog's text
// handler, which quotes anything containing spaces, quotes or newlines, so a
// hostile VmSet name cannot forge extra fields or lines.
type Log struct{ Logger *slog.Logger }

func (Log) Name() string { return "log" }

func (l Log) Send(ctx context.Context, alerts []model.Alert) error {
	for _, a := range alerts {
		args := []any{"alert", a.Name, "status", a.Status}
		for _, kv := range [][2]string{
			{"severity", a.Severity}, {"tenant", a.Tenant}, {"vmset", a.VmSet}, {"region", a.Region}, {"cluster", a.Cluster},
			{"summary", a.Summary}, {"description", a.Description},
		} {
			if kv[1] != "" {
				args = append(args, kv[0], kv[1])
			}
		}
		args = append(args, "started", a.StartsAt.UTC().Format(time.RFC3339), "fingerprint", a.Fingerprint)
		if !a.EndsAt.IsZero() {
			args = append(args, "ended", a.EndsAt.UTC().Format(time.RFC3339))
		}
		l.Logger.Log(ctx, level(a), "alert notification", args...)
	}
	return nil
}

// level makes firing alerts stand out: warning for most, error for critical.
func level(a model.Alert) slog.Level {
	switch {
	case a.Status == model.StatusResolved:
		return slog.LevelInfo
	case a.Severity == "critical":
		return slog.LevelError
	default:
		return slog.LevelWarn
	}
}
