#!/usr/bin/env python3
"""Service map backend: the telemetry-ingest query API + the Kubernetes API -> /api/graph.

The node-agent exports per-connection counters labelled with the calling
container (/k8s/<ns>/<pod>/<container>) and the destination (ip:port). This
reads them as rates from telemetry-ingest's read API, resolves containers to
workloads and destination IPs to Services or Pods with `kubectl get`, and serves
the result with the page in index.html. Standard library only.

  kubectl -n monitoring port-forward svc/telemetry-ingest 8080:80 &
  export TELEMETRY_READ_KEY=$(kubectl -n monitoring get secret telemetry-ingest-keys \\
      -o go-template='{{index .data "gke.read"}}' | base64 -d)
  python3 hack/platform-mvp/servicemap/server.py --project gke --kube-context <ctx>
  # open http://127.0.0.1:8099        (or add --print 1h to print the graph as JSON and exit)

To look back at something that is over, give a fixed range (RFC 3339 or unix seconds):

  python3 hack/platform-mvp/servicemap/server.py --project gke --kube-context <ctx> \\
      --from 2026-11-07T13:48:00Z --to 2026-11-07T13:52:00Z
  # or open http://127.0.0.1:8099/?from=2026-11-07T13:48:00Z&to=2026-11-07T13:52:00Z

The read key comes from the environment or a file, never from an argument, so it
does not show up in a process list. Metric names and labels are the node-agent's:
container_net_tcp_successful_connects_total{destination,actual_destination},
container_net_tcp_failed_connects_total{destination} and
container_http_requests_total{status,...}. Connections to loopback (127.0.0.0/8, ::1,
localhost) are not edges and are dropped. An edge's error rate is the share of
HTTP 5xx responses and gRPC server-side failures (UNAVAILABLE, INTERNAL, ...) among
its requests, with each failed TCP connect counted as one more failed call; where no
HTTP or gRPC is seen at all, it is the share of connects that failed.
"""
import argparse, collections, concurrent.futures, datetime, ipaddress, json, math, os, re, subprocess, sys, threading, time
import urllib.error, urllib.parse, urllib.request
from http.server import BaseHTTPRequestHandler, ThreadingHTTPServer

HERE = os.path.dirname(os.path.abspath(__file__))
WINDOW_RE = re.compile(r"^[1-9][0-9]{0,2}[smhd]$")
UNIT_SECONDS = {"s": 1, "m": 60, "h": 3600, "d": 86400}
CACHE_SECONDS = 30
MAX_SPAN_SECONDS = 30 * 86400

CONNECTS = "container_net_tcp_successful_connects_total"
FAILED_CONNECTS = "container_net_tcp_failed_connects_total"
HTTP_REQUESTS = "container_http_requests_total"
SERVER_ERROR = re.compile(r"5..")  # "500" and the "5xx" class form alike

# gRPC runs over HTTP/2, and the node-agent counts it in container_http_requests_total
# with status "grpc:<CODE>" whenever the call carries a gRPC status (otherwise the
# HTTP class, "2xx"). A server-side failure is the gRPC counterpart of a 5xx. The
# codes a caller causes (INVALID_ARGUMENT, NOT_FOUND, PERMISSION_DENIED,
# UNAUTHENTICATED, FAILED_PRECONDITION, ...) are not counted, just as 4xx is not;
# CANCELLED, ABORTED and RESOURCE_EXHAUSTED are left out for the same reason. This is
# the set OpenTelemetry's gRPC conventions treat as errors for a server span.
GRPC_SERVER_ERRORS = frozenset({"UNKNOWN", "DEADLINE_EXCEEDED", "UNIMPLEMENTED", "INTERNAL", "UNAVAILABLE", "DATA_LOSS"})
GRPC_PREFIX = "grpc:"


def is_server_error(status):
    """True for an HTTP 5xx and for a gRPC server-side failure code."""
    if status.startswith(GRPC_PREFIX):
        return status[len(GRPC_PREFIX):] in GRPC_SERVER_ERRORS
    return bool(SERVER_ERROR.fullmatch(status))


# Data stores and brokers. The node-agent counts what each client sends and receives for these
# protocols exactly as it does for HTTP, labelled with the destination, with a status of ok,
# failed or unknown, and keeps a latency histogram for most of them (RabbitMQ and NATS count
# messages by method and keep none). ZooKeeper (a different status vocabulary) and Dubbo (an RPC
# protocol, not a data store) are left out. Only the protocols a cluster reports are queried.
# The latency is the CLIENT's measurement, and it is only as good as the agent's matching of a
# request to its response for that protocol: see the note on ClickHouse below.
L7_PROTOCOLS = (
    # name, calls counter, latency histogram or None, unit
    ("postgres", "container_postgres_queries_total", "container_postgres_queries_duration_seconds_total", "queries/s"),
    ("mysql", "container_mysql_queries_total", "container_mysql_queries_duration_seconds_total", "queries/s"),
    ("redis", "container_redis_queries_total", "container_redis_queries_duration_seconds_total", "queries/s"),
    ("memcached", "container_memcached_queries_total", "container_memcached_queries_duration_seconds_total", "queries/s"),
    ("mongo", "container_mongo_queries_total", "container_mongo_queries_duration_seconds_total", "queries/s"),
    # No latency for ClickHouse: the client's measurement disagrees with the server's by two orders of
    # magnitude. On a real cluster telemetry-ingest's calls showed p95 8.7 s with 10% over 5 s, while the
    # ClickHouse container's own histogram said p95 22 ms with none over 5 s, and ClickHouse's query_log
    # agreed with the latter (p95 72 ms). The native protocol keeps a connection open across statements, so
    # matching a request to its response goes wrong. Redis, checked the same way, agrees to the digit.
    # Rate and errors are still reported. Put the histogram name back only after checking it the same way.
    ("clickhouse", "container_clickhouse_queries_total", None, "queries/s"),
    ("cassandra", "container_cassandra_queries_total", "container_cassandra_queries_duration_seconds_total", "queries/s"),
    ("foundationdb", "container_foundationdb_requests_total", "container_foundationdb_requests_duration_seconds_total", "req/s"),
    ("kafka", "container_kafka_requests_total", "container_kafka_requests_duration_seconds_total", "req/s"),
    ("rabbitmq", "container_rabbitmq_messages_total", None, "msgs/s"),
    ("nats", "container_nats_messages_total", None, "msgs/s"),
)
HTTP_UNIT = "req/s"
FAILED_STATUS = "failed"   # what the agent labels a failed query, as 5xx labels a failed HTTP request


class ApiError(Exception):
    """The telemetry API could not give an answer; the message is safe to show."""


def window_seconds(window):
    return int(window[:-1]) * UNIT_SECONDS[window[-1]]


def parse_time(value):
    """RFC 3339 or unix seconds, the two forms the read API accepts. A time with no
    zone is taken as UTC."""
    value = str(value).strip()
    try:
        return float(value)
    except ValueError:
        pass
    try:
        t = datetime.datetime.fromisoformat(value)
    except ValueError:
        raise ValueError("%r is neither RFC 3339 (2026-11-07T13:48:00Z) nor unix seconds" % value)
    if t.tzinfo is None:
        t = t.replace(tzinfo=datetime.timezone.utc)
    return t.timestamp()


def iso(epoch):
    return datetime.datetime.fromtimestamp(epoch, datetime.timezone.utc).strftime("%Y-%m-%dT%H:%M:%SZ")


def resolve_range(window, now=None):
    """-> (start, end, label) in epoch seconds. `window` is either a relative span such as
    "15m" or "1h", ending now, or a (from, to) pair of epoch seconds for a fixed range,
    which is how an incident that is over is looked at."""
    if isinstance(window, tuple):
        start, end = window
        label = "%s/%s" % (iso(start), iso(end))
    else:
        end = time.time() if now is None else now
        start, label = end - window_seconds(window), window
    if not start < end:
        raise ValueError("from must be before to")
    if end - start > MAX_SPAN_SECONDS:
        raise ValueError("the range is longer than %d days" % (MAX_SPAN_SECONDS // 86400))
    return start, end, label


class TelemetryAPI:
    """Read access to one project of telemetry-ingest."""

    def __init__(self, base, project, key, timeout=30):
        self.base, self.project, self.key, self.timeout = base.rstrip("/"), project, key, timeout

    def _get(self, path, params, what):
        url = "%s/api/v1/projects/%s/%s" % (self.base, urllib.parse.quote(self.project, safe=""), path)
        if params:
            url += "?" + urllib.parse.urlencode(params)
        req = urllib.request.Request(url, headers={"X-API-Key": self.key})
        try:
            with urllib.request.urlopen(req, timeout=self.timeout) as r:
                return json.load(r)
        except urllib.error.HTTPError as e:
            detail = ""
            try:
                detail = json.load(e).get("error", "")
            except Exception:
                pass
            if e.code in (401, 403):
                raise ApiError("the read key was rejected for project '%s' (HTTP %d): %s" % (self.project, e.code, detail or "check the key and the project name"))
            raise ApiError("telemetry-ingest answered HTTP %d for %s%s" % (e.code, what, ": " + detail if detail else ""))
        except urllib.error.URLError as e:
            raise ApiError("cannot reach telemetry-ingest at %s (%s); is the port-forward running?" % (self.base, e.reason))
        except (ValueError, OSError) as e:
            raise ApiError("unexpected answer from telemetry-ingest for %s: %s" % (what, e))

    def metric_names(self):
        """The metric names the project has, so only protocols the cluster reports are queried."""
        return set(self._get("metrics", None, "the metric list").get("data") or [])

    def rates(self, metric, start, end):
        """Mean per-second rate of each series of `metric` between two epoch times.

        Returns (list of (labels, rate), truncated). The service computes the
        rate per bucket with counter resets handled, and returns a point only for
        a bucket in which the series had readings. The rate over the range is the
        total of those points divided by ALL the buckets of the range, so a quiet
        stretch counts as zero traffic. Averaging only the points that exist would
        skip every quiet moment, and would count a restart twice: the pod IP is a
        label, so the new pod is a new series, each series would be averaged over
        its own short life and the two added together. An outage then read as busier
        than normal.

        from_zero=true asks the service to count a counter's first appearance as an
        increase. The agent creates these counters (failed connects, a request status)
        when the first event happens, so their first value is that event, which a rate
        would otherwise drop. The service applies it only to a series created inside the
        range; a service that does not know the option ignores it.
        """
        # About 40 buckets however long the range, so a busy cluster stays within
        # the service's per-call point limit.
        step = max(15, int((end - start) // 40))
        start, end = round(start, 3), round(end, 3)   # the service sees exactly what is sent
        # The service buckets on the epoch grid and a rate needs a previous reading, so the
        # first bucket of the range has no point: the range holds this many rate buckets.
        buckets = max(int(end // step) - int(start // step), 1)
        body = self._get("metrics/query_range", {
            "name": metric, "from": "%.3f" % start, "to": "%.3f" % end,
            "step": str(step), "rate": "true", "from_zero": "true",
        }, metric)
        data = body.get("data") or {}
        out = []
        for s in data.get("series") or []:
            pts = [p["v"] for p in s.get("points") or []]
            if pts:
                out.append((s.get("labels") or {}, sum(pts) / buckets))
        return out, bool(data.get("truncated"))


def histogram_quantile(q, buckets):
    """The q-quantile of a latency histogram, as Prometheus computes it. `buckets` maps each upper
    bound (seconds, float("inf") for the overflow bucket) to the CUMULATIVE per-second rate of calls
    at or under it. The value is interpolated linearly inside the bucket that holds the rank. When
    the rank lies in the overflow bucket the answer is the largest finite bound: "at least this".
    Returns None when there is nothing to report."""
    pts = sorted(buckets.items())
    if not pts:
        return None
    running, cumulative = 0.0, []
    for bound, count in pts:    # independently summed bucket rates can dip; a cumulative histogram never does
        running = max(running, count)
        cumulative.append((bound, running))
    total = cumulative[-1][1]
    if total <= 0:
        return None
    rank = q * total
    prev_bound, prev_count = 0.0, 0.0
    for bound, count in cumulative:
        if count >= rank:
            if math.isinf(bound):
                return prev_bound if prev_bound > 0 else None
            if count == prev_count:
                return bound
            return prev_bound + (bound - prev_bound) * (rank - prev_count) / (count - prev_count)
        prev_bound, prev_count = bound, count
    return None


def workload_name(pod):
    """Pod -> owning workload name; a ReplicaSet's hash suffix is stripped."""
    meta = pod["metadata"]
    for o in meta.get("ownerReferences", []):
        if o["kind"] == "ReplicaSet":
            return o["name"].rsplit("-", 1)[0], "Deployment"
        return o["name"], o["kind"]
    return meta["name"], "Pod"


class Cluster:
    """Pod and Service addresses, from `kubectl get -o json` or a fixture file.

    `name` is the cluster's name in a fleet: it is stamped on every workload so two clusters' workloads
    (every cluster has a shop/frontend) stay apart. A single cluster needs none."""

    def __init__(self, context, fixture, name=""):
        self.context, self.fixture, self.name = context, fixture, name
        self.lock, self.loaded, self.data = threading.Lock(), 0.0, None

    def _load(self):
        if self.fixture:
            with open(self.fixture) as f:
                return json.load(f)
        cmd = ["kubectl"] + (["--context", self.context] if self.context else [])
        cmd += ["get", "pods,services,nodes", "-A", "-o", "json"]
        return json.loads(subprocess.run(cmd, check=True, capture_output=True, text=True, timeout=30).stdout)

    def _refresh(self):
        with self.lock:
            if self.data is None or time.time() - self.loaded > CACHE_SECONDS:
                raw = self._load()
                pods, ip, svc, workloads, services, node_addrs = {}, {}, {}, {}, [], []
                for item in raw["items"]:
                    meta = item["metadata"]
                    if item["kind"] == "Pod":
                        name, kind = workload_name(item)
                        w = {"ns": meta["namespace"], "name": name, "kind": kind, "cluster": self.name}
                        pods[(meta["namespace"], meta["name"])] = w
                        workloads[(w["ns"], name)] = w
                        for p in item.get("status", {}).get("podIPs", []) or []:
                            ip[p["ip"]] = w
                    elif item["kind"] == "Service":
                        services.append(item)
                        cip = item["spec"].get("clusterIP")
                        if cip and cip != "None":
                            svc[cip] = (meta["namespace"], meta["name"])
                    elif item["kind"] == "Node":
                        node_addrs += [a["address"] for a in item.get("status", {}).get("addresses", []) or []
                                       if a.get("type") in ("InternalIP", "ExternalIP") and a.get("address")]
                workload_of = lambda ns, name: workloads.get((ns, name)) or {"ns": ns, "name": name, "kind": "Service", "cluster": self.name}
                for cip, (ns, name) in svc.items():
                    ip[cip] = workload_of(ns, name)
                # What this cluster exposes to the outside, which is the only way another cluster can
                # recognise a call to it: a load balancer IP or host name, an external IP, or a node
                # address with a NodePort. Pod and Service IPs are not here: they overlap between clusters.
                exposed = {}
                for item in services:
                    meta, spec = item["metadata"], item.get("spec", {})
                    w = workload_of(meta["namespace"], meta["name"])
                    ports = spec.get("ports") or []
                    ingress = (item.get("status", {}).get("loadBalancer", {}) or {}).get("ingress") or []
                    hosts = [i.get("ip") or i.get("hostname") for i in ingress] + list(spec.get("externalIPs") or [])
                    for h in filter(None, hosts):
                        for p in ports:
                            exposed[(h.lower(), int(p["port"]))] = w
                    for p in ports:
                        if p.get("nodePort"):
                            for a in node_addrs:
                                exposed[(a.lower(), int(p["nodePort"]))] = w
                self.data, self.loaded = (pods, ip, exposed), time.time()
            return self.data

    def index(self):
        return self._refresh()[:2]

    def exposed(self):
        """(host, port) -> workload, for every address this cluster exposes."""
        return self._refresh()[2]


ClusterSpec = collections.namedtuple("ClusterSpec", "name api inventory")
AMBIGUOUS = object()


class Fleet:
    """The addresses every cluster exposes, to recognise a call from one cluster to another."""

    def __init__(self, specs):
        self.exposed = {s.name: s.inventory.exposed() for s in specs}

    def lookup(self, own, addresses):
        """The workload an address leads to, from `own`'s point of view: AMBIGUOUS when two other
        clusters claim it and there is no telling which was meant, None when nobody exposes it. The
        caller's own cluster is asked first: a call to an address it exposes is its own."""
        for addr in addresses:
            try:
                port = int(addr.rsplit(":", 1)[1])
            except (IndexError, ValueError):
                continue
            key = (host_of(addr).lower(), port)
            mine = self.exposed.get(own, {}).get(key)
            if mine:
                return mine
            others = [exp[key] for c, exp in self.exposed.items() if c != own and key in exp]
            if len(others) == 1:
                return others[0]
            if len(others) > 1:
                return AMBIGUOUS
        return None


def host_of(address):
    """The host part of "ip:port", "[ipv6]:port" or "name:port"; a bare address is returned as is."""
    address = address.strip()
    if address.startswith("["):
        return address[1:].split("]", 1)[0]
    if address.count(":") == 1:
        return address.split(":", 1)[0]
    return address


def is_loopback(address):
    """True for 127.0.0.0/8, ::1 (also written as an IPv4-mapped address) and "localhost".
    Only a real loopback address counts: 128.0.0.1, 10.0.0.127 and 127.example.com do not."""
    host = host_of(address)
    if host.lower() == "localhost":
        return True
    try:
        ip = ipaddress.ip_address(host)
    except ValueError:
        return False
    if getattr(ip, "ipv4_mapped", None) is not None:
        ip = ip.ipv4_mapped
    return ip.is_loopback


def node_id(w, with_cluster=False):
    base = "%s/%s" % (w["ns"], w["name"]) if w["ns"] else w["name"]
    return "%s/%s" % (w["cluster"], base) if with_cluster and w.get("cluster") else base


def build_graph(api, cluster, window, fleet=None):
    """The graph of one cluster. With a `fleet`, ids carry the cluster name and a destination no local
    address explains is looked up among the addresses the clusters expose."""
    pods, ip = cluster.index()
    nid = lambda w: node_id(w, fleet is not None)
    start, end, label = resolve_range(window)
    present = api.metric_names()
    warnings = []

    def fetch(metric):
        series, truncated = api.rates(metric, start, end)
        if truncated:
            warnings.append("%s returned more series than one call allows, so the map is partial; use a shorter range" % metric)
        return series

    conn, fail, http = fetch(CONNECTS), fetch(FAILED_CONNECTS), fetch(HTTP_REQUESTS)
    # Each source is (what it adds to an edge, protocol, series). Summing every series of an edge is
    # the same as grouping by caller and destination first and then summing.
    sources = [
        ("conn", "", conn),
        ("fail", "", fail),
        ("calls", "http", http),
        ("errors", "http", [(m, v) for m, v in http if is_server_error(m.get("status", ""))]),
    ]
    units = {"http": HTTP_UNIT}
    for name, counter, histogram, unit in L7_PROTOCOLS:
        if counter not in present:
            continue
        units[name] = unit
        series = fetch(counter)
        sources.append(("calls", name, series))
        sources.append(("errors", name, [(m, v) for m, v in series if m.get("status") == FAILED_STATUS]))
        if histogram and histogram + "_bucket" in present:
            sources.append(("latency", name, fetch(histogram + "_bucket")))

    nodes, edges, serves = {}, {}, {}
    loopback, ambiguous = set(), set()
    for kind, proto, series in sources:
        for m, value in series:
            parts = m.get("container_id", "").split("/")
            if len(parts) < 5 or parts[1] != "k8s":
                continue  # a host process or systemd unit, not a pod
            src = pods.get((parts[2], parts[3]))
            if not src:
                continue
            dest = m.get("destination", "")
            # A connection to 127.0.0.0/8 or ::1 stays inside one host or one pod (an app and its
            # sidecar, a probe, a local agent): it is not a call between services. Either address
            # being loopback is enough: the peer that was dialled, or the one actually reached.
            if is_loopback(dest) or is_loopback(m.get("actual_destination", "")):
                loopback.add((m.get("container_id", ""), dest))
                continue
            target = ip.get(m.get("actual_destination", "").rsplit(":", 1)[0]) or ip.get(dest.rsplit(":", 1)[0])
            if target is None and fleet is not None:
                # Pod and Service addresses mean the caller's own cluster: they overlap between clusters.
                # Any other address is looked up among what each cluster exposes.
                found = fleet.lookup(cluster.name, [a for a in (dest, m.get("actual_destination", "")) if a])
                if found is AMBIGUOUS:
                    ambiguous.add((m.get("container_id", ""), dest))
                else:
                    target = found
            dst = target or {"ns": "", "name": dest, "kind": "External", "ext": True}
            if nid(dst) == nid(src):
                continue
            for w in (src, dst):
                nodes.setdefault(nid(w), dict(id=nid(w), name=w["name"], namespace=w["ns"] or None, kind=w["kind"],
                                              external=bool(w.get("ext")), cluster=w.get("cluster") or None))
            e = edges.setdefault((nid(src), nid(dst)), dict(conn=0.0, fail=0.0, calls={}, latency={},
                                                           cluster=src.get("cluster") or None,
                                                           cross=bool(dst.get("cluster")) and dst.get("cluster") != src.get("cluster")))
            if kind == "conn" or kind == "fail":
                e[kind] += value
            elif kind == "calls":
                e["calls"].setdefault(proto, [0.0, 0.0])[0] += value
                if proto != "http" and value > 0:
                    serves.setdefault(nid(dst), set()).add(proto)
            elif kind == "errors":
                e["calls"].setdefault(proto, [0.0, 0.0])[1] += value
            elif kind == "latency":
                try:
                    bound = float(m.get("le", ""))
                except ValueError:
                    continue
                buckets = e["latency"].setdefault(proto, {})
                buckets[bound] = buckets.get(bound, 0.0) + value

    out = []
    for (a, b), e in edges.items():
        active = {p: c for p, c in e["calls"].items() if c[0] > 0}
        total = sum(c[0] for c in active.values())
        failed_calls = sum(c[1] for c in active.values())
        if total > 0:
            # A connect that fails never becomes a request or a query, so the call counters cannot
            # see it: a callee that is refusing connections would look healthy for as long as some
            # calls still get through. Each failed connect is counted as one failed call. That is an
            # approximation (a client that retries makes several attempts), but it keeps the failure
            # visible. The share cannot pass 100%, because the call errors are part of the calls.
            err = 100 * (failed_calls + e["fail"]) / (total + e["fail"])
            basis = ("http" if set(active) == {"http"} else "calls") + ("+tcp" if e["fail"] > 0 else "")
        elif e["conn"] + e["fail"] > 0:
            err = 100 * e["fail"] / (e["conn"] + e["fail"])
            basis = "tcp"
        else:
            err, basis = 0.0, "tcp"
        dominant = max(active, key=lambda p: active[p][0]) if active else None
        p95 = None
        if dominant and dominant in e["latency"]:
            # One protocol's histogram only: HTTP and a database have different buckets and cannot be
            # added, so an edge that mixes protocols reports the latency of the one that carries most calls.
            q = histogram_quantile(0.95, e["latency"][dominant])
            p95 = round(q * 1000, 3) if q is not None else None
        out.append(dict(**{"from": a, "to": b}, rps=round(total, 3) if total > 0 else None,
                        connPerSec=round(e["conn"], 3), errPct=round(err, 2), errBasis=basis, p95ms=p95,
                        protocol=dominant, protocols=sorted(active), cluster=e["cluster"], crossCluster=e["cross"],
                        unit=(units[dominant] if len(active) == 1 else "calls/s") if active else None))
    for n in nodes.values():
        n["serves"] = sorted(serves.get(n["id"], ()))
    return dict(window=label, range=dict(**{"from": iso(start), "to": iso(end)}), generated=int(time.time()), warnings=warnings,
                ignored={"loopback": len(loopback), "ambiguous": len(ambiguous)},
                nodes=sorted(nodes.values(), key=lambda n: n["id"]), edges=out)


CLUSTER_ERRORS = (ApiError, subprocess.SubprocessError, OSError, ValueError, KeyError)


def describe_error(exc):
    if isinstance(exc, subprocess.CalledProcessError):
        first = (exc.stderr or "").strip().splitlines()[:1]
        return "kubectl failed: %s" % (first[0] if first else "exit %d" % exc.returncode)
    return str(exc) or exc.__class__.__name__


def build_fleet_graph(specs, window):
    """One map for several clusters. Each cluster is read from its own project and named from its own
    Kubernetes API, in parallel. A cluster that cannot be read is reported and the others are still drawn:
    a datacenter that is unreachable must not blank the map. Only when none can be read is that an error."""
    resolve_range(window)   # one range for everyone: a bad one is refused before any cluster is touched
    failed = {}

    def run(fn, items):
        results = {}
        with concurrent.futures.ThreadPoolExecutor(max_workers=max(len(items), 1)) as pool:
            futures = {s.name: pool.submit(fn, s) for s in items}
            for name, f in futures.items():
                try:
                    results[name] = f.result()
                except CLUSTER_ERRORS as exc:
                    failed[name] = describe_error(exc)
        return results

    run(lambda s: s.inventory.exposed(), specs)             # reads every inventory (kubectl, or its file)
    readable = [s for s in specs if s.name not in failed]
    graphs = {}
    if readable:
        # Only clusters whose Kubernetes inventory could be read take part in recognising calls between clusters:
        # without it a cluster's exposed addresses are unknown, so a call to its load balancer shows as an
        # external endpoint. A cluster whose telemetry alone failed is still known by its addresses, so calls
        # to it are still drawn, while its own outgoing calls are what is missing.
        fleet = Fleet(readable)
        graphs = run(lambda s: build_graph(s.api, s.inventory, window, fleet=fleet), readable)
    if not graphs:
        raise ApiError("no cluster could be read: " + "; ".join("%s: %s" % (s.name, failed.get(s.name, "?")) for s in specs))

    nodes, edges, warnings, clusters = {}, [], [], []
    ignored = {"loopback": 0, "ambiguous": 0}
    for s in specs:                                         # in the order given, so the result does not depend on timing
        g = graphs.get(s.name)
        if g is None:
            clusters.append({"name": s.name, "ok": False, "error": failed[s.name], "nodes": 0, "edges": 0})
            warnings.append("%s: %s" % (s.name, failed[s.name]))
            continue
        clusters.append({"name": s.name, "ok": True, "error": None, "edges": len(g["edges"]),
                         "nodes": len([n for n in g["nodes"] if n["cluster"] == s.name])})
        for n in g["nodes"]:
            have = nodes.get(n["id"])   # an external endpoint is one node however many clusters call it
            if have is None:
                nodes[n["id"]] = n
            else:
                have["serves"] = sorted(set(have["serves"]) | set(n["serves"]))
        edges += g["edges"]
        warnings += ["%s: %s" % (s.name, w) for w in g["warnings"]]
        for k in ignored:
            ignored[k] += g["ignored"][k]
    first = next(iter(graphs.values()))
    return dict(window=first["window"], range=first["range"], generated=int(time.time()), warnings=warnings,
                ignored=ignored, clusters=clusters, nodes=sorted(nodes.values(), key=lambda n: n["id"]), edges=edges)


CLUSTER_NAME = re.compile(r"^[a-z0-9]([-a-z0-9]{0,30}[a-z0-9])?$")
CLUSTER_FIELDS = {"name", "project", "api", "kubeContext", "k8sFile", "readKeyFile", "readKeyEnv"}
SECRET_FIELDS = {"readKey", "key", "apiKey", "token", "password", "secret"}


def load_fleet_config(path, environ=None):
    """The clusters of a fleet, from a JSON file:

      {"api": "http://127.0.0.1:8080",
       "clusters": [
         {"name": "us", "project": "gke-us", "kubeContext": "gke_proj_zone_us", "readKeyFile": "us.key"},
         {"name": "dc1", "project": "dc1", "k8sFile": "dc1-inventory.json", "readKeyEnv": "DC1_READ_KEY",
          "api": "https://telemetry.dc1.example.com"}]}

    A read key is never written in the file: it is read from a file (readKeyFile) or from an environment
    variable (readKeyEnv). Each cluster must say where its Kubernetes names come from (kubeContext or
    k8sFile): the map never falls back to whatever kubectl's current context happens to be, because reading
    the wrong cluster would give a plausible, wrong map. Relative paths are relative to the config file."""
    environ = os.environ if environ is None else environ
    base = os.path.dirname(os.path.abspath(path))
    with open(path) as f:
        data = json.load(f)
    if not isinstance(data, dict) or not isinstance(data.get("clusters"), list) or not data["clusters"]:
        raise ValueError("%s: needs a non-empty \"clusters\" list" % path)
    default_api = data.get("api") or "http://127.0.0.1:8080"
    specs, seen = [], set()
    for i, c in enumerate(data["clusters"]):
        where = "%s: clusters[%d]" % (path, i)
        if not isinstance(c, dict):
            raise ValueError("%s must be an object" % where)
        for field in c:
            if field in SECRET_FIELDS:
                raise ValueError("%s: \"%s\" is a secret and must not be written in the file: put the key in a file (readKeyFile) or an environment variable (readKeyEnv)" % (where, field))
            if field not in CLUSTER_FIELDS:
                raise ValueError("%s: unknown field \"%s\"" % (where, field))
        name, project = c.get("name", ""), c.get("project", "")
        if not CLUSTER_NAME.match(str(name)):
            raise ValueError("%s: name %r must be lowercase letters, digits and dashes" % (where, name))
        if name in seen:
            raise ValueError("%s: the cluster name %r is used twice" % (where, name))
        seen.add(name)
        if not project or not isinstance(project, str):
            raise ValueError("%s (%s): \"project\" is required" % (where, name))
        if bool(c.get("kubeContext")) == bool(c.get("k8sFile")):
            raise ValueError("%s (%s): give exactly one of kubeContext and k8sFile; the map does not guess which cluster to read" % (where, name))
        if bool(c.get("readKeyFile")) == bool(c.get("readKeyEnv")):
            raise ValueError("%s (%s): give exactly one of readKeyFile and readKeyEnv" % (where, name))
        if c.get("readKeyFile"):
            key_path = os.path.join(base, c["readKeyFile"])
            try:
                with open(key_path) as f:
                    key = f.read().strip()
            except OSError as exc:
                raise ValueError("%s (%s): cannot read the key file: %s" % (where, name, exc.strerror))
        else:
            key = environ.get(c["readKeyEnv"], "").strip()
            if not key:
                raise ValueError("%s (%s): the environment variable %s is not set" % (where, name, c["readKeyEnv"]))
        if not key:
            raise ValueError("%s (%s): the read key is empty" % (where, name))
        api = c.get("api") or default_api
        if not re.match(r"^https?://", api):
            raise ValueError("%s (%s): api %r must start with http:// or https://" % (where, name, api))
        fixture = os.path.join(base, c["k8sFile"]) if c.get("k8sFile") else ""
        specs.append(ClusterSpec(name, TelemetryAPI(api, project, key), Cluster(c.get("kubeContext", ""), fixture, name=name)))
    return specs


def make_handler(api, cluster, fleet=None):
    """The map's HTTP surface. With `fleet` (a list of ClusterSpec) it draws all those clusters."""
    class H(BaseHTTPRequestHandler):
        def log_message(self, *a):
            pass

        def send(self, code, ctype, body):
            self.send_response(code)
            self.send_header("Content-Type", ctype)
            self.send_header("Content-Length", str(len(body)))
            self.send_header("Cache-Control", "no-store")
            self.end_headers()
            self.wfile.write(body)

        def do_GET(self):
            u = urllib.parse.urlparse(self.path)
            if u.path in ("/", "/index.html"):
                with open(os.path.join(HERE, "index.html"), "rb") as f:
                    return self.send(200, "text/html; charset=utf-8", f.read())
            if u.path == "/healthz":
                return self.send(200, "text/plain", b"ok\n")
            if u.path == "/api/graph":
                params = urllib.parse.parse_qs(u.query)
                if "from" in params:   # a fixed range: ?from=<time>[&to=<time>], RFC 3339 or unix seconds
                    try:
                        start = parse_time(params["from"][0])
                        end = parse_time(params["to"][0]) if "to" in params else time.time()
                        resolve_range((start, end))
                    except ValueError as exc:
                        return self.send(400, "application/json", json.dumps({"error": str(exc)}).encode())
                    window = (start, end)
                else:
                    window = params.get("window", ["1h"])[0]
                    if not WINDOW_RE.match(window):
                        return self.send(400, "application/json", b'{"error":"window must look like 15m, 1h, 6h"}')
                try:
                    graph = build_fleet_graph(fleet, window) if fleet else build_graph(api, cluster, window)
                    body = json.dumps(graph).encode()
                except (ApiError, subprocess.SubprocessError, OSError, ValueError, KeyError) as exc:
                    return self.send(502, "application/json", json.dumps({"error": str(exc)}).encode())
                return self.send(200, "application/json", body)
            self.send(404, "text/plain", b"not found\n")

    return H


def read_key(args):
    if args.read_key_file:
        try:
            with open(args.read_key_file) as f:
                key = f.read().strip()
        except OSError as exc:
            sys.exit("error: cannot read the key file: %s" % exc.strerror)
    else:
        key = os.environ.get("TELEMETRY_READ_KEY", "").strip()
    if not key:
        sys.exit("error: no read key; set TELEMETRY_READ_KEY or pass --read-key-file (the key is never taken as an argument)")
    return key


def main():
    # No abbreviations: with them, --read-key <secret> would be taken as
    # --read-key-file <secret>, and the secret would end up in an error message.
    ap = argparse.ArgumentParser(description=__doc__.split("\n")[0], allow_abbrev=False)
    ap.add_argument("--api", default="http://127.0.0.1:8080", help="telemetry-ingest, e.g. through kubectl port-forward")
    ap.add_argument("--project", default="", help="the project (cluster) whose data to map, e.g. gke")
    ap.add_argument("--clusters", default="", metavar="FILE",
                    help="a JSON file describing several clusters, drawn on one map; replaces --project, "
                         "--kube-context, --k8s-file and --read-key-file (see load_fleet_config)")
    ap.add_argument("--read-key-file", default="", help="file holding the project's read key (default: $TELEMETRY_READ_KEY)")
    ap.add_argument("--kube-context", default="", help="kubectl context for pod and Service addresses")
    ap.add_argument("--k8s-file", default="", help="read `kubectl get pods,services -A -o json` from a file instead")
    ap.add_argument("--listen", default="127.0.0.1:8099")
    ap.add_argument("--print", dest="print_window", default="", metavar="WINDOW",
                    help="print the graph for WINDOW (e.g. 1h) as JSON and exit, instead of serving")
    ap.add_argument("--from", dest="from_time", default="", metavar="TIME",
                    help="print the graph for a fixed range starting at TIME (RFC 3339 or unix seconds) and exit; "
                         "ends at --to, default now")
    ap.add_argument("--to", dest="to_time", default="", metavar="TIME", help="end of the fixed range (used with --from)")
    a = ap.parse_args()
    fleet = None
    if a.clusters:
        if a.project or a.kube_context or a.k8s_file or a.read_key_file:
            sys.exit("error: --clusters replaces --project, --kube-context, --k8s-file and --read-key-file: put them in the file")
        try:
            fleet = load_fleet_config(a.clusters)
        except (OSError, ValueError) as exc:
            sys.exit("error: %s" % exc)
        api = cluster = None
    else:
        if not a.project:
            sys.exit("error: give --project (one cluster) or --clusters FILE (several)")
        api = TelemetryAPI(a.api, a.project, read_key(a))
        cluster = Cluster(a.kube_context, a.k8s_file)
    if a.to_time and not a.from_time:
        sys.exit("error: --to needs --from")
    if a.from_time and a.print_window:
        sys.exit("error: use either --print WINDOW or --from/--to, not both")
    if a.print_window or a.from_time:
        if a.from_time:
            try:
                window = (parse_time(a.from_time), parse_time(a.to_time) if a.to_time else time.time())
                resolve_range(window)
            except ValueError as exc:
                sys.exit("error: %s" % exc)
        else:
            window = a.print_window
            if not WINDOW_RE.match(window):
                sys.exit("error: window must look like 15m, 1h, 6h")
        try:
            print(json.dumps(build_fleet_graph(fleet, window) if fleet else build_graph(api, cluster, window), indent=2))
        except (ApiError, subprocess.SubprocessError, OSError) as exc:
            sys.exit("error: %s" % exc)
        return
    host, port = a.listen.rsplit(":", 1)
    srv = ThreadingHTTPServer((host, int(port)), make_handler(api, cluster, fleet))
    print("service map on http://%s" % a.listen, file=sys.stderr)
    srv.serve_forever()


if __name__ == "__main__":
    main()
