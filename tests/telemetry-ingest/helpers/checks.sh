#!/usr/bin/env bash
# Usage: checks.sh <check>. Each check is one Chainsaw step. Checks that read
# data use the run id that ingest_all stored, so they only look at what this
# run wrote, never at rows left by earlier runs. See lib.sh for how the service
# is reached and for TI_PROJECT / TI_OTHER_PROJECT.
HERE="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
# shellcheck source=lib.sh
. "$HERE/lib.sh"
connect

PRIMARY="${TI_PROJECT:-us}"
OTHER="${TI_OTHER_PROJECT-eu}"   # empty = a single-project deployment
METRIC=chainsaw_ti_connects_total
SERVICE=chainsaw-cart
CT='Content-Type: application/x-protobuf'

ingest_all() {
  local run tmp pk ok
  run="$(date +%s)-$RANDOM"
  echo "$run" >"$STATE"
  tmp="$(mktemp -d)"
  trap 'rm -rf "$tmp"' RETURN
  pk="$(key "$PRIMARY" ingest)"

  python3 "$HERE/payloads.py" metrics "$run" primary >"$tmp/m-primary.bin"
  expect_status 204 "remote-write for $PRIMARY" -X POST -H "X-API-Key: $pk" -H 'Content-Encoding: snappy' -H "$CT" --data-binary @"$tmp/m-primary.bin" "$URL/v1/metrics"
  if [ -n "$OTHER" ]; then
    ok="$(key "$OTHER" ingest)"
    python3 "$HERE/payloads.py" metrics "$run" other >"$tmp/m-other.bin"
    expect_status 204 "remote-write for $OTHER" -X POST -H "X-API-Key: $ok" -H 'Content-Encoding: snappy' -H "$CT" --data-binary @"$tmp/m-other.bin" "$URL/v1/metrics"
  fi

  python3 "$HERE/payloads.py" traces "$run" >"$tmp/traces.json"
  expect_status 200 "OTLP/JSON traces with hex ids" -X POST -H "X-API-Key: $pk" -H 'Content-Type: application/json' --data-binary @"$tmp/traces.json" "$URL/v1/traces"
  python3 "$HERE/payloads.py" logs "$run" >"$tmp/logs.json"
  expect_status 200 "OTLP/JSON logs" -X POST -H "X-API-Key: $pk" -H 'Content-Type: application/json' --data-binary @"$tmp/logs.json" "$URL/v1/logs"

  python3 "$HERE/payloads.py" profile "$run" >"$tmp/profile.pb"
  expect_status 200 "pprof profile" -X POST -H "X-API-Key: $pk" -H 'Content-Type: application/octet-stream' --data-binary @"$tmp/profile.pb" "$URL/v1/profiles?service.name=$SERVICE&pod=cart-1"
}

check_metrics() {
  local run read series raw rate
  run="$(run_id)"
  read="$(key "$PRIMARY" read)"
  wait_for "the run's series is listed" 30 \
    bash -c "$(declare -f get); URL='$URL'; get $PRIMARY '$read' 'metrics/series?name=$METRIC&match.run=$run' | jq -e '.data | length == 1'"

  series="$(get "$PRIMARY" "$read" "metrics/series?name=$METRIC&match.run=$run")"
  echo "$series" | jq -e '.data[0].labels.container_id == "/k8s/shop/cart/app"' >/dev/null || fail "series labels: $series"
  get "$PRIMARY" "$read" "metrics" | jq -e --arg m "$METRIC" '.data | index($m) != null' >/dev/null || fail "$METRIC is not in the metric list"

  wait_for "all five raw samples are readable" 30 \
    bash -c "$(declare -f get); URL='$URL'; get $PRIMARY '$read' 'metrics/query_range?name=$METRIC&match.run=$run' | jq -e '.data.series[0].points | length == 5'"
  raw="$(get "$PRIMARY" "$read" "metrics/query_range?name=$METRIC&match.run=$run")"
  echo "$raw" | jq -e '.data.series[0].points[0].v == 100 and .data.series[0].points[4].v == 40' >/dev/null || fail "raw values: $raw"
  echo "$raw" | jq -e '(.data.series[0].points | map(.t)) as $t | all(range(1; ($t | length)); $t[.] - $t[. - 1] == 15000)' >/dev/null || fail "sample times are not 15 s apart: $raw"
  echo "ok: raw samples keep their values and millisecond times"

  rate="$(get "$PRIMARY" "$read" "metrics/query_range?name=$METRIC&match.run=$run&step=15&rate=true")"
  echo "$rate" | jq -e '[.data.series[0].points[].v] | any(. > 1.99 and . < 2.01)' >/dev/null || fail "no rate of 2/s (+30 per 15 s): $rate"
  echo "$rate" | jq -e '[.data.series[0].points[].v] | any(. > 0.66 and . < 0.67)' >/dev/null || fail "the counter reset was not handled (expected 10/15 per s): $rate"
  echo "$rate" | jq -e '[.data.series[0].points[].v] | all(. >= 0)' >/dev/null || fail "a negative rate leaked through: $rate"
  echo "ok: rate=true gives 2/s and treats the drop as a counter reset"
}

check_traces() {
  local run read tid
  run="$(run_id)"
  read="$(key "$PRIMARY" read)"
  tid="$(python3 "$HERE/payloads.py" traceid "$run")"
  wait_for "the trace is stored" 30 \
    bash -c "$(declare -f get); URL='$URL'; get $PRIMARY '$read' 'traces/$tid' | jq -e '.data | length == 2'"

  get "$PRIMARY" "$read" "traces/$tid" | jq -e --arg tid "$tid" \
    'all(.data[]; .trace_id == $tid and .service == "chainsaw-cart") and (.data | map(.span_id) | sort == ["0101010101010101","0202020202020202"])' >/dev/null \
    || fail "the hex trace and span ids were not stored as sent"
  echo "ok: hex ids from OTLP/JSON are stored intact"

  get "$PRIMARY" "$read" "traces?service=$SERVICE&status=error&root=true&min_duration_ms=50" | jq -e --arg tid "$tid" \
    'any(.data[]; .trace_id == $tid and .name == "GET /cart" and .status_code == "ERROR")' >/dev/null \
    || fail "the error root span was not found by status, root and duration"
  get "$PRIMARY" "$read" "traces/services" | jq -e --arg s "$SERVICE" '.data | index($s) != null' >/dev/null || fail "$SERVICE is not in the trace services"
  echo "ok: spans are searchable by service, status, root and duration"
}

check_logs() {
  local run read tid
  run="$(run_id)"
  read="$(key "$PRIMARY" read)"
  tid="$(python3 "$HERE/payloads.py" traceid "$run")"
  wait_for "the run's logs are stored" 30 \
    bash -c "$(declare -f get); URL='$URL'; get $PRIMARY '$read' 'logs?service=$SERVICE&q=run=$run' | jq -e '.data | length == 2'"

  get "$PRIMARY" "$read" "logs?service=$SERVICE&severity=error&q=refused%20by%20redis" | jq -e --arg tid "$tid" --arg run "$run" \
    'any(.data[]; .trace_id == $tid and (.body | contains($run)) and .severity == "ERROR")' >/dev/null \
    || fail "case-insensitive body search with a severity filter found nothing"
  get "$PRIMARY" "$read" "logs?trace_id=$tid" | jq -e '.data | length >= 2' >/dev/null || fail "logs are not linked to the trace"
  echo "ok: logs are searchable by severity, text and trace id"
}

check_profiles() {
  local read
  read="$(key "$PRIMARY" read)"
  wait_for "the profile service is listed" 30 \
    bash -c "$(declare -f get); URL='$URL'; get $PRIMARY '$read' profiles | jq -e --arg s '$SERVICE' 'any(.data[]; .service == \$s and .type == \"cpu:nanoseconds\")'"

  get "$PRIMARY" "$read" "profiles/top?service=$SERVICE&type=cpu:nanoseconds" | jq -e \
    '.data[0].frames == ["main.hot","main.main"] and .data[0].value >= 900' >/dev/null \
    || fail "the hottest stack is not main.hot <- main.main with value >= 900"
  get "$PRIMARY" "$read" "profiles/top?service=$SERVICE&type=cpu:nanoseconds&format=folded" | grep -q '^main.main;main.hot [0-9][0-9]*$' \
    || fail "the folded output does not list the stack root first"
  echo "ok: profiles are stored once per stack and served as top stacks and folded text"
}

check_auth() {
  local in rd orp
  in="$(key "$PRIMARY" ingest)"
  rd="$(key "$PRIMARY" read)"
  local P="$URL/api/v1/projects"
  expect_status 401 "ingest with no key" -X POST -H 'Content-Encoding: snappy' --data-binary x "$URL/v1/metrics"
  expect_status 401 "ingest with an unknown key" -X POST -H 'X-API-Key: not-a-key-not-a-key' -H 'Content-Encoding: snappy' --data-binary x "$URL/v1/metrics"
  expect_status 401 "ingest with a read key" -X POST -H "X-API-Key: $rd" -H 'Content-Encoding: snappy' --data-binary x "$URL/v1/metrics"
  expect_status 401 "OTLP traces with no key" -X POST -H 'Content-Type: application/json' --data-binary '{}' "$URL/v1/traces"
  expect_status 401 "profiles with no key" -X POST --data-binary x "$URL/v1/profiles?service.name=x"
  expect_status 401 "query with no key" "$P/$PRIMARY/metrics"
  expect_status 403 "query with an ingest key" -H "X-API-Key: $in" "$P/$PRIMARY/metrics"
  if [ -n "$OTHER" ]; then
    orp="$(key "$OTHER" read)"
    expect_status 403 "query another project's path" -H "X-API-Key: $orp" "$P/$PRIMARY/metrics"
  else
    echo "skipped: query another project's path (this deployment has a single project)"
  fi
  expect_status 403 "query a project that does not exist" -H "X-API-Key: $rd" "$P/nosuch/metrics"
  expect_status 200 "query the key's own project" -H "X-API-Key: $rd" "$P/$PRIMARY/metrics"
  expect_status 200 "bearer token works like X-API-Key" -H "Authorization: Bearer $rd" "$P/$PRIMARY/metrics"
  expect_status 400 "a malformed query is a 400, not a 500" -H "X-API-Key: $rd" "$P/$PRIMARY/metrics/query_range?name=x&rate=true"
}

check_isolation() {
  if [ -z "$OTHER" ]; then
    echo "skipped: project isolation needs a second project (TI_OTHER_PROJECT); this deployment has a single project"
    return 0
  fi
  local run prd ord tid
  run="$(run_id)"
  prd="$(key "$PRIMARY" read)"
  ord="$(key "$OTHER" read)"
  tid="$(python3 "$HERE/payloads.py" traceid "$run")"

  wait_for "$OTHER's own series is readable" 30 \
    bash -c "$(declare -f get); URL='$URL'; get $OTHER '$ord' 'metrics/query_range?name=$METRIC&match.run=$run' | jq -e '.data.series | length == 1'"
  get "$OTHER" "$ord" "metrics/query_range?name=$METRIC&match.run=$run" | jq -e \
    '.data.series[0].labels.container_id == "/k8s/eu-only/worker/app" and ([.data.series[0].points[].v] | all(. == 7))' >/dev/null \
    || fail "$OTHER read someone else's samples"
  get "$PRIMARY" "$prd" "metrics/query_range?name=$METRIC&match.run=$run" | jq -e \
    '.data.series | length == 1 and .[0].labels.container_id == "/k8s/shop/cart/app"' >/dev/null \
    || fail "$PRIMARY read someone else's samples"
  echo "ok: the same metric and labels in two projects stay separate"

  get "$OTHER" "$ord" "traces/$tid" | jq -e '.data | length == 0' >/dev/null || fail "$OTHER can read the trace that was sent to $PRIMARY"
  get "$OTHER" "$ord" "traces?service=$SERVICE" | jq -e --arg tid "$tid" 'all(.data[]; .trace_id != $tid)' >/dev/null || fail "$OTHER's trace search shows $PRIMARY's span"
  get "$OTHER" "$ord" "logs?service=$SERVICE&q=run=$run" | jq -e '.data | length == 0' >/dev/null || fail "$OTHER can read $PRIMARY's logs"
  get "$OTHER" "$ord" "profiles" | jq -e --arg s "$SERVICE" 'all(.data[]; .service != $s)' >/dev/null || fail "$OTHER can see $PRIMARY's profile services"
  echo "ok: traces, logs and profiles sent to $PRIMARY are invisible to $OTHER"
}

# The real node-agent, not test payloads: the DaemonSet runs on every node and
# what it sends is stored and readable. Needs the agent installed in the cluster
# (TI_AGENT_NAMESPACE, default telemetry-agent). Metrics are required; traces,
# logs and profiles depend on what the cluster runs, so they are only reported.
check_node_agent() {
  local ns="${TI_AGENT_NAMESPACE:-telemetry-agent}" read desired ready cpu="container_resources_cpu_usage_seconds_total" nodes dropped
  read="$(key "$PRIMARY" read)"

  desired="$(kc -n "$ns" get daemonset node-agent -o jsonpath='{.status.desiredNumberScheduled}')"
  ready="$(kc -n "$ns" get daemonset node-agent -o jsonpath='{.status.numberReady}')"
  [ "${desired:-0}" -ge 1 ] || fail "daemonset node-agent in $ns schedules no pods"
  [ "${ready:-0}" = "$desired" ] || fail "daemonset node-agent: $ready of $desired pods ready"
  echo "ok: node-agent runs on $ready of $desired nodes"

  wait_for "the agents' container metrics are stored ($cpu)" 180 \
    bash -c "$(declare -f get); URL='$URL'; get $PRIMARY '$read' metrics | jq -e --arg m '$cpu' '.data | index(\$m) != null'"

  get "$PRIMARY" "$read" "metrics/series?name=$cpu&limit=500" \
    | jq -e '[.data[] | select((.labels.container_id // "") | startswith("/k8s/"))] | length >= 1' >/dev/null \
    || fail "no $cpu series has a /k8s/<namespace>/<pod>/<container> container_id: the agent is not attributing containers to pods"
  echo "ok: containers are attributed to pods (/k8s/<namespace>/<pod>/<container>)"

  nodes="$(get "$PRIMARY" "$read" "metrics/series?name=$cpu&limit=500" | jq '[.data[].labels.machine_id // empty] | unique | length')"
  [ "$nodes" -ge "$desired" ] || fail "only $nodes of $desired nodes are reporting container metrics"
  echo "ok: all $desired nodes are reporting"

  # Fresh: the newest sample is recent, so this is live data, not a leftover.
  get "$PRIMARY" "$read" "metrics/query_range?name=$cpu&from=$(( $(date +%s) - 300 ))&step=15&rate=true" \
    | jq -e '[.data.series[].points[] | select(.t > ((now - 300) * 1000))] | length > 0' >/dev/null \
    || fail "no $cpu samples in the last 5 minutes: the agents have stopped sending"
  echo "ok: samples from the last 5 minutes"

  dropped="$(curl -s "$URL/metrics" | awk '/^telemetry_rows_dropped_total\{signal="metrics"\}/ {print $2}')"
  [ "${dropped:-0}" = 0 ] || fail "the service dropped $dropped metric rows (ClickHouse was unreachable or too slow)"
  echo "ok: no metric rows dropped"

  echo "info: rows written by signal:"
  curl -s "$URL/metrics" | awk '/^telemetry_rows_written_total\{signal="(metrics|traces|logs|profile_samples)"\}/ {print "   ", $1, $2}'
}

"${1:?usage: checks.sh <ingest_all|check_metrics|check_traces|check_logs|check_profiles|check_auth|check_isolation|check_node_agent>}"
