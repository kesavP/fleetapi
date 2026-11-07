# telemetry-ui

A Vue 3 + Vite app for the telemetry-ingest read API, served by nginx. **Step 1 (this scaffold)** is the
connection screen only: a project and its read key, checked against the API before they are kept. The
views (services, service map, traces, logs, profiles) come next and plug into the router.

## Origin

Written for this repository. It borrows the *shape* of Coroot's `front` package (Vue 3 and a build step),
but **no Coroot source is copied**; Coroot's UI is a different product, tied to its own API. If code is ever
copied in from `coroot/coroot`, keep its Apache-2.0 notice: add a `NOTICE` here naming the files and the
upstream commit, and keep the licence headers in them.

## How it fits

```
browser ── /            static app (nginx)
        └─ /api/v1/projects/…  ──GET only──▶  telemetry-ingest   (X-API-Key passed through)
```

- Same origin, so no CORS and the browser never needs the service's address.
- nginx proxies **only** `GET`/`HEAD` on `/api/v1/projects/`. The agents' ingest endpoints (`/v1/…`) and anything
  else under `/api` answer 404, so exposing the UI does not expose ingestion.
- The read key is typed into the page and kept in `sessionStorage` (gone when the tab closes), sent in an
  `X-API-Key` header, never in a URL, log line or error message. The image holds no key.
- Strict CSP (`default-src 'none'`, scripts and styles from self only), `no-store` on the page and the API.

## Develop

Node runs in Docker; the host needs only Docker. From the repository root:

```sh
make ui-install        # npm ci, modules in a Docker volume
make test-ui           # lint, unit tests, production build
kubectl -n monitoring port-forward svc/telemetry-ingest 8080:80 &
make ui-dev            # http://localhost:5173, /api proxied to :8080 (UI_API=... to change)
```

With a local Node 22+ (`.nvmrc` says 24) the same scripts work: `npm ci && npm run dev|test|lint|build`.

## Build and deploy

```sh
make build-telemetry-ui-image push-telemetry-ui       # DOCKER_REGISTRY and IMAGE_TAG as for the other images
helm upgrade hub-services deploy/platform-mvp/chart/hub-services --reuse-values \
  --set telemetryUi.enabled=true --set telemetryUi.image.repository=<registry>/telemetry-ui
kubectl -n monitoring port-forward svc/telemetry-ui 8081:80      # http://localhost:8081
```

Then enter the project name and its read key (`telemetry-ingest-keys`, field `<project>.read`).

`API_UPSTREAM` (container env, chart value `telemetryUi.apiUpstream`) is where `/api` goes; the default is
the `telemetry-ingest` Service. nginx resolves it per request, so the pod starts before the API exists.

## Tests

| Command | Covers |
| --- | --- |
| `make test-ui` | 42 Vitest tests (API client, session store, connection form), lint, build |
| `make test-ui-chart` | chart rendering: off by default, service types, locked-down pod, no Secret |
| `make test-ui-image` | the real image in front of a stub API: proxying, GET-only, blocked paths, headers, read-only root, starts without its API |

The chart serves plain HTTP. The read key crosses it in a header: keep the Service private or put TLS in
front before exposing it beyond a port-forward.
