# Telemetry UI

A browser front end for the [telemetry-ingest](20-telemetry-ingest.md) read API. Package:
`platform-mvp/telemetry-ui` (see its README for development, build and test commands).

## Status

Step 1 (scaffold and connection screen) step 2 (the services list) step 3 (the per-service page) step 4 (the service map), step 5 (metrics on the service page) step 6 (the traces and logs views) step 7 (the profiles view) step 8 (alerts for services that are not serving) and step 9 (running the rules in a service) are done. The services list merges the services seen in traces, logs and profiles, with a time window, a name filter and per-source failure handling.
chart template, Makefile targets, tests. No data views yet; they are the next steps.

## Decisions

| Question | Choice | Why |
| --- | --- | --- |
| Stack | Vue 3 + Vite | Same family as Coroot's UI, so its views are a reference; small build, fast tests |
| Serving | nginx (unprivileged) image, own Deployment | One artefact, no Node at runtime, read-only root filesystem |
| Reaching the API | Same-origin `/api` proxy in nginx | No CORS; the service's address is not in the browser; the proxy can be narrowed |
| Auth | The user's read key, in `sessionStorage`, in a header | Matches the per-project keys; nothing stored server-side; gone with the tab |
| Exposure | Proxy allows GET/HEAD on `/api/v1/projects/` only | Exposing the UI must not expose ingestion (`/v1/*` is 404) |
| Coroot code | None copied | Different API and product; a `NOTICE` is required if that changes |

## Deploy

```sh
make build-telemetry-ui-image push-telemetry-ui
helm upgrade hub-services deploy/platform-mvp/chart/hub-services --reuse-values --set telemetryUi.enabled=true
kubectl -n monitoring port-forward svc/telemetry-ui 8081:80
```

## Verified

- `make test-ui`: lint clean, 400 unit tests, production build (about 95 kB JS, 37 kB gzip).
- `make test-ui-chart`: renders nothing when disabled; Service type and node-port validation; pod runs as uid 101 with a
  read-only root, no ServiceAccount token, no capabilities; no Secret; proxy target defaults and overrides.
- `make test-ui-image`: against a stub API, `X-API-Key` and the path arrive unchanged; POST gets 405; `/v1/*` and other
  `/api` paths get 404; CSP, `X-Frame-Options`, `nosniff` and `no-store` are set; the container starts with no API behind it.

Not yet done: a run against the real service on a cluster, TLS, and the data views.

## Service page: what the figures are

`/services/<name>` shows, for a window, the service's spans (counts, errors, p50/p95/p99, a timeline), recent errors from
spans and logs, recent spans, log counts by severity and the profile types. They come from the spans and logs the API
returns, not from the metrics: those are keyed by container, and nothing in the API ties a container to a service name.

- Spans are grouped by kind. A call is a CLIENT span in the caller and a SERVER span in the callee, so the two are shown
  apart, not added together.
- The API returns the newest 1000 rows. When a window has more, the figures describe only the newest part, and the page says so;
  the timeline starts where the answer stops instead of showing the missing part as quiet.
- Percentiles are nearest-rank over the returned spans.

## Service page: traffic and resources

The page's **Traffic** and **Resources** sections come from the metrics, for services named like `/k8s/<ns>/<name>` (what the
node-agent calls a pod workload; other names have no container-keyed metrics and the page says so).

- **Called by / Calls out to:** the map's edges for this service, with a total each way. Totals are computed from counts
  (failures over attempts), never by averaging percentages, which would let a quiet healthy caller hide a failing busy one.
- **Resources:** instances running, CPU used (mean cores over the window) against the limit, memory in use against the limit,
  restarts and OOM kills in the window. A pod that stopped reporting is left out, so a replaced pod is not counted twice, and
  a limit of 0 means none is set.
- One failing metric leaves the rest, with a warning. A metric the project does not have is simply not shown.

## Traces and logs

- **`/traces`** searches spans: service, span name (exact), status, slower than N ms, root spans only, window, and how many
  (newest 100, 500 or 1000). **`/traces/<id>`** shows one trace as a waterfall (spans nested under their parent, bars scaled
  to the trace) with the selected span's attributes, resource attributes and events, and a link to that trace's logs.
- **`/logs`** searches log lines: service, severity (exact, as logged: `WARN` and `WARNING` are different), text contained
  (case-insensitive), trace id, window and count. Bodies are shown as text, never as markup, and keep their line breaks.
- The filters are kept in the address (`?service=…&status=error`), so a search can be bookmarked and shared, the back button
  works, and the service page and trace page link in with the right filter already set.
- The API returns the newest N rows and cannot be paged. A full page is labelled "the newest N; more match" and the
  remedy is a shorter window or another filter, not a "load more".
- The node-agent records each request as its own span with no parent link, so most traces are one span and the waterfall is a
  flat list by start time (the page says so). Traces from an instrumented application, which do link, are nested.
- Times in a waterfall are read to the millisecond; durations are exact.

## Profiles

`/profiles` picks a service and one of its profile types (`cpu:nanoseconds`, `alloc_space:bytes`, ...), a window and how many
stacks to read (top 500, 2000 or 5000, the API's ceiling), then shows:

- a **flame graph** (root at the top, callees below, width = share of the value). Click a box to zoom to it, use the path above it to
  go back; type in *Highlight* to dim everything else. Frames narrower than 0.1% of the view are not drawn (the threshold
  rises if that would be more than 2500 boxes, and the page says so); zooming in brings them back.
- a **Hottest functions** table by self time, with self and total. This is the keyboard-accessible equivalent of the graph:
  the boxes are for the mouse. *Total* counts a function once per stack, so recursion is not counted twice.
- the amount in the profile's own unit (time, bytes or a count) and, for CPU profiles, the mean cores it represents.

The API returns the heaviest stacks first and at most N, so when N is reached the lighter ones are missing and every figure is of
the stacks returned; the page says so. The service page links each of its profile types here.

## Alerts: "service not serving"

`/alerts` shows what the evaluator found. The nav shows a count of firing alerts (red if any is critical) and the tab title carries it, on
every page.

**Where it runs.** In the alert service when one is deployed (below), otherwise in the browser. The page tries the service first on
every check and says which it is using, so deploying or removing the service takes effect without a reload. Browser mode checks every
60 seconds while the app is open in a tab and keeps what it found for that tab's session (sessionStorage, per project; never a key);
close the tab and nothing is checking. Each browser check is about 15 to 25 read calls (the metric list, then two looks at the
map's counters).

**Rules.** Each compares the last 5 minutes (the recent window) with the hour before it (the baseline):

| Rule | Fires when | Severity |
| --- | --- | --- |
| Stopped receiving traffic | a service that had at least 0.1 calls+connections/s now gets under 5% of that | critical at zero, else warning |
| Connections failing | at least 20% of connection attempts into it fail, and at least 5 failed in the window | critical at 90% |
| Calls failing | at least 5% of the calls it answers fail (5xx, gRPC server errors), over at least 20 calls | critical at 50% |
| No running instances | it was listening in the baseline or recent window and no instance reports a listen address now (suppresses the no-traffic alert that follows from it) | critical |
| No telemetry | all calls disappeared at once: the agents or ingest, not every service (replaces the per-service absence alerts) | critical |

In browser mode the thresholds are editable on the page (kept per tab) and reset to the defaults with one click; with the alert service
they are the chart's (`telemetryAlerts.thresholds`) and are shown read-only.

**Absence is not the same as a failed read.** A rule that depends on something being missing is skipped, and the page says so, when either
look was incomplete (a counter failed or was cut short). If a whole check fails, nothing is resolved: the alerts stay as of the last good
check and the page says it could not look. A condition must hold on two checks in a row to fire, and be absent on two to resolve, so
one odd reading or a wobble does not raise or clear anything. A service that has been gone longer than the baseline stops being flagged
by the absence rules (the baseline no longer remembers it); pick a longer baseline to keep a long outage visible.

**Root cause with Claude.** *Analyze with Claude* on an alert gathers the evidence and builds one Markdown report: the alert and its
evidence, the other alerts open at the time (earliest first, since a cause usually precedes its effects), who calls the service and
whom it calls, its resources, its failing spans and its error and warning logs (similar ones grouped and counted, so the report stays
small), a note on what the data cannot show, and a closing request for ranked causes and the next thing to check. You copy it into your
own Claude subscription; the page calls no AI service and sends nothing. Bearer and basic tokens, `password=`/`token=`-style values,
credentials in URLs, JWTs and AWS keys are masked, **best effort only**: read the report before pasting it.

## The alert service

`Dockerfile.alerts` builds a small Node service (`server/`) that runs the **same** rule and lifecycle modules as the browser
(`src/alerts`, `src/map`, `src/api/client.js`), so a rule behaves identically in both. It has no dependencies: the image is Node plus
about a dozen source files.

- **Checks** every project in `projects.json` (the file the hub chart already writes for telemetry-ingest) every `intervalSeconds`
  (60), each with its own read key, so it sees what a read key can see and nothing more. Only names and read keys are taken from the
  file. A project added to or removed from the Secret is picked up on the next check. One project failing never affects another.
- **Serves** `GET /api/v1/projects/<project>/alerts` to a caller that presents one of that project's read keys, in the read API's
  `{"data": ...}` envelope: 401 without a key, and the same 403 for a wrong key, another project's key, the ingest key or an unknown
  project (so it does not reveal which projects exist). The key must be in the header, not the URL. nginx routes exactly that
  path to it and everything else to telemetry-ingest (`ALERTS_UPSTREAM`); if the service is absent that path answers 502 and the page
  checks for itself.
- **Exports** `/metrics` (Prometheus): `telemetry_alerts_firing{project,rule,severity}`, `..._last_check_ok`, `..._checks_total`,
  `..._notify_ok`. `/healthz` is liveness; `/readyz` turns ready after the first check (successful or not), so a rollout does not
  wait on a failing API.
- **Tells Alertmanager** (optional, `alertmanagerUrl`): every check re-sends each firing alert to `/api/v2/alerts` with an end time three
  intervals ahead (if this service dies they expire by themselves), and sends a just-resolved alert once with its real end. They are
  re-sent even when a check fails, because what was firing is still firing as far as anyone knows. Labels: `alertname`
  (`ServiceCallsFailing`, `ServiceConnectionsFailing`, `ServiceStoppedReceivingTraffic`, `ServiceNoInstances`, `TelemetryMissing`),
  `severity`, `project`, `workload`, `service`, `rule`.
- **Remembers** what is firing in a file, so a restart does not forget it or restart its clock. By default that is an `emptyDir`
  (survives a container restart, not a new pod); `telemetryAlerts.persistence.enabled` uses a volume (and `Recreate`, since a
  ReadWriteOnce volume cannot be mounted twice).
- **One replica**: two would each keep their own idea of what is firing and each tell Alertmanager. It is a single point of failure for
  alerting; `telemetry_alerts_last_check_ok` and the page's "could not check" are how you notice.
- **Safe to apply alone**: its template makes no Secret (it mounts only `projects.json` of the existing keys Secret, read-only), so
  `helm template -s templates/telemetry-alerts.yaml | kubectl apply -f -` cannot overwrite your keys.

Deploy (the UI image must be rebuilt too, for the nginx route and the page):

```sh
make build-telemetry-alerts-image push-telemetry-alerts build-telemetry-ui-image push-telemetry-ui DOCKER_REGISTRY=$REG IMAGE_TAG=v10
helm template hub-services deploy/platform-mvp/chart/hub-services -s templates/telemetry-alerts.yaml \
  --set telemetryAlerts.enabled=true --set telemetryAlerts.image.repository=$REG/telemetry-alerts --set telemetryAlerts.image.tag=v10 \
  | kubectl --context "$CTX" apply -f -
helm template hub-services deploy/platform-mvp/chart/hub-services -s templates/telemetry-ui.yaml \
  --set telemetryUi.enabled=true --set telemetryUi.image.repository=$REG/telemetry-ui --set telemetryUi.image.tag=v10 \
  | kubectl --context "$CTX" apply -f -
kubectl --context "$CTX" -n monitoring logs deploy/telemetry-alerts --tail=5      # "listening", then "alert firing" lines
```

Tests: `make test-ui` (rules, lifecycle, the service's config, auth, evaluator, Alertmanager payloads, and an end-to-end run of the
real evaluator and handler against a stub read API and a stub Alertmanager over real sockets), `make test-telemetry-alerts-chart`, and
`make test-telemetry-alerts-image` (the real image, read-only and non-root, through firing, auth, metrics, Alertmanager, a restart and
resolution). `make test-ui-image` also checks the nginx routing of the alerts path.

## Service map: how it works

`/map` is built in the browser from the metrics the API serves, with no Kubernetes API and no extra backend:

1. **Who the caller is.** Each counter carries the caller's `container_id` (`/k8s/<ns>/<pod>/<container>`). The node-agent's own
   naming rule (the same one that names a service on its traces) turns it into a workload, `/k8s/<ns>/<name>`, so a map node and
   a Services-list entry are the same string and a node links to its service page.
2. **Who the callee is.** Every container reports the addresses it listens on (`container_net_tcp_listen_info`, one per pod IP,
   including for wildcard listeners). A call's `actual_destination` is the pod it really reached after the Service address was
   translated; matching it to a listening container names the callee. An address nobody claims is drawn as an external
   (dashed) node. An address two workloads claim at once (host networking) is not used, so it never names the wrong one.
3. **What an edge says.** Rate is the mean over the window with quiet buckets counted as zero. Error share is
   `(call errors + failed connects) / (calls + failed connects)`: a failed connect never becomes a request, so counting it keeps
   a callee that refuses connections visible. Errors are HTTP 5xx and the gRPC server-side codes; caller mistakes (4xx,
   NOT_FOUND, ...) are not. Databases and brokers (Postgres, MySQL, Redis, Kafka, ...) are edges to the node that serves
   them, with p95 from the client's histogram (none for ClickHouse, whose client-side figure was wrong by two orders of
   magnitude). Loopback calls stay inside a pod and are left out.

Limits: no HTTP latency yet; one project is one cluster (no cross-cluster view); a service the agent cannot see
listening, or called only through an external address, shows as external; if a call counter returns more series than one
call allows, the page says the map is partial and suggests a shorter window.

## Next

1. HTTP latency (the node-agent has a request-latency histogram; the map and the service page show p95 only for databases so far).
2. A fleet view over several projects (the alert service already checks every project in the file).
3. Alerts for the service itself failing, beyond its metrics: a second Alertmanager route, or a heartbeat.
