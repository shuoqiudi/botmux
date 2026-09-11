# Configuration recovery acceptance (#26)

Validation uses the agreed boundary: the real Python script against HTTP servers
with independent on-disk SQLite databases and instance keys, fake Telegram HTTP,
fake backend HTTP, and the existing memory outbound / Redis inbound queue fixtures.
No production credentials or business chats are used.

The fixture in `tests/e2e_config_recovery_test.go` includes two Bots, a destination,
a conditional forwarding rule, a notification service and subscription, a business
route and one workload source shared by service and route permissions.

| Acceptance | Evidence |
| --- | --- |
| One-file migration and actual rules | `ConfigFullLostResponseRestartAndBehavior` exports the source, drops the response after commit, reopens the target database/server, retries through Python, then verifies all three rule types send exactly once to the expected fake Bot/chat. |
| Complete concurrent exports | `ConfigFullConcurrentExportAndActivity` changes six configuration collections in transactions while the script exports; each file equals the complete before or after state. Configuration edits produce only the expected readable name differences. |
| Runtime exclusion | The same test changes forwarding counters, health, credential activity, service last-received and route validation timestamps without changing exported bytes. The live rule test also re-exports after delivery activity. |
| Atomic persistence failures | `ConfigFullPersistenceFailuresAreAtomic` injects failures at Bot, destination, conditional rule, credential, subscription, business route, permission and receipt insertion. API exports remain empty, no Telegram requests occur, and normal retries commit the full snapshot with `replayed: False`. |
| Lost response and restart | `ConfigFullLostResponseRestartAndBehavior` verifies `replayed: True` after reopening SQLite and exact re-export, with no duplicate configuration or historical sends. |
| Actual target conflicts | A management API rule edit makes retry return `target_conflict`; re-export differs only in the edited description. Existing backup/route suites also reject populated different configurations. |
| Partial runtime | `ConfigFullGatewayFailureRetry` leaves configuration committed when both Bot loading and Gateway loading fail; installing the workers and correcting fake Telegram access allows retries. A stopped outbound worker is reported again. The bidirectional business-route test also verifies a missing inbound worker and retry after installing it. |
| Failed downloads/writes | Existing `ConfigDownloadPreservesPreviousFile` covers error responses, invalid JSON, truncated transfers and redirects. `ConfigLocalWriteFailurePreservesBackup` applies a Linux file-size limit to the real script; its complete previous file survives and the temporary file is removed. |
| Strict rejection and no effects | `ConfigFullInvalidFilesLeaveNoConfiguration` uses complete snapshots with unknown versions/fields, missing tokens, overflow/floating chat IDs, dangling references across classes and duplicate effective subscriptions. All fail through the script with empty target exports and no Telegram requests. |
| Bidirectional migration | `ConfigBusinessRouteBidirectionalAndSharedSource` uses the existing Redis inbound queue and fake backend to check backend authentication, direction flags, action permissions and shared source behavior. |
| Secret-safe diagnostics | Persistence faults carry a synthetic secret that must not appear in terminal output. Existing tests cover runtime logs, field errors, credential migration and authorization. The new component and digest summaries accept only fixed component names and typed values. |

## Execution

Run from the repository root with Go 1.26+, Python 3 and a dedicated local Redis
at `127.0.0.1:6379`, with AOF enabled. Redis must be a disposable test instance.
The inbound tests use isolated namespaces; the broader project suite exercises
persistent queue recovery as well.

```sh
redis-server --appendonly yes --dir /path/to/test-redis-data
# In a separate terminal:
go build -o /tmp/botmux .
go test ./tests -run '^TestE2E_Config' -count=1
go test ./... -count=1
```

The local run on 2026-09-11 used an isolated `golang:1.26-alpine` container
(Go 1.26.8 linux/amd64), Python 3.14.7 and Redis 8.8.0 with AOF enabled. The
configuration suite passed (54.547s). The new Gateway-status test was observed
failing before implementation; the lost-response test likewise exposed the missing
script replay summary before that summary was added. Individual recovery,
bidirectional, concurrent-export and local-write-failure checks passed.

The binary build (`go build -o /tmp/botmux .`) passed. The opt-in durable Redis
reclaim/DLQ test was also run against a second disposable AOF-enabled Redis on port
6380 and passed:

```sh
go test ./internal/gateway -run '^TestRedisQueueDurableReclaimAndDLQMove$' \
  -gateway-redis-test-addr 127.0.0.1:6380 -count=1
```

Separate Standards and Spec reviews against starting commit `ff45bfd` reported
zero findings on both axes.

The final `go test ./... -count=1 -json` run passed, with 411 passing test/subtest
results and no failures; the main `tests` package completed in 214.934s. The
opt-in Redis durability test is skipped by that default command and passed in the
separate explicit run above. Three existing checks remained skipped:

- `TestE2E_Bridge/B06_MappingPersistence_SyntheticIDSticky` is explicitly skipped
  in the repository because its synthetic-ID helper differs from production.
- `TestE2E_ServiceChatPickerBrowser` and `TestE2E_ServiceSubscriptionBrowser` require
  `BROWSER_NODE` and `PUPPETEER_MODULE`, which were not configured. This change does
  not modify browser UI.

All configuration-backup and relevant persistent recovery tests ran without skips.
