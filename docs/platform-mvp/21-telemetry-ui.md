# Telemetry UI

A browser front end for the [telemetry-ingest](20-telemetry-ingest.md) read API. Package:
`platform-mvp/telemetry-ui` (see its README for development, build and test commands).

## Status

Step 1, the scaffold, is done: package, tooling, API client, session store, connection screen, nginx image,
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

- `make test-ui`: lint clean, 42 unit tests, production build (about 95 kB JS, 37 kB gzip).
- `make test-ui-chart`: renders nothing when disabled; Service type and node-port validation; pod runs as uid 101 with a
  read-only root, no ServiceAccount token, no capabilities; no Secret; proxy target defaults and overrides.
- `make test-ui-image`: against a stub API, `X-API-Key` and the path arrive unchanged; POST gets 405; `/v1/*` and other
  `/api` paths get 404; CSP, `X-Frame-Options`, `nosniff` and `no-store` are set; the container starts with no API behind it.

Not yet done: a run against the real service on a cluster, TLS, and the data views.

## Next

1. Services list and a per-service page (rates, errors, latency).
2. Service map view, ported from `hack/platform-mvp/servicemap` to a backend endpoint the UI can call.
3. Traces and logs, then profiles.
