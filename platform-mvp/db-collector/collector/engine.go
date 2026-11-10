package collector

import (
	"context"
	"fmt"
	"sort"
	"sync"
	"time"
)

// SignalConfig is how often one kind of data is collected and how much of it. A zero Interval means the default for that kind.
type SignalConfig struct {
	Enabled  bool
	Interval time.Duration
	Top      int
}

// SlowLogConfig says where an engine's slow-statement log is. Only a file is supported: the log of a database in a container is on the
// container's own output, which a collector outside it cannot read, so this is for a log that is on a path the collector can read.
type SlowLogConfig struct {
	Enabled bool
	Path    string
	// FromStart reads the file from its beginning the first time (the default is to follow it from its end, so a restart does not report the
	// whole of an old log again).
	FromStart bool
}

// TargetConfig is one database to collect from. DSN is a secret: it is read from the environment or a file by the config, kept only here,
// and never logged, never put in a batch and never part of an error that is shown.
type TargetConfig struct {
	Name       string
	Engine     string
	DSN        string
	Statements SignalConfig
	Activity   SignalConfig
	Pools      SignalConfig
	SlowLog    SlowLogConfig
}

// Signals says which kinds of data an engine can give. A database has statements, sessions and a slow log; a connection pooler has pools.
type Signals struct {
	Statements, Activity, SlowLog, Pools bool
}

// Source is a connection to one database: what an engine hands back from Open. It is safe for concurrent use (the statements and the
// activity are read from different goroutines), and recovers from a lost connection by itself: a Source that fails once is tried again.
type Source interface {
	Close()
	// Capabilities looks at the server: its version, whether the data can be read, what to change if it cannot.
	Capabilities(ctx context.Context) (Capabilities, error)
}

// StatementSource is a Source that has statement statistics.
type StatementSource interface {
	Snapshot(ctx context.Context) (Snapshot, error)
}

// ActivitySource is a Source that can say what its sessions are doing.
type ActivitySource interface {
	Sample(ctx context.Context, top int) (Activity, error)
}

// PoolSource is a Source that is a connection pooler: its pools, and its counters since it started counting (see PoolDiffer).
type PoolSource interface {
	PoolSnapshot(ctx context.Context) (PoolSnapshot, error)
}

// SlowLogSource follows a slow-statement log, calling emit for each statement it finds until ctx ends.
type SlowLogSource interface {
	Follow(ctx context.Context, emit func(SlowQuery)) error
}

// Engine is one kind of database. Open does not need to reach the database (a Source may connect when first asked), so that a database that
// is down when the collector starts is not a reason for it to stop.
type Engine interface {
	Name() string
	Signals() Signals
	Open(ctx context.Context, t TargetConfig) (Source, error)
	// SlowLog returns the follower of the target's slow-statement log, or an error if the engine has none or the path is not usable.
	SlowLog(t TargetConfig) (SlowLogSource, error)
}

var (
	mu      sync.RWMutex
	engines = map[string]Engine{}
)

// Register adds an engine. Registering a name twice is a programming error.
func Register(e Engine) {
	mu.Lock()
	defer mu.Unlock()
	if _, dup := engines[e.Name()]; dup {
		panic("collector: engine " + e.Name() + " registered twice")
	}
	engines[e.Name()] = e
}

// SupportedSignals says which signals a registered engine has; known is false for an engine that is not registered (in a process that did not
// import it), whose signals are then not known.
func SupportedSignals(name string) (s Signals, known bool) {
	mu.RLock()
	defer mu.RUnlock()
	if e, ok := engines[name]; ok {
		return e.Signals(), true
	}
	return Signals{}, false
}

func Lookup(name string) (Engine, error) {
	mu.RLock()
	defer mu.RUnlock()
	if e, ok := engines[name]; ok {
		return e, nil
	}
	return nil, fmt.Errorf("unknown database engine %q (known: %s)", name, joinNames())
}

func joinNames() string {
	names := make([]string, 0, len(engines))
	for n := range engines {
		names = append(names, n)
	}
	sort.Strings(names)
	out := ""
	for i, n := range names {
		if i > 0 {
			out += ", "
		}
		out += n
	}
	if out == "" {
		return "none"
	}
	return out
}
