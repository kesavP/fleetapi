// db-collector reads what a database knows about its own workload (statement statistics, what its sessions are doing now, the slow
// statements in its log) and sends it to telemetry-ingest, which stores it beside the metrics, traces, logs and profiles of the same project.
//
//	db-collector --config /etc/db-collector/config.json            # collect and send
//	db-collector --config ... --check                              # say what each database allows, and what to change; exit 1 if not ready
//	db-collector --config ... --stdout                             # print what would be sent instead of sending it
package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/sojoner/kro-lab/platform-mvp/db-collector/collector"
	"github.com/sojoner/kro-lab/platform-mvp/db-collector/config"
	_ "github.com/sojoner/kro-lab/platform-mvp/db-collector/pgbouncer" // registers the engines
	_ "github.com/sojoner/kro-lab/platform-mvp/db-collector/postgres"
)

func main() { os.Exit(run(os.Args[1:], os.Stdout, os.Stderr)) }

func run(args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("db-collector", flag.ContinueOnError)
	fs.SetOutput(stderr)
	path := fs.String("config", "", "Path to the configuration file (required)")
	check := fs.Bool("check", false, "Look at each database, say what can be collected and what to change, and exit")
	toStdout := fs.Bool("stdout", false, "Print each batch as a line of JSON instead of sending it")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	if *path == "" {
		fmt.Fprintln(stderr, "db-collector: --config is required")
		return 2
	}
	cfg, err := config.Load(*path)
	if err != nil {
		fmt.Fprintln(stderr, "db-collector:", err)
		return 1
	}
	log := slog.New(slog.NewJSONHandler(stderr, nil))
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	if *check {
		return checkTargets(ctx, cfg, stdout, stderr)
	}

	metrics := collector.NewMetrics()
	var sink collector.Sink
	var queue *collector.Queue
	switch {
	case *toStdout:
		sink = &collector.WriterSink{W: stdout}
	case cfg.IngestURL == "":
		fmt.Fprintln(stderr, "db-collector: ingest.url is not set: nowhere to send to (use --stdout to look at what would be sent, or --check)")
		return 1
	default:
		queue = collector.NewQueue(&collector.HTTPSink{URL: cfg.IngestURL, Key: cfg.IngestKey}, cfg.QueueSize, 5, time.Second, log, metrics)
		queue.Start(ctx)
		sink = queue
	}

	srv := &http.Server{Addr: cfg.Listen, ReadHeaderTimeout: 5 * time.Second, Handler: http.NewServeMux()}
	mux := srv.Handler.(*http.ServeMux)
	mux.HandleFunc("/healthz", func(w http.ResponseWriter, _ *http.Request) { _, _ = io.WriteString(w, "ok\n") })
	mux.Handle("/metrics", metrics.Handler())
	go func() {
		if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			log.Error("the metrics server stopped", "error", err)
		}
	}()

	r := &collector.Runner{Targets: cfg.Targets, Sink: sink, Log: log, Metrics: metrics}
	log.Info("collecting", "targets", len(cfg.Targets))
	err = r.Run(ctx)
	shutdown, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	_ = srv.Shutdown(shutdown)
	if queue != nil {
		queue.Wait()
	}
	if err != nil {
		fmt.Fprintln(stderr, "db-collector:", err)
		return 1
	}
	return 0
}

// checkTargets asks each database what it allows and says so; nothing is sent. Exit status 1 if any target cannot be asked, or lacks a kind of
// data that its configuration asks for.
func checkTargets(ctx context.Context, cfg config.Config, stdout, stderr io.Writer) int {
	status := 0
	for _, t := range cfg.Targets {
		eng, err := collector.Lookup(t.Engine)
		if err != nil {
			fmt.Fprintf(stderr, "target %s: %v\n", t.Name, err)
			status = 1
			continue
		}
		src, err := eng.Open(ctx, t)
		if err != nil {
			fmt.Fprintln(stderr, err)
			status = 1
			continue
		}
		cctx, cancel := context.WithTimeout(ctx, 20*time.Second)
		caps, err := src.Capabilities(cctx)
		cancel()
		src.Close()
		if err != nil {
			fmt.Fprintf(stderr, "target %s: cannot be asked: %v\n", t.Name, err)
			status = 1
			continue
		}
		out, _ := json.MarshalIndent(map[string]any{"target": t.Name, "capabilities": caps}, "", "  ")
		fmt.Fprintln(stdout, string(out))
		for kind, want := range map[string]struct {
			asked bool
			cap   collector.Capability
		}{
			"statements": {t.Statements.Enabled, caps.Statements},
			"activity":   {t.Activity.Enabled, caps.Activity},
			"slow log":   {t.SlowLog.Enabled, caps.SlowLog},
			"pools":      {t.Pools.Enabled, caps.Pools},
		} {
			switch {
			case want.asked && !want.cap.Available:
				fmt.Fprintf(stderr, "target %s: %s is asked for but cannot be collected: %v\n", t.Name, kind, want.cap.Problems)
				status = 1
			case want.asked && len(want.cap.Problems) > 0: // it works, with something worth changing: not a failure
				fmt.Fprintf(stderr, "target %s: warning for %s: %v\n", t.Name, kind, want.cap.Problems)
			}
		}
	}
	return status
}
