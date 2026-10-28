// alert-router receives Alertmanager webhooks and forwards the alerts to its
// sinks. Today the only sink is a text log; the triage agent will be another.
package main

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"github.com/sojoner/kro-lab/platform-mvp/alert-router/config"
	"github.com/sojoner/kro-lab/platform-mvp/alert-router/dedupe"
	"github.com/sojoner/kro-lab/platform-mvp/alert-router/server"
	"github.com/sojoner/kro-lab/platform-mvp/alert-router/sink"
)

func main() {
	log := slog.New(slog.NewTextHandler(os.Stdout, nil))

	cfg, err := config.Parse(os.Args[1:])
	if err != nil {
		log.Error("invalid configuration", "error", err)
		os.Exit(2)
	}

	srv := &server.Server{
		Token: func() (string, error) {
			b, err := os.ReadFile(cfg.TokenFile)
			return strings.TrimSpace(string(b)), err
		},
		// The log is the record of every alert, so it is the one required sink.
		Sinks:       []server.Registration{{Sink: sink.Log{Logger: log}, Required: true}},
		Deduper:     dedupe.New(cfg.DedupeTTL),
		SinkTimeout: cfg.SinkTimeout,
		Log:         log,
	}
	httpSrv := &http.Server{Addr: cfg.Listen, Handler: srv.Handler(), ReadHeaderTimeout: 10 * time.Second}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	go func() {
		<-ctx.Done()
		shutdown, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		_ = httpSrv.Shutdown(shutdown)
	}()

	log.Info("starting alert-router", "listen", cfg.Listen, "dedupe_interval", cfg.DedupeTTL.String())
	if err := httpSrv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
		log.Error("server exited with error", "error", err)
		os.Exit(1)
	}
}
