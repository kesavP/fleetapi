package collector

import (
	"fmt"
	"io"
	"net/http"
	"sort"
	"strings"
	"sync"
)

// Metrics are the collector's own counters and gauges, in the Prometheus text format: a collector that cannot be seen is a collector that
// fails quietly. Label values are fixed strings chosen by the caller (a target's name, a signal, a status), never anything read from a database.
type Metrics struct {
	mu sync.Mutex
	c  map[string]float64
	g  map[string]float64
}

func NewMetrics() *Metrics { return &Metrics{c: map[string]float64{}, g: map[string]float64{}} }

func series(name string, labels []string) string {
	if len(labels) == 0 {
		return name
	}
	var b strings.Builder
	b.WriteString(name)
	b.WriteByte('{')
	for i := 0; i+1 < len(labels); i += 2 {
		if i > 0 {
			b.WriteByte(',')
		}
		fmt.Fprintf(&b, "%s=%q", labels[i], labels[i+1])
	}
	b.WriteByte('}')
	return b.String()
}

func (m *Metrics) Inc(name string, labels ...string) { m.Add(name, 1, labels...) }
func (m *Metrics) Add(name string, v float64, labels ...string) {
	m.mu.Lock()
	m.c[series(name, labels)] += v
	m.mu.Unlock()
}
func (m *Metrics) Set(name string, v float64, labels ...string) {
	m.mu.Lock()
	m.g[series(name, labels)] = v
	m.mu.Unlock()
}

// Value returns a counter or gauge, for tests.
func (m *Metrics) Value(name string, labels ...string) float64 {
	m.mu.Lock()
	defer m.mu.Unlock()
	s := series(name, labels)
	if v, ok := m.c[s]; ok {
		return v
	}
	return m.g[s]
}

func (m *Metrics) Write(w io.Writer) {
	m.mu.Lock()
	lines := make([]string, 0, len(m.c)+len(m.g))
	for k, v := range m.c {
		lines = append(lines, fmt.Sprintf("%s %g", k, v))
	}
	for k, v := range m.g {
		lines = append(lines, fmt.Sprintf("%s %g", k, v))
	}
	m.mu.Unlock()
	sort.Strings(lines)
	for _, l := range lines {
		_, _ = io.WriteString(w, l+"\n")
	}
}

func (m *Metrics) Handler() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/plain; version=0.0.4")
		m.Write(w)
	})
}
