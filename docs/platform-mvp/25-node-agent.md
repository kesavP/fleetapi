# The node-agent (fleet fork)

The node-agent is Coroot's (`coroot-node-agent`), kept as a copy beside this repository in `../fleet-node-agent` with a few additions.
`fleet-node-agent/FLEET.md` lists them with the files they touch, so upstream changes can still be merged.

## Build and use it

```sh
make build-node-agent-image push-node-agent \
  DOCKER_REGISTRY=us-central1-docker.pkg.dev/fleetapi-513707/k8-pods-images IMAGE_TAG=v1     # NA_DIR=../fleet-node-agent
```

Then in the spoke values: `nodeAgent.image.repository` = `<registry>/node-agent` and `nodeAgent.image.tag` = the tag. The chart's
default is still `ghcr.io/coroot/coroot-node-agent`, which has none of the additions below. The build needs a few minutes and the
eBPF build tools the Dockerfile installs.

Go tests (Linux, Go 1.26, the systemd headers installed): `go test ./...` in the fork.

## What was added

**Host metrics the agent lacked**: load average, packet errors and drops per interface, and the connection-tracking table (entries,
limit, drops, failed inserts, early drops, invalid). A full table drops new connections with nothing logged by the application, so
usage against the limit is the figure to alert on.

**The agent's own health**, exported with the rest: the spool (bytes and files waiting, files thrown away because it was full), how
each send ended (accepted, failed and retried, refused by the hub), when the hub last accepted a batch, and the process's CPU, memory,
open files and start time (a restart loop is a start time that keeps changing). Without these, an agent that cannot reach the hub looks
the same as a quiet node. Nothing on the hub reads them yet: the detector that opens an incident for "this node's agent stopped
reporting, or has been losing data" is the next step, and `node_agent_remote_write_last_success_timestamp_seconds` and
`node_agent_spool_dropped_files_total` are what it would use.

**Log redaction**, on by default: see `FLEET.md` for what is removed and the two flags. The hub never sees what it removes, so it
cannot be turned back on later for data already stored.

## Behaviour change to know about

Redaction changes what is exported by default. A line that held `token=abc` now arrives as `token=[REDACTED]`. Anyone who searches
logs for such a value will find it gone; `--disable-log-redaction` restores the old behaviour.

## Still not covered

Inode use and filesystem health, TCP resets, disk queue depth, per-process and file-descriptor figures, log-volume anomaly detection,
error grouping by pattern, receiving OTLP from applications, signed configuration, and the Windows agent for any of the above (these
additions are Linux-only: they are in `_linux.go` files, or in code shared with the Windows build that was not rebuilt here).
