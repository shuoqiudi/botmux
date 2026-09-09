# BotMux Fork provenance

- Upstream: `https://github.com/skrashevich/botmux`
- Maintained Fork: `https://github.com/shuoqiudi/it_telegram` (formerly shuoqiudi/botmux)
- Verified upstream revision: `4819842ff90b4675d60b79eaf577a73d6740b6d8`
- Fixed Fork baseline: `cfae7f856f9864a401d6220e8ca42dd8b70a19b0`
- License: Apache-2.0; the authoritative license text is retained as
  `LICENSE`.
- Baseline date: 2026-09-08.

## Local modification boundary

The Fork baseline is based directly on the verified upstream revision. Its only
downstream commits are:

- `b3e039ed001687f6a553b3285dd9ef4944f4a3f6`: add token-file loading, stop the
  local poller on Telegram 409 ownership conflicts, and remove sensitive proxy
  request/response data from logs.
- `760afa87e6e468b3d2852930da82677aede9036b`: wait for the previous polling
  runner before restart, remove remaining credential/chat/content logs, and
  document the token-file interface.
- `741d6c1b2e02e6cff629e42ff22e85d6f350f1eb`: serialize concurrent polling
  lifecycle changes and redact route condition values.
- `6bdeeb345b406f6b3d94db16747d2522c006e03f`: sanitize Telegram authorization
  failures and invalid route-condition errors at their logging boundaries.
- `f606004ad5f724b173008dde271905546aee7fc7`: merge the reviewed downstream
  baseline into the maintained Fork `main` branch (PR #1).
- `6b3522d00e00f7b7172dda241d1b0381e5ecbc2b`: align token-safe `/tgapi/`
  authentication guidance before the Business Route merge.
- `aa93ec343e8821252d50866717b27e0f21c06621`: merge the reviewed Business Route
  foundation into the maintained Fork `main` branch (PR #2).

- `a3052650e620f6feda06790e7796bf548063d48a`: implement the embedded
  it_manage Jenkins Adapter and recovery contracts (#1234).
- `c17e57d4bebd37764b958631a8679a4dbe452fd6`: merge the reviewed Adapter
  into Fork main (PR #3, merged 2026-09-08).

- `1bad141d73c42d1f05d8add72ea85b565289687f`: recognize Jenkins Timestamper result frames while preserving strict identity checks; parser and integration regressions.
- `cfae7f856f9864a401d6220e8ca42dd8b70a19b0`: merge the timestamp parsing fix into Fork main (PR #4); source tree matches the DEV-tested candidate.

## Packaging migration (#6)

Packaging source: it_manage commit `8568d92cc1aa96003569f9e145cca10be250e13b`,
`ee/telegram_gateway/` and `tests/contract/test_telegram_gateway_contract.py`.
Gateway pre-migration revision: `90933f739823d245ffd5240cfe08b7c90c377e50`.
The donor packaging files had no uncommitted changes at migration. Unrelated
it_manage domain-document changes were left untouched.

The root Dockerfile now incorporates the accepted wrapper; deployment/smoke
remain in `ee/telegram_gateway/`, packaging tests in `tests/packaging/`.
The pinned Go/Alpine/Redis digests are unchanged. Builds use this checkout's
source and actual revision, rather than a gitlink or a historical hardcoded label.
The old source and gitlink remain available until a later it_manage cleanup.
