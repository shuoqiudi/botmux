# Ephemeral Gateway Test Instance

This is the Gateway-owned continuation of the [#1238 procedure](https://github.com/shuoqiudi/it_manage/blob/8568d92cc1aa96003569f9e145cca10be250e13b/docs/engineering/telegram-gateway-acceptance-1238.md).
It preserves the existing deployment host, `/opt/telegram_gateway`, application
identity, same-host networks and Gateway alias. It does not authorize production
cutover, a permanent DEV instance, or changes to DNS/certificates.

## Prerequisites

- Confirm a dedicated test Bot and Chat and that every possible polling owner is
  stopped. Inspecting only local containers cannot establish exclusive ownership.
- Put the dedicated token in `ee/telegram_gateway/secrets/telegram_bot_token` and
  the existing [Adapter JSON](embedded-adapter.md) in `secrets/adapter.json` beside it.
  Both files must be mode 0600. Preserve the existing username/API-token convention
  and Jenkins **developer** job; do not substitute a production job or identity.
- Confirm the test Route and workload credential and same-host Caddy/Monitor
  network attachment with the established `telegram_gateway` alias. Keep physical
  Bot/Chat configuration inside Gateway. Do not change the consumer origin or URL.
- Supply the previously agreed test domain from the operator's private configuration;
  no it_manage source file or implicit checkout discovery is needed by this package.
- Record the existing owner and immutable image/Compose revision. Back up SQLite,
  `.gateway-key` and Redis AOF together while stopped before reusing any state.

## Lifecycle

From a clean checkout, build a revision-tagged artifact. The Compose files consume
prebuilt images; `build.sh` is the source/provenance-aware build entrypoint.
On the existing acceptance host use the same image reference with `--no-build`.
For an image built elsewhere, transfer it with `docker save` / `docker load`.

```bash
set -euo pipefail
revision=$(git rev-parse HEAD)
ee/telegram_gateway/build.sh "it_manage_telegram_gateway:$revision"
# Create a local temporary override, without adding environment-variable conventions.
override=$(mktemp)
printf 'services:\n  telegram_gateway:\n    image: it_manage_telegram_gateway:%s\n' "$revision" > "$override"
compose=(docker compose -f "$PWD/ee/telegram_gateway/compose.acceptance.yaml" -f "$override")
trap '"${compose[@]}" down; rm -f "$override"' EXIT
"${compose[@]}" config --quiet
"${compose[@]}" up -d --no-build --wait
"${compose[@]}" port telegram_gateway 8080
# Perform the serial acceptance matrix below in this shell.
```

The fixed acceptance project has only Gateway and Redis, independent SQLite/Redis
volumes, AOF/always, a random loopback port and `restart: "no"`. Do not run this
project alongside another owner of its Bot Token, or run `down -v`. Configure the
administrator before attaching an external management ingress. Reattach only the
existing dedicated network/alias using the established host handoff procedure;
this Compose file intentionally does not create or rename ingress networks.

## Serial acceptance matrix

| Interface/scenario | Required observation |
| --- | --- |
| Container bootstrap | Token and Adapter copies are 0600, owned by process UID; UID nonzero, effective capabilities zero; credentials absent from logs/arguments/environment |
| Health and authentication | `/api/health` JSON 200; unauthorized management/ops 401; authorized management works; existing Caddy allowed/denied paths unchanged |
| Telegram → embedded Adapter → Jenkins DEV | Human sends test command; acknowledgment precedes execution; one terminal result in original conversation; request, queue/build and execution identities match |
| Monitor → Business Route | Existing workload auth, stable Route and idempotency key yield the same Delivery on replay; accepted is distinct from final Telegram delivery |
| Restart/reclaim | Restart only this Gateway/Redis while a test request is in flight; retain identity, one Jenkins execution, acknowledgment/result and eventual terminal state |
| Redis unavailable/recovery | Reject unpersisted submission; retry same identity after Redis restoration; one durable Delivery and no duplicate execution |
| Bounded failures | Existing fake Jenkins/Telegram tests cover timeout, 429, ambiguous sends, DLQ/replay; do not fault shared Jenkins or claim fake failures happened on real services |
| Restore | Restore stopped-state SQLite/key/Redis backup to dedicated resources; verify decryption and pending/terminal identities without starting a second polling owner |
| Exit | Stop temporary Gateway/Redis; retain protected evidence and volumes; no permanent DEV left running |

Real Telegram inbound requires a human Update; Bot API sendMessage is not an
inbound substitute. Record UTC time, source SHA, image ID/digest, Compose file
hashes, commands, expected/actual results, and safe Delivery/request/Jenkins/
Windmill identifiers. Exclude Bot tokens, Chat IDs, raw Updates, Jenkins console
and credentials. Separate offline/fake results from live evidence. Missing
prerequisites mean **live acceptance pending**, not full acceptance passed.

## Rollback

Stop the candidate and reconcile in-flight/ambiguous executions. Restore the
previous immutable image and matching configuration. Retain state when compatible;
otherwise restore the corresponding SQLite/key/Redis set while stopped and
reconcile executions since the backup before resuming a single owner. Recheck
health, auth, stable Routes and result correlation. Preserve existing ingress and
credentials; never delete user-provided DNS/certificates or silently restore an
old public instance with its initial admin password.
