# fleetctl reference

Common flags: `--hub`, `--project`, `--key-file` (instead of the environment), `-o table|json`, `--fields a,b|all` (JSON only),
`--since 15m|2h|24h` (default 1h) or `--from`/`--to` (RFC 3339 or unix seconds), `--limit N` (1–1000, default 100).

Exit codes: 0 ok; 1 the hub refused or the call failed (`fleetctl: the hub answered <status>: <message>` on stderr); 2 a wrong command line
or a missing setting.

## Commands

| Command | Flags |
|---|---|
| `whoami` | – |
| `logs` | `--service`, `--severity` (error, warn, info…), `--grep TEXT` (case does not matter), `--trace ID` |
| `logs services` | `--since` |
| `traces` | `--service`, `--name`, `--status ok\|error\|unset`, `--min-duration 500ms`, `--root` |
| `traces services` | `--since` |
| `trace <id>` | 32 hex characters; prints the spans as a tree (JSON: each span has `depth` and `parent_span_id`) |
| `metrics` | – |
| `series <metric>` | `--match label=value` (repeatable), `--limit` |
| `query <metric>` | `--match label=value` (repeatable), `--step SECONDS` (0–86400), `--rate` (needs `--step`), `--from-zero` |

## JSON fields

- **Logs** (default `time, service, severity, body`; body cut to 300 characters unless `--fields` is given): also `trace_id`, `span_id`,
  `attributes` (log attributes), `resource` (e.g. `container.id`).
- **Spans** (default `time, trace_id, span_id, name, service, duration_ms, status`): also `parent_span_id`, `kind`, `status_message`,
  `attributes`, `resource`. Status is `OK`, `ERROR` or `UNSET`.
- **query**: `{"series": [{"labels": {...}, "points": [[unix_ms, value], ...]}], "truncated": bool}`; `truncated` means narrow with
  `--match` or a shorter range.
- **series**: a list of label sets.

## Where the data comes from

- Logs: container logs the node-agent read, secrets removed before they left the node. Process lineage records (OOM kills, abnormal
  exits, new binaries) are logs with `attributes."log.source" = "process"`.
- Traces: spans the node-agent built from eBPF (HTTP, databases…) and spans applications sent over OTLP.
- Metrics: the node-agent's container and node metrics, e.g. `container_resources_cpu_usage_seconds_total`,
  `container_resources_memory_rss_bytes`, `container_net_tcp_failed_connects_total`, `container_http_requests_total`. Container series carry
  `container_id` (e.g. `/k8s/shop/checkout-7d9f8-x2bq4/app`).

## Limits

At most 1000 logs or spans per call; a trace at most 5000 spans; metric series and points are capped by the hub (`truncated`). The read
key reads its own project only.
