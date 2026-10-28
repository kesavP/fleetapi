package controller

import (
	"github.com/prometheus/client_golang/prometheus"
	"sigs.k8s.io/controller-runtime/pkg/metrics"
)

var (
	ReconcileTotal = prometheus.NewCounterVec(
		prometheus.CounterOpts{
			Name: "binding_controller_reconcile_total",
			Help: "Total number of RegionalWidgetRequest reconciles, partitioned by tenant and region",
		},
		[]string{"tenant_id", "region", "result"},
	)
)

func init() {
	ReconcileTotal.WithLabelValues("init", "none", "success").Inc()
	metrics.Registry.MustRegister(ReconcileTotal)
}
