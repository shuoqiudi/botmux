# DNS service subscription migration (#16)

This procedure requires a reconciled live inventory. The Git baseline
`v2:incident:domains:dns → test → it-monitor-alerts` is a starting point,
not a claim about the number of Keep alerts or live recipients.
See [the execution report](dns-subscription-migration-validation.md) for actual
progress. Code and offline checks do not complete the production migration.

## Inventory and backup

Record UTC time, source commits, running image IDs/digests, Compose project,
working directory and **all** `com.docker.compose.project.config_files` labels.
Inspect the Router's mounted effective configuration and incident snapshots.
Resolve every enabled fingerprint/target through its live Gateway Business Route,
including route revision, destination ID, Bot account ID, immutable Telegram Bot
ID and Chat ID. Identify the workload by its existing credential and grants;
do not create a replacement source or rotate secrets as part of import.

Use authenticated admin GET `/api/gateway/v1/routes`, `/destinations`,
`/bot-accounts`, `/workloads` (each suffix under `/api/gateway/v1`), and the
existing delivery/DLQ views. Keep physical identities and private configuration
in a mode-0600 inventory; the public report contains IDs and outcomes only.

Pause producers at their managed scheduler, let ingress queued/delivering work
finish, then reconcile Keep executions and all Gateway accepted/pending/retrying/
reconciling/DLQ records. Record the last old event and Delivery IDs. Unknown
Telegram outcomes need individual investigation, not a second send. Recheck at
the actual cutover barrier: an earlier zero-pending observation is insufficient.

Before replacing anything, preserve the source checkout/configuration, all
Compose overrides, old image references, credentials, encryption key, Router
snapshots, Keep/ingress SQLite and config/secret volumes, Gateway SQLite and Redis
AOF. Use Monitor's `scripts/migration/monitor-state.sh backup` and `verify` with
the observed project/overrides. For Gateway use SQLite's online backup API or a
stopped-writer volume archive, never a live copy of the SQLite main file alone.
Archive Redis only after its writers are stopped and durable work reconciled.
Keep backups restricted and verify archive readability and restore commands in
isolated volumes. Do not delete unrelated state or run `down -v`.

## Atomic pre-registration API

Deploy a verified Gateway build containing the importer first, retaining the
existing runtime arguments, networks, Redis, volumes, secrets and Adapter setup.
The existing admin session authorizes:

`POST /api/gateway/v1/services/import`

```json
{
  "migration_id": "dns-cutover-20260911",
  "services": [{
    "workload_id": 1,
    "fingerprint": "v2:incident:domains:dns",
    "display_name": "DNS",
    "destinations": [{
      "destination_id": 1,
      "bot_account_id": 1,
      "telegram_bot_id": 123456789,
      "chat_id": -1001234567890
    }]
  }]
}
```

All numeric values above are examples. Populate them from the reviewed live
inventory, including **every** effective recipient. Migration ID is 1–128
characters; up to 100 services with up to 100 destinations each are accepted.
Fingerprint/display-name rules match the notification API (512/256 characters).
An active workload and active destinations are required. Token-derived immutable
Bot identity, account and Chat must exactly match the manifest. No token is sent
in the manifest. Duplicate services/destinations are rejected; aliases of the
same physical Bot/Chat are rejected too.

Using a previously obtained admin cookie jar, without credentials in arguments:

```bash
umask 077
curl --fail-with-body --silent --show-error \
  --cookie "$GATEWAY_ADMIN_COOKIE_FILE" \
  --header 'Content-Type: application/json' \
  --data-binary @"$DNS_IMPORT_MANIFEST" \
  "$GATEWAY_ADMIN_ORIGIN/api/gateway/v1/services/import" \
  --output "$DNS_IMPORT_RECEIPT"
```

The entire batch, audit entry and receipt commit in one SQLite transaction. It
creates services/subscriptions only: no notification, Delivery, outbox item or
Telegram send. It does not grant workload permissions or change Business Routes.
A 200 receipt contains `migration_id` and ordered `service_ids`. Identical decoded
manifest retries return the original receipt, including after a lost response or
restart. Preserve array order. Reusing the ID for different content returns 409.
A cancelled subscription is **not** recreated by a receipt replay. The receipt
proves the original import, not current readiness: always GET each service and
compare its current subscriptions before enabling Workflow execution.

An existing service is accepted only if its current active destination set
exactly matches the manifest. Otherwise 409 leaves the **whole batch** unchanged;
resolve discrepancies through reviewed admin operations, not a new migration ID
or deletion of history. Unknown references return 400, authorization failures
401/403 and storage failures 503. For an unknown response, retry the exact same
manifest/ID. A failed batch can be corrected and retried with that ID because no
receipt committed. Identical imports are safe to repeat; import itself is not a
reconciliation loop and never overwrites later subscription choices.

## Cutover and acceptance

1. Read back all imported services/subscriptions and verify empty notification
   history for newly registered services. Repeat the import and compare receipt
   and subscription IDs; retain evidence. Run the lost-response/restart regression
   before production. Never manufacture a real notification just to create a service.
2. On the **original** workload, PUT `/api/gateway/v1/workloads/{id}/service-permissions`
   with its current `expected_revision`, `publish: true`, `query: true`. Read back
   permissions; check the mounted credential still authenticates as that workload.
3. At the drained barrier stop old Keep notification execution, then explicitly
   stop the old Router container. A new Compose profile does not stop an existing
   Router. Do not replay historical Workflows across old/new idempotency domains.
4. Follow Monitor's `docs/gateway-service-migration.md`: deploy its verified
   extended Keep image with the existing workload secret mounted read-only for
   uid 1000, populate managed config, start the single Backend and run validated
   config apply. Preserve the observed Compose overrides. Verify runtime Provider
   and transport match the pinned source. Record image/source/time on both sides.
5. Resume producers only after readiness checks. Observe an actual DNS failure
   and recovery end-to-end: ingress/Keep event, Workflow execution, Gateway
   notification receipt, child Delivery and Telegram message ID/final success.
   Do not cause an infrastructure outage merely to obtain this evidence. If no
   real event occurs, leave real DNS acceptance pending. For each event retain
   old-route counts/IDs and new notification/Delivery IDs proving only one path.
6. On authorized dedicated test targets, publish one notification to A+B; cancel
   B and publish another (A only); then fault to A, change to B, recovery to B.
   Use distinct test event identities and preserve the original subscription set.
   Restore it even on failure and read it back. Never infer authorization for an
   arbitrary discovered chat. Tests do not permanently expand DNS recipients.
7. Verify deployed UI service list, subscription form and notification detail;
   record both locales/themes if changed. Business callers retain only their
   service fingerprint and workload credential, without Bot Token/Chat/targets.
   Run both repositories' regressions and existing Business Route/Adapter checks.

## Rollback

Keep the barrier inventory and exact old Compose/config archives next to the
restricted receipt. Rollback is a path switch, not deletion of notification state.

```bash
# Set these to the recorded files/project; include every original override.
docker compose -p "$MONITOR_PROJECT" -f "$MONITOR_COMPOSE" \
  -f "$MONITOR_OVERRIDE" stop keep-backend
# Pause producers first; reconcile new accepted work before proceeding.
# Restore the archived old managed configuration using its original tooling.
docker compose -p "$MONITOR_PROJECT" -f "$OLD_MONITOR_COMPOSE" \
  -f "$OLD_MONITOR_OVERRIDE" up -d --no-deps notification-router keep-backend
```

Before the final command, drain or individually resolve all new Gateway work and
record the event boundary. Verify the old Route grants and sole notification
executor, then resume producers. Do not run new accepted events through the old
Workflow: its idempotency domain cannot deduplicate new service notifications.
Likewise do not replay old history into the new Workflow. Resolve ambiguous or
failed records in their original domain. Retain migrated services, subscriptions,
SQLite, AOF, credentials and all historical mappings. If a Gateway binary rollback
is necessary, verify old-binary compatibility against a cloned post-cutover DB
first; restoring an old production DB snapshot would erase accepted work and is
not a normal rollback step.

Clean up archives/legacy resources only after the agreed rollback retention
window, all ambiguous/in-flight work is resolved, evidence is retained, and an
explicit cleanup decision identifies exact owned resources. This procedure does
not authorize removal of unrelated business state.

## Moving an existing service configuration to another instance (#24)

Once the chain uses notification services, the administrator configuration backup
transfers sources, service permissions, full fingerprints and subscriptions together
with Bots and destinations. Follow
[the configuration backup procedure](configuration-backup.md#notification-services-and-original-workload-credentials):

```sh
# BOTMUX_ADMIN_KEY identifies an administrator of the source instance.
python3 scripts/config-backup.py export --url https://old-botmux.example --file settings/botmux.json
# After the cutover barrier, use the target instance's administrator key.
python3 scripts/config-backup.py restore --url https://new-botmux.example --file settings/botmux.json
```

The upstream producer keeps its existing plaintext workload key from its secret
store. Botmux exports only an explicitly tagged SHA-256 verifier and enabled state;
it neither recovers plaintext nor creates a replacement key. Sending the hash as a
Bearer token fails. Plaintext Bot tokens and webhook secrets in the same backup
are recoverable secrets, resealed using the new instance key. Keep this file private.

This is configuration-only recovery into an empty target, without old notifications,
idempotency history, recent-receive timestamps or historical sends. Verify a new
notification using the original upstream credential and check every intended Bot/
chat recipient before resuming producers. Remaining Business Routes or route grants
fail export explicitly until business-route restore is supported. The original
`POST /api/gateway/v1/services/import` API above keeps its existing semantics as the
separate one-time pre-registration operation on the current instance.
