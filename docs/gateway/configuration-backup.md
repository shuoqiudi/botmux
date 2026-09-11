# Bot, chat-target and conditional-routing configuration backups

Issues #22–#23 support all configured Bots, explicit Telegram destinations and
conditional forwarding rules. Notification services/subscriptions and
workload/business-route configuration remain reserved: export and restore reject
nonempty collections in those categories. Messages, discovered chats, counters,
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
(`name=value`). The snapshot intentionally contains plaintext Bot tokens and webhook
secrets. Terminal summaries contain counts and results only. Downloads are validated
before atomic replacement and new files have mode 0600.

The target must have no business configuration; its own admin accounts and instance
key remain in place. Restore remaps stable configuration references to local IDs and
seals credentials using the target key. Repeating a snapshot returns its durable
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
