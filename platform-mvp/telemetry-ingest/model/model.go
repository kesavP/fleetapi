// Package model holds the rows that flow from the ingest endpoints into
// ClickHouse and back out through the query API. Every row carries the
// Project it belongs to: one ClickHouse database serves all projects, and the
// project is the first column of every sort key.
package model

import "time"

type Sample struct {
	Project string
	Name    string
	Hash    uint64
	Time    time.Time
	Value   float64
}

// Series is one distinct label set of one metric. Labels exclude __name__.
type Series struct {
	Project string
	Name    string
	Hash    uint64
	Labels  map[string]string
	Seen    time.Time
}

type MetricMeta struct {
	Project string
	Family  string
	Type    string
	Help    string
	Unit    string
}

type Span struct {
	Project       string            `json:"project,omitempty"`
	Time          time.Time         `json:"time"`
	TraceID       string            `json:"trace_id"`
	SpanID        string            `json:"span_id"`
	ParentSpanID  string            `json:"parent_span_id,omitempty"`
	TraceState    string            `json:"trace_state,omitempty"`
	Name          string            `json:"name"`
	Kind          string            `json:"kind"`
	Service       string            `json:"service"`
	DurationNs    uint64            `json:"duration_ns"`
	StatusCode    string            `json:"status_code"`
	StatusMessage string            `json:"status_message,omitempty"`
	ResourceAttrs map[string]string `json:"resource_attributes,omitempty"`
	SpanAttrs     map[string]string `json:"span_attributes,omitempty"`

	EventTimes []time.Time         `json:"event_times,omitempty"`
	EventNames []string            `json:"event_names,omitempty"`
	EventAttrs []map[string]string `json:"event_attributes,omitempty"`

	LinkTraceIDs []string            `json:"link_trace_ids,omitempty"`
	LinkSpanIDs  []string            `json:"link_span_ids,omitempty"`
	LinkAttrs    []map[string]string `json:"link_attributes,omitempty"`
}

type LogRecord struct {
	Project        string            `json:"project,omitempty"`
	Time           time.Time         `json:"time"`
	TraceID        string            `json:"trace_id,omitempty"`
	SpanID         string            `json:"span_id,omitempty"`
	SeverityText   string            `json:"severity"`
	SeverityNumber int32             `json:"severity_number"`
	Service        string            `json:"service"`
	Body           string            `json:"body"`
	ResourceAttrs  map[string]string `json:"resource_attributes,omitempty"`
	LogAttrs       map[string]string `json:"log_attributes,omitempty"`
}

type ProfileSample struct {
	Project   string
	Service   string
	Type      string // "<sample type>:<unit>", e.g. "cpu:nanoseconds"
	Start     time.Time
	End       time.Time
	Labels    map[string]string
	Value     int64
	StackHash uint64
}

// Stack is stored once per distinct hash. Frames are leaf first.
type Stack struct {
	Project string
	Hash    uint64
	Frames  []string
	Seen    time.Time
}
