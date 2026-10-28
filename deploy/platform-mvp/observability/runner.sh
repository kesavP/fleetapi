#!/usr/bin/env bash
set -euo pipefail

BUNDLED_DIR="${BUNDLED_DIR:-/tests-bundled}"
CONFIGMAP_DIR="${CONFIGMAP_DIR:-/tests}"
LOKI_URL="${LOKI_URL:-http://loki.monitoring:3100}"
KUBECONFIG_HUB="${KUBECONFIG_HUB:-/kubeconfig/hub}"
KUBECONFIG_US="${KUBECONFIG_US:-/kubeconfig/us}"

echo "==> Starting chainsaw test run at $(date -u +%Y-%m-%dT%H:%M:%SZ)"

WORKDIR="$(mktemp -d)"
trap 'rm -rf "$WORKDIR"' EXIT
mkdir -p "${WORKDIR}/tests"

# Prefer bundled tests (image), fallback to ConfigMap
if [ -d "${BUNDLED_DIR}" ] && [ "$(ls -A "${BUNDLED_DIR}" 2>/dev/null)" ]; then
    echo "==> Using bundled tests from ${BUNDLED_DIR}"
    cp -r "${BUNDLED_DIR}"/* "${WORKDIR}/tests/"
elif [ -d "${CONFIGMAP_DIR}" ]; then
    echo "==> Using ConfigMap tests from ${CONFIGMAP_DIR}"
    for f in "${CONFIGMAP_DIR}"/*.yaml; do
        [ -f "$f" ] || continue
        base=$(basename "$f")
        name="${base%.yaml}"
        case "$name" in .chainsaw) continue ;; esac
        mkdir -p "${WORKDIR}/tests/${name}"
        sed -e '/^[[:space:]]*count:/d' "$f" > "${WORKDIR}/tests/${name}/chainsaw-test.yaml"
    done
else
    echo "ERROR: no test files found"
    exit 1
fi

cp "${KUBECONFIG_HUB}" "${WORKDIR}/kubeconfig-hub"
cp "${KUBECONFIG_US}"  "${WORKDIR}/kubeconfig-us-internal"

cat > "${WORKDIR}/tests/.chainsaw.yaml" <<EOF
apiVersion: chainsaw.kyverno.io/v1alpha1
kind: Configuration
metadata:
  name: platform-mvp
spec:
  timeouts:
    apply: 30s
    assert: 60s
    cleanup: 30s
    delete: 30s
    error: 10s
    exec: 5m
  failFast: false
  parallel: 1
  skipDelete: false
  reportFormat: JSON
  reportName: chainsaw-report
  fullName: true
EOF

cat >> "${WORKDIR}/tests/.chainsaw.yaml" <<EOF
  clusters:
    hub:
      kubeconfig: ${WORKDIR}/kubeconfig-hub
    us:
      kubeconfig: ${WORKDIR}/kubeconfig-us-internal
EOF

cd "${WORKDIR}/tests"
START_NS=$(date +%s%N)

if chainsaw test . \
    --config .chainsaw.yaml \
    --report-format JSON \
    --report-name chainsaw-report 2>&1; then
    RESULT="pass"
    EXIT_CODE=0
else
    RESULT="fail"
    EXIT_CODE=1
fi

END_NS=$(date +%s%N)
DURATION_MS=$(((END_NS - START_NS) / 1000000))

TIMESTAMP=$(date -u +%s%N)

if [ -f "chainsaw-report.json" ]; then
    REPORT_JSON=$(jq -c . "chainsaw-report.json" 2>/dev/null || echo '{}')
    REPORT_FILE="chainsaw-report.json"
elif [ -f "${WORKDIR}/tests/chainsaw-report.json" ]; then
    REPORT_JSON=$(jq -c . "${WORKDIR}/tests/chainsaw-report.json" 2>/dev/null || echo '{}')
    REPORT_FILE="${WORKDIR}/tests/chainsaw-report.json"
else
    REPORT_JSON='{"error":"report not found"}'
    REPORT_FILE=""
fi

# Push individual test case results first
RUN_ID=$(date -u +%s)
if [ -n "${REPORT_FILE}" ] && [ -f "${REPORT_FILE}" ]; then
    jq -c '.tests[]' "${REPORT_FILE}" 2>/dev/null | while read -r test; do
        TEST_NAME=$(echo "$test" | jq -r '.name // "unknown"')
        TEST_BASE=$(echo "$test" | jq -r '.basePath // .name // "unknown"')
        TEST_STATUS=$(echo "$test" | jq -r '.status // "skipped"')
        TEST_START=$(echo "$test" | jq -r '.startTime // ""')
        TEST_END=$(echo "$test" | jq -r '.endTime // ""')
        STEP_COUNT=$(echo "$test" | jq '.steps | length // 0')

        if [ -n "$TEST_START" ] && [ -n "$TEST_END" ]; then
            START_EPOCH=$(date -d "$TEST_START" +%s%N 2>/dev/null || echo 0)
            END_EPOCH=$(date -d "$TEST_END" +%s%N 2>/dev/null || echo 0)
            if [ "$START_EPOCH" != "0" ] && [ "$END_EPOCH" != "0" ]; then
                TEST_DURATION=$(((END_EPOCH - START_EPOCH) / 1000000))
            else
                TEST_DURATION=0
            fi
        else
            TEST_DURATION=0
        fi

        TEST_ENTRY=$(jq -n \
            --arg name "$TEST_NAME" \
            --arg base "$TEST_BASE" \
            --arg status "$TEST_STATUS" \
            --arg duration "$TEST_DURATION" \
            --arg steps "$STEP_COUNT" \
            --arg run_id "$RUN_ID" \
            '{
                kind: "test_case",
                test_name: $name,
                base_path: $base,
                status: $status,
                duration_ms: $duration | tonumber,
                steps: $steps | tonumber,
                run_id: $run_id
            }')

        curl -s -o /dev/null \
            -H "Content-Type: application/json" \
            -X POST "${LOKI_URL}/loki/api/v1/push" \
            -d "$(jq -n \
                --arg ts "${TIMESTAMP}" \
                --argjson entry "${TEST_ENTRY}" \
                '{
                    streams: [{
                        stream: { job: "chainsaw-runner", kind: "test_case" },
                        values: [[$ts, ($entry | tostring)]]
                    }]
                }')" 2>/dev/null || true
    done

    # Compute pass/fail counts from report
    COUNTS=$(jq -r '
        [.tests[].status] |
        {
            passed: (map(select(. == "passed")) | length),
            failed: (map(select(. == "failed")) | length),
            total: length
        }' "${REPORT_FILE}")
    PASSED_COUNT=$(echo "$COUNTS" | jq '.passed')
    FAILED_COUNT=$(echo "$COUNTS" | jq '.failed')
    TOTAL_COUNT=$(echo "$COUNTS" | jq '.total')
fi

# Push summary entry with counts
LOG_ENTRY=$(jq -n \
    --arg result "${RESULT}" \
    --arg duration "${DURATION_MS}" \
    --argjson report "${REPORT_JSON}" \
    --argjson passed "${PASSED_COUNT:-0}" \
    --argjson failed "${FAILED_COUNT:-0}" \
    --argjson total "${TOTAL_COUNT:-0}" \
    '{
        kind: "summary",
        result: $result,
        duration_ms: $duration | tonumber,
        passed: $passed,
        failed: $failed,
        total: $total,
        report: $report
    }')

curl -s -o /dev/null -w "%{http_code}" \
    -H "Content-Type: application/json" \
    -X POST "${LOKI_URL}/loki/api/v1/push" \
    -d "$(jq -n \
        --arg ts "${TIMESTAMP}" \
        --argjson entry "${LOG_ENTRY}" \
        '{
            streams: [{
                stream: { job: "chainsaw-runner", kind: "summary" },
                values: [[$ts, ($entry | tostring)]]
            }]
        }')" || true

echo "==> Chainsaw run: ${RESULT} (${DURATION_MS}ms)"
exit "${EXIT_CODE}"