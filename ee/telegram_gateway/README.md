# Telegram Gateway Fork baseline

This directory owns the hardened deployment assets migrated from it_manage.
The root `Dockerfile` is the only image definition; `build.sh` builds the current
clean checkout and records its full Git revision in the binary and OCI labels.
No it_manage checkout is required. See `UPSTREAM.md` for fixed migration sources
and [migration validation](../../docs/gateway-packaging-validation.md).

Use [Ephemeral Gateway Test Instance](../../docs/gateway-acceptance.md) for
acceptance. The historical DEV deployment below must not establish a permanent
Gateway DEV as part of acceptance.

## DEV startup

Create `ee/telegram_gateway/secrets/telegram_bot_token` from the dedicated DEV Bot
token. Keep the file untracked and readable only by the operator and container
user. Do not reuse a token while another polling service owns it.

```bash
ee/telegram_gateway/build.sh
docker compose -f ee/telegram_gateway/compose.yaml up -d --no-build telegram_gateway
docker compose -f ee/telegram_gateway/compose.yaml ps
curl --fail --silent http://127.0.0.1:18081/api/health
```

The service binds only to loopback. SQLite data, including BotMux configuration,
lives in the `telegram_gateway_data` named volume. Compose mounts only
`secrets/telegram_bot_token` as the read-only `/run/secrets/telegram_bot_token`;
the root entrypoint passes it over stdin so the application user creates its own
`0600` copy in tmpfs, then uses `su-exec` to start the Gateway as the unprivileged
`telegram-gateway` user. Compose grants only `DAC_OVERRIDE`, `SETUID` and `SETGID`
during this bootstrap; `CHOWN` is not granted, and the unprivileged Gateway
process retains no effective capabilities. The token is never exported or
printed. Do not use `down -v` during normal stop or upgrade operations.

## Smoke validation

Run all preserved-interface tests serially with fake Telegram and Backend adapters:

```bash
ee/telegram_gateway/smoke.sh
```

The four stages are management/API authentication, Telegram polling, Push Proxy,
and Bot API Proxy. They include token-file loading, polling-owner conflict, and
proxy-log redaction coverage. They execute with `go test -p 1`; no real Token,
Chat ID or administrator credential is consumed or printed. `--list` reports the
stage names without building an image.

## DEV host deployment

The real DEV instance runs on the existing Windmill DEV host. With
`WINDMILL_SSH_USER` and `WINDMILL_SSH_IP` already set, deploy the locally built,
fixed image and dedicated Bot secret with:

```bash
ee/telegram_gateway/deploy_dev.sh
```

The script serializes deployments, rejects an unsafe local secret, checks remote
port `18081`, transfers the image without publishing it, stops the local Gateway
poller before starting the remote owner, rejects other local or remote containers
whose command, environment, entrypoint, or bind-mounted file contains the same
token, and observes the Gateway's complete long-poll interval while
verifying remote health, unprivileged execution, zero effective capabilities,
token-safe logs, and polling ownership. Docker volumes and pollers outside these
two hosts cannot be inferred by inspection; they are excluded operationally by
using a dedicated DEV Bot.
Remote state is kept under `/opt/telegram_gateway`; SQLite remains in the Compose
named volume.

## Polling ownership

The Compose project uses the fixed container name
`it-manage-telegram-gateway-dev`, and the smoke runner uses a non-blocking host
lock. These prevent parallel copies of the repository-managed DEV instance or its
smoke suite. Before registering the dedicated test Bot, stop and verify the old
poller for the same Bot token. Never start two Compose projects with the same token.

## Upstream upgrade

1. Fetch upstream in this repository and inspect the candidate, release notes,
   license, schema and security changes against the current deployed revision.
2. Merge/cherry-pick into a branch here; preserve the downstream boundary in UPSTREAM.md.
3. Commit changes, run `build.sh` and `smoke.sh`, and run `test.sh` for the complete
   serial suite with isolated Redis. Record the full source SHA and image ID.
4. Back up SQLite, its encryption key and Redis together while stopped. Build a
   revision-tagged image and validate it using the ephemeral procedure before any handoff.

## Roll back to the fixed baseline

Stop the candidate polling owner first. Restore the previous immutable image and
Compose configuration; retain SQLite, `.gateway-key`, Redis AOF and idempotency
history. If schemas differ, restore the corresponding stopped-state backup as a
set and reconcile executions after its timestamp before resuming one owner.
Never use `down -v` for an upgrade or rollback.

The pre-migration packaging remains available in it_manage at
`8568d92cc1aa96003569f9e145cca10be250e13b`, using Gateway
`cfae7f856f9864a401d6220e8ca42dd8b70a19b0` (includes the embedded Adapter).
Its fixed Dockerfile can rebuild the old image. No gitlink or old entrypoint is
removed by #6. Follow-up cleanup must pin the independently validated delivery
commit/image recorded in the migration report before deleting those assets.
