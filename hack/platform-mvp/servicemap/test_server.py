#!/usr/bin/env python3
"""Tests for server.py against a stub of telemetry-ingest's read API and a
fixture of the cluster's pods and Services. No cluster, network or Docker.

  python3 -m unittest hack/platform-mvp/servicemap/test_server.py -v
"""
import json, os, re, sys, tempfile, threading, unittest, urllib.error, urllib.parse, urllib.request
from http.server import BaseHTTPRequestHandler, ThreadingHTTPServer

sys.path.insert(0, os.path.dirname(os.path.abspath(__file__)))
import server  # noqa: E402

KEY = "gke-read-key-0123456789abcdef"
PROJECT = "gke"

FRONTEND = "/k8s/shop/frontend-7d9f6b5c8-abcde/server"
CART = "/k8s/shop/cartservice-5c8b9d6f4-xyz12/server"
CART_VIA_SERVICE = "10.96.0.20:7070"   # the Service's ClusterIP
CART_POD = "10.0.0.11:7070"            # the pod behind it
REDIS_POD = "10.0.0.12:6379"

CLUSTER = {"items": [
    {"kind": "Pod", "metadata": {"namespace": "shop", "name": "frontend-7d9f6b5c8-abcde",
                                 "ownerReferences": [{"kind": "ReplicaSet", "name": "frontend-7d9f6b5c8"}]},
     "status": {"podIPs": [{"ip": "10.0.0.10"}]}},
    {"kind": "Pod", "metadata": {"namespace": "shop", "name": "cartservice-5c8b9d6f4-xyz12",
                                 "ownerReferences": [{"kind": "ReplicaSet", "name": "cartservice-5c8b9d6f4"}]},
     "status": {"podIPs": [{"ip": "10.0.0.11"}]}},
    {"kind": "Pod", "metadata": {"namespace": "shop", "name": "redis-0",
                                 "ownerReferences": [{"kind": "StatefulSet", "name": "redis"}]},
     "status": {"podIPs": [{"ip": "10.0.0.12"}]}},
    {"kind": "Pod", "metadata": {"namespace": "shop", "name": "productcatalog-6f7c9d8b5-q1w2e",
                                 "ownerReferences": [{"kind": "ReplicaSet", "name": "productcatalog-6f7c9d8b5"}]},
     "status": {"podIPs": [{"ip": "10.0.0.13"}]}},
    {"kind": "Service", "metadata": {"namespace": "shop", "name": "cartservice"},
     "spec": {"clusterIP": "10.96.0.20"}},
    {"kind": "Service", "metadata": {"namespace": "shop", "name": "productcatalog"},
     "spec": {"clusterIP": "10.96.0.21"}},
    {"kind": "Service", "metadata": {"namespace": "shop", "name": "redis"},
     "spec": {"clusterIP": "10.96.0.30"}},
    {"kind": "Service", "metadata": {"namespace": "shop", "name": "headless"}, "spec": {"clusterIP": "None"}},
]}


def series(labels, rate, active=(0.0, 1.0)):
    """A series that runs at a constant `rate` (per second) during the fraction `active` of
    whatever range is asked for, and has no data at all outside it. The stub turns it into
    the per-bucket points the API returns (see StubAPI.render): a bucket with nothing in it
    is absent, as in the real API, not zero."""
    return {"labels": labels, "rate": rate, "active": active}


def range_requests():
    """The rate queries the map made (not the metric-name listing)."""
    return [r for r in StubAPI.requests if r[0].endswith("/query_range")]


def bucket_starts_ms(q):
    """The buckets a rate query returns points for: the API buckets on the epoch grid, and a
    rate needs a previous reading, so the first bucket has none."""
    step, start, end = int(q["step"]), float(q["from"]), float(q["to"])
    return [b * step * 1000 for b in range(int(start // step) + 1, int(end // step) + 1)]


# What the stub API returns for each metric.
DATA = {
    server.CONNECTS: {"series": [
        # frontend -> cartservice, through the Service address; two series (two label sets,
        # as a restart or a second node gives) running at 1.0 and 0.5 per second
        series({"container_id": FRONTEND, "destination": CART_VIA_SERVICE, "actual_destination": CART_POD}, 1.0),
        series({"container_id": FRONTEND, "destination": CART_VIA_SERVICE, "actual_destination": CART_POD, "machine_id": "m2"}, 0.5),
        # frontend -> an address nothing in the cluster owns
        series({"container_id": FRONTEND, "destination": "8.8.8.8:443", "actual_destination": "8.8.8.8:443"}, 0.2),
        # cartservice -> redis (TCP only, no HTTP)
        series({"container_id": CART, "destination": REDIS_POD, "actual_destination": REDIS_POD}, 1.0),
        # cartservice -> itself: dropped
        series({"container_id": CART, "destination": CART_POD, "actual_destination": CART_POD}, 5.0),
        # a host process and a pod the cluster does not know: skipped
        series({"container_id": "/system.slice/kubelet.service", "destination": CART_POD}, 9.0),
        series({"container_id": "/k8s/shop/gone-pod-1/c", "destination": CART_POD}, 9.0),
    ], "truncated": False},
    server.FAILED_CONNECTS: {"series": [
        series({"container_id": FRONTEND, "destination": CART_VIA_SERVICE}, 0.5),   # no actual_destination label
        series({"container_id": CART, "destination": REDIS_POD}, 1.0),
    ], "truncated": False},
    server.HTTP_REQUESTS: {"series": [
        series({"container_id": FRONTEND, "destination": CART_VIA_SERVICE, "actual_destination": CART_POD, "status": "200"}, 9.0),
        series({"container_id": FRONTEND, "destination": CART_VIA_SERVICE, "actual_destination": CART_POD, "status": "500"}, 1.0),
        series({"container_id": FRONTEND, "destination": CART_VIA_SERVICE, "actual_destination": CART_POD, "status": "5xx"}, 0.0),
        series({"container_id": FRONTEND, "destination": CART_VIA_SERVICE, "actual_destination": CART_POD, "status": "404"}, 0.0),
    ], "truncated": False},
}


class StubAPI(BaseHTTPRequestHandler):
    requests = []      # (path, query dict, headers dict), shared with the tests
    data = DATA        # the data of PROJECT
    other = {}         # project -> data, for the other projects of a fleet
    keys = {}          # project -> read key, for those projects (PROJECT uses KEY)

    def log_message(self, *a):
        pass

    def do_GET(self):
        u = urllib.parse.urlparse(self.path)
        q = {k: v[0] for k, v in urllib.parse.parse_qs(u.query).items()}
        StubAPI.requests.append((u.path, q, {k.lower(): v for k, v in self.headers.items()}))
        m = re.match(r"^/api/v1/projects/([^/]+)/(.*)$", u.path)
        project, route = (m.group(1), m.group(2)) if m else ("", "")
        keys = dict(StubAPI.keys, **{PROJECT: KEY})
        if project not in keys or self.headers.get("X-API-Key") != keys[project]:
            return self.reply(403, {"error": "this key cannot read that project"})   # an unknown project looks like a wrong key
        data = StubAPI.data if project == PROJECT else StubAPI.other.get(project, {})
        if route == "metrics":
            return self.reply(200, {"data": sorted(data)})
        if route != "metrics/query_range":
            return self.reply(404, {"error": "no such route"})
        if q.get("rate") != "true" or "step" not in q or "from" not in q or "to" not in q:
            return self.reply(400, {"error": "rate=true needs a step"})
        d = data.get(q.get("name"), {"series": [], "truncated": False})
        self.reply(200, {"data": self.render(d, q)})

    @staticmethod
    def render(d, q):
        ts = bucket_starts_ms(q)
        out = []
        for s in d["series"]:
            lo, hi = s["active"]
            pts = [{"t": t, "v": s["rate"]} for t in ts[int(lo * len(ts)):int(hi * len(ts))]]
            if pts:
                out.append({"labels": s["labels"], "points": pts})
        return {"series": out, "truncated": d.get("truncated", False)}

    def reply(self, code, body):
        b = json.dumps(body).encode()
        self.send_response(code)
        self.send_header("Content-Type", "application/json")
        self.send_header("Content-Length", str(len(b)))
        self.end_headers()
        self.wfile.write(b)


def serve(handler):
    srv = ThreadingHTTPServer(("127.0.0.1", 0), handler)
    threading.Thread(target=srv.serve_forever, daemon=True).start()
    return srv


def stop(srv):
    srv.shutdown()
    srv.server_close()


class GraphTests(unittest.TestCase):
    @classmethod
    def setUpClass(cls):
        cls.stub = serve(StubAPI)
        cls.base = "http://127.0.0.1:%d" % cls.stub.server_address[1]
        cls.tmp = tempfile.TemporaryDirectory()
        path = os.path.join(cls.tmp.name, "cluster.json")
        with open(path, "w") as f:
            json.dump(CLUSTER, f)
        cls.cluster = server.Cluster("", path)

    @classmethod
    def tearDownClass(cls):
        stop(cls.stub)
        cls.tmp.cleanup()

    def setUp(self):
        StubAPI.requests.clear()
        StubAPI.data = DATA

    def api(self, key=KEY, project=PROJECT, base=None):
        return server.TelemetryAPI(base or self.base, project, key)

    def graph(self, window="1h"):
        return server.build_graph(self.api(), self.cluster, window)

    def edge(self, g, a, b):
        found = [e for e in g["edges"] if e["from"] == a and e["to"] == b]
        self.assertEqual(len(found), 1, "edge %s -> %s in %s" % (a, b, g["edges"]))
        return found[0]

    def test_workloads_are_named_from_pods(self):
        g = self.graph()
        ids = {n["id"]: n for n in g["nodes"]}
        # a ReplicaSet's hash is stripped; a StatefulSet keeps its name; the Service resolves to its workload
        self.assertEqual(sorted(ids), ["8.8.8.8:443", "shop/cartservice", "shop/frontend", "shop/redis"])
        self.assertEqual(ids["shop/frontend"]["kind"], "Deployment")
        self.assertEqual(ids["shop/redis"]["kind"], "StatefulSet")
        self.assertTrue(ids["8.8.8.8:443"]["external"])
        self.assertIsNone(ids["8.8.8.8:443"]["namespace"])

    def test_http_edge_sums_series_and_counts_server_errors(self):
        e = self.edge(self.graph(), "shop/frontend", "shop/cartservice")
        # mean(0.5, 1.5) + mean(0.5, 0.5) = 1.0 + 0.5
        self.assertAlmostEqual(e["connPerSec"], 1.5)
        # 200 averages 9, 500 averages 1, 5xx and 404 are 0: 10 requests/s, 1 of them an error
        self.assertAlmostEqual(e["rps"], 10.0)
        # plus 0.5 failed connects/s to the same Service (the fixture's failed-connects series):
        # (1 error + 0.5 failed connects) of (10 requests + 0.5 failed connects) = 14.29%
        self.assertAlmostEqual(e["errPct"], 100 * 1.5 / 10.5, places=2)
        self.assertEqual(e["errBasis"], "http+tcp")

    def test_tcp_only_edge_uses_failed_connects(self):
        e = self.edge(self.graph(), "shop/cartservice", "shop/redis")
        self.assertIsNone(e["rps"])
        self.assertEqual(e["errBasis"], "tcp")
        self.assertAlmostEqual(e["errPct"], 50.0)   # 1.0 failed of 2.0 attempts

    def test_unresolved_destination_is_an_external_node(self):
        e = self.edge(self.graph(), "shop/frontend", "8.8.8.8:443")
        self.assertAlmostEqual(e["connPerSec"], 0.2)
        self.assertEqual(e["errPct"], 0.0)

    def test_skips_self_edges_host_processes_and_unknown_pods(self):
        g = self.graph()
        self.assertEqual(len(g["edges"]), 3, g["edges"])
        self.assertFalse([e for e in g["edges"] if e["from"] == e["to"]])
        self.assertNotIn("gone-pod", json.dumps(g))
        self.assertNotIn("kubelet", json.dumps(g))

    def test_no_data_is_an_empty_graph_not_an_error(self):
        StubAPI.data = {}
        g = self.graph()
        self.assertEqual((g["nodes"], g["edges"], g["warnings"]), ([], [], []))

    def test_request_shape(self):
        self.graph("1h")
        names = sorted(q["name"] for _, q, _ in range_requests())
        self.assertEqual(names, sorted([server.CONNECTS, server.FAILED_CONNECTS, server.HTTP_REQUESTS]))
        for path, q, headers in StubAPI.requests:   # every request, the metric listing included, carries the key in a header
            self.assertEqual(headers.get("x-api-key"), KEY)
            self.assertNotIn(KEY, path, "the key must travel in a header, never in the URL")
        for path, q, headers in range_requests():
            self.assertEqual(headers.get("x-api-key"), KEY)
            self.assertNotIn(KEY, path, "the key must travel in a header, never in the URL")
            self.assertEqual((q["rate"], q["step"]), ("true", "90"))   # 3600 s / 40 buckets
            self.assertEqual(q.get("from_zero"), "true")   # a counter's first appearance is an increase
            self.assertAlmostEqual(float(q["to"]) - float(q["from"]), 3600, delta=2)
        StubAPI.requests.clear()
        self.graph("5m")
        self.assertEqual({q["step"] for _, q, _ in range_requests()}, {"15"})   # never finer than the agent's scrape

    def test_truncated_result_is_flagged(self):
        StubAPI.data = dict(DATA, **{server.CONNECTS: dict(DATA[server.CONNECTS], truncated=True)})
        g = self.graph()
        self.assertEqual(len(g["warnings"]), 1)
        self.assertIn(server.CONNECTS, g["warnings"][0])
        self.assertTrue(g["edges"], "a truncated result still draws what it has")

    def test_rejected_key_is_a_clear_error_that_does_not_echo_the_key(self):
        with self.assertRaises(server.ApiError) as cm:
            server.build_graph(self.api(key="wrong-key-wrong-key-wrong"), self.cluster, "1h")
        self.assertIn("read key was rejected", str(cm.exception))
        self.assertIn("'gke'", str(cm.exception))
        self.assertNotIn("wrong-key", str(cm.exception))

    def test_unknown_project_looks_like_a_rejected_key(self):
        # the service answers 403 for any project the key does not own, whether or not it exists
        with self.assertRaises(server.ApiError) as cm:
            server.build_graph(self.api(project="nosuch"), self.cluster, "1h")
        self.assertIn("rejected", str(cm.exception))
        self.assertIn("'nosuch'", str(cm.exception))

    def test_unreachable_api_suggests_the_port_forward(self):
        dead = serve(StubAPI)
        base = "http://127.0.0.1:%d" % dead.server_address[1]
        stop(dead)
        with self.assertRaises(server.ApiError) as cm:
            server.build_graph(self.api(base=base), self.cluster, "1h")
        self.assertIn("port-forward", str(cm.exception))


def grpc_edge(*statuses):
    """HTTP/2 series from the cart container to productcatalog, one per (status, mean rate)."""
    return {server.HTTP_REQUESTS: {"series": [
        series({"container_id": CART, "destination": "10.96.0.21:3550", "actual_destination": "10.0.0.13:3550", "status": st}, rate)
        for st, rate in statuses], "truncated": False}}


class ErrorRateTests(unittest.TestCase):
    """gRPC runs over HTTP/2 and is counted in container_http_requests_total with
    status "grpc:<CODE>"; its failures must count as errors like an HTTP 5xx does."""

    @classmethod
    def setUpClass(cls):
        cls.stub = serve(StubAPI)
        cls.base = "http://127.0.0.1:%d" % cls.stub.server_address[1]
        cls.tmp = tempfile.TemporaryDirectory()
        path = os.path.join(cls.tmp.name, "cluster.json")
        with open(path, "w") as f:
            json.dump(CLUSTER, f)
        cls.cluster = server.Cluster("", path)

    @classmethod
    def tearDownClass(cls):
        stop(cls.stub)
        cls.tmp.cleanup()

    def tearDown(self):
        StubAPI.data = DATA

    def edge(self, data):
        StubAPI.data = data
        g = server.build_graph(server.TelemetryAPI(self.base, PROJECT, KEY), self.cluster, "1h")
        found = [e for e in g["edges"] if e["from"] == "shop/cartservice" and e["to"] == "shop/productcatalog"]
        self.assertEqual(len(found), 1, g["edges"])
        return found[0]

    def test_grpc_server_failures_are_errors(self):
        e = self.edge(grpc_edge(("grpc:OK", 8.0), ("grpc:UNAVAILABLE", 1.0), ("grpc:DEADLINE_EXCEEDED", 0.5), ("grpc:NOT_FOUND", 0.5)))
        self.assertAlmostEqual(e["rps"], 10.0)
        # 1.5 of 10 calls failed on the server side; NOT_FOUND is the caller's doing, like a 404
        self.assertAlmostEqual(e["errPct"], 15.0)
        self.assertEqual(e["errBasis"], "http")

    def test_all_ok_grpc_edge_is_clean(self):
        e = self.edge(grpc_edge(("grpc:OK", 5.0), ("grpc:INVALID_ARGUMENT", 1.0), ("grpc:UNAUTHENTICATED", 1.0)))
        self.assertAlmostEqual(e["rps"], 7.0)
        self.assertEqual(e["errPct"], 0.0)

    def test_http_and_grpc_on_one_edge_add_up(self):
        e = self.edge(grpc_edge(("2xx", 6.0), ("5xx", 1.0), ("grpc:OK", 2.0), ("grpc:INTERNAL", 1.0)))
        self.assertAlmostEqual(e["rps"], 10.0)
        self.assertAlmostEqual(e["errPct"], 20.0)   # one 5xx and one INTERNAL among ten

    def test_status_classification(self):
        every_grpc_code = ["OK", "CANCELLED", "UNKNOWN", "INVALID_ARGUMENT", "DEADLINE_EXCEEDED", "NOT_FOUND",
                           "ALREADY_EXISTS", "PERMISSION_DENIED", "RESOURCE_EXHAUSTED", "FAILED_PRECONDITION",
                           "ABORTED", "OUT_OF_RANGE", "UNIMPLEMENTED", "INTERNAL", "UNAVAILABLE", "DATA_LOSS",
                           "UNAUTHENTICATED"]   # the node-agent's Status.GRPC() names
        failing = {c for c in every_grpc_code if server.is_server_error("grpc:" + c)}
        self.assertEqual(failing, {"UNKNOWN", "DEADLINE_EXCEEDED", "UNIMPLEMENTED", "INTERNAL", "UNAVAILABLE", "DATA_LOSS"})
        for st in ("5xx", "500", "503", "599"):
            self.assertTrue(server.is_server_error(st), st)
        for st in ("1xx", "2xx", "3xx", "4xx", "404", "200", "unknown", "", "grpc:", "grpc:unavailable", "grpc:NOPE"):
            self.assertFalse(server.is_server_error(st), st)


class FailedConnectTests(unittest.TestCase):
    """A failed connect never becomes a request, so the request counters cannot see it.
    Each failed connect counts as one failed call, whether or not requests also get through."""

    @classmethod
    def setUpClass(cls):
        cls.stub = serve(StubAPI)
        cls.base = "http://127.0.0.1:%d" % cls.stub.server_address[1]
        cls.tmp = tempfile.TemporaryDirectory()
        path = os.path.join(cls.tmp.name, "cluster.json")
        with open(path, "w") as f:
            json.dump(CLUSTER, f)
        cls.cluster = server.Cluster("", path)

    @classmethod
    def tearDownClass(cls):
        stop(cls.stub)
        cls.tmp.cleanup()

    def tearDown(self):
        StubAPI.data = DATA

    SRC = {"container_id": CART, "destination": "10.96.0.21:3550"}

    def edge(self, conn=None, fail=None, http=None):
        """cartservice -> productcatalog with the given mean rates (None = no series at all)."""
        data = {}
        if conn is not None:
            data[server.CONNECTS] = {"series": [series(dict(self.SRC, actual_destination="10.0.0.13:3550"), conn)], "truncated": False}
        if fail is not None:
            data[server.FAILED_CONNECTS] = {"series": [series(dict(self.SRC), fail)], "truncated": False}
        if http:
            data[server.HTTP_REQUESTS] = {"series": [
                series(dict(self.SRC, actual_destination="10.0.0.13:3550", status=st), r) for st, r in http], "truncated": False}
        StubAPI.data = data
        g = server.build_graph(server.TelemetryAPI(self.base, PROJECT, KEY), self.cluster, "1h")
        found = [e for e in g["edges"] if e["from"] == "shop/cartservice" and e["to"] == "shop/productcatalog"]
        self.assertEqual(len(found), 1, g["edges"])
        return found[0]

    def test_failed_connects_count_next_to_healthy_requests(self):
        # 10 requests all succeed, but 10 connection attempts per second are refused: half the calls fail
        e = self.edge(conn=1.0, fail=10.0, http=[("2xx", 10.0)])
        self.assertAlmostEqual(e["errPct"], 50.0)
        self.assertEqual(e["errBasis"], "http+tcp")
        self.assertAlmostEqual(e["rps"], 10.0)   # the request rate is still requests only

    def test_request_errors_and_failed_connects_add_up(self):
        e = self.edge(fail=5.0, http=[("2xx", 8.0), ("5xx", 2.0)])
        self.assertAlmostEqual(e["errPct"], 100 * (2.0 + 5.0) / (10.0 + 5.0), places=2)

    def test_no_failed_connects_leaves_the_http_rate_as_it_was(self):
        e = self.edge(conn=1.0, http=[("2xx", 9.0), ("5xx", 1.0)])
        self.assertAlmostEqual(e["errPct"], 10.0)
        self.assertEqual(e["errBasis"], "http")
        e = self.edge(conn=1.0, fail=0.0, http=[("2xx", 4.0)])   # a series that is present but flat at zero
        self.assertEqual((e["errPct"], e["errBasis"]), (0.0, "http"))

    def test_without_requests_it_is_the_share_of_connects_that_failed(self):
        e = self.edge(conn=3.0, fail=1.0)
        self.assertAlmostEqual(e["errPct"], 25.0)
        self.assertEqual(e["errBasis"], "tcp")
        self.assertIsNone(e["rps"])

    def test_a_callee_that_refuses_every_connection_is_fully_failing(self):
        e = self.edge(fail=2.0)
        self.assertEqual((e["errPct"], e["errBasis"]), (100.0, "tcp"))

    def test_the_share_never_passes_100(self):
        e = self.edge(fail=50.0, http=[("5xx", 1.0)])
        self.assertLessEqual(e["errPct"], 100.0)
        self.assertAlmostEqual(e["errPct"], 100.0)


class HttpTests(unittest.TestCase):
    """The map's own HTTP surface: what the page calls."""

    @classmethod
    def setUpClass(cls):
        cls.stub = serve(StubAPI)
        cls.tmp = tempfile.TemporaryDirectory()
        path = os.path.join(cls.tmp.name, "cluster.json")
        with open(path, "w") as f:
            json.dump(CLUSTER, f)
        api = server.TelemetryAPI("http://127.0.0.1:%d" % cls.stub.server_address[1], PROJECT, KEY)
        cls.app = serve(server.make_handler(api, server.Cluster("", path)))
        cls.base = "http://127.0.0.1:%d" % cls.app.server_address[1]

    @classmethod
    def tearDownClass(cls):
        stop(cls.app)
        stop(cls.stub)
        cls.tmp.cleanup()

    def setUp(self):
        StubAPI.data = DATA

    def get(self, path):
        try:
            with urllib.request.urlopen(self.base + path, timeout=10) as r:
                return r.status, r.read()
        except urllib.error.HTTPError as e:
            return e.code, e.read()

    def test_page_health_and_graph(self):
        code, body = self.get("/")
        self.assertEqual(code, 200)
        self.assertIn(b"warnings", body)             # the page surfaces API warnings
        self.assertIn(b"URLSearchParams", body)      # and takes a fixed range from its URL
        self.assertIn(b"e.unit", body)               # and shows queries/s, msgs/s as the edge says
        self.assertEqual(self.get("/healthz")[0], 200)
        code, body = self.get("/api/graph?window=15m")
        g = json.loads(body)
        self.assertEqual((code, g["window"], len(g["nodes"])), (200, "15m", 4))

    def test_bad_window_and_unknown_path(self):
        for w in ("1w", "abc", "0m", "1h;rm", "9999h"):
            self.assertEqual(self.get("/api/graph?window=" + urllib.parse.quote(w))[0], 400, w)
        self.assertEqual(self.get("/nope")[0], 404)

    def test_api_failure_is_a_502_with_the_reason(self):
        bad = server.TelemetryAPI("http://127.0.0.1:%d" % self.stub.server_address[1], PROJECT, "wrong-key-wrong-key-wrong")
        app = serve(server.make_handler(bad, server.Cluster("", os.path.join(self.tmp.name, "cluster.json"))))
        try:
            with self.assertRaises(urllib.error.HTTPError) as cm:
                urllib.request.urlopen("http://127.0.0.1:%d/api/graph" % app.server_address[1], timeout=10)
            self.assertEqual(cm.exception.code, 502)
            msg = json.loads(cm.exception.read())["error"]
            self.assertIn("read key was rejected", msg)
            self.assertNotIn("wrong-key", msg)
        finally:
            stop(app)


class CommandLineTests(unittest.TestCase):
    def run_cli(self, args, env=None):
        import subprocess
        e = {k: v for k, v in os.environ.items() if k != "TELEMETRY_READ_KEY"}
        e.update(env or {})
        return subprocess.run([sys.executable, os.path.join(os.path.dirname(os.path.abspath(__file__)), "server.py")] + args,
                              capture_output=True, text=True, env=e, timeout=30)

    def test_requires_a_key_and_never_takes_it_as_an_argument(self):
        r = self.run_cli(["--project", "gke", "--print", "1h"])
        self.assertNotEqual(r.returncode, 0)
        self.assertIn("TELEMETRY_READ_KEY", r.stderr)
        r = self.run_cli(["--project", "gke", "--read-key", "x"])
        self.assertNotEqual(r.returncode, 0)
        self.assertIn("unrecognized arguments", r.stderr)
        self.assertNotIn("Traceback", r.stderr)
        r = self.run_cli(["--project", "gke", "--read-key-file", "/nonexistent/key"])
        self.assertNotEqual(r.returncode, 0)
        self.assertIn("cannot read the key file", r.stderr)
        self.assertNotIn("Traceback", r.stderr)

    def test_print_mode_end_to_end(self):
        stub = serve(StubAPI)
        with tempfile.TemporaryDirectory() as d:
            cluster = os.path.join(d, "c.json")
            keyfile = os.path.join(d, "key")
            with open(cluster, "w") as f:
                json.dump(CLUSTER, f)
            with open(keyfile, "w") as f:
                f.write(KEY + "\n")
            try:
                base = "http://127.0.0.1:%d" % stub.server_address[1]
                r = self.run_cli(["--api", base, "--project", PROJECT, "--read-key-file", keyfile, "--k8s-file", cluster, "--print", "1h"])
                self.assertEqual(r.returncode, 0, r.stderr)
                g = json.loads(r.stdout)
                self.assertEqual(len(g["edges"]), 3)
                r = self.run_cli(["--api", base, "--project", PROJECT, "--k8s-file", cluster, "--print", "1h"], env={"TELEMETRY_READ_KEY": "wrong-key-wrong-key-wrong"})
                self.assertNotEqual(r.returncode, 0)
                self.assertIn("read key was rejected", r.stderr)
                self.assertNotIn("wrong-key", r.stderr)
            finally:
                stop(stub)


T0 = 1794059280.0   # 2026-11-07T13:48:00Z
T1 = T0 + 240       # 13:52:00Z


class RangeParsingTests(unittest.TestCase):
    def test_parse_time_forms(self):
        self.assertEqual(server.parse_time("1794059280"), T0)
        self.assertEqual(server.parse_time("1794059280.5"), T0 + 0.5)
        self.assertEqual(server.parse_time("2026-11-07T13:48:00Z"), T0)
        self.assertEqual(server.parse_time("2026-11-07T15:48:00+02:00"), T0)   # the same instant, written with an offset
        self.assertEqual(server.parse_time("2026-11-07T13:48:00"), T0)         # no zone: UTC
        self.assertEqual(server.parse_time(" 2026-11-07T13:48:00Z "), T0)
        for bad in ("", "yesterday", "13:48", "2026-13-45T00:00:00Z"):
            with self.assertRaises(ValueError, msg=bad):
                server.parse_time(bad)

    def test_resolve_range(self):
        self.assertEqual(server.resolve_range("15m", now=10_000.0), (10_000.0 - 900, 10_000.0, "15m"))
        start, end, label = server.resolve_range((T0, T1))
        self.assertEqual((start, end), (T0, T1))
        self.assertEqual(label, "2026-11-07T13:48:00Z/2026-11-07T13:52:00Z")
        with self.assertRaises(ValueError):
            server.resolve_range((T1, T0))          # backwards
        with self.assertRaises(ValueError):
            server.resolve_range((T0, T0))          # empty
        with self.assertRaises(ValueError):
            server.resolve_range((T0, T0 + 31 * 86400))   # past the 30-day cap
        server.resolve_range((T0, T0 + 30 * 86400))       # exactly the cap is allowed


class AggregationTests(unittest.TestCase):
    """What the map reports must be the traffic over the WHOLE range. A bucket in which a series
    sent nothing has no data point, so averaging only the points that exist skips every quiet
    moment, and a restart (the pod IP is a label, so the new pod is a new series) was counted
    twice. Both made an outage look like normal traffic."""

    @classmethod
    def setUpClass(cls):
        cls.stub = serve(StubAPI)
        cls.base = "http://127.0.0.1:%d" % cls.stub.server_address[1]
        cls.tmp = tempfile.TemporaryDirectory()
        path = os.path.join(cls.tmp.name, "cluster.json")
        with open(path, "w") as f:
            json.dump(CLUSTER, f)
        cls.cluster = server.Cluster("", path)

    @classmethod
    def tearDownClass(cls):
        stop(cls.stub)
        cls.tmp.cleanup()

    def tearDown(self):
        StubAPI.data = DATA

    DEST = {"container_id": CART, "destination": "10.96.0.21:3550"}
    RANGE = (T0, T0 + 600)      # 600 s in 15 s steps: 40 rate buckets

    def edge(self, data):
        StubAPI.data = data
        g = server.build_graph(server.TelemetryAPI(self.base, PROJECT, KEY), self.cluster, self.RANGE)
        found = [e for e in g["edges"] if e["from"] == "shop/cartservice" and e["to"] == "shop/productcatalog"]
        self.assertEqual(len(found), 1, g["edges"])
        return found[0]

    def http(self, *specs):
        """(rate, active, extra labels) per series on the cart -> productcatalog edge."""
        return {server.HTTP_REQUESTS: {"series": [
            series(dict(self.DEST, status="2xx", **extra), rate, active) for rate, active, extra in specs], "truncated": False}}

    def test_a_steady_edge_reports_its_rate(self):
        self.assertAlmostEqual(self.edge(self.http((4.0, (0.0, 1.0), {"actual_destination": "10.0.0.13:3550"})))["rps"], 4.0)

    def test_a_gap_in_the_data_counts_as_no_traffic(self):
        # busy for the first quarter, silent for the rest: that is a quarter of the rate, not the rate
        e = self.edge(self.http((8.0, (0.0, 0.25), {"actual_destination": "10.0.0.13:3550"})))
        self.assertAlmostEqual(e["rps"], 2.0)

    def test_a_restart_is_not_counted_twice(self):
        # the callee restarts: the old pod's series ends, the new pod's series (another
        # actual_destination) starts after a 20% gap. 5/s for 40% + 5/s for 40% of the range = 4/s.
        e = self.edge(self.http((5.0, (0.0, 0.4), {"actual_destination": "10.0.0.13:3550"}),
                                (5.0, (0.6, 1.0), {"actual_destination": "10.0.0.99:3550"})))
        self.assertAlmostEqual(e["rps"], 4.0)    # the old code gave 10.0: each series averaged over its own life, then added

    def test_an_outage_reads_lower_than_the_quiet_period_before_it(self):
        quiet = self.edge(self.http((6.0, (0.0, 1.0), {"actual_destination": "10.0.0.13:3550"})))["rps"]
        outage = self.edge(self.http((6.0, (0.0, 0.3), {"actual_destination": "10.0.0.13:3550"}),
                                     (6.0, (0.8, 1.0), {"actual_destination": "10.0.0.99:3550"})))["rps"]
        self.assertAlmostEqual(quiet, 6.0)
        self.assertLess(outage, quiet)
        self.assertAlmostEqual(outage, 6.0 * 0.5)

    def test_failures_that_cover_part_of_the_range_are_weighted_by_that_part(self):
        # requests: 4/s all the time. Connects fail at 10/s but only in the last tenth of the range
        # (a callee that went down late), so over the range that is 1/s: 1 of 5 calls failed.
        data = self.http((4.0, (0.0, 1.0), {"actual_destination": "10.0.0.13:3550"}))
        data[server.FAILED_CONNECTS] = {"series": [series(dict(self.DEST), 10.0, (0.9, 1.0))], "truncated": False}
        e = self.edge(data)
        self.assertAlmostEqual(e["rps"], 4.0)
        self.assertAlmostEqual(e["errPct"], 100 * 1.0 / 5.0, places=1)    # the old code gave 71%: 10/s taken as the whole range's rate
        self.assertEqual(e["errBasis"], "http+tcp")

    def test_a_range_shorter_than_two_buckets_does_not_divide_by_zero(self):
        StubAPI.data = self.http((3.0, (0.0, 1.0), {"actual_destination": "10.0.0.13:3550"}))
        g = server.build_graph(server.TelemetryAPI(self.base, PROJECT, KEY), self.cluster, (T0, T0 + 5))
        self.assertIsInstance(g["edges"], list)


class LoopbackTests(unittest.TestCase):
    """A connection to 127.0.0.0/8 or ::1 stays inside one host or one pod (an app talking to its
    sidecar, a probe, a local agent). It is not a call between services, so it is not an edge."""

    @classmethod
    def setUpClass(cls):
        cls.stub = serve(StubAPI)
        cls.base = "http://127.0.0.1:%d" % cls.stub.server_address[1]
        cls.tmp = tempfile.TemporaryDirectory()
        path = os.path.join(cls.tmp.name, "cluster.json")
        with open(path, "w") as f:
            json.dump(CLUSTER, f)
        cls.cluster = server.Cluster("", path)

    @classmethod
    def tearDownClass(cls):
        stop(cls.stub)
        cls.tmp.cleanup()

    def tearDown(self):
        StubAPI.data = DATA

    def graph(self, destinations, real_edge=True):
        """frontend -> each destination (connects and HTTP), plus a genuine frontend -> cartservice edge."""
        conn, http = [], []
        for dest in destinations:
            labels = {"container_id": FRONTEND, "destination": dest} if isinstance(dest, str) else dict(container_id=FRONTEND, **dest)
            conn.append(series(dict(labels), 1.0))
            http.append(series(dict(labels, status="2xx"), 2.0))
        if real_edge:
            real = {"container_id": FRONTEND, "destination": CART_VIA_SERVICE, "actual_destination": CART_POD}
            conn.append(series(dict(real), 1.0))
            http.append(series(dict(real, status="2xx"), 3.0))
        StubAPI.data = {server.CONNECTS: {"series": conn, "truncated": False}, server.HTTP_REQUESTS: {"series": http, "truncated": False}}
        return server.build_graph(server.TelemetryAPI(self.base, PROJECT, KEY), self.cluster, "1h")

    def test_every_loopback_form_is_dropped(self):
        for dest in ("127.0.0.1:2020", "127.1.2.3:80", "127.255.255.254:9", "[::1]:8080", "localhost:9000",
                     "LOCALHOST:9000", "[::ffff:127.0.0.1]:80"):
            g = self.graph([dest])
            self.assertEqual([n["id"] for n in g["nodes"]], ["shop/cartservice", "shop/frontend"], dest)
            self.assertEqual([(e["from"], e["to"]) for e in g["edges"]], [("shop/frontend", "shop/cartservice")], dest)

    def test_a_loopback_actual_destination_is_dropped_whatever_the_destination(self):
        # the address the container dialled was not loopback, but the peer it reached is local
        g = self.graph([{"destination": CART_VIA_SERVICE, "actual_destination": "127.0.0.1:7070"}], real_edge=False)
        self.assertEqual((g["nodes"], g["edges"]), ([], []))

    def test_addresses_that_only_look_like_loopback_are_kept(self):
        kept = ("128.0.0.1:80", "10.0.0.127:80", "1.127.0.0.1:80", "127.example.com:80", "12.7.0.0:80", "[2001:db8::1]:80")
        g = self.graph(list(kept))
        ids = {n["id"] for n in g["nodes"]}
        for dest in kept:
            self.assertIn(dest, ids, "%s is not loopback and must stay on the map" % dest)

    def test_the_real_edge_survives_and_is_not_diluted(self):
        g = self.graph(["127.0.0.1:2020", "[::1]:8080"])
        e, = g["edges"]
        self.assertEqual((e["from"], e["to"]), ("shop/frontend", "shop/cartservice"))
        self.assertAlmostEqual(e["rps"], 3.0)
        self.assertAlmostEqual(e["connPerSec"], 1.0)

    def test_how_much_was_ignored_is_reported(self):
        g = self.graph(["127.0.0.1:2020", "127.0.0.1:2022", "[::1]:8080"])
        self.assertEqual(g["ignored"], {"loopback": 3, "ambiguous": 0})           # distinct container -> destination pairs
        self.assertEqual(self.graph([])["ignored"], {"loopback": 0, "ambiguous": 0})

    def test_the_same_pair_in_every_metric_is_one_ignored_pair(self):
        # connects, failed connects and HTTP all mention it; it is still one pair
        labels = {"container_id": FRONTEND, "destination": "127.0.0.1:2020"}
        StubAPI.data = {server.CONNECTS: {"series": [series(dict(labels), 1.0)], "truncated": False},
                        server.FAILED_CONNECTS: {"series": [series(dict(labels), 0.5)], "truncated": False},
                        server.HTTP_REQUESTS: {"series": [series(dict(labels, status="2xx"), 2.0)], "truncated": False}}
        g = server.build_graph(server.TelemetryAPI(self.base, PROJECT, KEY), self.cluster, "1h")
        self.assertEqual((g["edges"], g["ignored"]), ([], {"loopback": 1, "ambiguous": 0}))

    def test_host_parsing(self):
        for addr, host in (("10.0.0.1:80", "10.0.0.1"), ("[::1]:80", "::1"), ("[fe80::1]:443", "fe80::1"),
                           ("name.example:443", "name.example"), ("::1", "::1"), ("10.0.0.1", "10.0.0.1"), ("", "")):
            self.assertEqual(server.host_of(addr), host, addr)
        for addr in ("127.0.0.1:1", "[::1]:1", "localhost:1", "[::ffff:127.0.0.9]:1", "127.0.0.1"):
            self.assertTrue(server.is_loopback(addr), addr)
        for addr in ("", "10.0.0.1:1", "0.0.0.0:80", "128.0.0.1:1", "[::]:80", "garbage"):
            self.assertFalse(server.is_loopback(addr), addr)


REDIS_BASE = {"container_id": CART, "destination": "10.96.0.30:6379", "actual_destination": REDIS_POD}
REDIS_CALLS = "container_redis_queries_total"
REDIS_BUCKETS = "container_redis_queries_duration_seconds_total_bucket"


def calls(name, labels, **by_status):
    return {name: {"series": [series(dict(labels, status=st), rate) for st, rate in by_status.items()], "truncated": False}}


def buckets(name, labels, cumulative):
    """A latency histogram as the API returns it: one series per `le`, holding the cumulative per-second rate."""
    return {name: {"series": [series(dict(labels, le=le), rate) for le, rate in cumulative.items()], "truncated": False}}


class DatabaseEdgeTests(unittest.TestCase):
    """The agent counts a database's queries like HTTP requests (status ok / failed / unknown) and keeps a
    latency histogram for most of them. An edge to a database should carry that, not just connections."""

    @classmethod
    def setUpClass(cls):
        cls.stub = serve(StubAPI)
        cls.base = "http://127.0.0.1:%d" % cls.stub.server_address[1]
        cls.tmp = tempfile.TemporaryDirectory()
        path = os.path.join(cls.tmp.name, "cluster.json")
        with open(path, "w") as f:
            json.dump(CLUSTER, f)
        cls.cluster = server.Cluster("", path)

    @classmethod
    def tearDownClass(cls):
        stop(cls.stub)
        cls.tmp.cleanup()

    def setUp(self):
        StubAPI.requests.clear()

    def tearDown(self):
        StubAPI.data = DATA

    def graph(self, *parts):
        data = {}
        for p in parts:
            data.update(p)
        StubAPI.data = data
        return server.build_graph(server.TelemetryAPI(self.base, PROJECT, KEY), self.cluster, "1h")

    def edge(self, g, a="shop/cartservice", b="shop/redis"):
        found = [e for e in g["edges"] if e["from"] == a and e["to"] == b]
        self.assertEqual(len(found), 1, g["edges"])
        return found[0]

    def test_a_database_edge_has_rate_errors_and_protocol(self):
        g = self.graph(calls(REDIS_CALLS, REDIS_BASE, ok=4.0, failed=1.0))
        e = self.edge(g)
        self.assertAlmostEqual(e["rps"], 5.0)
        self.assertAlmostEqual(e["errPct"], 20.0)                  # 1 failed of 5 queries
        self.assertEqual((e["protocol"], e["protocols"], e["unit"], e["errBasis"]), ("redis", ["redis"], "queries/s", "calls"))
        self.assertIsNone(e["p95ms"])                              # no histogram was sent
        nodes = {n["id"]: n for n in g["nodes"]}
        self.assertEqual(nodes["shop/redis"]["serves"], ["redis"])   # the database knows what it serves
        self.assertEqual(nodes["shop/cartservice"]["serves"], [])

    def test_p95_comes_from_the_latency_histogram(self):
        # cumulative per-second rates: 2 of 10 calls took <= 1 ms, 6 <= 5 ms, 9 <= 10 ms, all <= 50 ms.
        # The 95th percentile (rank 9.5) falls in the 10-50 ms bucket, half way through its single call: 30 ms.
        hist = buckets(REDIS_BUCKETS, REDIS_BASE, {"0.001": 2.0, "0.005": 6.0, "0.01": 9.0, "0.05": 10.0, "+Inf": 10.0})
        e = self.edge(self.graph(calls(REDIS_CALLS, REDIS_BASE, ok=10.0), hist))
        self.assertAlmostEqual(e["p95ms"], 30.0, places=3)

    def test_p95_when_everything_is_slower_than_the_last_bucket(self):
        hist = buckets(REDIS_BUCKETS, REDIS_BASE, {"0.1": 0.0, "+Inf": 10.0})
        e = self.edge(self.graph(calls(REDIS_CALLS, REDIS_BASE, ok=10.0), hist))
        self.assertAlmostEqual(e["p95ms"], 100.0, places=3)        # "at least the largest bound", as Prometheus reports it

    def test_status_unknown_is_a_call_but_not_an_error(self):
        e = self.edge(self.graph(calls(REDIS_CALLS, REDIS_BASE, ok=8.0, unknown=2.0)))
        self.assertAlmostEqual(e["rps"], 10.0)
        self.assertEqual(e["errPct"], 0.0)

    def test_refused_connections_count_next_to_healthy_queries(self):
        failed = {server.FAILED_CONNECTS: {"series": [series({"container_id": CART, "destination": "10.96.0.30:6379"}, 10.0)], "truncated": False}}
        e = self.edge(self.graph(calls(REDIS_CALLS, REDIS_BASE, ok=10.0), failed))
        self.assertAlmostEqual(e["errPct"], 50.0)                  # (0 failed queries + 10 failed connects) of (10 + 10)
        self.assertEqual(e["errBasis"], "calls+tcp")

    def test_http_and_a_database_protocol_to_one_workload_add_up(self):
        http = {server.HTTP_REQUESTS: {"series": [series(dict(CART_TO_REDIS_POD_HTTP, status="2xx"), 3.0)], "truncated": False}}
        native = calls("container_clickhouse_queries_total", {"container_id": CART, "destination": "10.0.0.12:9000", "actual_destination": "10.0.0.12:9000"}, ok=7.0)
        e = self.edge(self.graph(http, native))
        self.assertAlmostEqual(e["rps"], 10.0)
        self.assertEqual((e["protocol"], e["protocols"], e["unit"], e["errBasis"]), ("clickhouse", ["clickhouse", "http"], "calls/s", "calls"))

    def test_only_protocols_the_cluster_reports_are_queried(self):
        self.graph(calls(REDIS_CALLS, REDIS_BASE, ok=1.0))
        asked = {q["name"] for _, q, _ in range_requests()}
        self.assertEqual(asked, {server.CONNECTS, server.FAILED_CONNECTS, server.HTTP_REQUESTS, REDIS_CALLS})
        StubAPI.requests.clear()
        self.graph(calls(REDIS_CALLS, REDIS_BASE, ok=1.0), buckets(REDIS_BUCKETS, REDIS_BASE, {"1": 1.0, "+Inf": 1.0}))
        self.assertIn(REDIS_BUCKETS, {q["name"] for _, q, _ in range_requests()})

    def test_a_loopback_database_is_not_an_edge(self):
        g = self.graph(calls(REDIS_CALLS, {"container_id": CART, "destination": "127.0.0.1:6379"}, ok=5.0))
        self.assertEqual((g["edges"], g["ignored"]), ([], {"loopback": 1, "ambiguous": 0}))

    def test_a_broker_without_latency_still_gets_a_rate(self):
        # RabbitMQ and NATS count messages by method and keep no latency histogram
        rabbit = calls("container_rabbitmq_messages_total", dict(REDIS_BASE, method="publish"), ok=6.0)
        e = self.edge(self.graph(rabbit))
        self.assertEqual((e["protocol"], e["unit"], e["p95ms"]), ("rabbitmq", "msgs/s", None))
        self.assertAlmostEqual(e["rps"], 6.0)

    def test_clickhouse_latency_is_not_reported(self):
        # on a real cluster the client-side histogram said p95 8.7 s where ClickHouse itself said 22 ms,
        # so the map reports ClickHouse's rate and errors and no latency, and does not even ask for it
        base = {"container_id": CART, "destination": "10.0.0.12:9000", "actual_destination": "10.0.0.12:9000"}
        slow = buckets("container_clickhouse_queries_duration_seconds_total_bucket", base, {"1": 0.0, "10": 5.0, "+Inf": 10.0})
        e = self.edge(self.graph(calls("container_clickhouse_queries_total", base, ok=9.0, failed=1.0), slow))
        self.assertEqual((e["protocol"], e["p95ms"]), ("clickhouse", None))
        self.assertAlmostEqual(e["rps"], 10.0)
        self.assertAlmostEqual(e["errPct"], 10.0)
        asked = {q["name"] for _, q, _ in range_requests()}
        self.assertNotIn("container_clickhouse_queries_duration_seconds_total_bucket", asked)

    def test_every_protocol_with_a_histogram_names_a_real_agent_metric(self):
        # a typo here would silently drop a protocol's latency: the names must follow the agent's pattern
        for name, counter, histogram, unit in server.L7_PROTOCOLS:
            self.assertRegex(counter, r"^container_%s_(queries|requests|messages)_total$" % name)
            if histogram:
                self.assertEqual(histogram, counter.replace("_total", "_duration_seconds_total"), name)
            self.assertIn(unit, ("queries/s", "req/s", "msgs/s"), name)

    def test_an_http_only_edge_is_unchanged(self):
        e = self.edge(self.graph(DATA), "shop/frontend", "shop/cartservice")
        self.assertEqual((e["protocol"], e["protocols"], e["unit"], e["errBasis"]), ("http", ["http"], "req/s", "http+tcp"))


CART_TO_REDIS_POD_HTTP = {"container_id": CART, "destination": "10.0.0.12:8123", "actual_destination": "10.0.0.12:8123"}


class QuantileTests(unittest.TestCase):
    def test_interpolates_inside_the_bucket_like_prometheus(self):
        b = {0.001: 2.0, 0.005: 6.0, 0.01: 9.0, 0.05: 10.0, float("inf"): 10.0}
        self.assertAlmostEqual(server.histogram_quantile(0.95, b), 0.03)
        self.assertAlmostEqual(server.histogram_quantile(0.5, b), 0.004)        # rank 5 of 10 lies in (1 ms, 5 ms]: 1 ms + 4 ms * (5-2)/(6-2) = 4 ms
        self.assertAlmostEqual(server.histogram_quantile(0.1, b), 0.0005)       # rank 1 inside the first bucket, from 0

    def test_the_overflow_bucket_reports_the_largest_finite_bound(self):
        self.assertAlmostEqual(server.histogram_quantile(0.95, {0.1: 0.0, float("inf"): 10.0}), 0.1)

    def test_nothing_to_report(self):
        self.assertIsNone(server.histogram_quantile(0.95, {}))
        self.assertIsNone(server.histogram_quantile(0.95, {0.1: 0.0, float("inf"): 0.0}))
        self.assertIsNone(server.histogram_quantile(0.95, {float("inf"): 5.0}))   # no finite bound to report

    def test_buckets_that_are_not_cumulative_are_repaired(self):
        # summing independent per-bucket rates can leave a bucket a little under its predecessor
        self.assertAlmostEqual(server.histogram_quantile(0.5, {0.01: 6.0, 0.05: 5.9, float("inf"): 10.0}),
                               server.histogram_quantile(0.5, {0.01: 6.0, 0.05: 6.0, float("inf"): 10.0}))


# ---- a fleet of clusters ----

def _pod(ns, name, ip, owner=None):
    meta = {"namespace": ns, "name": name}
    if owner:
        meta["ownerReferences"] = [{"kind": "ReplicaSet", "name": owner}]
    return {"kind": "Pod", "metadata": meta, "status": {"podIPs": [{"ip": ip}]}}


def _svc(ns, name, cluster_ip, ports=(80,), type="ClusterIP", node_ports=(), lb=(), external_ips=()):
    spec = {"clusterIP": cluster_ip, "type": type, "externalIPs": list(external_ips),
            "ports": [dict(port=p, **({"nodePort": node_ports[i]} if i < len(node_ports) else {})) for i, p in enumerate(ports)]}
    return {"kind": "Service", "metadata": {"namespace": ns, "name": name}, "spec": spec,
            "status": {"loadBalancer": {"ingress": [({"hostname": a} if not a[0].isdigit() else {"ip": a}) for a in lb]}}}


def _node(name, *addresses):
    return {"kind": "Node", "metadata": {"name": name}, "status": {"addresses": [{"type": "InternalIP", "address": a} for a in addresses]}}


# Cluster "us": a frontend, and a cartservice at 10.0.0.11.
US = {"items": [
    _pod("shop", "frontend-aaa-1", "10.0.0.10", "frontend-aaa"),
    _pod("shop", "cartservice-bbb-1", "10.0.0.11", "cartservice-bbb"),
    _svc("shop", "cartservice", "10.96.0.11", ports=(7070,)),
]}
# Cluster "eu": ALSO has a shop/frontend and an api pod at the SAME address, 10.0.0.11, as us' cartservice.
# It exposes api on a load balancer, queue on a NodePort, edge on a load balancer hostname.
EU = {"items": [
    _pod("shop", "frontend-ccc-1", "10.0.0.10", "frontend-ccc"),
    _pod("shop", "api-ddd-1", "10.0.0.11", "api-ddd"),
    _pod("shop", "queue-0", "10.0.0.12"),
    _pod("shop", "edge-eee-1", "10.0.0.13", "edge-eee"),
    _svc("shop", "api", "10.96.1.1", ports=(443,), type="LoadBalancer", lb=("203.0.113.10",)),
    _svc("shop", "queue", "10.96.1.2", ports=(5672,), type="NodePort", node_ports=(30081,)),
    _svc("shop", "edge", "10.96.1.3", ports=(8443,), type="LoadBalancer", lb=("edge.eu.example.com",)),
    _svc("shop", "shared", "10.96.1.4", ports=(443,), type="LoadBalancer", lb=("203.0.113.99",)),
    _node("eu-node-1", "192.0.2.5"),
]}
# Cluster "ap" ALSO claims 203.0.113.99:443, so no other cluster can tell which one a caller meant.
AP = {"items": [
    _pod("shop", "shared-fff-1", "10.0.0.20", "shared-fff"),
    _svc("shop", "shared", "10.96.2.1", ports=(443,), type="LoadBalancer", lb=("203.0.113.99",)),
]}
US_FRONTEND = "/k8s/shop/frontend-aaa-1/server"
EU_FRONTEND = "/k8s/shop/frontend-ccc-1/server"
FLEET_KEYS = {"us": "us-read-key-0123456789abcd", "eu": "eu-read-key-0123456789abcd", "ap": "ap-read-key-0123456789abcd"}


def calls_from(container, dest, conn=None, http=None, actual=None):
    """The three base metrics for one caller and destination."""
    labels = {"container_id": container, "destination": dest}
    if actual:
        labels["actual_destination"] = actual
    out = {}
    if conn is not None:
        out[server.CONNECTS] = [series(dict(labels), conn)]
    if http is not None:
        out[server.HTTP_REQUESTS] = [series(dict(labels, status="2xx"), http)]
    return out


def project_data(*parts):
    data = {}
    for p in parts:
        for metric, ser in p.items():
            data.setdefault(metric, {"series": [], "truncated": False})["series"].extend(ser)
    return data


class FleetTests(unittest.TestCase):
    """Several clusters on one map. Names collide between clusters (every cluster has a shop/frontend),
    pod and Service addresses overlap between them, and a call from one cluster to another can only be
    recognised by an address the other cluster exposes."""

    @classmethod
    def setUpClass(cls):
        cls.stub = serve(StubAPI)
        cls.base = "http://127.0.0.1:%d" % cls.stub.server_address[1]
        cls.tmp = tempfile.TemporaryDirectory()
        cls.files = {}
        for name, inv in (("us", US), ("eu", EU), ("ap", AP)):
            cls.files[name] = os.path.join(cls.tmp.name, name + ".json")
            with open(cls.files[name], "w") as f:
                json.dump(inv, f)

    @classmethod
    def tearDownClass(cls):
        stop(cls.stub)
        cls.tmp.cleanup()

    def setUp(self):
        StubAPI.keys = dict(FLEET_KEYS)
        StubAPI.other = {
            "us": project_data(
                calls_from(US_FRONTEND, "203.0.113.10:443", conn=0.5, http=4.0),                    # eu's api by load balancer IP
                calls_from(US_FRONTEND, "192.0.2.5:30081", conn=1.0),                               # eu's queue by node IP + NodePort
                calls_from(US_FRONTEND, "edge.eu.example.com:8443", conn=0.5),                      # eu's edge by load balancer name
                calls_from(US_FRONTEND, "10.0.0.11:7070", conn=2.0, http=6.0),                      # an address eu ALSO has: ours wins
                calls_from(US_FRONTEND, "198.51.100.7:443", conn=0.3),                              # exposed by nobody
                calls_from(US_FRONTEND, "203.0.113.99:443", conn=0.2)),                             # claimed by eu AND ap
            "eu": project_data(
                calls_from(EU_FRONTEND, "10.96.1.1:443", conn=3.0, http=5.0, actual="10.0.0.11:443"),   # eu's own api
                calls_from(EU_FRONTEND, "198.51.100.7:443", conn=0.1)),
            "ap": project_data(),
        }

    def tearDown(self):
        StubAPI.other, StubAPI.keys = {}, {}

    def specs(self, names=("us", "eu", "ap")):
        return [server.ClusterSpec(n, server.TelemetryAPI(self.base, n, FLEET_KEYS[n]), server.Cluster("", self.files[n], name=n)) for n in names]

    def graph(self, names=("us", "eu", "ap")):
        return server.build_fleet_graph(self.specs(names), "1h")

    def edge(self, g, a, b):
        found = [e for e in g["edges"] if e["from"] == a and e["to"] == b]
        self.assertEqual(len(found), 1, (a, b, [(e["from"], e["to"]) for e in g["edges"]]))
        return found[0]

    def test_workloads_with_the_same_name_in_two_clusters_are_two_nodes(self):
        g = self.graph()
        ids = {n["id"]: n for n in g["nodes"]}
        self.assertIn("us/shop/frontend", ids)
        self.assertIn("eu/shop/frontend", ids)
        self.assertEqual((ids["us/shop/frontend"]["cluster"], ids["eu/shop/frontend"]["cluster"]), ("us", "eu"))
        self.assertEqual(ids["eu/shop/frontend"]["name"], "frontend")      # the name stays as it is; the id and cluster tell them apart

    def test_a_load_balancer_ip_leads_to_the_cluster_that_exposes_it(self):
        e = self.edge(self.graph(), "us/shop/frontend", "eu/shop/api")
        self.assertTrue(e["crossCluster"])
        self.assertAlmostEqual(e["rps"], 4.0)
        self.assertEqual(e["cluster"], "us")                                # the edge belongs to the caller's cluster

    def test_a_node_ip_and_node_port_lead_to_the_service(self):
        e = self.edge(self.graph(), "us/shop/frontend", "eu/shop/queue")
        self.assertTrue(e["crossCluster"])
        self.assertAlmostEqual(e["connPerSec"], 1.0)

    def test_a_load_balancer_host_name_leads_to_the_service(self):
        self.assertTrue(self.edge(self.graph(), "us/shop/frontend", "eu/shop/edge")["crossCluster"])

    def test_an_address_both_clusters_have_means_the_callers_own(self):
        # eu's api pod is at 10.0.0.11, as us' cartservice is. A call from us to 10.0.0.11 is a call to us' own pod.
        g = self.graph()
        e = self.edge(g, "us/shop/frontend", "us/shop/cartservice")
        self.assertFalse(e["crossCluster"])
        self.assertAlmostEqual(e["rps"], 6.0)
        self.assertNotIn(("us/shop/frontend", "eu/shop/api"), [(x["from"], x["to"]) for x in g["edges"] if x["rps"] == 6.0])

    def test_a_call_inside_one_cluster_stays_inside_it(self):
        e = self.edge(self.graph(), "eu/shop/frontend", "eu/shop/api")
        self.assertFalse(e["crossCluster"])
        self.assertAlmostEqual(e["rps"], 5.0)

    def test_an_address_nobody_exposes_is_one_external_node_shared_by_the_clusters(self):
        g = self.graph()
        ext = [n for n in g["nodes"] if n["id"] == "198.51.100.7:443"]
        self.assertEqual(len(ext), 1)
        self.assertTrue(ext[0]["external"])
        self.assertIsNone(ext[0]["cluster"])
        callers = {e["from"] for e in g["edges"] if e["to"] == "198.51.100.7:443"}
        self.assertEqual(callers, {"us/shop/frontend", "eu/shop/frontend"})

    def test_an_address_two_other_clusters_claim_is_not_guessed(self):
        g = self.graph()
        self.assertIn("203.0.113.99:443", {n["id"] for n in g["nodes"]})    # left as an external endpoint
        self.assertEqual(g["ignored"]["ambiguous"], 1)
        self.assertFalse([e for e in g["edges"] if e["to"] in ("eu/shop/shared", "ap/shop/shared")])

    def test_the_callers_own_exposed_address_beats_other_clusters(self):
        # eu calls its own load balancer address, which only eu exposes and ap also claims: it is eu's own
        StubAPI.other["eu"] = project_data(calls_from(EU_FRONTEND, "203.0.113.99:443", conn=1.0))
        e = self.edge(self.graph(), "eu/shop/frontend", "eu/shop/shared")
        self.assertFalse(e["crossCluster"])

    def test_every_cluster_reports_its_status(self):
        g = self.graph()
        self.assertEqual([(c["name"], c["ok"]) for c in g["clusters"]], [("us", True), ("eu", True), ("ap", True)])
        self.assertTrue(all(c["error"] is None for c in g["clusters"]))

    def test_an_unreachable_cluster_does_not_hide_the_others(self):
        StubAPI.keys["eu"] = "another-key-entirely"          # the key for eu is now wrong: its project answers 403
        g = self.graph()
        status = {c["name"]: c for c in g["clusters"]}
        self.assertFalse(status["eu"]["ok"])
        self.assertIn("read key was rejected", status["eu"]["error"])
        self.assertTrue(status["us"]["ok"] and status["ap"]["ok"])
        self.assertTrue([w for w in g["warnings"] if w.startswith("eu:")])
        self.assertIn("us/shop/frontend", {n["id"] for n in g["nodes"]})
        # eu's telemetry could not be read, but its inventory could, so its load balancer address is still
        # known: us' call to it is a real call, recorded in us' data, and it still leads to eu/shop/api
        e = self.edge(g, "us/shop/frontend", "eu/shop/api")
        self.assertTrue(e["crossCluster"])
        self.assertNotIn("203.0.113.10:443", {n["id"] for n in g["nodes"]})
        self.assertEqual([x for x in g["edges"] if x["from"].startswith("eu/")], [])   # eu's own calls are what is missing

    def test_a_cluster_whose_inventory_cannot_be_read_is_reported_too(self):
        specs = self.specs()
        specs[1] = server.ClusterSpec("eu", specs[1].api, server.Cluster("", os.path.join(self.tmp.name, "missing.json"), name="eu"))
        g = server.build_fleet_graph(specs, "1h")
        status = {c["name"]: c for c in g["clusters"]}
        self.assertFalse(status["eu"]["ok"])
        self.assertTrue(status["us"]["ok"])
        # without eu's inventory its addresses are unknown, so a call to its load balancer is an external endpoint
        self.assertIn("203.0.113.10:443", {n["id"] for n in g["nodes"]})
        self.assertNotIn("eu/shop/api", {n["id"] for n in g["nodes"]})

    def test_when_no_cluster_can_be_read_that_is_an_error(self):
        StubAPI.keys = {n: "wrong-%s-key-wrong-key" % n for n in FLEET_KEYS}
        with self.assertRaises(server.ApiError) as cm:
            self.graph()
        self.assertIn("no cluster could be read", str(cm.exception))
        self.assertNotIn("wrong-us-key", str(cm.exception))

    def test_a_single_cluster_keeps_its_plain_ids(self):
        g = server.build_graph(server.TelemetryAPI(self.base, "us", FLEET_KEYS["us"]), server.Cluster("", self.files["us"]), "1h")
        self.assertIn("shop/frontend", {n["id"] for n in g["nodes"]})
        self.assertFalse([e for e in g["edges"] if e.get("crossCluster")])

    def test_the_fleet_graph_is_the_same_whatever_the_order_of_work(self):
        a, b = self.graph(), self.graph()
        key = lambda g: ([n["id"] for n in g["nodes"]], sorted((e["from"], e["to"]) for e in g["edges"]))
        self.assertEqual(key(a), key(b))


class FleetConfigTests(unittest.TestCase):
    """The file that describes a fleet. It must never hold a key, and it must never let the map guess
    which cluster to read."""

    def setUp(self):
        self.tmp = tempfile.TemporaryDirectory()
        self.dir = self.tmp.name

    def tearDown(self):
        self.tmp.cleanup()

    def write(self, name, content):
        p = os.path.join(self.dir, name)
        with open(p, "w") as f:
            f.write(content if isinstance(content, str) else json.dumps(content))
        return p

    def good(self, **overrides):
        c = {"name": "us", "project": "gke-us", "kubeContext": "ctx-us", "readKeyFile": "us.key"}
        c.update(overrides)
        return c

    def load(self, clusters, top=None, env=None, key="us-key-0123456789abcdef"):
        self.write("us.key", key + "\n")
        cfg = dict(top or {}, clusters=clusters)
        return server.load_fleet_config(self.write("fleet.json", cfg), environ=env or {})

    def refused(self, clusters, why, **kw):
        with self.assertRaises(ValueError) as cm:
            self.load(clusters, **kw)
        self.assertIn(why, str(cm.exception))
        return str(cm.exception)

    def test_a_valid_file(self):
        self.write("dc1.json", "{}")
        specs = self.load([
            self.good(),
            {"name": "dc1", "project": "dc1", "k8sFile": "dc1.json", "readKeyEnv": "DC1_READ_KEY", "api": "https://telemetry.dc1.example.com"},
        ], top={"api": "http://10.0.0.5:8080"}, env={"DC1_READ_KEY": "dc1-key-0123456789abcdef"})
        self.assertEqual([s.name for s in specs], ["us", "dc1"])
        self.assertEqual((specs[0].api.base, specs[0].api.project, specs[0].api.key), ("http://10.0.0.5:8080", "gke-us", "us-key-0123456789abcdef"))
        self.assertEqual(specs[1].api.base, "https://telemetry.dc1.example.com")      # a per-cluster api overrides the default
        self.assertEqual((specs[0].inventory.context, specs[0].inventory.name), ("ctx-us", "us"))
        self.assertEqual(specs[1].inventory.fixture, os.path.join(self.dir, "dc1.json"))   # relative to the file

    def test_a_key_is_never_written_in_the_file(self):
        for field in ("readKey", "key", "apiKey", "token", "password", "secret"):
            msg = self.refused([self.good(**{field: "do-not-leak-this-0123456789"})], "must not be written in the file")
            self.assertNotIn("do-not-leak-this", msg)
            self.assertIn("readKeyFile", msg)

    def test_the_cluster_to_read_is_never_guessed(self):
        self.refused([self.good(kubeContext="")], "does not guess which cluster to read")
        self.refused([self.good(k8sFile="x.json")], "exactly one of kubeContext and k8sFile")

    def test_exactly_one_way_to_get_the_key(self):
        c = self.good()
        del c["readKeyFile"]
        self.refused([c], "exactly one of readKeyFile and readKeyEnv")
        self.refused([self.good(readKeyEnv="X_KEY")], "exactly one of readKeyFile and readKeyEnv")

    def test_a_missing_key_says_where_it_looked_and_never_the_value(self):
        msg = self.refused([self.good(readKeyFile="", readKeyEnv="NOPE_KEY")], "NOPE_KEY is not set")
        self.assertNotIn("us-key", msg)
        self.refused([self.good(readKeyFile="absent.key")], "cannot read the key file")
        self.refused([self.good()], "the read key is empty", key="")

    def test_names_must_be_valid_and_unique(self):
        self.refused([self.good(name="US_East")], "lowercase letters, digits and dashes")
        self.refused([self.good(name="")], "lowercase letters, digits and dashes")
        self.refused([self.good(), self.good()], "used twice")

    def test_other_mistakes(self):
        self.refused([self.good(project="")], "\"project\" is required")
        self.refused([self.good(colour="red")], "unknown field \"colour\"")
        self.refused([self.good(api="ftp://x")], "must start with http")
        self.refused([], "non-empty \"clusters\" list")
        self.refused(["us"], "must be an object")
        with self.assertRaises(ValueError):
            server.load_fleet_config(self.write("bad.json", "[]"), environ={})
        with self.assertRaises(ValueError):       # not JSON at all (a JSON error is a ValueError)
            server.load_fleet_config(self.write("bad2.json", "{nope"), environ={})
        with self.assertRaises(OSError):
            server.load_fleet_config(os.path.join(self.dir, "missing.json"), environ={})


class FleetSurfaceTests(unittest.TestCase):
    """The HTTP handler and the command line, with a fleet."""

    @classmethod
    def setUpClass(cls):
        cls.stub = serve(StubAPI)
        cls.api_url = "http://127.0.0.1:%d" % cls.stub.server_address[1]
        cls.tmp = tempfile.TemporaryDirectory()
        cls.dir = cls.tmp.name
        for name, inv in (("us", US), ("eu", EU)):
            with open(os.path.join(cls.dir, name + "-inv.json"), "w") as f:
                json.dump(inv, f)
            with open(os.path.join(cls.dir, name + ".key"), "w") as f:
                f.write(FLEET_KEYS[name] + "\n")
        cls.config = os.path.join(cls.dir, "fleet.json")
        with open(cls.config, "w") as f:
            json.dump({"api": cls.api_url, "clusters": [
                {"name": n, "project": n, "k8sFile": n + "-inv.json", "readKeyFile": n + ".key"} for n in ("us", "eu")]}, f)
        cls.specs = server.load_fleet_config(cls.config)
        cls.app = serve(server.make_handler(None, None, cls.specs))
        cls.base = "http://127.0.0.1:%d" % cls.app.server_address[1]

    @classmethod
    def tearDownClass(cls):
        stop(cls.app)
        stop(cls.stub)
        cls.tmp.cleanup()

    def setUp(self):
        StubAPI.keys = dict(FLEET_KEYS)
        StubAPI.other = {
            "us": project_data(calls_from(US_FRONTEND, "203.0.113.10:443", conn=0.5, http=4.0)),
            "eu": project_data(calls_from(EU_FRONTEND, "10.96.1.1:443", conn=3.0, http=5.0, actual="10.0.0.11:443")),
        }

    def tearDown(self):
        StubAPI.other, StubAPI.keys = {}, {}

    def get(self, path):
        try:
            with urllib.request.urlopen(self.base + path, timeout=10) as r:
                return r.status, json.loads(r.read())
        except urllib.error.HTTPError as e:
            return e.code, json.loads(e.read())

    def test_the_page_offers_a_cluster_filter(self):
        with urllib.request.urlopen(self.base + "/", timeout=10) as r:
            html = r.read()
        self.assertIn(b'id="cluster"', html)
        self.assertIn(b"crossCluster", html)

    def test_the_graph_over_http_covers_every_cluster(self):
        code, g = self.get("/api/graph?window=15m")
        self.assertEqual(code, 200)
        self.assertEqual([c["name"] for c in g["clusters"]], ["us", "eu"])
        self.assertIn("eu/shop/api", {n["id"] for n in g["nodes"]})
        self.assertTrue([e for e in g["edges"] if e["crossCluster"]])

    def test_a_failing_cluster_is_reported_and_the_rest_drawn(self):
        StubAPI.keys["eu"] = "not-the-key-anymore"
        code, g = self.get("/api/graph?window=15m")
        self.assertEqual(code, 200)
        self.assertEqual({c["name"]: c["ok"] for c in g["clusters"]}, {"us": True, "eu": False})
        self.assertTrue(g["warnings"])

    def test_no_readable_cluster_is_a_502_with_the_reasons(self):
        StubAPI.keys = {"us": "wrong-us-key-0123456789", "eu": "wrong-eu-key-0123456789"}
        code, body = self.get("/api/graph?window=15m")
        self.assertEqual(code, 502)
        self.assertIn("no cluster could be read", body["error"])
        self.assertNotIn(FLEET_KEYS["us"], json.dumps(body))

    def test_a_fixed_range_works_for_a_fleet(self):
        code, g = self.get("/api/graph?from=2026-11-07T13:48:00Z&to=2026-11-07T13:52:00Z")
        self.assertEqual((code, g["window"]), (200, "2026-11-07T13:48:00Z/2026-11-07T13:52:00Z"))

    def test_the_command_line(self):
        import subprocess
        base = [sys.executable, os.path.join(os.path.dirname(os.path.abspath(__file__)), "server.py")]
        run = lambda args: subprocess.run(base + args, capture_output=True, text=True, timeout=30)
        r = run(["--clusters", self.config, "--print", "1h"])
        self.assertEqual(r.returncode, 0, r.stderr)
        g = json.loads(r.stdout)
        self.assertEqual([c["name"] for c in g["clusters"]], ["us", "eu"])
        for args, why in ((["--clusters", self.config, "--project", "gke"], "--clusters replaces"),
                          (["--clusters", self.config, "--kube-context", "x"], "--clusters replaces"),
                          ([], "give --project"),
                          (["--clusters", os.path.join(self.dir, "missing.json")], "error:")):
            r = run(args + ["--print", "1h"])
            self.assertNotEqual(r.returncode, 0, args)
            self.assertIn(why, r.stderr, args)
            self.assertNotIn("Traceback", r.stderr, args)


class FixedRangeTests(unittest.TestCase):
    """Looking back at something that is over: a fixed range instead of the last N minutes."""

    @classmethod
    def setUpClass(cls):
        cls.stub = serve(StubAPI)
        cls.tmp = tempfile.TemporaryDirectory()
        cls.cluster_file = os.path.join(cls.tmp.name, "cluster.json")
        with open(cls.cluster_file, "w") as f:
            json.dump(CLUSTER, f)
        cls.api_url = "http://127.0.0.1:%d" % cls.stub.server_address[1]
        api = server.TelemetryAPI(cls.api_url, PROJECT, KEY)
        cls.api = api
        cls.cluster = server.Cluster("", cls.cluster_file)
        cls.app = serve(server.make_handler(api, cls.cluster))
        cls.base = "http://127.0.0.1:%d" % cls.app.server_address[1]

    @classmethod
    def tearDownClass(cls):
        stop(cls.app)
        stop(cls.stub)
        cls.tmp.cleanup()

    def setUp(self):
        StubAPI.requests.clear()
        StubAPI.data = DATA

    def get(self, path):
        try:
            with urllib.request.urlopen(self.base + path, timeout=10) as r:
                return r.status, json.loads(r.read())
        except urllib.error.HTTPError as e:
            return e.code, json.loads(e.read())

    def test_the_api_is_asked_for_exactly_that_range(self):
        g = server.build_graph(self.api, self.cluster, (T0, T1))
        self.assertEqual(len(range_requests()), 3)
        for _, q, _ in range_requests():
            self.assertAlmostEqual(float(q["from"]), T0, places=2)
            self.assertAlmostEqual(float(q["to"]), T1, places=2)
            self.assertEqual(q["step"], "15")          # 240 s / 40 is below the 15 s floor
            self.assertEqual((q["rate"], q.get("from_zero")), ("true", "true"))
        self.assertEqual(g["window"], "2026-11-07T13:48:00Z/2026-11-07T13:52:00Z")
        self.assertEqual(g["range"], {"from": "2026-11-07T13:48:00Z", "to": "2026-11-07T13:52:00Z"})
        self.assertEqual(len(g["edges"]), 3)           # the same graph the fixture gives for a relative window

    def test_a_long_range_gets_coarser_steps(self):
        server.build_graph(self.api, self.cluster, (T0, T0 + 6 * 3600))
        self.assertEqual({q["step"] for _, q, _ in range_requests()}, {"540"})   # 21600 s / 40

    def test_a_relative_window_reports_its_range_too(self):
        g = server.build_graph(self.api, self.cluster, "15m")
        self.assertEqual(g["window"], "15m")
        self.assertAlmostEqual(
            server.parse_time(g["range"]["to"]) - server.parse_time(g["range"]["from"]), 900, delta=1)

    def test_http_accepts_rfc3339_and_unix_seconds(self):
        for qs in ("from=2026-11-07T13:48:00Z&to=2026-11-07T13:52:00Z",
                   "from=1794059280&to=1794059520",
                   "from=2026-11-07T15:48:00%2B02:00&to=2026-11-07T15:52:00%2B02:00"):
            code, g = self.get("/api/graph?" + qs)
            self.assertEqual(code, 200, qs)
            self.assertEqual(g["window"], "2026-11-07T13:48:00Z/2026-11-07T13:52:00Z", qs)

    def test_http_without_to_ends_now(self):
        code, g = self.get("/api/graph?from=" + str(int(server.time.time()) - 600))
        self.assertEqual(code, 200)
        self.assertAlmostEqual(server.parse_time(g["range"]["to"]), server.time.time(), delta=5)

    def test_http_refuses_bad_ranges_with_a_400_that_says_why(self):
        cases = {
            "from=1794059520&to=1794059280": "from must be before to",
            "from=1794059280&to=1794059280": "from must be before to",
            "from=yesterday": "neither RFC 3339",
            "from=1794059280&to=soon": "neither RFC 3339",
            "from=1794059280&to=%d" % (1794059280 + 31 * 86400): "longer than 30 days",
        }
        for qs, why in cases.items():
            code, body = self.get("/api/graph?" + qs)
            self.assertEqual(code, 400, qs)
            self.assertIn(why, body["error"], qs)
        self.assertEqual(StubAPI.requests, [], "a refused range must not reach the telemetry API")

    def test_command_line_prints_a_fixed_range(self):
        import subprocess
        keyfile = os.path.join(self.tmp.name, "key")
        with open(keyfile, "w") as f:
            f.write(KEY)
        base = [sys.executable, os.path.join(os.path.dirname(os.path.abspath(__file__)), "server.py"),
                "--api", self.api_url, "--project", PROJECT, "--read-key-file", keyfile, "--k8s-file", self.cluster_file]
        r = subprocess.run(base + ["--from", "2026-11-07T13:48:00Z", "--to", "2026-11-07T13:52:00Z"], capture_output=True, text=True, timeout=30)
        self.assertEqual(r.returncode, 0, r.stderr)
        g = json.loads(r.stdout)
        self.assertEqual(g["range"], {"from": "2026-11-07T13:48:00Z", "to": "2026-11-07T13:52:00Z"})
        self.assertEqual(len(g["edges"]), 3)
        for args, why in ((["--to", "1794059520"], "--to needs --from"),
                          (["--from", "1794059280", "--print", "1h"], "either --print WINDOW or --from/--to"),
                          (["--from", "nonsense"], "neither RFC 3339"),
                          (["--from", "1794059520", "--to", "1794059280"], "from must be before to")):
            r = subprocess.run(base + args, capture_output=True, text=True, timeout=30)
            self.assertNotEqual(r.returncode, 0, args)
            self.assertIn(why, r.stderr, args)
            self.assertNotIn("Traceback", r.stderr, args)


if __name__ == "__main__":
    unittest.main()
