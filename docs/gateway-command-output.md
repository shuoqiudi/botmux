# Management Command Output delivery

Implements [it_telegram #9](https://github.com/shuoqiudi/it_telegram/issues/9), consuming
[it_manage's output contract](https://github.com/shuoqiudi/it_manage/blob/main/docs/architecture/management-command-output.md)
from [#1253](https://github.com/shuoqiudi/it_manage/issues/1253).

## Protocol and presentation

The existing base64 Management Result v1 frame carries optional `command_output`:

```json
{"format":"text/plain","encoding":"utf-8","text":"名称  值\n中文  <a>&\n","state":"complete","reason":""}
```

Request ID, delivery ID, schema and status/code validation still precede output
consumption. Unknown infrastructure fields and console logs are never displayed.
The Gateway consumes the CLI-rendered text; it does not implement business rendering.
Only the terminal result is sent, after the existing trigger acknowledgment.

ANSI CSI/OSC/control strings and nonprinting C0/C1 controls are removed. Normal
spaces, tabs, Unicode and newlines remain. CRLF and CR normalize to LF. Lines are
logical lines; a single trailing LF does not create an extra line. The output
threshold counts Unicode code points, not UTF-8 bytes. Up to **40 lines and 3500
characters inclusive** can use an HTML `<pre>` block, with HTML characters escaped.
The full rendered message, including status and request identity, must also fit
4096 UTF-16 units (a conservative check against Telegram's message length limit).
Escaped HTML source length is not the displayed length.

Exceeding either threshold or the full message limit selects one `sendDocument`
upload, with a short status/correlation caption. The file is
`command-output-<request-id>.txt`, `text/plain; charset=utf-8`, containing the entire
cleaned text, without HTML escaping or truncation. The caption and file travel in
one outbound Delivery. Chat, bot, topic and reply association remain pinned to the
original inbound message, including after destination migration.

| Result | Presentation |
| --- | --- |
| Complete, empty text | Explicit empty Command Output |
| Missing optional object | Legacy result; output unknown |
| Partial text, including empty | Partial label and validated reason; all acquired text retained |
| Unavailable | Explicit unavailable reason; never represented as complete empty output |
| Invalid output object | `invalid_output_contract`, preserving valid business status |
| Business failure / timeout | Original status/code plus the acquired complete or partial output |

Only contract-defined reasons are displayed. Output completeness and delivery
failure do not overwrite the business status. A Gateway observation timeout cannot
recover text the upstream service has not returned.

## Capacity and persistence

The producer caps serialized output JSON at **1 MiB**, including escaping; excess
returns `unavailable/output_limit_exceeded`. The Gateway enforces that contract.
It reads up to **8 MiB** of Jenkins console, allowing base64 expansion and log
headroom. It reads one extra byte to detect excess; oversized console responses
become `RESULT_LIMIT_EXCEEDED`, without accepting an earlier frame from a truncated
response. Arbitrarily large Jenkins logs remain an explicit retrieval boundary.

SQLite stores the complete prepared reply inside the leased execution's private
state, atomically with the terminal business status. That payload is excluded from
operational JSON. Enqueue copies it to the existing outbound payload BLOB; Redis
contains the Delivery ID. SQLite TEXT/BLOB have no application-level truncation
here. JSON escaping can expand storage beyond the producer's raw text size; no
Telegram message-size bound is imposed on those persisted payloads.

Upload accepts at most 1 MiB of UTF-8 content, which covers every valid producer
output. Telegram's hosted API currently allows **50 MB** documents and captions
up to **1024 characters**; the sender checks captions conservatively in UTF-16
units before dispatch. Checked 2026-09-09 against
[sendDocument](https://core.telegram.org/bots/api#senddocument),
[Sending Files](https://core.telegram.org/bots/api#sending-files), and
[sendMessage](https://core.telegram.org/bots/api#sendmessage).

Reopening SQLite after result retrieval or after enqueue restores the same bytes;
there are no temporary paths or one-shot file objects. Durable replies retain
idempotent phase identity and existing bot/route authorization. Definite transient
failures use existing bounded retries and persisted rate-limit deadlines. Lost
responses and interrupted sends stay `reconciling`; they are not resent blindly.
Permanent/retry-exhausted sends use the existing DLQ path. None retriggers Jenkins.

## Verification

Run serially against a disposable Redis at `127.0.0.1:6379`; document retry tests
also use Redis database 15. No production Redis is appropriate for this suite.

```sh
go build -p 1 ./...
go test -p 1 ./internal/gateway -run '^TestJenkins' -count=1
go test -p 1 ./tests -run '^TestGatewayAdapter' -count=1
go test -p 1 -parallel 1 ./... -count=1
```

The Adapter/fake-HTTP tests inspect actual message JSON, multipart file bytes,
filename, MIME type, caption, chat, topic and reply. Cases cover 39/40/41 lines,
3499/3500/3501 characters, long lines, emoji/full-message length, HTML, ANSI,
newlines, empty/legacy/unavailable/failed/timeout results and capacity failures.
Recovery reopens SQLite at both result and attachment-enqueue boundaries, migrates
the destination, removes the Jenkins result and verifies the original output is
sent once to the original conversation. Real Redis tests cover a 429 across
restart, ambiguous delivery reconciliation, and permanent send failure.
The upstream protocol samples are vendored in `tests/fixtures/management_result_v1`.

Live short-result and long-attachment acceptance follows
[the ephemeral Gateway procedure](gateway-acceptance.md). It requires a dedicated
bot/chat with exclusive polling ownership, existing DEV Jenkins credentials,
`IT_MANAGE_DEVELOP=ON`, a containerized temporary Gateway and human test commands.
Offline checks do not establish live Telegram delivery or phone presentation.
