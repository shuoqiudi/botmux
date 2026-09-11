# DNS migration execution report (#16)

Status: **in progress; not a completed production migration**.

## Read-only preflight — 2026-09-11 UTC

- Gateway source baseline: `eb49168c8e156c68dbcdc111743ca16f59bd70fc`.
- Prerequisites Gateway #15 and Monitor #10 are closed.
- Live Router version-2 configuration contains one effective subscription:
  `v2:incident:domains:dns → test → it-monitor-alerts`. This does not establish
  the number of alerts in Keep.
- DNS route ID 2/revision 3 uses Bot account 1 and destination 1, enabled for
  outbound sending. Existing Monitor workload ID 1 is active, revision 2.
- Gateway snapshot: route 1 has 23 succeeded Deliveries; route 2 has 10 succeeded
  Deliveries, with no other states in this observation. This must be rechecked
  at cutover; Keep/ingress work has not yet been drained.
- Running Gateway image tag:
  `it_manage_telegram_gateway:c2d947a27ae0e7cd868030ff997dc4030cbf5f0c`;
  image ID `sha256:81e65ac5bdd23468f24bcdf6b99c9bbc1aa05a58b621f2341a573664e8f7e3c2`.
  Its schema predates service permissions/subscriptions.
- Running Keep image ID:
  `sha256:eba54048dd0b72c887dd68219827d1c5332b8284df50efaf6c050460c6b8e87e`;
  Router `sha256:d99ffa337fa76022b1f1647b37b87e6dba42abb85bad671d708ea08fdbf91dd2`.
  Legacy Router remains running. Production has not switched.
- Actual Compose project/override labels and persistent mounts were inspected.
  No production state, credentials, subscription or notification was changed by
  these checks. Physical Chat/Bot IDs and private content are excluded here.

## Implementation and validation

The admin import API implements an atomic manifest, pinned workload/physical
recipients, durable same-ID receipt replay, conflict detection, audit, and no
notification side effect. See [executable procedure and schema](dns-subscription-migration.md).

Focused HTTP regressions cover repeat import, preserved target, no notification,
retry after cancellation, invalid-batch rollback, changed-manifest conflict,
workload authentication rejection, and lost response followed by database/server
restart. The first test failed with HTTP 405 before implementation, then passed.
Go build passed using the existing pinned smoke toolchain (host Go is unavailable).
Full packaging and regression results will be recorded after candidate validation.

## Outstanding acceptance

- Review/build/deploy final Gateway and Monitor versions, record source/image/time.
- Consistent backups and restore validation; drain Keep/ingress and old deliveries.
- Actual source/physical identity reconciliation and service import; workload grants.
- Workflow cutover with one production sender; deployed UI verification.
- Real DNS failure/recovery correlated with Keep, notification, Delivery and Telegram.
- Authorized second dedicated test destination, multi-target/cancellation/A-to-B
  checks, restored subscription set and no-double-send evidence.
- Operational rollback exercise, final cross-repository/Adapter checks.

None of these items is satisfied by synthetic tests. Keep #16 open until evidence
is recorded. The second test recipient has been requested from the user rather
than selecting an arbitrary discovered chat.
