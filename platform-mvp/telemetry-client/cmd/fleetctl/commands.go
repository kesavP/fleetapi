package main

import (
	"fmt"
	"io"
	"strings"
	"time"

	"github.com/sojoner/kro-lab/platform-mvp/telemetry-client/client/logs"
	"github.com/sojoner/kro-lab/platform-mvp/telemetry-client/client/metrics"
	"github.com/sojoner/kro-lab/platform-mvp/telemetry-client/client/traces"
	"github.com/sojoner/kro-lab/platform-mvp/telemetry-client/fleet"
)

// stringList is a repeatable flag.
type stringList []string

func (s *stringList) String() string     { return strings.Join(*s, ",") }
func (s *stringList) Set(v string) error { *s = append(*s, v); return nil }

func cmdWhoami(args []string, getenv func(string) string, out io.Writer) error {
	c := newCommon("whoami", false, false)
	if _, err := c.parse(args); err != nil {
		return err
	}
	cl, hub, err := c.connect(getenv)
	if err != nil {
		return err
	}
	cx, cancel := ctx()
	defer cancel()
	from, to := fleet.Since(time.Minute)
	res, err := cl.Logs.ListLogServices(logs.NewListLogServicesParams().WithContext(cx).WithProject(cl.Project).WithFrom(from).WithTo(to), nil)
	if err != nil {
		return err
	}
	n := len(res.Payload.Data)
	if c.output == "json" {
		return writeJSON(out, map[string]any{"hub": hub, "project": cl.Project, "read_key": "ok", "services_logging_last_minute": n})
	}
	fmt.Fprintf(out, "hub:      %s\nproject:  %s\nread key: ok (%d service(s) sent logs in the last minute)\n", hub, cl.Project, n)
	return nil
}

func cmdLogs(args []string, getenv func(string) string, out io.Writer) error {
	c := newCommon("logs", true, true)
	var service, severity, grep, trace string
	c.fs.StringVar(&service, "service", "", "application id, e.g. /k8s/shop/checkout")
	c.fs.StringVar(&severity, "severity", "", "e.g. error, warn")
	c.fs.StringVar(&grep, "grep", "", "text the log contains (case does not matter)")
	c.fs.StringVar(&trace, "trace", "", "only the logs of this trace id")
	pos, err := c.parse(args)
	if err != nil {
		return err
	}
	cl, _, err := c.connect(getenv)
	if err != nil {
		return err
	}
	cx, cancel := ctx()
	defer cancel()
	from, to := c.timeRange()
	switch {
	case len(pos) == 1 && pos[0] == "services":
		res, err := cl.Logs.ListLogServices(logs.NewListLogServicesParams().WithContext(cx).WithProject(cl.Project).WithFrom(from).WithTo(to), nil)
		if err != nil {
			return err
		}
		return writeStrings(out, c.output, res.Payload.Data)
	case len(pos) > 0:
		return usageError{fmt.Sprintf("unexpected %q (fleetctl logs takes flags, or \"services\")", strings.Join(pos, " "))}
	}
	p := logs.NewSearchLogsParams().WithContext(cx).WithProject(cl.Project).WithFrom(from).WithTo(to).WithLimit(fleet.Int64(c.limit))
	p.Service, p.Severity, p.Q, p.TraceID = opt(service), opt(severity), opt(grep), opt(trace)
	res, err := cl.Logs.SearchLogs(p, nil)
	if err != nil {
		return err
	}
	return writeLogs(out, c.output, c.fields, res.Payload.Data)
}

func cmdTraces(args []string, getenv func(string) string, out io.Writer) error {
	c := newCommon("traces", true, true)
	var service, name, status string
	var minDuration time.Duration
	var root bool
	c.fs.StringVar(&service, "service", "", "application id")
	c.fs.StringVar(&name, "name", "", "span name")
	c.fs.StringVar(&status, "status", "", "ok, error or unset")
	c.fs.DurationVar(&minDuration, "min-duration", 0, "only spans at least this long, e.g. 500ms")
	c.fs.BoolVar(&root, "root", false, "only root spans")
	pos, err := c.parse(args)
	if err != nil {
		return err
	}
	cl, _, err := c.connect(getenv)
	if err != nil {
		return err
	}
	cx, cancel := ctx()
	defer cancel()
	from, to := c.timeRange()
	switch {
	case len(pos) == 1 && pos[0] == "services":
		res, err := cl.Traces.ListTraceServices(traces.NewListTraceServicesParams().WithContext(cx).WithProject(cl.Project).WithFrom(from).WithTo(to), nil)
		if err != nil {
			return err
		}
		return writeStrings(out, c.output, res.Payload.Data)
	case len(pos) > 0:
		return usageError{fmt.Sprintf("unexpected %q (fleetctl traces takes flags, or \"services\")", strings.Join(pos, " "))}
	}
	p := traces.NewSearchSpansParams().WithContext(cx).WithProject(cl.Project).WithFrom(from).WithTo(to).WithLimit(fleet.Int64(c.limit))
	p.Service, p.Name, p.Status = opt(service), opt(name), opt(strings.ToLower(status))
	if minDuration > 0 {
		p.MinDurationMs = fleet.Int64(minDuration.Milliseconds())
	}
	if root {
		p.Root = fleet.Bool(true)
	}
	res, err := cl.Traces.SearchSpans(p, nil)
	if err != nil {
		return err
	}
	return writeSpans(out, c.output, c.fields, res.Payload.Data)
}

func cmdTrace(args []string, getenv func(string) string, out io.Writer) error {
	c := newCommon("trace", false, false)
	pos, err := c.parse(args)
	if err != nil {
		return err
	}
	if len(pos) != 1 {
		return usageError{"fleetctl trace takes one trace id (32 hex characters)"}
	}
	cl, _, err := c.connect(getenv)
	if err != nil {
		return err
	}
	cx, cancel := ctx()
	defer cancel()
	res, err := cl.Traces.GetTrace(traces.NewGetTraceParams().WithContext(cx).WithProject(cl.Project).WithTraceID(strings.ToLower(pos[0])), nil)
	if err != nil {
		return err
	}
	return writeTrace(out, c.output, c.fields, res.Payload.Data)
}

func cmdMetrics(args []string, getenv func(string) string, out io.Writer) error {
	c := newCommon("metrics", false, false)
	pos, err := c.parse(args)
	if err != nil {
		return err
	}
	if len(pos) > 0 {
		return usageError{"fleetctl metrics takes no arguments (fleetctl series <metric> lists a metric's label sets)"}
	}
	cl, _, err := c.connect(getenv)
	if err != nil {
		return err
	}
	cx, cancel := ctx()
	defer cancel()
	res, err := cl.Metrics.ListMetrics(metrics.NewListMetricsParams().WithContext(cx).WithProject(cl.Project), nil)
	if err != nil {
		return err
	}
	return writeStrings(out, c.output, res.Payload.Data)
}

func matchFlag(c *common) *stringList {
	var m stringList
	c.fs.Var(&m, "match", "a label matcher label=value (repeatable)")
	return &m
}

func checkMatchers(m []string) error {
	for _, kv := range m {
		if k, _, ok := strings.Cut(kv, "="); !ok || k == "" {
			return usageError{fmt.Sprintf("--match must be label=value, not %q", kv)}
		}
	}
	return nil
}

func cmdSeries(args []string, getenv func(string) string, out io.Writer) error {
	c := newCommon("series", false, true)
	match := matchFlag(c)
	pos, err := c.parse(args)
	if err != nil {
		return err
	}
	if len(pos) != 1 {
		return usageError{"fleetctl series takes one metric name"}
	}
	if err := checkMatchers(*match); err != nil {
		return err
	}
	cl, _, err := c.connect(getenv)
	if err != nil {
		return err
	}
	cx, cancel := ctx()
	defer cancel()
	res, err := cl.Metrics.ListSeries(metrics.NewListSeriesParams().WithContext(cx).WithProject(cl.Project).WithName(pos[0]).
		WithMatch(*match).WithLimit(fleet.Int64(c.limit)), nil)
	if err != nil {
		return err
	}
	return writeSeries(out, c.output, res.Payload.Data)
}

func cmdQuery(args []string, getenv func(string) string, out io.Writer) error {
	c := newCommon("query", true, false)
	match := matchFlag(c)
	var step int64
	var rate, fromZero bool
	c.fs.Int64Var(&step, "step", 0, "seconds between points (0: the raw samples)")
	c.fs.BoolVar(&rate, "rate", false, "per-second rate of a counter (needs --step)")
	c.fs.BoolVar(&fromZero, "from-zero", false, "with --rate: count a series that starts in the range from zero")
	pos, err := c.parse(args)
	if err != nil {
		return err
	}
	if len(pos) != 1 {
		return usageError{"fleetctl query takes one metric name"}
	}
	if err := checkMatchers(*match); err != nil {
		return err
	}
	if rate && step == 0 {
		return usageError{"--rate needs --step"}
	}
	cl, _, err := c.connect(getenv)
	if err != nil {
		return err
	}
	cx, cancel := ctx()
	defer cancel()
	from, to := c.timeRange()
	p := metrics.NewQueryRangeParams().WithContext(cx).WithProject(cl.Project).WithName(pos[0]).WithMatch(*match).WithFrom(from).WithTo(to).
		WithStep(fleet.Int64(step))
	if rate {
		p.Rate = fleet.Bool(true)
	}
	if fromZero {
		p.FromZero = fleet.Bool(true)
	}
	res, err := cl.Metrics.QueryRange(p, nil)
	if err != nil {
		return err
	}
	return writeMetric(out, c.output, res.Payload.Data)
}

func opt(s string) *string {
	if strings.TrimSpace(s) == "" {
		return nil
	}
	return fleet.String(strings.TrimSpace(s))
}
