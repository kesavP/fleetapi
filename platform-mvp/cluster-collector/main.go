// Command cluster-collector reports what is in a Kubernetes cluster (nodes, namespaces, workloads, pods, volume claims, autoscalers, events)
// to the hub's telemetry-ingest. It runs once in each cluster, with a read-only service account, and only ever pushes: the hub never reaches
// into the cluster.
package main

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"sort"
	"syscall"
	"time"

	"k8s.io/client-go/dynamic"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/clientcmd"

	"github.com/sojoner/kro-lab/platform-mvp/cluster-collector/collector"
	"github.com/sojoner/kro-lab/platform-mvp/cluster-collector/config"
	"github.com/sojoner/kro-lab/platform-mvp/cluster-collector/kube"
)

func main() {
	log := slog.New(slog.NewTextHandler(os.Stderr, nil))
	cfg, err := config.Parse(os.Args[1:], config.OSEnvironment())
	if err != nil {
		log.Error("invalid configuration", "error", err)
		os.Exit(2)
	}
	rc, err := restConfig(cfg.Kubeconfig)
	if err != nil {
		log.Error("cannot reach the cluster", "error", err)
		os.Exit(2)
	}
	kc, err := kubernetes.NewForConfig(rc)
	if err != nil {
		log.Error("cannot build a Kubernetes client", "error", err)
		os.Exit(2)
	}
	dc, err := dynamic.NewForConfig(rc)
	if err != nil {
		log.Error("cannot build a Kubernetes client", "error", err)
		os.Exit(2)
	}
	src := kube.New(kc, dc, kube.Options{ExcludeNamespaces: cfg.Exclude, Events: cfg.Events})

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	if cfg.Check {
		os.Exit(check(ctx, src))
	}

	var sink collector.Sink = collector.WriterSink{W: os.Stdout}
	if !cfg.Stdout {
		client, err := httpClient(cfg)
		if err != nil {
			log.Error("invalid configuration", "error", err)
			os.Exit(2)
		}
		sink = &collector.HTTPSink{URL: cfg.IngestURL(), Key: cfg.Key, Client: client}
	}
	r := &collector.Runner{Source: src, Sink: sink, Cluster: cfg.Cluster, Interval: cfg.Interval, Resync: cfg.Resync, Log: log}

	if cfg.Listen != "" {
		mux := http.NewServeMux()
		mux.HandleFunc("/healthz", func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusOK) })
		// ready once a look has been reported, and while they keep being: a hub that is down does not get the collector restarted (that is
		// liveness's question, and /healthz does not ask it), but it is not "ready" either.
		mux.HandleFunc("/readyz", func(w http.ResponseWriter, _ *http.Request) {
			if !r.Healthy(3*cfg.Interval + time.Minute) {
				http.Error(w, "no look at the cluster has been reported lately", http.StatusServiceUnavailable)
				return
			}
			w.WriteHeader(http.StatusOK)
		})
		srv := &http.Server{Addr: cfg.Listen, Handler: mux, ReadHeaderTimeout: 10 * time.Second}
		go func() {
			if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
				log.Error("probe server exited", "error", err)
			}
		}()
		defer srv.Close()
	}
	r.Run(ctx)
}

func restConfig(kubeconfig string) (*rest.Config, error) {
	if kubeconfig != "" {
		return clientcmd.BuildConfigFromFlags("", kubeconfig)
	}
	return rest.InClusterConfig()
}

func httpClient(cfg config.Config) (*http.Client, error) {
	tc := &tls.Config{MinVersion: tls.VersionTLS12, InsecureSkipVerify: cfg.TLSSkipVerify} //nolint:gosec // an explicit, documented lab switch
	if cfg.CAFile != "" {
		pem, err := os.ReadFile(cfg.CAFile)
		if err != nil {
			return nil, fmt.Errorf("read the CA file: %w", err)
		}
		pool := x509.NewCertPool()
		if !pool.AppendCertsFromPEM(pem) {
			return nil, errors.New("the CA file holds no certificate")
		}
		tc.RootCAs = pool
	}
	return &http.Client{Timeout: 30 * time.Second, Transport: &http.Transport{TLSClientConfig: tc}}, nil
}

// check looks once and says what it found, for someone setting the collector up: what can be read, what cannot (a missing permission), and
// what would be left out.
func check(ctx context.Context, src *kube.Source) int {
	snap := src.Snapshot(ctx)
	counts := map[string]int{}
	for _, o := range snap.Objects {
		counts[o.Kind]++
	}
	kinds := make([]string, 0, len(snap.Listed))
	for k := range snap.Listed {
		kinds = append(kinds, k)
	}
	sort.Strings(kinds)
	for _, k := range kinds {
		n := counts[k]
		if k == collector.EventKey {
			n = len(snap.Events)
		}
		fmt.Printf("%-14s %d\n", k, n)
	}
	problems := make([]string, 0, len(snap.Problems))
	for k := range snap.Problems {
		problems = append(problems, k)
	}
	sort.Strings(problems)
	for _, k := range problems {
		fmt.Printf("%-14s CANNOT BE READ: %v\n", k, snap.Problems[k])
	}
	if snap.Skipped > 0 {
		fmt.Printf("%d objects would be left out (a name or a size the service does not take)\n", snap.Skipped)
	}
	if len(snap.Listed) == 0 {
		return 1
	}
	if len(snap.Problems) > 0 {
		return 3
	}
	return 0
}
