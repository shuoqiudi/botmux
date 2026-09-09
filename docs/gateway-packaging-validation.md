# Gateway packaging migration — issue #6

Implementation baseline: `90933f739823d245ffd5240cfe08b7c90c377e50`.
Donor: it_manage `8568d92cc1aa96003569f9e145cca10be250e13b`.
Preserved Gateway baseline: `cfae7f856f9864a401d6220e8ca42dd8b70a19b0`.
Full provenance: [UPSTREAM.md](../ee/telegram_gateway/UPSTREAM.md).

## Asset mapping

| Former it_manage asset | Independent it_telegram entry |
| --- | --- |
| `ee/telegram_gateway/Dockerfile` | Root `Dockerfile`, consolidated with original native build; pinned Go/Alpine, smoke-tests/runtime targets, actual source labels |
| `ee/telegram_gateway/entrypoint.sh` | Same path and unchanged bootstrap behavior |
| DEV/acceptance Compose, deploy_dev.sh | Same paths; build through `build.sh`, consume images with `--no-build`; names, mounts, host and capabilities retained |
| smoke.sh | Same four interface stages, clean source check and shared serial lock; actual source revision replaces gitlink pin |
| Gateway packaging contract | `tests/packaging/test_telegram_gateway_contract.py`; all seven contracts retained, gitlink assertions replaced with source ancestry/provenance checks |
| Container/Go validation | `test.sh`, isolated AOF Redis, synthetic packaged-process check, existing Go/Jenkins/recovery suites |
| #1238 lifecycle | [Gateway-owned ephemeral runbook](gateway-acceptance.md); historical evidence stays linked to its fixed donor revision |

The original root Compose remains an upstream-compatible local example. Maintained
Gateway releases use the hardened `ee/telegram_gateway` deployment entries and
the single root Dockerfile. The existing GHCR address is unchanged. No it_manage
source is needed to build or test; no donor files or gitlinks were removed.

## Validation — 2026-09-09 UTC

Tested source: **`b2e8332b162cdb77634ca914bc50858433a75f9e`**.
Runtime image:
`it_manage_telegram_gateway_smoke_runtime:b2e8332b162cdb77634ca914bc50858433a75f9e`,
ID **`sha256:e505cf6c3919b035a2a614e7775056475e3767b26518f193a7db15674ada9a91`**.
Test image:
`it_manage_telegram_gateway_smoke:b2e8332b162cdb77634ca914bc50858433a75f9e`,
ID `sha256:33e3b1e414e7220a8a27794de4138c86170cf02222d1136c58a16ba5908a83c6`.
These are local image IDs, not published registry manifest digests. No image was
published and no existing Gateway was redeployed. Subsequent report-only commits
do not change the tested artifact; use the explicit source SHA above to rebuild.

Environment: Linux amd64, Docker Engine 29.1.3 with the existing legacy builder,
Go 1.26.8 from the pinned builder, `GOMAXPROCS=2` for Go tests. Redis used the
unchanged pinned digest with AOF/always in a disposable container namespace.
ARM64 publication was not exercised on this host.

| Command/check | Result |
| --- | --- |
| `python3 -m unittest discover -s tests/packaging -v` | Seven migrated contracts pass; initially reproduced failure before receiving assets |
| `ee/telegram_gateway/test.sh` | Exit 0 from clean tested source; independently builds both images, runs packaging/runtime checks and Go suites |
| `CGO_ENABLED=0 go build -p 1 ./...` inside test image | Pass; no host Go or donor source required |
| `go test -p 1 -parallel 1 ./internal/gateway -run '^TestRedisQueueDurableReclaimAndDLQMove$' -count=1 -gateway-redis-test-addr=127.0.0.1:6379` | Pass against isolated Redis; explicitly runs the otherwise opt-in test |
| `go test -p 1 -parallel 1 ./... -count=1` | Pass; `tests` package 64.815 seconds, including Gateway/Jenkins/recovery tests with Redis available |
| Packaged runtime against synthetic Telegram and Adapter configuration | Pass: secret copies match, mode 0600/application UID, non-root PID 1, zero effective capabilities, health JSON, unauthorized/authorized authentication, no synthetic credentials in either log stream |
| Populated-volume restart and former-native ownership migration | Pass: authenticated session and encryption key preserved through stopped-volume ownership migration and process restart |
| `ee/telegram_gateway/smoke.sh` | Exit 0; management, polling, push-proxy, bot-api-proxy all PASS, serially |
| `sh -n` for entrypoint/build/test/smoke; `bash -n` for deploy; both Compose configurations; `git diff --check` | Pass |
| Runtime `-version` and OCI labels | Both report the exact tested source above and commit timestamp `2026-09-09T09:52:02+08:00` |
| Donor integrity | No changes to it_manage packaging/tests/.gitmodules; its gitlink still points to `cfae7f856f9864a401d6220e8ca42dd8b70a19b0` |

The pre-existing skipped bridge-helper test is unchanged; the full-suite Redis
opt-in test is separately covered by the explicit invocation above. These are
controlled endpoint/container results, not real Telegram/Jenkins execution.
All temporary test containers and their disposable volumes were removed; the
candidate image artifacts remain available locally. Existing services were not stopped.

Reproducible configuration SHA-256 values:

| File | SHA-256 |
| --- | --- |
| `Dockerfile` | `d42bd604978c768f71afa6ef43177f8fb09563a80f981a8be26c57481579bcfd` |
| `ee/telegram_gateway/entrypoint.sh` | `3bf1e4eb0f843cf83947a7ed7773dfa4b8de4f5a41ba5d006b19877f63f66ecf` |
| `ee/telegram_gateway/compose.yaml` | `16ad9f571ca530508cfe0f6d3db9e25ed74469d288123a8973ee9cc59e6a4303` |
| `ee/telegram_gateway/compose.acceptance.yaml` | `7eb4bbe300cdadb47775d6d7d22c4303d1cf0dc5ccd6707861d7c9a13a868a41` |

## Standards

Independent review found one upgrade issue: the former native root-running image's
existing volumes need ownership migration. The documented offline procedure and
populated-volume regression address it without adding CHOWN to the running Gateway.
Follow-up review of the secret bind mount and build-context exclusions found no
new regression. No blocking Standards findings remain. Donor token-inspection
duplication and literal packaging contracts were deliberately retained, with
behavioral runtime checks added.

## Spec

Independent review found no business-protocol change, scope creep or incorrect
migration. The initially missing immutable delivery references are recorded above.
**One acceptance requirement remains pending:** live Ephemeral Gateway Test Instance
validation with confirmed dedicated Bot/Chat, sole polling ownership, Jenkins DEV
resources and operator participation. Prior #1238 evidence is historical and does
not validate this image. Issue #6 must not be reported as fully accepted yet.

Review totals: Standards 0 remaining blocking findings; Spec 1 pending acceptance
requirement (live validation).

## Delivery reference and follow-up

Check out `b2e8332b162cdb77634ca914bc50858433a75f9e` to reproduce this tested
migration artifact, then use `build.sh` with a full-SHA tag. Compare the binary and
OCI revision; retain the resulting image ID (base-package downloads can change
image bytes, so a rebuild need not reproduce the historical local ID). The build
requires a clean checkout and injects the full commit and commit timestamp.
Mutable `:dev` tags alone are not handoff evidence. For transfer use `docker save`
with the recorded image and `docker load` on the existing host, following the
[ephemeral runbook](gateway-acceptance.md).

The subsequent it_manage cleanup must pin the validated source/image, rerun its
Management Request/Jenkins and Monitor consumer contracts, and complete the live
acceptance gap before removing old entries. This ticket does not delete its
gitlink. For rollback, keep donor `8568d92cc1aa96003569f9e145cca10be250e13b`
and its fixed Gateway `cfae7f856f9864a401d6220e8ca42dd8b70a19b0`, plus matching
stopped-state backups; see the [runbook](gateway-acceptance.md#rollback).
