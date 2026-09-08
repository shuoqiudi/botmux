# Embedded Adapter delivery verification

Issue: [it_manage #1234](https://github.com/shuoqiudi/it_manage/issues/1234).
Contract: [it_manage #1233](https://github.com/shuoqiudi/it_manage/issues/1233).
Reviewed baseline: `6b3522d00e00f7b7172dda241d1b0381e5ecbc2b`.
Validated on 2026-09-08 using Go 1.26.8 in the existing Go container image,
`GOMAXPROCS=2`, and a disposable Redis 7 instance with AOF enabled.

## Verification

| Requirement | Evidence |
| --- | --- |
| Embedded Route target without external backend configuration | `TestGatewayAdapterRouteAPI`; setup/edit selector, backend-field hiding, EN/RU browser checks |
| Bot + Chat resolution and generic v1 argv | Existing Gateway inbound suite plus `TestGatewayAdapterRoundTrip`; unknown module/action and quoted arguments preserved |
| Jenkins secret is not readable in management or result surfaces | `TestGatewayAdapterHealthAndSecretConfig`, round-trip/recovery redaction assertions, existing Gateway security suite |
| Acknowledgment before execution and final result in original conversation | Round-trip fake Telegram/Jenkins flow; `TestGatewayAdapterRestartAndRouteMigration` also verifies original topic |
| Duplicate Updates, restart and reclaim do not retrigger Jenkins | Duplicate assertions, ambiguous-trigger restart, concurrent reclaim, interrupted in-flight dispatch with SQLite reopening |
| Unrelated messages ignored and invalid generic syntax rejected | `TestGatewayAdapterIgnoresUnrelatedMessages` |
| Bounded rejection, timeout, build/result and network outcomes | `TestGatewayAdapterBoundedOutcomes`, including expired queue + failed recovery regression |
| Separate Telegram, Redis and Jenkins/Adapter health | Health integration test, route metrics and browser checks |
| Same application/image/deployment; no new runtime environment variables | Main process wiring; Compose overlay renders exactly `redis` and `botmux`; mounted JSON secret is selected by CLI flag |

Commands completed successfully:

```sh
CGO_ENABLED=0 go build -p 1 ./...
go test -race -p 1 ./tests -run '^TestGatewayAdapter' -count=1
go test -p 1 ./internal/gateway -run '^TestRedisQueueDurableReclaimAndDLQMove$' \
  -count=1 -gateway-redis-test-addr=127.0.0.1:6379
go test -p 1 -parallel 1 ./... -count=1
node take-screenshots.mjs --gateway-only
docker compose -f docker-compose.yml -f docker-compose.adapter.yml config --services
```

The Adapter race suite passed in 60.010s. The complete `tests` package passed in
38.477s; all other packages built or passed. The repository's pre-existing skipped
bridge-helper test remains unchanged. The opt-in Redis package test was run
explicitly; Adapter and Gateway inbound tests used the live disposable Redis.
Screenshots contain synthetic identifiers and were visually inspected.

## Standards

No remaining actionable Standards findings. The initial review found that an
expired Jenkins queue item reset consecutive recovery failures before fallback
lookup. A regression first reproduced the incorrect queue-timeout outcome; the
fix resets failures only after the entire observation succeeds. The staged fix
was independently re-reviewed and the regression and race suite passed.

The review confirmed durable intent before submission, lease ownership checks,
no credential-bearing redirects, original conversation pinning, and omission of
Jenkins credentials from operational state and management serialization.

## Spec

No remaining Spec findings or scope creep. Independent review verified the
embedded target, v1 generic conversion, mounted secrets, reply order, conversation
pinning, restart/reclaim behavior, bounded outcomes, separate health and unchanged
service topology. Added interrupted-dispatch coverage confirms recovery after a
lost in-flight response using retained intent, lease reclaim and reopened SQLite.
This is a worker-interruption simulation, not an OS kill test or a live Jenkins
business execution. Real production deployment was not part of this verification.

Review totals: Standards 1 initial finding fixed, 0 remaining; Spec 0 findings.
