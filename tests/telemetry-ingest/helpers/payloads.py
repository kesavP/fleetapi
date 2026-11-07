#!/usr/bin/env python3
"""Builds the request bodies the Coroot agents send, with the standard library
only, so the Chainsaw checks need nothing installed beyond python3 and curl.

  payloads.py metrics <run> <role>      Prometheus remote-write body (snappy block), on stdout;
                                        role is "primary" (a counter that resets) or "other"
  payloads.py profile <run>             pprof profile, on stdout
  payloads.py traces <run>              OTLP/JSON traces (hex ids, as OTel SDKs send them)
  payloads.py logs <run>                OTLP/JSON logs
  payloads.py traceid <run>             the trace id the traces payload uses
"""
import hashlib
import json
import struct
import sys
import time


def varint(n):
    out = b""
    while True:
        b = n & 0x7F
        n >>= 7
        if n:
            out += bytes([b | 0x80])
        else:
            return out + bytes([b])


def field(num, wire, payload):
    return varint(num << 3 | wire) + payload


def ld(num, data):  # length-delimited field
    return field(num, 2, varint(len(data)) + data)


def snappy_block(data):
    """A valid snappy block that stores everything as literals: no compression,
    but every decoder accepts it, and it needs no snappy library."""
    out = varint(len(data))
    for i in range(0, len(data), 60):
        chunk = data[i:i + 60]
        out += bytes([(len(chunk) - 1) << 2]) + chunk
    return out


def label(name, value):
    return ld(1, name.encode()) + ld(2, value.encode())


def sample(value, ts_ms):
    return field(1, 1, struct.pack("<d", value)) + field(2, 0, varint(ts_ms))


def series(labels, samples):
    body = b"".join(ld(1, label(n, v)) for n, v in labels)
    body += b"".join(ld(2, sample(v, t)) for v, t in samples)
    return body


def trace_id(run):
    return hashlib.md5(run.encode()).hexdigest()  # 32 hex characters


def metrics(run, role):
    now_ms = int(time.time()) * 1000
    ts = [now_ms - 60000 + i * 15000 for i in range(5)]
    if role == "primary":
        # +30 per 15 s, then the counter restarts: 160 -> 10.
        vals = [100, 130, 160, 10, 40]
        container = "/k8s/shop/cart/app"
    else:
        vals = [7, 7, 7, 7, 7]
        container = "/k8s/eu-only/worker/app"
    labels = [("__name__", "chainsaw_ti_connects_total"), ("container_id", container), ("run", run)]
    return snappy_block(ld(1, series(labels, list(zip(vals, ts)))))


def profile(run):
    strings = ["", "cpu", "nanoseconds", "main.hot", "main.main"]
    p = b""
    p += ld(1, field(1, 0, varint(1)) + field(2, 0, varint(2)))  # sample_type: cpu/nanoseconds
    p += ld(2, ld(1, varint(1) + varint(2)) + ld(2, varint(900)))  # stack hot<-main, 900
    p += ld(2, ld(1, varint(2)) + ld(2, varint(100)))  # stack main, 100
    p += ld(4, field(1, 0, varint(1)) + ld(4, field(1, 0, varint(1))))  # location 1 -> function 1
    p += ld(4, field(1, 0, varint(2)) + ld(4, field(1, 0, varint(2))))  # location 2 -> function 2
    p += ld(5, field(1, 0, varint(1)) + field(2, 0, varint(3)))  # function 1 = main.hot
    p += ld(5, field(1, 0, varint(2)) + field(2, 0, varint(4)))  # function 2 = main.main
    for s in strings:
        p += ld(6, s.encode())
    p += field(9, 0, varint(int((time.time() - 30) * 1e9)))  # time_nanos
    p += field(10, 0, varint(10 * 10**9))  # duration_nanos
    return p


def traces(run):
    start = int((time.time() - 20) * 1e9)
    tid = trace_id(run)

    def span(span_id, parent, name, code, dur_ms):
        return {
            "traceId": tid, "spanId": span_id, "parentSpanId": parent, "name": name, "kind": 2,
            "startTimeUnixNano": str(start), "endTimeUnixNano": str(start + dur_ms * 1_000_000),
            "status": {"code": code},
            "attributes": [{"key": "run", "value": {"stringValue": run}}],
        }

    return {"resourceSpans": [{
        "resource": {"attributes": [{"key": "service.name", "value": {"stringValue": "chainsaw-cart"}}]},
        "scopeSpans": [{"spans": [
            span("0101010101010101", "", "GET /cart", 2, 80),
            span("0202020202020202", "0101010101010101", "SELECT carts", 1, 5),
        ]}],
    }]}


def logs(run):
    now = int((time.time() - 10) * 1e9)
    tid = trace_id(run)
    rec = lambda sev, body: {
        "timeUnixNano": str(now), "severityText": sev, "traceId": tid,
        "body": {"stringValue": body},
    }
    return {"resourceLogs": [{
        "resource": {"attributes": [{"key": "service.name", "value": {"stringValue": "chainsaw-cart"}}]},
        "scopeLogs": [{"logRecords": [
            rec("error", "Connection REFUSED by redis run=" + run),
            rec("info", "request finished run=" + run),
        ]}],
    }]}


def main():
    cmd = sys.argv[1]
    run = sys.argv[2] if len(sys.argv) > 2 else ""
    out = sys.stdout.buffer
    if cmd == "metrics":
        if sys.argv[3] not in ("primary", "other"):
            sys.exit('role must be "primary" or "other"')
        out.write(metrics(run, sys.argv[3]))
    elif cmd == "profile":
        out.write(profile(run))
    elif cmd == "traces":
        out.write(json.dumps(traces(run)).encode())
    elif cmd == "logs":
        out.write(json.dumps(logs(run)).encode())
    elif cmd == "traceid":
        out.write(trace_id(run).encode())
    else:
        sys.exit("unknown command " + cmd)


main()
