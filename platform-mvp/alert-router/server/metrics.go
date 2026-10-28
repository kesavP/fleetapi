package server

import (
	"fmt"
	"net/http"
	"sort"
	"sync"
)

// Metrics are plain counters rendered in the Prometheus text format, which
// keeps the router free of third-party dependencies.
type Metrics struct {
	mu         sync.Mutex
	received   int
	deduped    int
	delivered  int
	sinkErrors map[string]int
}

func (m *Metrics) add(field *int, n int) {
	m.mu.Lock()
	*field += n
	m.mu.Unlock()
}

func (m *Metrics) sinkError(sink string) {
	m.mu.Lock()
	if m.sinkErrors == nil {
		m.sinkErrors = map[string]int{}
	}
	m.sinkErrors[sink]++
	m.mu.Unlock()
}

func (m *Metrics) serve(w http.ResponseWriter, _ *http.Request) {
	m.mu.Lock()
	defer m.mu.Unlock()
	w.Header().Set("Content-Type", "text/plain; version=0.0.4")
	counter := func(name, help string, v int) {
		fmt.Fprintf(w, "# HELP %s %s\n# TYPE %s counter\n%s %d\n", name, help, name, name, v)
	}
	counter("alert_router_alerts_received_total", "Alerts received from Alertmanager.", m.received)
	counter("alert_router_alerts_deduped_total", "Firing alerts dropped as unchanged repeats.", m.deduped)
	counter("alert_router_alerts_delivered_total", "Alerts accepted by every required sink.", m.delivered)

	fmt.Fprint(w, "# HELP alert_router_sink_errors_total Failed deliveries per sink.\n# TYPE alert_router_sink_errors_total counter\n")
	names := make([]string, 0, len(m.sinkErrors))
	for n := range m.sinkErrors {
		names = append(names, n)
	}
	sort.Strings(names)
	for _, n := range names {
		fmt.Fprintf(w, "alert_router_sink_errors_total{sink=%q} %d\n", n, m.sinkErrors[n])
	}
}
