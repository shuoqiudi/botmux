# Service subscriptions validation — issue #14

Baseline: `4666f8f2e2d4974b1d7f9a28d55103f45da76c10`.
Scope: subscription forms, fixed recipient snapshots, independent Deliveries and recovery through the existing outbound worker.

## HTTP integration

Tests use Gateway HTTP, real temporary SQLite, fake Telegram HTTP, and disposable Redis processes with isolated directories and AOF/always. The smoke-test image includes Redis for these cases; the runtime image has no new Redis dependency.

- `TestE2E_ServiceSubscriptionSends`: validated account/target creation, subscription, HTML delivery, no-subscriber replay, stable notification and Delivery IDs.
- `TestE2E_ServiceSubscriptionManagementConflicts`: stale/missing revisions (including legacy URL aliases), duplicate actual Bot/Chat across logical accounts, target-change collision, role restrictions and audit.
- `TestE2E_ServiceSubscriptionSnapshotsAndRotation`: failure to A, recovery to B, cancellation preserving accepted deliveries, same-Bot token rotation, request replay.
- `TestE2E_ServiceSubscriptionBotReplacementFails`: replacing the actual Bot produces an explicit DLQ failure and no Telegram send.
- `TestE2E_ServiceSubscriptionDurableRecovery`: missing Redis append and lost append response, then SQLite reopen and killed/restarted AOF Redis; automatic recovery with unchanged targets and identities, without producer replay.
- `TestE2E_ServiceSubscriptionDeadLetterReplayRecovery`: finite retries, DLQ, cancellation/target changes, lost Redis replay response and automatic recovery of the same replay generation to the original Chat.
- `TestE2E_ServiceSubscriptionUncertainSendDoesNotRetry`: ambiguous Telegram response remains reconciling through producer replay and worker restart.
- `TestE2E_ServiceSubscriptionRateLimitSurvivesWorkerRestart`: persisted Bot rate limit gates both a retry and a new Delivery after restart.
- `TestE2E_ServiceSubscriptionLegacyDeliveryMigration`: older mandatory-Route Delivery/DLQ schema upgrades without losing HTTP-visible history; independent service deliveries work afterward.

The first sending test failed with HTTP 405 before implementation. Further red tests reproduced configuration conflict handling, missing service failures in the DLQ, stuck replay_pending after a lost Redis response, and missing-version bypass through legacy management URL aliases. Focused tests passed after each correction.

## Browser

`TestE2E_ServiceSubscriptionBrowser` runs `tests/browser_service_subscriptions.cjs` through a configured local Puppeteer installation. Set `BROWSER_NODE` to Node's executable and `PUPPETEER_MODULE` to that installation; no application npm dependency is added. `SUBSCRIPTION_SCREENSHOTS` optionally selects an output directory. The unrelated version check is intercepted to avoid network access.

Chromium exercises real Gateway forms and APIs: select an existing Bot/chat, add a subscription, observe pending and successful Delivery states, literal HTML-body rendering, cancel, migrate a target, create and validate a new Bot and Chat through forms, subscribe again, and view a failed Delivery. English/Russian and dark/light presentation are checked with no page JavaScript errors. All accounts, tokens, targets and bodies are synthetic.

Screenshots: [English/dark](../../screenshots/service-subscriptions-en-dark.png), [Russian/light](../../screenshots/service-subscriptions-ru-light.png).

## Final checks

Standards review: one untranslated Chat ID label was corrected to use the existing EN/RU key and re-reviewed; zero remaining findings. Spec review: zero findings against #14.

Validated implementation: `ba8ea435660867cfa27d71ca491f511df44d9587`, from a clean detached checkout.

- Focused service notification/subscription HTTP tests passed with real SQLite and isolated AOF Redis.
- The pure-Go build and Chromium browser acceptance passed, including the corrected translation and regenerated screenshots.
- `ee/telegram_gateway/test.sh` passed all seven packaging contracts plus packaged secrets, privilege drop, health, authentication, SQLite/key restart and native-volume migration.
- The explicit Redis durable reclaim/DLQ check passed.
- `go test -p 1 -parallel 1 ./... -count=1` passed in the smoke-test image with isolated Redis. The `tests` package completed in 139.729 seconds, including the new subprocess AOF recovery tests. Existing Business Route, compatible Telegram proxy and Adapter regressions passed.

The full suite passed on its first final run. Browser acceptance ran separately because the packaged Go suite does not install Chromium/Node. These are local acceptance checks, not a production rollout.
