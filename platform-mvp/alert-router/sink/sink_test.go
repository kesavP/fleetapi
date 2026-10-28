package sink

import (
	"bytes"
	"context"
	"log/slog"
	"strings"
	"testing"
	"time"

	"github.com/sojoner/kro-lab/platform-mvp/alert-router/model"
)

func textLogger(buf *bytes.Buffer) *slog.Logger {
	return slog.New(slog.NewTextHandler(buf, &slog.HandlerOptions{
		ReplaceAttr: func(_ []string, a slog.Attr) slog.Attr {
			if a.Key == slog.TimeKey {
				return slog.Attr{} // keep test output stable
			}
			return a
		},
	}))
}

func TestLogSinkWritesOneTextLinePerAlert(t *testing.T) {
	var buf bytes.Buffer
	err := Log{Logger: textLogger(&buf)}.Send(context.Background(), []model.Alert{
		{Name: "VmSetNotReady", Status: "firing", Severity: "warning", Tenant: "acme-corp", VmSet: "web", Region: "us",
			Summary: "VmSet web is not ready in us", Fingerprint: "abc", StartsAt: time.Date(2026, 10, 28, 10, 0, 0, 0, time.UTC)},
		{Name: "KubePodCrashLooping", Status: "resolved", Fingerprint: "def", Cluster: "eu",
			StartsAt: time.Date(2026, 10, 28, 9, 0, 0, 0, time.UTC), EndsAt: time.Date(2026, 10, 28, 9, 30, 0, 0, time.UTC)},
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	lines := strings.Split(strings.TrimSpace(buf.String()), "\n")
	if len(lines) != 2 {
		t.Fatalf("lines = %d, want 2:\n%s", len(lines), buf.String())
	}
	want := `level=WARN msg="alert notification" alert=VmSetNotReady status=firing severity=warning tenant=acme-corp vmset=web region=us summary="VmSet web is not ready in us" started=2026-10-28T10:00:00Z fingerprint=abc`
	if lines[0] != want {
		t.Errorf("firing line =\n%s\nwant\n%s", lines[0], want)
	}
	if !strings.HasPrefix(lines[1], "level=INFO") || !strings.Contains(lines[1], "ended=2026-10-28T09:30:00Z") || !strings.Contains(lines[1], "cluster=eu") || strings.Contains(lines[1], "tenant=") {
		t.Errorf("resolved line = %s", lines[1])
	}
}

func TestLogSinkCannotBeForgedThroughLabelValues(t *testing.T) {
	var buf bytes.Buffer
	_ = Log{Logger: textLogger(&buf)}.Send(context.Background(), []model.Alert{{
		Name: "VmSetNotReady", Status: "firing", Fingerprint: "x",
		// A VmSet name is user-controlled; try to start a fake second record.
		VmSet: "web\nlevel=ERROR msg=\"fake\" alert=Forged",
	}})
	if n := strings.Count(strings.TrimSpace(buf.String()), "\n"); n != 0 {
		t.Errorf("a newline in a value must not create a second line, got %d extra:\n%s", n, buf.String())
	}
}

func TestLogSinkLevels(t *testing.T) {
	tests := map[string]struct {
		a    model.Alert
		want string
	}{
		"critical firing": {model.Alert{Name: "A", Status: "firing", Severity: "critical", Fingerprint: "f"}, "level=ERROR"},
		"warning firing":  {model.Alert{Name: "A", Status: "firing", Severity: "warning", Fingerprint: "f"}, "level=WARN"},
		"resolved":        {model.Alert{Name: "A", Status: "resolved", Severity: "critical", Fingerprint: "f"}, "level=INFO"},
	}
	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			var buf bytes.Buffer
			_ = Log{Logger: textLogger(&buf)}.Send(context.Background(), []model.Alert{tt.a})
			if !strings.HasPrefix(buf.String(), tt.want) {
				t.Errorf("line = %s, want prefix %s", buf.String(), tt.want)
			}
		})
	}
}
