# telemetry-client

A client for the hub's read API (telemetry-ingest): the logs, traces and metrics that the platform's agents sent. It has three parts:

| Part | What |
|---|---|
| `api/swagger.json` | The API description (Swagger 2.0): the contract. telemetry-ingest's tests check that the server answers every operation in it and that every logs/traces/metrics route it serves is in it. |
| `client/`, `models/` | The Go client, **generated** from the spec by go-swagger (`make generate`) and committed. Do not edit by hand. |
| `fleet/` | A thin wrapper: `fleet.New(hub, project, readKey)` gives `.Logs`, `.Traces`, `.Metrics`, plus `Since`, `Match`, `Explain`. |
| `cmd/fleetctl` | A command-line tool on top, also used by the Claude Code skill `fleet-telemetry`. |

The node-agent stores nothing: it pushes to the hub, so this client reads from the hub, for every node and cluster of a project.

## The key

Every call takes one project's **read key** (`X-API-Key`): it reads that project only and cannot change anything. `fleetctl` reads it from
the file named by `FLEET_READ_KEY_FILE` (or from `FLEET_READ_KEY`), never from a flag, and never prints it.

## fleetctl

```sh
go install github.com/sojoner/kro-lab/platform-mvp/telemetry-client/cmd/fleetctl@latest
export FLEET_HUB=https://hub.example.com FLEET_PROJECT=shop FLEET_READ_KEY_FILE=~/.config/fleet/shop.key

fleetctl whoami
fleetctl logs --service /k8s/shop/checkout --severity error --since 1h
fleetctl logs services
fleetctl traces --service /k8s/shop/checkout --status error --min-duration 500ms
fleetctl trace 4bf92f3577b34da6a3ce929d0e0e4736
fleetctl metrics
fleetctl series container_resources_cpu_usage_seconds_total --match container_id=/k8s/shop/checkout-7d9f8-x2bq4/app
fleetctl query container_resources_cpu_usage_seconds_total --match container_id=... --step 60 --rate --since 3h
```

`-o json` prints compact JSON (one array); `--fields a,b` or `--fields all` chooses what is in it. Exit codes: 0, 1 when the hub refused
(its message on stderr), 2 for a wrong command line or configuration.

## The Go API

See `fleet/example_test.go`. The generated services have the go-swagger shape: `c.Logs.SearchLogs(logs.NewSearchLogsParams()...)`.
Metric label matchers are a repeated `match=label=value` (`fleet.Match`).

## Changing the API

1. Change the handler in telemetry-ingest and `api/swagger.json`.
2. `make generate` here, and commit `client/` and `models/` (`make check-generated` fails when they differ from the spec).
3. In telemetry-ingest: `go test ./server -run APISpec -update-fixtures` rewrites `testdata/` from the real handlers; this module's tests
   decode them.
