// vmset-gateway is a thin HTTP API in front of the hub's VmSet resource. It
// forwards each caller's bearer token to the hub API server, so Kubernetes
// RBAC stays the only authorization layer.
package main

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/sojoner/kro-lab/platform-mvp/vmset-gateway/config"
	"github.com/sojoner/kro-lab/platform-mvp/vmset-gateway/model"
	"github.com/sojoner/kro-lab/platform-mvp/vmset-gateway/server"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/clientcmd"
)

func main() {
	log := slog.New(slog.NewJSONHandler(os.Stdout, nil))

	cfg, err := config.Parse(os.Args[1:])
	if err != nil {
		log.Error("invalid configuration", "error", err)
		os.Exit(2)
	}

	hub, err := hubConfig(cfg.HubKubeconfig)
	if err != nil {
		log.Error("failed to build hub config", "error", err)
		os.Exit(1)
	}

	srv := &server.Server{
		Clients:  server.NewClientFactory(hub),
		Resource: cfg.Resource,
		Rules: model.Rules{
			Sizes: cfg.Sizes, DefaultSize: cfg.DefaultSize,
			MaxReplicas: cfg.MaxReplicas, SSHKeyFormat: cfg.SSHKeyFormat,
		},
		Heartbeat: cfg.Heartbeat,
		Log:       log,
	}
	httpSrv := &http.Server{
		Addr:              cfg.Listen,
		Handler:           srv.Handler(),
		ReadHeaderTimeout: 10 * time.Second,
		// No WriteTimeout: it would cut off the event streams.
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	go func() {
		<-ctx.Done()
		shutdown, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		_ = httpSrv.Shutdown(shutdown)
	}()

	log.Info("starting vmset-gateway", "listen", cfg.Listen, "resource", cfg.Resource)
	if err := httpSrv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
		log.Error("server exited with error", "error", err)
		os.Exit(1)
	}
}

// hubConfig reads the hub's address and CA. Credentials in it are dropped by
// the client factory; callers always bring their own token.
func hubConfig(kubeconfig string) (*rest.Config, error) {
	if kubeconfig == "" {
		return rest.InClusterConfig()
	}
	return clientcmd.BuildConfigFromFlags("", kubeconfig)
}
