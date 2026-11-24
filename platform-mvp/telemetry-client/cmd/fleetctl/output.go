package main

import (
	"encoding/json"
	"fmt"
	"io"
	"math"
	"sort"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/go-openapi/strfmt"

	"github.com/sojoner/kro-lab/platform-mvp/telemetry-client/models"
)

const maxBody = 300 // a log body in a table or in default JSON, in characters

var (
	defaultLogFields  = []string{"time", "service", "severity", "body"}
	allLogFields      = []string{"time", "service", "severity", "body", "trace_id", "span_id", "attributes", "resource"}
	defaultSpanFields = []string{"time", "trace_id", "span_id", "name", "service", "duration_ms", "status"}
	allSpanFields     = []string{"time", "trace_id", "span_id", "parent_span_id", "name", "kind", "service", "duration_ms", "status", "status_message", "attributes", "resource"}
)

// fieldsOf reads --fields: empty for the default, "all" for every field, or a comma separated list of known ones.
func fieldsOf(spec string, def, all []string) ([]string, error) {
	switch strings.TrimSpace(spec) {
	case "":
		return def, nil
	case "all":
		return all, nil
	}
	known := map[string]bool{}
	for _, f := range all {
		known[f] = true
	}
	var out []string
	for _, f := range strings.Split(spec, ",") {
		f = strings.TrimSpace(f)
		if !known[f] {
			return nil, usageError{fmt.Sprintf("unknown field %q (one of %s)", f, strings.Join(all, ", "))}
		}
		out = append(out, f)
	}
	return out, nil
}

func writeJSON(w io.Writer, v any) error {
	b, err := json.Marshal(v)
	if err != nil {
		return err
	}
	_, err = fmt.Fprintf(w, "%s\n", b)
	return err
}

func stamp(t strfmt.DateTime) string {
	tt := time.Time(t)
	if tt.IsZero() {
		return ""
	}
	return tt.UTC().Format("2006-01-02T15:04:05.000Z")
}

func clip(s string, n int) string {
	s = strings.Join(strings.Fields(s), " ")
	if r := []rune(s); len(r) > n {
		return string(r[:n-1]) + "…"
	}
	return s
}

func writeStrings(w io.Writer, output string, xs []string) error {
	if xs == nil {
		xs = []string{}
	}
	sort.Strings(xs)
	if output == "json" {
		return writeJSON(w, xs)
	}
	if len(xs) == 0 {
		_, err := fmt.Fprintln(w, "(none)")
		return err
	}
	_, err := fmt.Fprintln(w, strings.Join(xs, "\n"))
	return err
}

func logRow(r *models.LogRecord, fields []string, bodyMax int) map[string]any {
	m := map[string]any{}
	for _, f := range fields {
		switch f {
		case "time":
			m[f] = stamp(r.Time)
		case "service":
			m[f] = r.Service
		case "severity":
			m[f] = r.Severity
		case "body":
			if bodyMax > 0 {
				m[f] = clip(r.Body, bodyMax)
			} else {
				m[f] = r.Body
			}
		case "trace_id":
			m[f] = r.TraceID
		case "span_id":
			m[f] = r.SpanID
		case "attributes":
			m[f] = r.LogAttributes
		case "resource":
			m[f] = r.ResourceAttributes
		}
	}
	return m
}

func writeLogs(w io.Writer, output, fieldSpec string, recs []*models.LogRecord) error {
	if output == "json" {
		fields, err := fieldsOf(fieldSpec, defaultLogFields, allLogFields)
		if err != nil {
			return err
		}
		bodyMax := maxBody
		if fieldSpec != "" {
			bodyMax = 0 // asked for explicitly: whole
		}
		rows := make([]map[string]any, 0, len(recs))
		for _, r := range recs {
			if r != nil {
				rows = append(rows, logRow(r, fields, bodyMax))
			}
		}
		return writeJSON(w, rows)
	}
	if len(recs) == 0 {
		_, err := fmt.Fprintln(w, "(no logs)")
		return err
	}
	tw := tabwriter.NewWriter(w, 0, 2, 2, ' ', 0)
	fmt.Fprintln(tw, "TIME\tSEVERITY\tSERVICE\tBODY")
	for _, r := range recs {
		if r != nil {
			fmt.Fprintf(tw, "%s\t%s\t%s\t%s\n", stamp(r.Time), r.Severity, r.Service, clip(r.Body, maxBody))
		}
	}
	return tw.Flush()
}

func durationMs(ns uint64) float64 { return math.Round(float64(ns)/1e4) / 100 } // 2 decimals

func spanRow(s *models.Span, fields []string) map[string]any {
	m := map[string]any{}
	for _, f := range fields {
		switch f {
		case "time":
			m[f] = stamp(s.Time)
		case "trace_id":
			m[f] = s.TraceID
		case "span_id":
			m[f] = s.SpanID
		case "parent_span_id":
			m[f] = s.ParentSpanID
		case "name":
			m[f] = s.Name
		case "kind":
			m[f] = s.Kind
		case "service":
			m[f] = s.Service
		case "duration_ms":
			m[f] = durationMs(s.DurationNs)
		case "status":
			m[f] = s.StatusCode
		case "status_message":
			m[f] = s.StatusMessage
		case "attributes":
			m[f] = s.SpanAttributes
		case "resource":
			m[f] = s.ResourceAttributes
		}
	}
	return m
}

func writeSpans(w io.Writer, output, fieldSpec string, spans []*models.Span) error {
	if output == "json" {
		fields, err := fieldsOf(fieldSpec, defaultSpanFields, allSpanFields)
		if err != nil {
			return err
		}
		rows := make([]map[string]any, 0, len(spans))
		for _, s := range spans {
			if s != nil {
				rows = append(rows, spanRow(s, fields))
			}
		}
		return writeJSON(w, rows)
	}
	if len(spans) == 0 {
		_, err := fmt.Fprintln(w, "(no spans)")
		return err
	}
	tw := tabwriter.NewWriter(w, 0, 2, 2, ' ', 0)
	fmt.Fprintln(tw, "TIME\tTRACE\tSERVICE\tNAME\tDURATION\tSTATUS")
	for _, s := range spans {
		if s != nil {
			fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%s\t%s\n", stamp(s.Time), s.TraceID, s.Service, s.Name, fmtMs(durationMs(s.DurationNs)), s.StatusCode)
		}
	}
	return tw.Flush()
}

func fmtMs(ms float64) string {
	if ms >= 1000 {
		return fmt.Sprintf("%.2fs", ms/1000)
	}
	return fmt.Sprintf("%.2fms", ms)
}

// node is a span in the trace tree.
type node struct {
	span     *models.Span
	depth    int
	children []*node
}

// treeOf orders a trace's spans parent before child, children by start time. A span whose parent is not in the trace (cut off, or sent by
// a service that does not report) is a root.
func treeOf(spans []*models.Span) []*node {
	byID := map[string]*node{}
	var all []*node
	for _, s := range spans {
		if s == nil {
			continue
		}
		n := &node{span: s}
		all = append(all, n)
		if s.SpanID != "" {
			byID[s.SpanID] = n
		}
	}
	var roots []*node
	for _, n := range all {
		if p, ok := byID[n.span.ParentSpanID]; ok && n.span.ParentSpanID != "" && p != n {
			p.children = append(p.children, n)
		} else {
			roots = append(roots, n)
		}
	}
	byTime := func(ns []*node) {
		sort.SliceStable(ns, func(i, j int) bool { return time.Time(ns[i].span.Time).Before(time.Time(ns[j].span.Time)) })
	}
	var out []*node
	seen := map[*node]bool{}
	var walk func(n *node, depth int)
	walk = func(n *node, depth int) {
		if seen[n] { // a cycle in bad data
			return
		}
		seen[n] = true
		n.depth = depth
		out = append(out, n)
		byTime(n.children)
		for _, c := range n.children {
			walk(c, depth+1)
		}
	}
	byTime(roots)
	for _, r := range roots {
		walk(r, 0)
	}
	for _, n := range all { // spans only reachable through a cycle
		if !seen[n] {
			walk(n, 0)
		}
	}
	return out
}

func writeTrace(w io.Writer, output, fieldSpec string, spans []*models.Span) error {
	tree := treeOf(spans)
	if output == "json" {
		fields, err := fieldsOf(fieldSpec, append([]string{"parent_span_id"}, defaultSpanFields...), allSpanFields)
		if err != nil {
			return err
		}
		rows := make([]map[string]any, 0, len(tree))
		for _, n := range tree {
			row := spanRow(n.span, fields)
			row["depth"] = n.depth
			rows = append(rows, row)
		}
		return writeJSON(w, rows)
	}
	if len(tree) == 0 {
		_, err := fmt.Fprintln(w, "(no spans: the trace is unknown or has expired)")
		return err
	}
	start := time.Time(tree[0].span.Time)
	for _, n := range tree {
		if t := time.Time(n.span.Time); t.Before(start) {
			start = t
		}
	}
	tw := tabwriter.NewWriter(w, 0, 2, 2, ' ', 0)
	fmt.Fprintln(tw, "SPAN\tSERVICE\tSTART\tDURATION\tSTATUS")
	for _, n := range tree {
		s := n.span
		offset := time.Time(s.Time).Sub(start)
		fmt.Fprintf(tw, "%s%s\t%s\t+%s\t%s\t%s\n", strings.Repeat("  ", n.depth), s.Name, s.Service, fmtMs(float64(offset.Microseconds())/1000),
			fmtMs(durationMs(s.DurationNs)), s.StatusCode)
	}
	return tw.Flush()
}

func labelString(l map[string]string) string {
	keys := make([]string, 0, len(l))
	for k := range l {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	parts := make([]string, len(keys))
	for i, k := range keys {
		parts[i] = fmt.Sprintf("%s=%q", k, l[k])
	}
	return "{" + strings.Join(parts, ", ") + "}"
}

func writeSeries(w io.Writer, output string, series []*models.SeriesInfo) error {
	if output == "json" {
		rows := make([]map[string]string, 0, len(series))
		for _, s := range series {
			if s != nil {
				rows = append(rows, s.Labels)
			}
		}
		return writeJSON(w, rows)
	}
	if len(series) == 0 {
		_, err := fmt.Fprintln(w, "(no series)")
		return err
	}
	for _, s := range series {
		if s != nil {
			fmt.Fprintln(w, labelString(s.Labels))
		}
	}
	return nil
}

func writeMetric(w io.Writer, output string, r *models.MetricResult) error {
	if r == nil {
		r = &models.MetricResult{}
	}
	if output == "json" {
		type series struct {
			Labels map[string]string `json:"labels"`
			Points [][2]float64      `json:"points"` // [unix ms, value]
		}
		out := struct {
			Series    []series `json:"series"`
			Truncated bool     `json:"truncated"`
		}{Series: []series{}, Truncated: r.Truncated}
		for _, s := range r.Series {
			if s == nil {
				continue
			}
			ps := make([][2]float64, 0, len(s.Points))
			for _, p := range s.Points {
				if p != nil {
					ps = append(ps, [2]float64{float64(p.T), p.V})
				}
			}
			out.Series = append(out.Series, series{Labels: s.Labels, Points: ps})
		}
		return writeJSON(w, out)
	}
	if len(r.Series) == 0 {
		_, err := fmt.Fprintln(w, "(no data)")
		return err
	}
	tw := tabwriter.NewWriter(w, 0, 2, 2, ' ', 0)
	fmt.Fprintln(tw, "SERIES\tPOINTS\tMIN\tMAX\tLAST\tLAST AT")
	for _, s := range r.Series {
		if s == nil || len(s.Points) == 0 {
			continue
		}
		lo, hi := math.Inf(1), math.Inf(-1)
		var last *models.Point
		for _, p := range s.Points {
			if p == nil {
				continue
			}
			lo, hi = math.Min(lo, p.V), math.Max(hi, p.V)
			if last == nil || p.T > last.T {
				last = p
			}
		}
		if last == nil {
			continue
		}
		fmt.Fprintf(tw, "%s\t%d\t%g\t%g\t%g\t%s\n", labelString(s.Labels), len(s.Points), lo, hi, last.V,
			time.UnixMilli(last.T).UTC().Format("2006-01-02T15:04:05Z"))
	}
	if err := tw.Flush(); err != nil {
		return err
	}
	if r.Truncated {
		_, err := fmt.Fprintln(w, "(truncated: narrow with --match or a shorter range)")
		return err
	}
	return nil
}
