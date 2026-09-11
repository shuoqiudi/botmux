# DNS migration execution report (#16)

Status: **production path switched; #16 remains incomplete pending a genuine DNS fault/recovery cycle**.

Cutover: 2026-09-11 **01:40:23 UTC**. Gateway and Keep are healthy; the old
Router is stopped. Ingress and the original scheduled DNS producer are resumed.

## Read-only preflight — 2026-09-11 UTC

- Gateway source baseline: `eb49168c8e156c68dbcdc111743ca16f59bd70fc`.
- Prerequisites Gateway #15 and Monitor #10 are closed.
- Live Router version-2 configuration contains one effective subscription:
  `v2:incident:domains:dns → test → it-monitor-alerts`. This does not establish
  the number of alerts in Keep.
- DNS route ID 2/revision 3 uses Bot account 1 and destination 1, enabled for
  outbound sending. Existing Monitor workload ID 1 is active, revision 2.
- Gateway snapshot: route 1 has 23 succeeded Deliveries; route 2 has 10 succeeded
  Deliveries, with no other states in this observation. This was rechecked
  at the cutover barrier, with Keep/ingress drained as recorded below.
- Running Gateway image tag:
  `it_manage_telegram_gateway:c2d947a27ae0e7cd868030ff997dc4030cbf5f0c`;
  image ID `sha256:81e65ac5bdd23468f24bcdf6b99c9bbc1aa05a58b621f2341a573664e8f7e3c2`.
  Its schema predates service permissions/subscriptions.
- Running Keep image ID:
  `sha256:eba54048dd0b72c887dd68219827d1c5332b8284df50efaf6c050460c6b8e87e`;
  Router `sha256:d99ffa337fa76022b1f1647b37b87e6dba42abb85bad671d708ea08fdbf91dd2`.
  These are the pre-cutover images. The Router was subsequently stopped.
- Actual Compose project/override labels and persistent mounts were inspected.
  The read-only checks changed no production state. Physical Chat/Bot IDs and
  private content are excluded from this report.

## Implementation and validation

The admin import API implements an atomic manifest, pinned workload/physical
recipients, durable same-ID receipt replay, conflict detection, audit, and no
notification side effect. See [executable procedure and schema](dns-subscription-migration.md).

Focused HTTP regressions cover repeat import, preserved target, no notification,
retry after cancellation, invalid-batch rollback, changed-manifest conflict,
workload authentication rejection, and lost response followed by database/server
restart. The first test failed with HTTP 405 before implementation, then passed.
Go build passed using the existing pinned smoke toolchain (host Go is unavailable).
Candidate `019494e907396b0c5c200b7fb89b901cea4464c9` passed the complete
`ee/telegram_gateway/test.sh` in a clean detached checkout:

- Seven packaging contracts and runtime secrets/privilege/health/SQLite/key restart.
- Pure-Go build, explicit isolated Redis durable reclaim/DLQ integration.
- Full serial `go test -p 1 -parallel 1 ./... -count=1`, including the legacy
  Business Route and embedded Adapter suites; the main E2E package took 152.532s.
- Monitor `tests/*.py` and `tests/*.sh` all passed serially using its existing
  virtual environment; the extended Keep image built successfully.

Standards review: 0 unresolved findings. Initial missing usage documentation was
addressed with the linked procedure and README entry. Spec review: 0 unresolved
code/procedure findings. A committed-response-loss/server-restart regression
addressed the initial interruption-coverage gap. Operational acceptance is
reported separately below, not inferred from these test results.

## Deployed state and import

| Component | Source / image |
| --- | --- |
| Gateway source | `019494e907396b0c5c200b7fb89b901cea4464c9` |
| Gateway image | `sha256:9ec584e6ba97d16fdc63edba47078688b3eaa843213bee5eba8987f257a579f4` |
| Monitor source | `bd29d562160b4ce4a1a55168da5174b654496889` |
| Keep image | `sha256:28245f649350c34f437da7f45e5595242cf0f28f5bfd55b077343cab98fae9cc` |

The existing Docker/Compose image-transfer workflow preserved runtime environment,
commands, networks/aliases, named volumes, credentials and Adapter configuration.
The Gateway image tag is
`it_manage_telegram_gateway_smoke_runtime:019494e907396b0c5c200b7fb89b901cea4464c9`;
it is the verified runtime Dockerfile target, without test tools.
Keep uses `it_monitor_keep:bd29d562160b4ce4a1a55168da5174b654496889`.

Live Bot identity was verified with Telegram `getMe` and matched to the existing
account; the manifest pins the original Bot/Chat. The existing mounted workload
credential hash resolves to workload 1, preserving the original source.

At 01:37:11 UTC import `dns-cutover-20260911` returned service **1** and
subscription **1**, destination **1**. A second identical import returned the
same receipt and IDs, with **zero notifications**. The before/after relationship
is `workload 1 + v2:incident:domains:dns → original Bot/Chat` in both models;
the subscription is now managed in BotMux. Workload 1's publish/query grants
were enabled and read back; an authenticated missing-notification query returned
404, without sending a readiness notification.

Keep started only after import/readiness. Provider/transport source comparison
and full configuration apply/verification passed. There is one enabled Workflow,
which submits service notifications directly. Old Router expansion/snapshots no
longer participate. The Monitor deployment source is a fixed Git archive at
`/opt/it_monitor/issue16-bd29d562`, with `docker-compose.yml` plus restricted
`compose.runtime.json`. Because the archive has no `.git`, apply reports
`uncommitted working tree`; its exact provenance is the recorded commit/archive
and image label. Other Monitor components retain their prior images.

## Live dedicated-target acceptance

The admin API revealed two existing, active, explicitly named acceptance
receivers: destination 1 (`acceptance-1238-chat`) and destination 2
(`acceptance-1238-alternate-bot-chat`). Both Bot identities were verified, and
physical Bot/Chat pairs are distinct. These established test receivers resolved
the earlier request for a second destination; no arbitrary discovered chat was used.

Four clearly labelled test events on
`v2:incident:issue16:subscriptions` traversed **real ingress → Keep → Gateway →
Telegram**. These are controlled acceptance events, not real DNS faults.

| Case | Gateway notification | Delivery | Telegram message | Attempts |
| --- | --- | --- | --- | --- |
| one notification to A and B | `d9643b89-957d-4d46-ac2c-ede6ff63f26a` | `0591cdd8-5189-489a-8a4c-466ebfeb6efd` | 56 | 1 |
| one notification to A and B | `d9643b89-957d-4d46-ac2c-ede6ff63f26a` | `2bc89b9d-c7e0-4510-94cf-3c5fd0fe317b` | 273 | 1 |
| cancel B then new notification to A only | `53f41943-eb29-401b-91f7-b04e49b6d99f` | `382400b3-1583-446a-900c-ec8f9249eb51` | 57 | 1 |
| fault to A before subscription change | `a3d11a32-92ba-48aa-806d-08fdffcf3a13` | `20e99333-f406-4d43-bfa9-aab9fabf1525` | 58 | 1 |
| recovery to B after subscription change | `22132e72-53b7-426c-9385-19ba8cf11ec6` | `7863b636-ecc3-46df-8455-917eeb7b3ada` | 274 | 1 |

All five Deliveries are `succeeded`. Each event produced exactly one Gateway
notification; each target had one send attempt. Keep execution results contain
the corresponding notification IDs:

| Ingress event | Keep execution | Result / UTC submission |
| --- | --- | --- |
| `e88e9d51-7359-4051-9648-4619b7a19214` | `fd4c1dce-5212-499f-8881-cab7e42b915e` | success / 2026-09-11T01:42:44.914656+00:00 |
| `495cc95a-d1c5-4e90-bdca-ccb2e3f03fe2` | `f1a5dc57-e4b3-42bd-a6ea-a9a1deaabddc` | success / 2026-09-11T01:42:46.938816+00:00 |
| `8e203b04-0d80-41cc-9c50-d3a8375d5040` | `37182744-65a5-4354-bf04-ae04700875b6` | success / 2026-09-11T01:42:47.952610+00:00 |
| `b1bf481a-9439-4916-9360-bbd6099f6372` | `53b0c710-556c-494e-95db-1f1119bf0558` | success / 2026-09-11T01:42:48.982994+00:00 |

The test subscriptions were removed in a `finally` cleanup and read back as
empty. The real DNS service's subscription list remained unchanged. No lasting
recipient expansion occurred. Test notifications and cancelled subscriptions
remain as audit history; no history was deleted.

The old Router remains stopped and old Business Route counts remain **23 and
10 succeeded**, matching the cutover barrier. Together with the four receipts,
five Telegram message IDs and one attempt per Delivery, this provides evidence
of no old-path or duplicate target sends during these acceptance events.

At 01:51 UTC, live route operations probes also reported healthy embedded
Adapter, Jenkins job availability, Telegram polling/authentication/destination,
Redis and inbound/outbound workers. The separate backend-health field is
not applicable to the embedded Adapter (and no health endpoint is configured
on the outbound-only Monitor route). No business command was launched by these
read-only probes.

A browser against the deployed Gateway loaded the service list, subscription
form and detail view with no page errors. No frontend code changed. Physical
recipient details remain in the administrator view; business callers maintain
neither targets, Chat IDs nor Bot Tokens.

## Backup and rollback validation

At the paused barrier ingress had no queued/delivering work, Keep had no
nonterminal execution and all 33 old Gateway Deliveries had succeeded. The 660
historical Keep errors (latest 2026-09-02) were retained and not replayed.
The existing DNS scheduler lock paused production without editing its crontab.

Eight restricted archives preserve four Monitor volumes, Gateway SQLite/key,
Redis AOF, old source/configuration and credentials. Archives were checksummed,
extracted into isolated directories, and verified with three SQLite
`integrity_check` results plus Redis AOF validation. `redis-check-aof` required
a writable **clone** to open the base RDB; the original archives were unchanged.

The old Gateway binary also started healthy against a cloned **post-import**
database with new history retained and `--network none`. The first clone attempt
had incorrect file ownership; preserving original numeric ownership resolved
that fixture error. Production was not downgraded and this check could not send
Telegram messages. The [rollback procedure](dns-subscription-migration.md)
pauses new acceptance, reconciles accepted work and then restores the old sender;
it never overwrites current history with a pre-cutover database.

Restricted evidence, archives, resolved before/after Compose snapshots and the
exact operator scripts are retained on the runtime host at
`/opt/telegram_gateway/acceptance-1238/issue16-20260911`. Retain these and all
legacy volumes/secrets until the rollback window and outstanding reconciliation
are complete. No unrelated business state was removed.

## Remaining: genuine DNS fault/recovery cycle

A fresh actual DNS check completed successfully after cutover. Its ingress ID
is `3dbafe11-ce45-4338-b69b-3cc11af5c37a`; Keep execution
`2d679f1f-0543-4cc2-9258-ba36b1a4a924` succeeded at 01:42:59 UTC.
The existing incident remained `firing` with **unresolvedCounter 718**. The
Workflow correctly suppressed another fault notification in that same incident;
service 1 therefore still had no notification at this observation.

**The required real DNS fault and recovery deliveries are not yet evidenced.**
Do not reset the incident, fabricate recovery, replay historical Workflows across
idempotency domains, or describe controlled test events as a genuine DNS cycle.
On actual business-state transitions, record each Keep event/execution, Gateway
receipt/Delivery, final Telegram message and unchanged legacy-route counts.
Keep #16 open until that evidence is complete. Production cutover and successful
controlled Telegram tests alone do not satisfy this final acceptance criterion.
