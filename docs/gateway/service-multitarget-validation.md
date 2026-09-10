# Multi-target service notification validation — issue #15

Baseline: `32da29911e4b9d3e9aadc1791e392426a5ba41e3`.
Spec: [issue #15](https://github.com/shuoqiudi/it_telegram/issues/15), using the HTTP/Telegram and browser boundaries agreed in [parent #12](https://github.com/shuoqiudi/it_telegram/issues/12).

The existing #14 transaction and outbox already support multiple recipients. This change exposes immutable per-target identities in administrator history, adds delivered/total counts, and preserves a physical Bot's 429 deadline even when the triggering Delivery exhausts its retries. Different logical accounts for one Telegram Bot share the new durable gate; existing account gates remain readable for compatibility. Producer and operator receipt views continue to omit physical targets and body text.

## HTTP and recovery coverage

Tests use the real Gateway HTTP handlers, temporary SQLite databases, and fake Telegram HTTP. Recovery and rate-limit cases start disposable Redis processes with AOF/always; they do not use a deployed Redis instance.

| Test | Observable contract |
| --- | --- |
| `TestE2E_ServiceMultiTargetIdentities` | One request produces four independent targets: same Bot/multiple Chats, different Bots/multiple Chats, same Chat/different Bots. Administrator history identifies snapshots; producer responses omit Chat IDs. All four send once. |
| `TestE2E_ServiceMultiTargetFinal429GatesSameBot` | A last-attempt 429 enters DLQ but persists its deadline through SQLite reopen and AOF Redis restart. A newly created alias of that Bot also waits; another Bot sends before the deadline. |
| `TestE2E_ServiceMultiTargetFailureIsolationAndReplay` | A retry-exhausted target, an ambiguous target and a successful target coexist. Cancellation, Chat migration and same-Bot Token rotation preserve old targets. Lost DLQ replay acknowledgement is recovered automatically, without resending siblings. User replay/discard is forbidden; authorized replay is audited. |
| `TestE2E_ServiceMultiTargetPartialEnqueueRecovery` | Stop on target two before append or after Redis stores it. Reopen SQLite and restart Redis, change subscriptions, accept a new notification while Redis is stopped, and duplicate enqueue. Both complete plans recover with stable IDs and the correct old/new target sets. |
| `TestE2E_ServiceMultiTargetLostReceiptAndConcurrentEdit` | Drop the HTTP connection after durable receipt while adding a subscriber. Same-key retry returns the original complete snapshot and sends exactly that many targets. |
| `TestE2E_ServiceMultiTargetConcurrentConfigurationSnapshot` | Race notification acceptance with account edits affecting two subscribers. Every snapshot contains both old or both new Bot identities. A later Bot replacement fails old targets explicitly while another Bot continues. |
| `TestE2E_ServiceMultiTargetInterruptedDispatch` | Stop the worker during Telegram dispatch and restart AOF Redis. Reclaim leaves that target reconciling and sends the sibling; producer replay does not resend the uncertain target. |

The target-history test failed before the fields were implemented. The final-attempt 429 test first reproduced a send before the requested deadline, then reproduced an account-alias bypass; both passed after their respective fixes. Existing #14 tests additionally cover duplicate subscription rejection, legacy schema migration, permission conflicts and no-subscriber history.

## Browser

`TestE2E_ServiceSubscriptionBrowser` uses Chromium and real Gateway forms/APIs to add two Bot/chat subscriptions, observe one success and one failure, cancel a subscription while retaining both historical targets, and discard the failed Delivery with an audit record. It also retains the #14 no-subscriber, pending/success, target configuration, literal HTML rendering, English/Russian and theme checks.

Run the compiled Go test from `tests/` with `BROWSER_NODE` and `PUPPETEER_MODULE` pointing at local tooling. Optional `SUBSCRIPTION_SCREENSHOTS` selects the screenshot directory. Browser dependencies are not application dependencies. All displayed data is synthetic.

Screenshots: [English/dark](../../screenshots/service-subscriptions-en-dark.png), [Russian/light](../../screenshots/service-subscriptions-ru-light.png).

## Validation status

Focused multi-target and existing subscription HTTP tests passed, including the isolated AOF recovery cases. Browser acceptance passed. The initial implementation candidate was `a318dbb`. Final validated implementation: `844aed646d468581a9c813e4acdbdcb99e6897a5`, tested from a clean detached checkout.

### Standards review

No hard violations of AGENTS.md or CONTRIBUTING.md. One nonblocking, low-priority judgement call: `targetTelegramBotID` duplicates part of the store's token identity parser. The store strictly validates accepted service targets, while the worker's prefix extraction adds physical rate limits alongside the legacy repository contract. This review retained the existing boundary rather than changing token validation or repository requirements. README usage documentation was also updated.

### Spec review

No actionable findings against #15 and parent #12. The review covered complete durable plans, immutable snapshots, independent outcomes, persistent physical-Bot limits, administrator target visibility, producer privacy, and authorized/audited per-target operations. The first full run found that ordinary retry exhaustion unnecessarily invoked the backoff callback. The worker now computes backoff only when another attempt is allowed, while final-attempt 429 still persists Telegram’s Retry-After. Both review axes rechecked this correction with no new findings. The focused legacy fault-lifecycle and final-429 regression tests passed; the complete corrected run passed as recorded below.

These are local acceptance checks. Keep/Monitor integration and DNS migration belong to subsequent tickets; no production cutover is part of #15.

## Final checks — passed

`ee/telegram_gateway/test.sh` exited 0 against `844aed646d468581a9c813e4acdbdcb99e6897a5`:

- Source-pinned smoke-test and runtime images built successfully with pure Go.
- All seven packaging contracts passed.
- Packaged secret handling, privilege drop, health, authentication, SQLite/key restart and native-volume migration passed.
- Explicit real-Redis durable reclaim and DLQ integration passed.
- `CGO_ENABLED=0 go build -p 1 ./...` passed.
- `go test -p 1 -parallel 1 ./... -count=1` passed, including existing Business Route, Telegram proxy and Adapter regressions. The `tests` package completed in 150.219 seconds.
- Chromium browser acceptance passed separately with no JavaScript errors; both synthetic screenshots were regenerated and visually checked. The final retry-only correction does not change that frontend.

Review result: Standards — zero hard violations, one nonblocking parser-duplication observation; Spec — zero actionable findings. The final follow-up commit only records validation evidence and does not change executable code.
