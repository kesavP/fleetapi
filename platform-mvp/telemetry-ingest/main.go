// telemetry-ingest receives the Coroot agents' telemetry from the spokes,
// stores it in ClickHouse and serves it back per project. It replaces the
// Coroot server's collector for the fleet; Coroot's UI is not part of it.
package main

import (
	"context"
	"errors"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/signal"
	"sync/atomic"
	"syscall"
	"time"

	"github.com/sojoner/kro-lab/platform-mvp/telemetry-ingest/config"
	"github.com/sojoner/kro-lab/platform-mvp/telemetry-ingest/pipeline"
	"github.com/sojoner/kro-lab/platform-mvp/telemetry-ingest/project"
	"github.com/sojoner/kro-lab/platform-mvp/telemetry-ingest/server"
	"github.com/sojoner/kro-lab/platform-mvp/telemetry-ingest/store"
)

func main() {
	log := slog.New(slog.NewTextHandler(os.Stdout, nil))
	cfg, err := config.Parse(os.Args[1:])
	if err != nil {
		log.Error("invalid configuration", "error", err)
		os.Exit(2)
	}
	reg, err := project.Load(cfg.ProjectsFile)
	if err != nil {
		log.Error("cannot load projects", "error", err)
		os.Exit(2)
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	go reg.Watch(ctx, cfg.ReloadEvery, log)

	count := server.NewCounters()
	var app atomic.Pointer[http.ServeMux] // set once ClickHouse is reachable and migrated
	var st atomic.Pointer[store.Store]
	var pipe atomic.Pointer[pipeline.Pipeline]

	// The probes and /metrics answer from the start. Liveness must not depend
	// on ClickHouse, or an outage there would restart a healthy service.
	root := http.NewServeMux()
	root.HandleFunc("GET /healthz", func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusOK) })
	root.HandleFunc("GET /readyz", func(w http.ResponseWriter, r *http.Request) {
		s := st.Load()
		if app.Load() == nil || s == nil {
			http.Error(w, "starting", http.StatusServiceUnavailable)
			return
		}
		pctx, cancel := context.WithTimeout(r.Context(), 3*time.Second)
		defer cancel()
		if err := s.Ping(pctx); err != nil {
			http.Error(w, "clickhouse unreachable", http.StatusServiceUnavailable)
			return
		}
		w.WriteHeader(http.StatusOK)
	})
	root.Handle("GET /metrics", server.MetricsHandler(count, func() map[string]pipeline.Stats {
		if p := pipe.Load(); p != nil {
			return p.Stats()
		}
		return nil
	}))
	root.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		if m := app.Load(); m != nil {
			m.ServeHTTP(w, r)
			return
		}
		http.Error(w, "starting: storage not ready", http.StatusServiceUnavailable)
	})
	httpSrv := &http.Server{Addr: cfg.Listen, Handler: root, ReadHeaderTimeout: 10 * time.Second}
	go func() {
		log.Info("listening", "http", cfg.Listen)
		if err := httpSrv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			log.Error("http server exited", "error", err)
			os.Exit(1)
		}
	}()

	s := openStore(ctx, cfg, log)
	if s == nil { // interrupted while waiting for ClickHouse
		_ = httpSrv.Shutdown(context.Background())
		return
	}
	st.Store(s)
	p := pipeline.New(s, pipeline.PipelineOptions{
		Options: pipeline.Options{
			BatchRows: cfg.BatchRows, MaxBuffered: cfg.MaxBuffered, FlushInterval: cfg.FlushInterval,
			Retries: 3, Backoff: time.Second,
		},
		SeriesRefresh: 10 * time.Minute,
	}, log)
	pipe.Store(p)

	mux := http.NewServeMux()
	(&server.Ingest{Projects: reg, Sink: p, MaxBody: cfg.MaxBodyBytes, Log: log, Count: count}).Register(mux)
	(&server.Query{Projects: reg, Reader: s, Log: log, DefaultRange: time.Hour, MaxSeries: 500, MaxPoints: 200000}).Register(mux)
	app.Store(mux)

	var grpcStop func()
	if cfg.GRPCListen != "" {
		lis, err := net.Listen("tcp", cfg.GRPCListen)
		if err != nil {
			log.Error("grpc listen failed", "error", err)
			os.Exit(1)
		}
		g := server.NewGRPC(reg, p, int(cfg.MaxBodyBytes), count)
		go func() { _ = g.Serve(lis) }()
		grpcStop = g.GracefulStop
		log.Info("listening", "grpc", cfg.GRPCListen)
	}
	log.Info("ready", "clickhouse", cfg.ClickHouseAddrs, "database", cfg.ClickHouseDatabase)

	<-ctx.Done()
	log.Info("shutting down")
	shutdown, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	_ = httpSrv.Shutdown(shutdown)
	if grpcStop != nil {
		grpcStop()
	}
	if err := p.Close(shutdown); err != nil {
		log.Error("buffers not fully flushed", "error", err)
	}
	_ = s.Close()
}

// openStore connects and migrates, retrying until it works or ctx ends.
func openStore(ctx context.Context, cfg config.Config, log *slog.Logger) *store.Store {
	wait := time.Second
	for {
		s, err := store.Open(ctx, store.Options{
			Addrs: cfg.ClickHouseAddrs, Database: cfg.ClickHouseDatabase, User: cfg.ClickHouseUser,
			Password: cfg.ClickHousePassword, TLS: cfg.ClickHouseTLS, CreateDatabase: cfg.ClickHouseCreateDatabase,
			TTL: store.TTLs{Metrics: cfg.TTLMetrics, Traces: cfg.TTLTraces, Logs: cfg.TTLLogs, Profiles: cfg.TTLProfiles},
		})
		if err == nil {
			mctx, cancel := context.WithTimeout(ctx, 2*time.Minute)
			err = s.Migrate(mctx)
			cancel()
			if err == nil {
				return s
			}
			_ = s.Close()
		}
		log.Error("clickhouse not ready", "error", err, "retry_in", wait.String())
		select {
		case <-ctx.Done():
			return nil
		case <-time.After(wait):
		}
		if wait < 30*time.Second {
			wait *= 2
		}
	}
}
