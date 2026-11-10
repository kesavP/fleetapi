package detect

import (
	"math"
	"regexp"
	"strconv"
	"strings"
)

// Which application a container belongs to, and whether a request failed. The same rules as the UI's map/workload.js, kept in step with
// them: the incident the detector opens must name an application exactly as the page that shows it does.

// The node-agent names a service from its container id with these patterns (common/otel.go there). Their order matters: a Deployment's
// pod name is the most specific, a bare pod the least.
var (
	deploymentRE  = regexp.MustCompile(`^(/k8s/[a-z0-9-]+/[a-z0-9-]+)-[0-9a-f]{1,10}-[bcdfghjklmnpqrstvwxz2456789]{5}/.+`)
	daemonsetRE   = regexp.MustCompile(`^(/k8s/[a-z0-9-]+/[a-z0-9-]+)-[bcdfghjklmnpqrstvwxz2456789]{5}/.+`)
	statefulsetRE = regexp.MustCompile(`^(/k8s/[a-z0-9-]+/[a-z0-9-]+)-\d+/.+`)
	cronjobRE     = regexp.MustCompile(`^(/k8s-cronjob/[a-z0-9-]+/[a-z0-9-]+)/.+`)
	barePodRE     = regexp.MustCompile(`^(/k8s/[a-z0-9-]+/[a-z0-9-]+)/.+`)
	// Orchestrators the platform does not group (a Nomad job, an ECS service, a Swarm service): left out rather than taken for hosts.
	unsupportedRE = regexp.MustCompile(`^/(nomad|ecs|swarm)/`)
	kubernetesRE  = regexp.MustCompile(`^/k8s(-cronjob)?/`)
)

// Workload is an application: what a deployment, a daemon set, a stateful set, a cron job or a process on a plain host is called.
type Workload struct {
	ID    string // "/k8s/shop/frontend": the application id, also the service name on its traces
	Label string // "shop/frontend"
	Host  bool   // a process on a plain host
}

// WorkloadOf is the application a container id belongs to; false for an id that belongs to none.
func WorkloadOf(containerID string) (Workload, bool) {
	for _, re := range []*regexp.Regexp{deploymentRE, daemonsetRE, statefulsetRE, cronjobRE, barePodRE} {
		if m := re.FindStringSubmatch(containerID); m != nil {
			parts := strings.Split(m[1], "/") // "", "k8s", namespace, name
			return Workload{ID: m[1], Label: parts[2] + "/" + parts[3]}, true
		}
	}
	return hostWorkload(containerID)
}

// hostWorkload: a process or container on a plain host has its cgroup path as its id ("/system.slice/nginx.service"), the same on every
// machine that runs it; the name is the last part without ".service".
func hostWorkload(id string) (Workload, bool) {
	if !strings.HasPrefix(id, "/") || id == "/" || unsupportedRE.MatchString(id) || kubernetesRE.MatchString(id) {
		return Workload{}, false
	}
	var last string
	for _, p := range strings.Split(id, "/") {
		if p != "" {
			last = p
		}
	}
	name := strings.TrimSuffix(strings.TrimSuffix(last, ".service"), ".slice")
	if name == "" {
		name = last
	}
	if name == "" {
		return Workload{}, false
	}
	return Workload{ID: id, Label: name, Host: true}, true
}

// A failed gRPC call is counted with the status "grpc:<CODE>". Server-side failures are the counterpart of a 5xx; the codes a caller causes
// (INVALID_ARGUMENT, NOT_FOUND, ...) are not counted, as 4xx is not.
var grpcServerErrors = map[string]bool{"UNKNOWN": true, "DEADLINE_EXCEEDED": true, "UNIMPLEMENTED": true, "INTERNAL": true, "UNAVAILABLE": true, "DATA_LOSS": true}

var fiveXX = regexp.MustCompile(`^5..$`)

// IsServerError is true for an HTTP 5xx and for a gRPC server-side failure code.
func IsServerError(status string) bool {
	if rest, ok := strings.CutPrefix(status, "grpc:"); ok {
		return grpcServerErrors[rest]
	}
	return fiveXX.MatchString(status)
}

var leRE = regexp.MustCompile(`^[0-9.eE+-]+$`)

// ParseLe reads a histogram bound ("0.5", "+Inf"); NaN when it is not one.
func ParseLe(le string) float64 {
	s := strings.TrimSpace(le)
	if strings.EqualFold(s, "+inf") || strings.EqualFold(s, "inf") {
		return math.Inf(1)
	}
	if !leRE.MatchString(s) {
		return math.NaN()
	}
	f, err := strconv.ParseFloat(s, 64)
	if err != nil {
		return math.NaN()
	}
	return f
}
