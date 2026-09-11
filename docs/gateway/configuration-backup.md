# Bot and chat-target configuration backups

Issue #22 supports all configured Bots and explicit Telegram destinations. Conditional
forwarding rules, notification services/subscriptions and workload/business-route
configuration are reserved for the following tickets: export and restore reject
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
empty arrays; unknown fields and versions fail. Bot and destination `ref` values are
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
