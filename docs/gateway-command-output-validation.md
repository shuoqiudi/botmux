# Issue #9 implementation verification

Issue: [it_telegram #9](https://github.com/shuoqiudi/it_telegram/issues/9).
Starting commit: `8b11aca9103990ba5cce5ec7943585bdc62af54e` on `main`.
Validation date: 2026-09-09. Build/test runtime: `golang:1.26-alpine`, Go 1.26.8,
`GOMAXPROCS=2`, pure-Go application build. A disposable Redis instance uses AOF
and `appendfsync always`; tests run serially.

## Automated verification

The initial Adapter regression failed because the final message contained only
status. After implementation it passed with sanitized, HTML-escaped Command
Output and the original chat/topic/reply association.

Focused tests cover the exact line/character boundaries, Chinese, emoji,
HTML source expansion, full-message length fallback, ANSI/C0/C1 controls, CRLF/CR,
empty/missing/unavailable/failed/timeout output, and explicit capacity errors.
Vendored prerequisite fixtures are consumed unchanged at the Jenkins boundary.
Console tests cover 5 MiB, exactly 8 MiB and one byte over the reader bound.
Multipart checks inspect actual filename, MIME type, full bytes and caption.

Recovery reopens SQLite both after result acquisition and after enqueue, with
Jenkins output subsequently unavailable and the Route destination migrated.
The restored document retains its original text and conversation, including a
900 KB case. Operational JSON excludes the private output. An upgrade regression
first reproduced an idempotency conflict for an existing legacy status-only reply;
loading its previously stored bytes now preserves delivery without retriggering.

Real Redis outbound checks verify a 429 followed by restart and success, a lost
HTTP response remaining `reconciling` after restart, and permanent send rejection.
Business status and the single Jenkins trigger remain unchanged in each case.
The older memory queue cannot reclaim pending sends and is not used as evidence
for retry semantics.

The pure-Go build and initial full serial suite passed (the `tests` package took
105.270 seconds). Both subsequent review regressions failed for their intended
reasons before the fixes; the corrected output-state suite passed in 14.390 seconds.
Final post-review verification is recorded below.

## Standards

Independent Standards review found no documented-standard violations or actionable
baseline code smells. It checked store ownership, private payload serialization,
transport/recovery boundaries and original conversation pinning.

## Spec

Initial independent Spec review found two implementation issues:

1. Valid `partial/result_unavailable` output from a polling failure was rejected.
2. Failed legacy results missing `command_output` were classified as unavailable
   instead of legacy/unknown.

Both were reproduced by failing Adapter regressions and corrected. Missing
`command_output` now always identifies a consumed legacy result, while local
retrieval failures remain unavailable. Partial output accepts the contract-defined
`result_unavailable` reason and retains the acquired text and failed business status.
The live acceptance requirement below is separate and outstanding.
Independent re-review confirmed both fixes and found no remaining implementation
findings or scope creep. Review totals: Standards 0 findings; Spec 0 remaining
implementation findings, 1 outstanding live acceptance requirement.

## Live acceptance: pending

No live short-output or long-attachment Telegram delivery is claimed. The
prerequisite's successful Jenkins DEV runs validate its producer, not this Gateway
change. Offline tests and a containerized Go test process are not substitutes for
the real Telegram → temporary Gateway → Jenkins DEV → Telegram path.

Existing credential files and the prerequisite DEV procedure were located.
Execution awaits identification of the dedicated test bot/chat, confirmation that
all other polling owners are stopped, and human test commands. These requirements
come from [the ephemeral Gateway procedure](gateway-acceptance.md). The user was
asked for that missing information; no new credentials or permanent deployment
were created. Issue #9 must remain open until both live cases have actual evidence.

After those prerequisites are available, run the temporary candidate with the
existing developer job and `IT_MANAGE_DEVELOP=ON`, choose read-only test commands
producing short and long output, and record safe request/build/execution IDs,
expected/observed delivery kind, output byte count and hash. Preserve credentials
and raw content outside the report, and stop the temporary instance afterward.
Phone presentation and the comprehensive acceptance matrix belong to the follow-up
ticket as specified by #9.

## Final post-review checks

Passed:

```sh
CGO_ENABLED=0 go build -p 1 ./...
go test -p 1 -parallel 1 ./internal/gateway \
  -run '^TestRedisQueueDurableReclaimAndDLQMove$' -count=1 \
  -gateway-redis-test-addr=127.0.0.1:6379
go test -p 1 -parallel 1 ./... -count=1
```

The final full `tests` package passed in 110.463 seconds; every other Go package
built or passed. The opt-in Redis test passed when invoked explicitly. Changed
Markdown local links and `git diff --check` passed. Logs are available locally at
`/tmp/it-telegram-issue9-tests.log` and `/tmp/it-telegram-issue9-final-tests.log`.
No live acceptance evidence is implied by these results.
