package detect

import (
	"math"
	"testing"
)

func TestWorkloadOf(t *testing.T) {
	cases := []struct {
		id         string
		want       Workload
		ok         bool
		describing string
	}{
		{"/k8s/shop/frontend-5d6f8c7b9-xk2lp/server", Workload{ID: "/k8s/shop/frontend", Label: "shop/frontend"}, true, "a deployment's pod"},
		{"/k8s/kube-system/node-agent-7xk2l/agent", Workload{ID: "/k8s/kube-system/node-agent", Label: "kube-system/node-agent"}, true, "a daemon set's pod"},
		{"/k8s/monitoring/telemetry-clickhouse-0/clickhouse", Workload{ID: "/k8s/monitoring/telemetry-clickhouse", Label: "monitoring/telemetry-clickhouse"}, true, "a stateful set's pod"},
		{"/k8s-cronjob/shop/cleanup/job", Workload{ID: "/k8s-cronjob/shop/cleanup", Label: "shop/cleanup"}, true, "a cron job"},
		{"/k8s/shop/standalone/app", Workload{ID: "/k8s/shop/standalone", Label: "shop/standalone"}, true, "a pod with no controller"},
		{"/system.slice/nginx.service", Workload{ID: "/system.slice/nginx.service", Label: "nginx", Host: true}, true, "a process on a host"},
		{"/docker/postgres", Workload{ID: "/docker/postgres", Label: "postgres", Host: true}, true, "a plain docker container"},
		{"/nomad/abc/task", Workload{}, false, "an orchestrator that is not grouped"},
		{"/k8s/onlytwo", Workload{}, false, "a Kubernetes id that is too short is not a host"},
		{"/", Workload{}, false, "the root"},
		{"", Workload{}, false, "nothing"},
		{"nginx", Workload{}, false, "no leading slash"},
	}
	for _, c := range cases {
		got, ok := WorkloadOf(c.id)
		if ok != c.ok || got != c.want {
			t.Errorf("%s (%q): got %+v %v, want %+v %v", c.describing, c.id, got, ok, c.want, c.ok)
		}
	}
}

func TestIsServerError(t *testing.T) {
	for s, want := range map[string]bool{
		"500": true, "503": true, "5xx": true, "599": true,
		"200": false, "404": false, "499": false, "": false, "50": false, "5000": false,
		"grpc:INTERNAL": true, "grpc:UNAVAILABLE": true, "grpc:DEADLINE_EXCEEDED": true,
		"grpc:NOT_FOUND": false, "grpc:INVALID_ARGUMENT": false, "grpc:OK": false, "grpc:": false,
	} {
		if got := IsServerError(s); got != want {
			t.Errorf("IsServerError(%q) = %v, want %v", s, got, want)
		}
	}
}

func TestParseLe(t *testing.T) {
	for s, want := range map[string]float64{"0.5": 0.5, "1": 1, "+Inf": math.Inf(1), "inf": math.Inf(1), " 0.25 ": 0.25, "1e-3": 0.001} {
		if got := ParseLe(s); got != want {
			t.Errorf("ParseLe(%q) = %v, want %v", s, got, want)
		}
	}
	for _, s := range []string{"", "abc", "0.5s", "1..2", "-", "+"} {
		if got := ParseLe(s); !math.IsNaN(got) {
			t.Errorf("ParseLe(%q) = %v, want NaN", s, got)
		}
	}
}
