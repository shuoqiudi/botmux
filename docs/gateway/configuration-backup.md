# Botmux configuration backups

Issues #22–#25 support Bots, explicit Telegram destinations, conditional forwarding
rules, notification services/subscriptions, business routes and workload sources
with credential verifiers, service permissions and per-route action permissions. Messages, discovered chats, counters,
timestamps and platform login accounts are not backed up. Independent LLM routing,
bridges and platform settings are outside this format.

Run from the repository root with Python 3 (standard library only). Create an admin
API key on each instance through its existing management UI. Authentication is
supplied through the environment; it is not part of the backup:

```sh
read -rs BOTMUX_ADMIN_KEY; export BOTMUX_ADMIN_KEY
python3 scripts/config-backup.py export --url https://old-botmux.example
# Default output: settings/botmux.json
# Commit this file explicitly when you want to record a configuration version.
git add settings/botmux.json
git commit -m 'Back up Botmux configuration'

# Stop the old instance's consumers and sending paths before enabling the new one.
# Set BOTMUX_ADMIN_KEY to the NEW instance's administrator API key.
read -rs BOTMUX_ADMIN_KEY; export BOTMUX_ADMIN_KEY
python3 scripts/config-backup.py restore --url https://new-botmux.example
# The same command safely retries a lost response or incomplete runtime load.
python3 scripts/config-backup.py restore --url https://new-botmux.example
```

Use `--file /path/to/botmux.json` to choose another backup or an exported historical
Git version. `BOTMUX_ADMIN_COOKIE` may instead contain a valid admin session cookie
(`name=value`). The snapshot intentionally contains plaintext Bot tokens, webhook
secrets and inbound backend credentials. Terminal summaries contain counts and results only. Downloads are validated
before atomic replacement and new files have mode 0600.

The target must have no business configuration; its own admin accounts and instance
key remain in place. Restore remaps stable configuration references to local IDs and
seals plaintext Bot/webhook/backend secrets using the target key. Workload authentication
verifiers are installed unchanged; they are independent of the instance key. Repeating a snapshot returns its durable
receipt only if the current configuration still matches. A changed target returns
HTTP 409. Invalid formats/references return 400, unsupported configuration returns
422, missing authentication returns 401, insufficient privileges return 403, and
storage failures return 503.

`GET /api/config/export` returns readable deterministic JSON with `schema_version: 1`.
`POST /api/config/restore` validates the entire document and commits configuration and
a SHA-256 receipt together. All fields are required, including explicit booleans and
empty arrays; unknown fields and versions fail. Bot, destination and conditional-rule `ref` values are
persistent opaque configuration identities, unrelated to credentials or database IDs.
Telegram chat IDs are decimal strings, preserving signed 64-bit precision.

A receipt separates `configuration_committed`, `runtime_loaded` and
`external_health: "not_verified"`. Runtime failure leaves the whole configuration
committed; retry or restart to load it. Disabled Bots stay stopped. Restore does not
send test notifications or replay historical application deliveries. Check Telegram
identity/access, webhook settings, backend addresses and business ingress before
switching upstream traffic. The script does not stop servers or change DNS/webhooks
on the old deployment. CLI startup webhook URLs are deployment settings and must be
configured on the new server as appropriate.

Validation uses `tests/e2e_config_backup_test.go`: real Python script invocations
against independent SQLite HTTP instances and fake Telegram. It covers stable
exports during runtime activity and concurrent configuration writes, legacy aliases,
same-chat/different-Bot targets, disabled Bots, exact large IDs, cross-key credential
migration, strict validation and authorization, rollback at receipt persistence,
lost-response and restart retries, runtime failures and safe diagnostics. Run it with
`go test ./tests -run '^TestE2E_Config' -count=1`; Python 3 must be on PATH. The Docker
`smoke-tests` stage includes Python for this suite.

## Conditional forwarding example

The same export/restore commands include every rule, including disabled rules and
identical duplicates. A rule in `conditional_routes` looks like this (Bot refs must
match entries in the snapshot's `bots` array):

```json
{
  "ref": "a0fe814a15e84c149445c715644ac151",
  "source_bot_ref": "source_bot_ref_from_bots",
  "target_bot_ref": "target_bot_ref_from_bots",
  "source_chat_id": "0",
  "target_chat_id": "-1001234567890",
  "condition_type": "text",
  "condition_value": "urgent|error",
  "action": "forward",
  "description": "Send matching alerts to operations",
  "enabled": true
}
```

Array order is execution order; changing it changes the configuration digest.
Ordinary page edits preserve each rule's `ref`. Source chat `"0"` matches any chat;
target chat `"0"` uses the incoming chat. IDs are signed 64-bit decimal strings and
need not appear in discovered chats or explicit destinations. Conditions support
case-insensitive `text` regexes, `user_id` and `chat_id`. Actions preserve existing
runtime semantics: `forward` sends text with `sendMessage`, `copy` calls
`forwardMessage`, and `drop` stops processing subsequent rules when matched.
An empty text condition remains a nonmatching rule, as in the existing runtime.
Unknown actions/conditions, malformed regexes or integers, and missing Bot references
fail the entire snapshot, including disabled rules. No partial Bot or rule restore
is committed. A repeat restore after a target rule edit returns a conflict.

Message mappings and old reply chains are excluded. Restore does not synthesize
updates or send messages; new incoming Telegram updates execute the restored rules
and create new reply mappings normally. Routing activity leaves the exported
configuration unchanged. LLM routing is not included.

`tests/e2e_config_routes_test.go` covers rule round trips and live fake Telegram
requests, duplicate identity and order, strict validation and transaction rollback.

## Notification services and original workload credentials

After migrating the notification chain to services, use the same script to move its
configuration to an empty instance. A source with fingerprint
`v2:incident:domains:dns` can retain subscriptions to both Operations and Backup
chats. Restore resolves each `destination_ref` through its restored Bot and exact
chat ID. The producer keeps its original workload credential and sends one new
service notification; only current subscribers receive it. A different source with
the same fingerprint remains a separate service, even without subscriptions.

Each `workloads` entry contains `ref`, `name`, `status`, `service_permissions`
(`publish` and `query` booleans), `route_permissions`, and
`credentials`. Each credential carries `name`, `algorithm: "sha256"`, a `verifier`
(64 lowercase hexadecimal characters), and `enabled`. Route permissions contain
`route_key` and `action` fields. Actions are `messages.send`, `callbacks.answer` or
`deliveries.read`; missing routes, unknown actions and duplicate grants reject the
whole snapshot. A workload shared by routes and services remains one source with
exactly its original permissions.

Bot tokens and webhook secrets are plaintext recoverable secrets in this private
backup. Workload credentials are different: Botmux only stores their SHA-256
verification values and cannot export the original plaintext. Restore installs
those values directly, without generating replacement keys or hashing them again.
Keep the existing plaintext workload credential in the upstream secret store.
Do **not** configure upstream to send the `verifier` as its Bearer token. Disabled
sources, disabled credentials and credentials invalidated by rotation retain their
invalid status. Workload credentials cannot access administrator backup APIs.

`notification_services` entries contain `workload_ref`, the complete `fingerprint`
and the current `display_name`. `subscriptions` entries contain a persistent `ref`,
`workload_ref`, `fingerprint`, `destination_ref` and `active`. Cancelled subscriptions
are preserved with `active: false`; destination disabled states are preserved too.
Duplicate active physical Bot/chat recipients and dangling references reject the
whole document, including all its Bots and sources. Services are rebuilt by source
plus fingerprint; local database IDs and revisions are not copied.

Old notifications, deliveries, delivery retries, business idempotency records,
last-received/last-used timestamps and one-time import receipts are excluded.
There is no historical resend. New notifications retain normal automatic service
registration and display-name updates. Repeating a restore succeeds only while all
current configuration matches its receipt; source permissions, credential rotation,
service names or subscription changes cause HTTP 409. Runtime activity alone does
not change the snapshot.

The script and notification/permission integration tests in
`tests/e2e_config_notifications_test.go` exercise original and revoked credentials,
independent instance keys, source isolation, cancelled/disabled state, multiple
Bot/chat recipients, rollback, strict validation and retries.

## Business routes and backend connections

`business_routes` preserves `route_key`, `display_name`, `bot_ref`,
`destination_ref`, `enabled`, `status`, `inbound_target`, `inbound_enabled`,
`outbound_enabled`, `inbound_backend_url`, `inbound_backend_health_url`,
`inbound_backend_token` and `allowed_callers`. Route keys remain immutable business
identities. Local route/account/destination IDs and revisions are rebuilt. Backend
credentials are plaintext in this private snapshot and sealed with the target
instance key in both the current route and its initial revision; never copy the
source encryption key. Validation timestamps and health results are excluded.

The original workload Bearer credential can call
`POST /api/v1/routes/{route_key}/messages` on the new instance. Route permissions
remain specific to each action; the descriptive `allowed_callers` list is preserved
and does not create grants. Route-only sources are included even if they have no
notification services. Disabled routes and directions keep their configured state.
Active inbound routes must resolve to unique physical Telegram Bot/chat targets,
including destination aliases. Invalid references or grants roll back the whole
restore, including sources, subscriptions and backend configuration.

Verify the new business ingress URL, caller credentials, Telegram Bot/chat access,
backend delivery URL, dedicated health URL and backend authentication before moving
traffic. `runtime_loaded` reports Bot loader results; routes are resolved from the
committed database, without separate route registrations. It is not proof that a
backend is healthy. An `it_manage` target retains its adapter selection but requires
the new instance's adapter environment. Restore does not deploy external backends,
configure adapter processes/Jenkins credentials, or change DNS or Keep. Coordinate
the old/new consumer cutover separately to avoid competing Telegram pollers.

`tests/e2e_config_business_routes_test.go` runs the real transfer script and verifies
both directions against fake Telegram and backend HTTP endpoints, shared source
subscriptions, permission refusal, disabled configuration and atomic rollback.
