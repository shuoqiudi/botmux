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

## Validation

- Migrated seven packaging checks: red before assets were received, green after migration.
- Build, runtime, smoke and full-suite execution results are recorded below once run.
- Live acceptance: pending dedicated Bot/Chat ownership confirmation and operator
  participation. Prior #1238 evidence is historical and does not validate this image.

## Delivery reference and follow-up

Use the full commit containing this report (resolve with
`git log -1 --format=%H -- docs/gateway-packaging-validation.md`) and the runtime
image's `org.opencontainers.image.revision` label. `build.sh` requires a clean
checkout and injects the full commit and commit timestamp into both labels and
the binary. Build a full-SHA image tag and retain its `docker image inspect` ID;
mutable `:dev` tags alone are not handoff evidence.

The subsequent it_manage cleanup must pin the validated source/image, rerun its
Management Request/Jenkins and Monitor consumer contracts, and check the live
acceptance status before removing old entries. This ticket does not delete its
gitlink. For rollback, keep the donor revision above and its fixed Gateway image,
plus matching stopped-state backups; see the [runbook](gateway-acceptance.md#rollback).
