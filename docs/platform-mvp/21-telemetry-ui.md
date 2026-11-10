# Telemetry UI

A browser front end for the [telemetry-ingest](20-telemetry-ingest.md) read API. Package:
`platform-mvp/telemetry-ui` (see its README for development, build and test commands).

## Status

Step 1 (scaffold and connection screen) step 2 (the services list) step 3 (the per-service page) step 4 (the service map), step 5 (metrics on the service page) step 6 (the traces and logs views) step 7 (the profiles view) step 8 (alerts for services that are not serving) step 9 (running the rules in a service) and step 10 (a sidebar menu and the Applications page) are done. The services list merges the services seen in traces, logs and profiles, with a time window, a name filter and per-source failure handling.
chart template, Makefile targets, tests. No data views yet; they are the next steps.

## The menu and the Applications page

The main menu is a **sidebar** with one entry per page, each its own page: **Applications**, **Map**, **Traces**, **Logs**,
**Profiles** and **Alerts** (with a count of what is firing). At the bottom are the connected **project** (it links to the
Connection page, where you change it) and **Collapse**, which shrinks the menu to icons and is remembered; on a narrow window it is
icons only. The Alerts count stays visible on its icon when the menu is collapsed. `/` goes to the applications when there is a
connection and to the connection page when there is not, and connecting opens the applications. The old Services addresses
(`/services`, `/services/<name>`) still work and land on the applications.

**Applications** (`/applications`) replaces the Services page. It lists every application of the project, as the reference page does:

- **Namespaces** are a drop-down you can fill with any number of namespaces (none means all), with a filter, select-all and clear.
  Three checkboxes choose the *kinds* of namespace shown: **application** (on by default), **control-plane** (`kube-system`, `gke-*`,
  `istio-system`, ...) and **monitoring** (`monitoring`, `prometheus*`, `telemetry*`, ...). The drop-down offers only the namespaces
  of the kinds that are on. Search matches an application or namespace name.
- **Status chips** count the applications by how they are doing and narrow the table when pressed: **Critical** (an alert firing at
  that level, or it has pods and none is running), **Warning** (a warning alert, a restart or OOM kill in the window, or at least 1% of
  the calls it answers failing), **Errors in logs** (only its logs show errors), **No data** (only a name is known) and **OK**. The
  coloured bar on each row is the same status. A firing alert changes an application's status on the page without a reload.
- **Columns:** type (from the node-agent, else the protocol it serves), traffic in (calls a second), errors (share of those calls
  failing), upstreams (calls out that are healthy over all), instances (pods running over pods seen in the window), restarts in the
  window, CPU (mean cores), memory, and unique errors in its logs (a link to them).
- The time range is at the top right. The choices are in the address, so a view can be shared.

**Where the list comes from.** The API has no inventory, so it is assembled, and the node-agent's own metrics are the source, so an
application that stopped sending traces still appears (as down, not as missing): every workload with a container in the memory gauge,
plus any service the traces or logs name and any node of the call map. Pods that stopped reporting keep their last reading, which is
how "0 of 1 running" is told from "never existed". Instances are pods, not containers, so a sidecar is not a second instance. Each
metric is read once for all applications, about 25 reads per load however many applications there are.

Limits: there is no latency column (HTTP latency is not read yet), no sorting by column (rows are sorted worst first), and the logs
column counts unique `ERROR` lines among the newest 1000 of the window, grouped so that lines differing only in numbers count once. If a
metric hits the series cap the page says the list may be partial.

**Trying it without a cluster.** `server/testing/demo-read-api.js` is a small stand-in for the read API with a shop that has a few
things wrong (a restarting service, failing calls to Redis, error logs, a stopped pod), for screenshots and development:

```sh
docker run --rm -d --name demo-api -p 8090:8080 -v "$PWD/platform-mvp/telemetry-ui":/app:ro -w /app \
  -e KEY=demo-read-key -e PROJECT=demo node:24-alpine node server/testing/demo-read-api.js
make ui-dev UI_API=http://host.docker.internal:8090      # then connect to project "demo" with the key "demo-read-key"
```

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

- `make test-ui`: lint clean, 869 unit tests, production build (about 95 kB JS, 37 kB gzip).
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

## The Service map page

The map is laid out to be read, after the reference page:

- **Header and filters** match the Applications page: the time range at the top right; a search, the **namespaces** drop-down (several at
  once) and the **application / control-plane / monitoring** checkboxes. Only application namespaces are drawn at first. The picture shows the
  applications of the namespaces and kinds chosen, the external endpoints they call, and the calls among those. Search dims what does not
  match (and says how many match) without moving anything. A namespace chosen earlier whose kind is then switched off stops filtering, as the
  picker shows "all" for it (the Applications page had the same fault and is fixed too).
- **Cards, not boxes.** Each application is a card: a status dot, its name, `type / ns:<namespace>` below it (`redis / ns:shop`,
  `ns:shop`), and an icon for what it is (workload, database, queue, external endpoint, which is a dashed card with a hollow dot). The dot
  is green, amber or red from a firing alert or from the share of calls it answers that fail (1% and 5%); it is calls and alerts only, since
  the map has no instance or restart data (the Applications page has those).
- **Thin straight lines** from caller to callee with a small arrowhead, green when healthy, amber or red when not (arrowheads keep one size
  whatever the line's width). Nothing is written on a healthy line: a failing one carries its rate and error share, and selecting an
  application labels its lines, in the gap before the target where no card is. Hover a line for its numbers. A call back to an earlier layer
  arcs, so it does not run along the forward ones.
- **Left to right or top to bottom**, with the toggle at the right of the filters. Layers are the longest path through the calls, as in the
  reference's "flow" mode; an application with no calls stays, on its own.
- **Fits the page.** The picture is scaled down to the width it has (never up, and never below 65%, where text would stop being readable;
  past that it scrolls), with zoom in, zoom out and fit buttons. Selecting an application does not change its size: its details (calls in
  and out, with rates, error shares and its status) open below it, with a link to its page; double-click opens it. The full list of calls is
  below, collapsed, as a table.
- The choices (search, namespaces, kinds, direction, time range) are kept in the address, so a view can be shared.

## Plain hosts (VMs, bare metal, Docker)

The map and the Applications page now cover machines that are not Kubernetes nodes, matched the way Coroot's server does it:

- **An application per unit or container.** The node-agent names a process by its cgroup path, such as `/system.slice/nginx.service` or
  `/docker/postgres`, the same on every machine that runs it. That path is the application id (and the service name on its traces and logs), the
  name drops the `.service`, and the namespace shows as `host`. The same unit on two machines is **one application with two instances**: every
  series carries `machine_id`, and instances are counted per machine (per pod for Kubernetes). Nomad, ECS and Swarm ids are left out, not
  mis-grouped.
- **Address and port, not the IP.** One host IP carries many services, so an address is matched on its exact `ip:port`, from the listen-address
  metric. An IP alone is trusted only for a Kubernetes pod, where an IP is one pod's. A call to a port nothing listens on shows as external, never
  as whichever service happens to listen on another port of that machine. Two applications claiming the same `ip:port` at the same moment is
  ambiguous and resolves to nothing; a pod IP reused later goes to the latest.
- **Loopback on a host is a call.** A process dialling `127.0.0.1:5432` is talking to whatever listens there on **the same machine**
  (`machine_id` plus port), and on no other. Inside a pod loopback still stays in the pod and is not drawn.
- The Service-address learning (above) works on hosts too, for a virtual IP in front of a service.

Not covered: Nomad, ECS and Swarm; a host process with no listen-address metric cannot be found as a callee; a loopback listener is found only
when the caller's series carries `machine_id` (every series from the node-agent does).

## Hover menu, categories and DNS on the Map

- **Hover a card** for a menu of how that application is doing: **Instances** (running of seen), **CPU** and **Memory** against their limits (an
  out-of-memory kill is bad whatever the usage), **DNS** (share of lookups that fail) and **Logs** (unique errors), each with a coloured dot and a
  short reason. Each entry is a link to that part of the application's page (`/applications/<id>#memory`), which scrolls there once it has its data.
  The data is the Applications page's, read with the map and shared through a 30-second cache, so moving between the two pages does not reread it.
  If it cannot be read the map is still drawn and the menu says "not loaded".
- **⋮ on a card** opens **Move the app to a category**: *+ a new category*, then every other category. A category is where you put applications to
  show or hide them with the checkboxes (application, control-plane, monitoring, and any you make; the **+** next to the checkboxes makes one too).
  Moving an application into a category that is not shown takes it off the picture, so the page says so and offers **Show it** and **Undo**.
- **Where categories are kept: in this browser** (localStorage, per project). They are your own way of organising what you look at, so they are
  not shared with other people or other browsers. The store (`src/categories.js`) is a small interface, so keeping them somewhere shared later does
  not touch the pages. Names are lowercase letters, digits, spaces and dashes, up to 32 characters, up to 12 of your own.
- **DNS** is a section on the application page: lookups a second, the share that fail (SERVFAIL, refused, malformed), and the names with the most
  failures. NXDOMAIN is counted apart and is **not** a failure: with search domains every short name misses once, so a healthy application has many.
  (The agent also exports `ip_to_fqdn`, which could name external endpoints; not used yet.)

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

**Service addresses (learned).** An app dials a Service's cluster IP and the kernel translates it to a pod; the node-agent recovers the pod
from the conntrack table and reports both addresses (`destination`, `actual_destination`). A connection that *fails* has no pod to look up,
so the agent reports only the address that was dialled, and without more help the map would draw a refused connection to a Service as an
external endpoint, which is exactly when the callee matters most. So the map learns the table itself, with no Kubernetes data: every
successful connection pairs a dialled address with a reached pod whose application is known, which is one vote for "this address leads to
that application". Failed connects are then drawn against the application behind the address (and count against it in the error share and
in the alerts).

- Only the exact `ip:port` is learned (a Service's other ports may lead elsewhere), and an address a connection actually reached is never
  overridden by what was learned.
- An address that leads to more than one application (overlapping selectors) goes to the one with the most traffic, ties by name, and the
  page warns that it chose.
- The learning reads successful connects once more over a longer lookback (four windows, at least an hour, at most a day), so a Service that
  has been failing for the whole window is still recognised by what it did before. If that read fails the map is drawn without it, with a
  warning, and is not marked incomplete.
- A Service that never had a successful connection in the lookback (down since before it, or brand new) still shows as external.
- When it was used, the Map page says so ("matched from the successful connections to it earlier").

Limits: no HTTP latency yet; one project is one cluster (no cross-cluster view); a service the agent cannot see
listening, or called only through an external address, shows as external; if a call counter returns more series than one
call allows, the page says the map is partial and suggests a shorter window.

## Configuration: the Project entry and the ClickHouse tab

The **Project** entry at the foot of the menu opens **Configuration**, which has a tab strip: **Connection** (`/configuration/connection`; the old `/connection` address redirects there) and **ClickHouse** (`/configuration/clickhouse`).

The ClickHouse tab follows Coroot's ClickHouse settings page, but is **read-only**: the protocol (Native), database and TLS are shown disabled, and the address and credentials read `<hidden>`. There is no "Test & Save" because the connection is set when the telemetry service is deployed, and a browser with a read key must not be able to change it. Instead the tab shows a status line (version, answer time) and a **Test connection** button, which asks the service again.

Below it, **Storage Usage** lists, for each kind of data, this project's size (an estimate from its share of the table's rows), rows, compression ratio, TTL and how long ago its oldest data is. **Cluster Topology** lists the server's disks with free and total space and a usage bar (green, amber from 75%, red from 90%). Both the platform-wide columns and the disks need `telemetryIngest.exposeClusterInfo=true`; without it the tab says the operator has not shared them. The endpoint is described in [20-telemetry-ingest.md](20-telemetry-ingest.md#clickhouse-information).

The other tabs of Coroot's Configuration page (Prometheus, AI, cloud integrations, applications, notifications, organization) are not built.

Needs a telemetry-ingest image that has the `clickhouse` endpoint: an older one makes the tab say it did not answer with ClickHouse information.

## Incidents: finding them and saying why

**Incidents** (menu, `/incidents`) lists the times an application used its error budget up too fast, and each opens an analysis (`/incidents/<id>`) laid out like Coroot's incident page: a headline naming the cause, when it started and ended, the objectives with compliance and burn rates, then the root cause, a graph of the affected applications, the request path, what changed, error logs, CPU hot spots and, on the Traces tab, the failed and slowest requests. Everything is computed in the browser from the read API; nothing new is stored or deployed beyond the UI image (and a telemetry-ingest that serves the metrics it already did).

**Detection** (`src/incidents/inbound.js`, `detect.js`). Two objectives per application, from the node-agent's inbound HTTP metrics: 99.9% of requests should not fail (5xx and gRPC server failures) and 99.9% should be served in under 500 ms (the histogram bound at or above the threshold; if the agent has no such bound the next one is used and the page says so). The burn rate is the bad share over the budget (0.1%). An incident is a stretch where a 5-minute and a 1-hour window both burn at 14.4x or more (the usual multi-window rule), at least 2 minutes long, joining breaches closer than 5 minutes; its start and end are the first and last minute that burned that fast. A window with fewer than 20 requests is never judged: a quiet service is not an incident. Applications with no inbound HTTP metrics have no objective and so no incidents.

**Analysis** (`analyze.js`). The incident's window is compared with the hour (at least) before it, leaving out the two minutes the trouble was starting. For the hurt application and everything it calls, however far down, it looks for: request errors and p95 against the baseline; CPU at its limit (or, with no limit, several times the cores); restarts and out-of-memory kills that began in the incident; error log lines per second; and for each call, its error share, p95, connection time and network round trip. Round trip and connection time are new on the service map's edges (`rttMs`, `connMs`; the round trip is per callee IP, so only for pods). The cause is picked by how directly it points: (1) a call whose round trip or connection time rose, (2) a resource problem in an application none of whose own calls got slower, (3) the deepest call that got slower or started failing, shown as only "possible". Trouble travels up the call chain, so among causes the one furthest from the hurt application wins. When nothing qualifies it says there is no cause among the dependencies and lists what was compared; it does not guess. Thresholds are in `ANALYSIS_DEFAULTS` and `SLO_DEFAULTS` and can be passed in.

**The words** (`narrative.js`) come from fixed sentence shapes over the findings, so every name and number in the text is in the findings and the text is the same every time. The findings object (`analyze()`'s result: issues with before and during values, the affected nodes and calls, the root cause with its path and confidence, what was checked) is the contract: a language model can be added later by giving it that object plus the evidence and asking for a remediation section, with the templated text as the fallback when it is off or fails. Nothing about the analysis depends on one.

**Rules you can change.** *Configuration → Incidents* sets the objectives (availability, latency, what "fast enough" means), objectives of single applications that differ, when it is an incident (burn rate, the two windows, fewest requests, shortest incident), what size of change counts in the analysis, and which checks run at all (a check turned off is not listed and cannot be the cause). They are kept **in this browser, per project** (`localStorage`, `src/incidents/settings.js`), the same as the application categories: someone else, or the same person in another browser, sees the defaults. The read key cannot write to the service, and sharing needs a write endpoint and a decision on who may change it; the store's interface (get, set, reset) is small so that can replace the browser later. Values are typed in percent, minutes and milliseconds, validated against ranges, and cleaned on the way in from storage (a value that is not a number in its range goes back to the default), so a stale or hand-edited entry cannot make the pages misjudge. A change applies the next time the Incidents pages load; an incident that no longer breaches the new rules disappears from the list.

**Not covered.** Events that are not in the telemetry (a chaos experiment, a deploy, a config change) cannot be named as a trigger: the node-agent does not send Kubernetes events. The latency threshold (what is "fast enough") is one value for all applications, because the agent's histogram is read at one bound; only the objectives (percent) can differ per application. Rules of your own (a new metric, filter and threshold) are not supported. The alert service does not open incidents: this is a page, not a notifier. A cause is a likely explanation from correlated changes, not a proof.

## "The node-agent is not set up" notice

The UI cannot look into a cluster, but it can look at what the hub stored, and an empty project is almost always the node-agent not being set up. Applications, Map, Traces, Logs and Profiles show an **error at the top** when the project has no telemetry from it (`src/components/TelemetryNotice.vue`, `src/api/telemetry.js`):

| State | Seen as | Says |
|---|---|---|
| `none` | the project has no metrics at all | no telemetry has arrived: the node-agent is probably not installed, or cannot reach the hub |
| `other` | metrics exist, none named `container_…` (the node-agent's) | metrics arrive from something else; the node-agent is missing |
| `stale` | the node-agent's newest metric has no point in the last 10 minutes | the agents have stopped or lost their connection to the hub |

Each carries the three things to check in the cluster that should be reporting: is the DaemonSet installed (`kubectl -n telemetry-agent get daemonset node-agent`, and where to turn it on in the spoke chart), is it running on every node (`get pods -o wide`, and the usual reasons: image tag, Pod Security, taints), and can it reach the hub with the right key (`logs ds/node-agent`, how to read a 401 against a refusal or timeout and why `collectorURL` must not be `localhost` or a `ClusterIP`). A **Check again** button re-asks, and the check repeats when the connected project changes.

It never guesses: a check that cannot be made (the read fails, or only the freshness probe fails) shows nothing rather than telling someone to fix what is not broken, and the page reports a failed read itself. It needs the injected client and session, so a page mounted without them (the page tests) is unaffected. The demo API can show it: `NO_AGENT=1` (no metrics) or `NO_AGENT=other`.

## Which version is deployed

The foot of the sidebar says which build is running: the name and version, then the commit and the day it was built (hover for the whole of it). It is hidden when the menu is collapsed. The same facts are served as `GET /version.json` (never cached), so what is deployed can be read with curl or a browser, without cluster access:

```sh
curl -s http://<ui address>/version.json
# {"name": "telemetry-ui", "version": "v20", "commit": "7ce1cf979ce8-dirty", "built": "2026-11-10T09:30:06Z"}
```

They are fixed at **build time** (`vite.config.js` defines them from `APP_VERSION`, `APP_COMMIT` and `APP_BUILT`; `src/buildInfo.js` checks them), so they describe what was built, not what is configured where it runs. `make build-telemetry-ui-image` fills them: the version is `IMAGE_TAG`, the commit is `git rev-parse --short=12 HEAD` with `-dirty` added when the working tree has uncommitted changes (and none outside a git checkout), and the time is now (UTC); any can be set on the command line (`UI_COMMIT=...`, `UI_BUILT=...`). A build without them (`npm run build`, or `docker build` with no arguments) says `dev`. A value that is not the shape of a version, commit or time is shown as `unknown` or left out, never put in the page. The version is the image tag, so tag each build differently (v20, v21, …): `dev` or `latest` would make the footer say that and nothing more.

## Next

1. HTTP latency (the node-agent has a request-latency histogram; the map and the service page show p95 only for databases so far).
2. A fleet view over several projects (the alert service already checks every project in the file).
3. Alerts for the service itself failing, beyond its metrics: a second Alertmanager route, or a heartbeat.
