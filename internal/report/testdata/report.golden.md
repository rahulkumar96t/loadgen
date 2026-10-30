# Load test report

| | |
|---|---|
| Tool | loadgen test |
| Model | open |
| Target | `GET http://127.0.0.1:8080/` |
| Profile | 10s @ 101 rps |
| Started | 2026-01-02T03:04:05Z |
| Elapsed | 10.000 s |
| Max in-flight | 64 |
| Timeout | 10s |
| Expected status | 200-399 |

## Requests

| Scheduled | Sent | Unsent | Completed | OK | Failed | Error rate | Throughput |
|---:|---:|---:|---:|---:|---:|---:|---:|
| 1010 | 1000 | 10 | 1000 | 988 | 12 | 1.20% | 100.0 rps |

## Latency (ms)

| Measure | Count | Min | Mean | p50 | p90 | p99 | p99.9 | Max |
|---|---:|---:|---:|---:|---:|---:|---:|---:|
| latency (from intended send time) | 1000 | 1.000 | 500.500 | 500.171 | 900.202 | 990.380 | 999.293 | 1000.000 |
| service time | 1000 | 0.500 | 250.250 | 250.085 | 450.101 | 495.190 | 499.646 | 500.000 |
| send lag | 1000 | 0.500 | 250.250 | 250.085 | 450.101 | 495.190 | 499.646 | 500.000 |

## Status codes

| Code | Count |
|---|---:|
| 200 | 990 |
| 503 | 2 |

## Transport errors

| Class | Count |
|---|---:|
| connection_reset | 2 |
| timeout | 8 |

## Thresholds

| Fail if | Actual | Result |
|---|---:|---|
| `p99>250ms` | 990.380 ms | FAIL |
| `error_rate>5%` | 1.20% | PASS |
| `service.p50>1s` | 250.085 ms | PASS |

## Warnings

- 10 scheduled requests were never sent because max in-flight (64) was reached; the target could not keep up with the offered rate
- send lag p99 is 495.2 ms: requests left the generator late (in-flight limit reached or generator CPU-bound); latency is still measured from the intended time
