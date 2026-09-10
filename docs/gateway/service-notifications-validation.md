# Service notification validation — issue #13

Baseline: `f6d7246add062eb40214f5b59818abd949eb72cd`.
Scope: authenticated service registration and durable no-subscriber receipts; subscription delivery belongs to later tickets.

## HTTP integration

`go test ./tests -run '^TestE2E_ServiceNotification' -count=1` passes using Go 1.26 in the local container toolchain and real temporary SQLite databases. Tests cover:

- Publication/query grants, stale revisions, audit, disabled and unauthenticated callers, administrator-session rejection, independent query/publish permissions.
- Full Unicode fingerprint/name limits, byte-limited text, parse modes, malformed/duplicate/unknown/null JSON fields, unpaired surrogates, content type and idempotency-key limits.
- First registration, renaming, fallback names, distinct fingerprints with the same name, source isolation and indistinguishable foreign/missing queries.
- Same-request replay, conflicting payloads, replay after rename, concurrent first requests with identical and distinct keys, and credential rotation.
- Reopening SQLite and querying/replaying the original receipt; administrator versus operator/user body access; storage unavailability returning 503.
- No Telegram HTTP calls from the no-subscriber path.

The initial registration test failed with HTTP 405 before implementation. The unpaired-surrogate test failed with HTTP 202 before strict Unicode validation was added.

## Browser

Chromium 148 via Puppeteer exercised the embedded SPA against a local `server.BuildMux` process with real SQLite, synthetic users and a synthetic workload. No production credentials or external notifications were used. Missing browser shared libraries were extracted into a temporary directory, without changing application dependencies.

Verified the real workload grant form, HTTP publication, service discovery and detail, no-subscriber status, literal rendering of HTML bodies, English/Russian translation, dark/light themes, administrator/operator/user access, and absence of browser JavaScript errors.

The spec review identified cached administrator bodies surviving logout. A browser assertion reproduced the failure; the fix clears and closes service UI on authentication changes, discards late responses from former identities, and checks the current role when rendering bodies. Browser regression also holds an administrator detail response until after logout and verifies that releasing it cannot restore the body.

Synthetic screenshots: [English/dark](../../screenshots/service-notifications-en-dark.png), [Russian/light](../../screenshots/service-notifications-ru-light.png).

## Review

Standards: no actionable findings against AGENTS.md/CONTRIBUTING.md or the review smell baseline.

Spec: one privacy finding, corrected and independently re-reviewed with no remaining findings.

## Packaged validation

Tested implementation: `78992940edbe02eee750b89cc4ef613bb216383a`, in a clean detached checkout. `ee/telegram_gateway/test.sh` built the pinned smoke-tests and runtime images, passed all seven packaging contracts, and passed packaged secrets, privilege drop, health, authentication, SQLite/key restart and native-volume migration. The pure-Go build and explicit Redis durable reclaim/DLQ test passed against disposable Redis with AOF/always.

The first full serial suite run failed only `TestGatewayAdapterAmbiguousTriggerRestart` with `jenkins_trigger_uncertain`. The same focused test reproduced that failure on the unchanged baseline `f6d7246add062eb40214f5b59818abd949eb72cd` (`-count=10`); the candidate passed five focused repetitions. This is a pre-existing timing-sensitive Adapter test, not a new notification failure. No Adapter implementation or test was changed.

The subsequent `go test -p 1 -parallel 1 ./... -count=1` run passed completely in the candidate image with isolated Redis; the `tests` package completed in 114.248 seconds. Existing Business Route, compatible proxy and Adapter regression suites passed. This rerun was justified by the initial baseline-reproducible failure.

This is local acceptance, not a production rollout or real Telegram delivery test.
