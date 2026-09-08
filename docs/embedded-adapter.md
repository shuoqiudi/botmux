# Embedded it_manage Gateway Adapter

The Adapter runs inside the BotMux process and image. Redis supplies durable
Streams; Jenkins supplies the existing `it_manage` containerized entry point.
There is no Adapter listener, Backend URL, Gateway-to-Adapter credential or
Approval container. Telegram transport still selects a Business Route solely by
Bot identity and Chat identity, before the Adapter examines message content.

## Configuration

Create a private `secrets/adapter.json` outside version control, preferably from
your deployment secret manager:

```json
{
  "job_url": "https://jenkins.example.com/job/it_manage_dev",
  "username": "gateway-service-account",
  "api_token": "REPLACE_WITH_JENKINS_API_TOKEN",
  "poll_interval_seconds": 2,
  "queue_timeout_seconds": 300,
  "build_timeout_seconds": 1800,
  "reply_timeout_seconds": 120,
  "request_timeout_seconds": 10,
  "max_failures": 5
}
```

Use a Jenkins API token (not a password) with permission to read the job, queue
and build console and trigger that job. The job must include the Management
Request v1 support delivered by it_manage issue #1233. Job URLs support nested
jobs and Jenkins context paths. Only the configured Jenkins origin and job are
used; redirects are not followed. Jenkins credentials remain in process memory,
loaded from the secret file. They are absent from database records, management
responses, audit events and logs. Never place credentials in the job URL.

For a binary deployment:

```sh
./botmux -db /data/botdata.db -redis-addr redis:6379 \
  -adapter-config-file /run/secrets/adapter_config
```

For the existing Compose deployment, the optional overlay builds the same BotMux
image and mounts the secret. It adds no service or environment variable:

```sh
chmod 600 secrets/adapter.json
docker compose -f docker-compose.yml -f docker-compose.adapter.yml up -d --build
```

In **Business Routes**, select **Embedded it_manage Adapter** as the inbound
target, enable inbound and outbound, and select the intended Bot and Chat. The
API equivalent is `"inbound_target":"it_manage"`; `"backend"` remains the default
for existing routes. Adapter routes reject backend URL/health/credential fields.
Jenkins configuration is deployment-wide and is not entered in the UI. An
unconfigured Adapter is reported separately in health and fails deliveries with
`adapter_not_configured` rather than forwarding to an external backend.

## Request and reply contract

Send an original Telegram message such as:

```text
/it_manage dns list -d example.com
/it_manage@your_bot arbitrary-module arbitrary-action "two words" --flag='a b'
```

The Adapter recognizes only the command boundary and tokenizes quotes and
backslash escapes. It performs no shell expansion or execution and has no
module/action allowlist. Empty arguments, unmatched quotes, control characters,
more than 128 arguments, arguments over 4096 bytes and aggregate argv over 64 KiB
are rejected. Business parsing and normal envelope validation remain inside the
versioned it_manage wrapper. Unrelated messages, commands for another bot, edits
and callbacks are durably consumed without a Jenkins call or Telegram reply.

Jenkins receives a form POST to `buildWithParameters` with
`IT_MANAGE_REQUEST_JSON_B64` containing base64 JSON:

```json
{
  "schema": "it_manage.management-request/v1",
  "argv": ["dns", "list", "-d", "example.com"],
  "identity": {"request_id": "DELIVERY_UUID", "delivery_id": "DELIVERY_UUID"},
  "source": {"type": "telegram_gateway", "route_key": "it_manage", "update_id": "1234"}
}
```

`IT_MANAGE_REQUEST_LABEL` is `gateway:DELIVERY_UUID`. No Bot Token, Chat ID,
Jenkins credential or shell command string is included. The Gateway Delivery
UUID supplies both stable correlation identities; the downstream contract also
provides stable Windmill execution identity.

The normal flow sends an acknowledgment through the durable outbound queue,
waits for Telegram to confirm delivery, and only then triggers Jenkins. After
completion, the Adapter reads the bounded console response for a matching
`IT_MANAGE_MANAGEMENT_RESULT_BEGIN:<base64-json>:IT_MANAGE_MANAGEMENT_RESULT_END`
line. Schema, both identities, status and code must match v1. Console text,
free-form result details and external URLs are never forwarded or retained.
Telegram receives a fixed success/failure summary, safe code and request ID.
Replies retain the original Bot identity, Chat and topic even if the Business
Route destination changes while Jenkins is running.

## Recovery and operational states

SQLite records execution stages, queue/build numbers and time bounds; Redis
reclaim drives pending work after restart. A database lease fences concurrent
consumers. **The trigger intent is committed before the POST.** If the response
or process is lost, the Adapter searches the configured queue and the latest 100
job builds using request identity. It never blindly repeats the POST, including
on operator DLQ replay. This favors an explicit uncertain outcome over a second
Jenkins business execution. The persisted database and encryption key must be
retained across restarts; deleting them discards deduplication history.

| Condition | Safe code / outcome |
| --- | --- |
| Business success | `succeeded` |
| Business failure / timeout | `BUSINESS_FAILED` / `BUSINESS_TIMEOUT` |
| Jenkins rejects trigger or cancels queue item | `JENKINS_TRIGGER_FAILED` |
| Ambiguous trigger cannot be located before its deadline | `JENKINS_TRIGGER_UNCERTAIN` |
| Queue / build exceeds configured deadline | `JENKINS_QUEUE_TIMEOUT` / `JENKINS_BUILD_TIMEOUT` |
| Failed build without valid business result | `JENKINS_BUILD_FAILED` |
| Missing, invalid or mismatched result | `RESULT_UNAVAILABLE` |
| Consecutive temporary network failures exhausted | `JENKINS_UNAVAILABLE` |
| Job URL changed during recovery | `ADAPTER_CONFIG_CHANGED` |
| Acknowledgment / final reply cannot be delivered | `adapter_ack_failed` / `adapter_reply_failed` |

Failures become traceable inbound DLQ deliveries after the final notification
has been handled; successful and ignored Updates complete normally. The
acknowledgment and final notification also have their own outbound Delivery
states. Temporary read failures retry with bounded polling and a failure limit.
Timeouts and uncertain outcomes describe Gateway observation: they do not assert
that Jenkins cancelled an accepted execution. Operator investigation must use
the correlated queue/build before creating a new business request.

Telegram does not provide outbound idempotency. If a reply may have been sent
but its response was lost, the existing outbound worker marks it `reconciling`
instead of sending a duplicate. If an acknowledgment cannot be confirmed, the
Adapter does not trigger Jenkins. DLQ replay preserves the same Adapter terminal
state and cannot create another execution; it is not a request to run the command
again. Send a new Telegram command only when a new execution is intended.

Gateway health and per-route metrics report Redis, Telegram transport, the
embedded Adapter and Jenkins separately. Delivery detail includes an `adapter`
object with safe stage, queue ID, build number and result code; it contains no
message text or credential. The Jenkins health probe is a read-only job check
cached for five seconds. A healthy job probe does not guarantee Build permission;
trigger rejection is represented on the Delivery.

## Validation

Tests are serial and use fake Jenkins and Telegram with real SQLite and Redis
Streams. Run against a disposable Redis with AOF enabled at `127.0.0.1:6379`:

```sh
GOMAXPROCS=2 go test -p 1 -parallel 1 ./tests -run '^TestGatewayAdapter' -count=1
GOMAXPROCS=2 go test -p 1 -parallel 1 ./...
```

The suite covers v1 argument preservation, acknowledgment/result order,
duplicate Updates, restart and lost-response reconciliation, destination/topic
pinning, bounded failures, ignored messages, secret configuration and health.
No real business execution or Telegram production message is required.

See the [delivery verification and independent review](embedded-adapter-validation.md) for the completed checks.
