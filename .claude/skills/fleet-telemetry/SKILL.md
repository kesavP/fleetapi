---
name: fleet-telemetry
description: Query the fleet platform's logs, traces and metrics from the hub with fleetctl. Use when the user asks what an application logged, why requests failed or were slow, what a trace shows, which services are sending data, or how a metric behaved over time.
allowed-tools: Bash(fleetctl:*)
---

# Fleet telemetry

The platform's node-agents push logs, traces and metrics to the hub; `fleetctl` reads them back for one project with a **read key**,
which can read and cannot change anything. Use only `fleetctl` for this; it is read only.

## 1. Check the setup first

Run `fleetctl whoami`. If it fails, tell the user which setting is missing, from its message, and how to set it:

- `FLEET_HUB` – the hub's URL, e.g. `https://hub.example.com`
- `FLEET_PROJECT` – the project
- `FLEET_READ_KEY_FILE` – a file holding the project's read key

Never ask for the key in the conversation, and never put a key in a command, print it or echo it. If `fleetctl` is not installed:
`go install github.com/sojoner/kro-lab/platform-mvp/telemetry-client/cmd/fleetctl@latest`.

## 2. Find the application

Application ids look like `/k8s/<namespace>/<workload>` (e.g. `/k8s/shop/checkout`). When the user names one loosely ("checkout"), run
`fleetctl logs services -o json` (or `traces services`) and pick the match; ask if several fit.

## 3. Narrow before widening

Start with a service, a severity or status, `--since 1h` and `--limit 50`. Widen the time or drop a filter only when nothing comes back.
Always use `-o json`.

| The question | Commands, in order |
|---|---|
| What went wrong / errors | `fleetctl logs --service S --severity error --since 1h -o json`, then `fleetctl traces --service S --status error -o json`, then one whole trace: `fleetctl trace <trace_id> -o json` |
| Slow requests | `fleetctl traces --service S --min-duration 500ms --root -o json`, then `fleetctl trace <the slowest trace_id> -o json` |
| A log line's request | `fleetctl logs --trace <trace_id> -o json` and `fleetctl trace <trace_id> -o json` |
| A metric | `fleetctl metrics -o json`, `fleetctl series <name> -o json`, then `fleetctl query <name> --match label=value --step 60 -o json` (add `--rate` for counters, names ending in `_total`) |

More flags, the JSON fields and the limits: [reference.md](reference.md).

## 4. Answer

- Times in UTC, as absolute times (e.g. `10:42:07Z`), not "a while ago".
- Counts and the services involved; quote at most five log lines, shortened.
- Say what you did not look at (e.g. "I only looked at the last hour" or "metrics not checked").
- If nothing is found, say so and what was searched. Do not guess a cause the data does not show.
- If the user asks to change something (restart, scale, delete), say that this skill only reads, and do not run anything for it.
