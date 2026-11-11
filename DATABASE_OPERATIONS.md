# Database operations

The production shape of the NavListener historian, why its PostgreSQL and
TimescaleDB settings differ from defaults, and how to roll those settings out
without losing receiver data. Measurements below are from the production store
on 2026-11-11 (PostgreSQL 17.11, TimescaleDB 2.31.0, 12 GB) unless stated.
The schema is owned by [`go/internal/store/schema.sql`](go/internal/store/schema.sql);
this document explains the operating envelope rather than replacing schema or
configuration history.

## What owns what

The collector's writer startup is the schema and policy reconciler. It applies
`schema.sql`, sets the `nav_frames` interval used for future chunks, and replaces
the configured compression and retention jobs in one transaction. A read-only
store connection does none of those things. A collector deployment therefore
needs one successful writer startup before its database policy can be considered
current.

The application repository owns:

- hypertable definitions, indexes, compression layout and idempotent migrations;
- the one-day `nav_frames` interval for future chunks;
- validation of `compress_after` and `raw_retention`; and
- integration tests proving schema application and cross-chunk reads.

The deployment configuration owns PostgreSQL process settings and the actual
retention horizon. On the reference deployment, PostgreSQL's generated main
configuration includes `/usr/local/etc/postgresql/navlistener.conf`. Confirm
`SHOW config_file` and `pg_settings.sourcefile` before changing anything. Do not
replace the generated cluster file with the smaller deployment include.

## Production shape

The 2026-11-11 store held 74,771,023 raw frames from 2026-09-15 through the
measurement instant. `nav_frames` occupied 3.67 GiB: 1.33 GiB of table data,
356 MiB of indexes and 2.00 GiB of TOAST data. It had 1,358 chunks, of which
1,333 were compressed. The database as a whole was 12 GB.

| Hypertable | New-chunk interval | Compression | Retention | Purpose |
|---|---:|---:|---:|---|
| `nav_frames` | 1 day | after 1 day | 365 days in production | Raw receiver frames and decoded projection |
| `gnss_snapshots` | TimescaleDB default | after 7 days | 90 days | Periodic API snapshots |
| `gnss_events` | TimescaleDB default | after 30 days | none | Durable integrity transitions |
| `observer_samples` | 1 day | after 1 day | 365 days in production | Private board, environment and clock evidence |
| `rf_samples` | 1 day | after 1 day | 365 days in production | Private receiver RF and solution evidence |
| `event_evidence_samples` | 7 days | after 30 days | none | Durable inputs captured for an event |

The production retention horizon is deliberately longer than the seven-day
example default. `raw_retention` is also the replay-deduplication horizon, so
changing it affects both stored evidence and replay behavior. Confirm both
effects before changing the value.

Events and captured event evidence have no retention policy. They are the durable
record explaining an integrity transition. Do not add retention to them merely to
make their policy table look symmetrical.

## Capacity and availability posture

### Growth has explicit bounds

The raw evidence tables do not grow forever: production retention drops their
chunks after 365 days, and feed snapshots expire after 90 days. Confirmed events
and their captured evidence are intentionally retention-less and therefore need
continued size monitoring, although their present growth is much smaller than the
raw and RF streams.

There is no implemented cold archive or compact long-term decoded ephemeris
history today. If the product must retain replayable observations beyond one
year, build and verify that export before the first raw chunks reach retention.
The backup receivers' seven-daily/four-weekly snapshot policy is disaster
recovery, not a substitute for a historical observation archive.

### Use 250 MB per client per day for first-pass planning

For present-day budgeting, count one production client at approximately **250 MB
of live database growth per day**. Here, a client means one station/feeder with a
message cadence and receiver-telemetry mix broadly like the 2026-11-11 reference
installation. The number deliberately rounds above the measured average so an
operator can estimate capacity without reconstructing every table and index.

The useful first approximation is:

```text
live database = 250 MB × clients × retained days
```

At a 365-day raw-evidence horizon, that is about 91 GB per similar client before
adding filesystem snapshots, WAL retention, operating-system data and free-space
headroom. TimescaleDB and filesystem compression can reduce physical allocation,
but capacity planning must not depend on an unmeasured future compression ratio.
Re-measure after changing receiver message rates, adding high-cadence telemetry,
or deploying a materially different station profile.

This estimate is not feeder-spool consumption, network traffic or daily backup
transfer size. Those have different framing, compression and retention behavior.

### Separate observation growth from backup churn

The reference database generated about 18 GB of logical WAL in the seven days
preceding the measurement, while its current physical database reference grew by
roughly 160–180 MB per day. Neither number is the daily backup-stream size. The
WAL statistic is cumulative and includes segments PostgreSQL later recycled;
`pg_wal` occupied only about 79 MiB at the measurement instant.

The backup system sends recursive ZFS incrementals with `zfs send -c`, so blocks
that changed between snapshots travel in their compressed on-disk form. This is
changed-block replication, not a copy of only the files or rows that made the
live database larger. See the OpenZFS
[send and receive description](https://openzfs.github.io/openzfs-docs/Basic%20Concepts/Operations/Send%20and%20Receive.html).

Six consecutive 24-hour compressed-send estimates from the reference backup
receiver, ending 2026-11-10, separated the two dominant datasets:

| Dataset | Daily range | Daily mean | What the measurement includes |
|---|---:|---:|---|
| Host root | 1.16–1.72 GiB | 1.48 GiB | Operating-system files, application files and caches |
| PostgreSQL | 0.82–1.01 GiB | 0.87 GiB | New and changed database, index and WAL blocks |

The last pre-exclusion recursive stream was 2,704 MiB (2.64 GiB) by the puller's
estimate; about 1.60 GiB was the host root, 1.01 GiB was PostgreSQL and 35 MiB
was the remaining datasets and replication metadata. With two receivers
independently advancing daily copies of essentially the same source state, that
design sent approximately 145–160 GiB per 30 days across both backup links. That
was a backup bandwidth estimate, not retained customer data.

The root-dataset cost has a concrete, non-database cause. Its file-level snapshot
diff was dominated by `/var/cache/rpki-client`, a regenerable validation cache
that occupied 3.18 GiB and was refreshed daily. Its size and rewrite cadence are
consistent with most of the 1.48 GiB average compressed root stream. This cache
is useful to its producer but does not need historical disaster-recovery copies.

Production removed that churn on 2026-11-11. The warm cache moved to the
dedicated `zroot/rpki-cache` dataset. The forced backup command now removes that
dataset's just-created backup snapshot and uses OpenZFS `-X` to omit the subtree
from the recursive stream, as documented by FreeBSD's
[`zfs-send(8)`](https://man.freebsd.org/cgi/man.cgi?query=zfs-send&sektion=8).
OpenZFS also needs `-s` because that snapshot is deliberately missing, so the
wrapper first proves that every non-cache dataset has the target tag and that no
excluded snapshot remains. This keeps the stream resumable and fail-closed: the
cache consumes no new source-snapshot, wire or receiver-snapshot space, while an
unrelated missing dataset stops the backup.

Both live transition runs passed GUID-for-GUID verification and moved their
holds. The 13.7-hour navbk62 transition estimated 654 MiB and completed in 31
seconds; the 3.7-day navbk61 transition estimated 1,745 MiB and completed in 175
seconds. These unequal windows prove the exclusion path but are not a new daily
steady-state estimate. Re-measure at least seven complete daily intervals before
replacing the pre-exclusion capacity figure. Legacy receiver snapshots still
contain the old cache blocks and expire under normal retention.

The PostgreSQL delta is real changed-block churn but is not 0.87 GiB of new
long-lived observations. New rows and indexes, TimescaleDB chunk compression,
database maintenance, checkpoints and recycled files can all replace blocks
without growing the live dataset by the same amount. The snapshot endpoints and
send estimate do not attribute those bytes precisely enough to label the whole
delta as WAL. In particular, the current `pg_wal` footprint was much smaller
than the measured PostgreSQL stream.

The WAL present in a complete, atomic database snapshot is not disposable. It
lets PostgreSQL recover that running cluster to a consistent state, as the
existing restore rehearsal demonstrated. Do not omit `pg_wal`, copy only selected
files from the data directory or otherwise weaken the tested snapshot while
trying to save wire bytes. PostgreSQL's
[filesystem-backup guidance](https://www.postgresql.org/docs/17/backup-file.html)
describes the consistency requirement.

Continue in this order:

1. Record the compressed recursive estimate and per-dataset estimates for each
   backup run. Alert on both failed runs and unexplained growth.
2. Keep the cache exclusion narrow. Configuration, credentials, validated RPKI
   output and every non-regenerable file remain in the recursive stream. The
   forced command must continue to reject any missing non-cache snapshot.
3. Rehearse both a database restore and a complete-host restore after material
   storage changes. The post-exclusion root-clone check confirmed an empty cache
   mountpoint, retained configuration and output, and the deployed wrapper, but
   it does not replace the existing PostgreSQL crash-recovery rehearsal.
4. If database churn becomes the limiting cost as clients are added, evaluate
   PostgreSQL 17 incremental base backups plus continuous WAL archiving. Compare
   measured wire bytes, retained bytes, restore time and point-in-time recovery
   before changing methods. PostgreSQL documents both
   [incremental base backups and continuous archiving](https://www.postgresql.org/docs/17/continuous-archiving.html);
   a PostgreSQL-aware method does not make the required WAL chain optional.

The known host-cache efficiency debt is removed without weakening PostgreSQL
recovery. The remaining database changed-block multiplier is neither a recovery
failure nor an architectural corner; measure it before deciding whether a
PostgreSQL-aware backup method would be materially better.

### Brief planned downtime is currently acceptable

The present deployment deliberately uses one PostgreSQL primary with no live
standby. The operating target accepts up to five minutes of planned database
downtime per month for updates and reboots. The production feeder's durable ACK
and persistent spool make that a comfortable maintenance window, provided the
normal stop-database-start-validation sequence is followed and spool loss
counters remain zero.

Do not add database HA solely to remove this small accepted maintenance window.
The next availability step, when justified, is a physical streaming standby plus
continuous WAL archiving and a rehearsed promotion procedure, not logical
multi-primary replication. TimescaleDB supports PostgreSQL streaming replication
and does not recommend ordinary logical replication for hypertables; see its
[high-availability guidance](https://www.tigerdata.com/docs/deploy/self-hosted/replication-and-ha/about-ha).

This is a reversible staging point, not an architectural dead end. Production
already runs with `wal_level = replica`; a physical standby can be added without
changing the historian schema or feeder protocol. Retention and chunk intervals
are explicit policies, the source rows carry collector identity, and the feeder
reconnect/replay contract tolerates a controlled promotion window. Adding HA will
require another host, private replication access, monitoring, routing, fencing
and a tested runbook, but not a redesign of the stored observation model.

Choices that would create avoidable future constraints are different: ordinary
logical multi-primary replication of hypertables, arbitrarily splitting
neighbouring stations between live fusion processes, or moving PostgreSQL onto a
volume that the backup system does not cover. None of those is part of the
current deployment.

Revisit that decision when any of these becomes true:

- the acceptable planned outage becomes materially shorter than five minutes;
- losing acknowledged data since the latest completed backup is unacceptable;
- measured restore time no longer meets the recovery target;
- read traffic needs a dedicated replica;
- maintenance or unplanned database interruptions become frequent; or
- several active collector processes need a formally owned schema, policy and
  failover topology.

Physical replication does not make the collector's in-memory fusion state
multi-primary. Regional environmental context and other neighbour comparisons
still require the relevant stations to reach the same active collector, or an
explicit shared/federated fusion design.

## Why the production settings are larger

The active settings are:

```conf
max_connections = 40
max_locks_per_transaction = 1024
max_worker_processes = 13
max_parallel_workers = 2
timescaledb.max_background_workers = 8
```

`max_locks_per_transaction`, `max_worker_processes` and
`timescaledb.max_background_workers` were tuned together on 2026-11-11.
`max_connections` and `max_parallel_workers` were retained. These are not
portable defaults for an arbitrary database.

### Lock table

At the old `max_locks_per_transaction = 64`, a manual
`SELECT count(*) FROM nav_frames` failed with `out of shared memory` once the
hypertable had more than a thousand chunks. That message referred to PostgreSQL's
shared lock table, not exhausted host RAM. A whole-hypertable statement takes
locks on the chunk relations and their indexes before it reads their rows.

The production horizon would have approached 8,800 one-hour chunks. TimescaleDB's
self-hosted guidance estimates the lock setting as twice the largest chunk count
divided by `max_connections`, then calls for growth headroom. That gives 440 for
8,800 chunks and 40 connections; 1024 leaves room for indexes, other relations
and concurrent work. PostgreSQL documents this as an average lock-object budget
per server process or prepared transaction: one transaction can exceed it while
the shared table still has capacity. The setting is startup-only.

After the restart, the exact formerly failing aggregate counted 74,769,006 rows
in 5.72 seconds. That timing is one observation, not a latency target. Its purpose
was to prove that planning and execution could lock the complete hypertable.

References: [PostgreSQL lock management](https://www.postgresql.org/docs/17/runtime-config-locks.html)
and [TimescaleDB self-hosted troubleshooting](https://www.tigerdata.com/docs/deploy/self-hosted/troubleshooting).

### Chunk interval

One production hour held about 57,000 rows, 54 MB in total and 12 MB of indexes.
That was far too small to justify a separate relation. A day projects to about
288 MB of active indexes, below the documented target of 25% of the host's 4 GiB
memory. Compression begins after one day, so old row-oriented indexes do not
accumulate indefinitely.

`set_chunk_time_interval` affects new chunks only. The 1,358 historical chunks
were not rewritten or merged during the rollout; retention will age them out.
This matters both for rollback and for interpreting chunk-count graphs after the
change. The count should decline gradually, not collapse immediately.

Reference: [TimescaleDB hypertable performance](https://www.tigerdata.com/docs/build/performance-optimization/improve-hypertable-performance).

### Background workers

The database has twelve scheduled jobs: six compression jobs, four retention
jobs, TimescaleDB job-history retention and extension telemetry with telemetry
disabled by configuration. With only four TimescaleDB worker slots, PostgreSQL
logged repeated job-launch starvation even though delayed jobs later succeeded.

Eight TimescaleDB slots allow one scheduler for the database and up to seven
concurrent jobs. `max_worker_processes = 13` covers those eight, two parallel
query workers and three slots of margin for other registered background workers.
If another TimescaleDB database, subscription or worker extension is added,
recalculate the total rather than consuming that margin silently.

## Controlled rollout

These settings require a PostgreSQL restart. The chunk interval does not, but it
should ship in the same collector revision so every later writer startup keeps
the policy reconciled.

1. Commit and push the application schema and deployment configuration first.
2. Build the exact collector revision and record its SHA-256 digest.
3. Stage the binary and PostgreSQL include, then validate both without stopping
   services. Run the collector with `-check-config`, and run `postgres -C` as the
   PostgreSQL operating-system user for every startup setting.
4. Record health, newest-frame age, feeder connection state and store error,
   drop and quarantine counters.
5. Copy the active include and collector binary to a timestamped, root-only
   maintenance directory.
6. Stop `navlistener`. The edge feeder's durable spool covers the planned gap.
7. Install both files atomically, restart PostgreSQL, and verify the live values
   through fresh SQL connections.
8. Start `navlistener`. Its schema startup applies the one-day interval for future
   chunks and reconciles the policy jobs.
9. Wait for collector health and feeder authentication, then verify fresh frames,
   zero store loss counters, job success and public API health.
10. Run the formerly failing whole-hypertable aggregate with a statement timeout.

The 2026-11-11 maintenance stopped the feeder-facing collector for about 36
seconds. The feeder reauthenticated, the newest stored frame was one second old,
and store errors, dropped rows and quarantined rows remained zero. All ten
compression and retention jobs ran successfully immediately after restart, and
the job statistics showed no failures. Collector and station health endpoints
returned 200 over both IPv4 and IPv6.

### Rollback

If PostgreSQL does not start, restore the previous include before doing anything
to the database:

```sh
cp -p /root/maintenance/<run>/navlistener.conf.before \
  /usr/local/etc/postgresql/navlistener.conf
service postgresql start
```

If the collector fails its health check, restore its previous binary and start it
against the recovered database:

```sh
cp -p /root/maintenance/<run>/navlistener.before /usr/local/bin/navlistener
service navlistener start
```

Restoring the old binary does not undo a successfully applied chunk interval.
The interval can be set back to one hour for future chunks, but an already created
one-day chunk cannot be split in place. That is normally harmless, so do not
rewrite or drop chunks as part of an emergency rollback.

## Routine validation

Run catalog queries as a role allowed to read the TimescaleDB information views.
These are read-only unless explicitly noted.

```sql
-- Startup settings and the file that supplied each one.
SELECT name, setting, sourcefile, sourceline
  FROM pg_settings
 WHERE name IN (
   'max_connections',
   'max_locks_per_transaction',
   'max_worker_processes',
   'max_parallel_workers',
   'timescaledb.max_background_workers'
 )
 ORDER BY name;

-- Current interval and the historical chunk count.
SELECT hypertable_name, column_name, time_interval
  FROM timescaledb_information.dimensions
 WHERE hypertable_name = 'nav_frames';

SELECT hypertable_name,
       count(*) AS chunks,
       count(*) FILTER (WHERE is_compressed) AS compressed
  FROM timescaledb_information.chunks
 GROUP BY hypertable_name
 ORDER BY hypertable_name;

-- Policy definitions and scheduler results.
SELECT job_id, hypertable_name, proc_name, schedule_interval, config
  FROM timescaledb_information.jobs
 ORDER BY job_id;

SELECT hypertable_name, job_id, last_run_status, job_status,
       last_run_started_at, last_successful_finish,
       total_runs, total_successes, total_failures
  FROM timescaledb_information.job_stats
 ORDER BY job_id;

-- Storage cost. This touches every nav_frames chunk and therefore exercises
-- the same relation-lock budget as other whole-hypertable maintenance.
SELECT * FROM hypertable_detailed_size('nav_frames');

-- Explicit lock-budget proof. Keep a timeout on manual production diagnostics.
SET statement_timeout = '2min';
SELECT count(*) FROM nav_frames;

-- Freshness without turning a missing stream into a zero or a healthy state.
SELECT clock_timestamp() AS checked_at,
       max(ts) AS newest_frame,
       clock_timestamp() - max(ts) AS age
  FROM nav_frames;
```

For a bounded history query, calculate an absolute UTC cutoff in the client and
bind it as a parameter:

```sql
SELECT count(*), max(ts)
  FROM nav_frames
 WHERE ts >= $1::timestamptz;
```

Do not casually replace `$1` with `now() - interval '10 minutes'` in a diagnostic.
Depending on the expression and plan, a non-constant cutoff can prevent plan-time
chunk exclusion and acquire locks on every chunk. The application history paths
already pass fixed timestamp parameters.

Collector validation must include both health and loss counters:

```sh
curl -fsS http://127.0.0.1:9100/healthz
curl -fsS http://127.0.0.1:9100/metrics | \
  egrep 'navlistener_build_info|navlistener_push_observers_up|navlistener_store_(errors|dropped|quarantined)_total'
```

Healthy database connectivity is not proof of fresh ingest. Require a connected
feeder, a recent non-NULL `max(ts)`, and unchanged zero loss counters. If no rows
exist, `max(ts)` must remain NULL; do not coalesce it to zero or treat it as
recovery.

## Diagnosing the two failure modes

`out of shared memory` with a hint about `max_locks_per_transaction` means the
lock table is exhausted. Check chunk growth, active statements and the live
startup setting before considering host memory:

```sql
SELECT hypertable_name, num_chunks
  FROM timescaledb_information.hypertables
 ORDER BY num_chunks DESC;

SELECT pid, usename, state, xact_start, query_start, wait_event_type,
       wait_event, left(query, 160) AS query
  FROM pg_stat_activity
 WHERE pid <> pg_backend_pid()
 ORDER BY xact_start NULLS LAST, query_start;
```

`failed to start a background worker` means scheduled jobs cannot obtain a
worker slot. Compare the three worker settings, inspect `job_stats`, and account
for every TimescaleDB database and other registered worker. Do not infer data
loss from one delayed launch; verify each job's last success and the retention or
compression state it was meant to enforce.

In either case, preserve missing or stale evidence as unknown. A stopped policy
job, disconnected feeder or failed historian write is not a normal measurement
and is not evidence that an earlier anomaly recovered.

## Comparable measurements

Use the following read-only inventory when revisiting the sizing decision. Record
the date, PostgreSQL and TimescaleDB versions, host memory, configured retention
and ingest rate beside the output.

```sql
SELECT version();
SELECT extversion AS timescaledb_version
  FROM pg_extension
 WHERE extname = 'timescaledb';
SELECT pg_size_pretty(pg_database_size(current_database())) AS database_size;

SELECT stats_reset, wal_records, wal_fpi,
       pg_size_pretty(wal_bytes::bigint) AS wal_bytes
  FROM pg_stat_wal;

SELECT min(ts) AS oldest_frame,
       max(ts) AS newest_frame,
       count(*) AS raw_frames
  FROM nav_frames;

SELECT * FROM hypertable_detailed_size('nav_frames');

SELECT hypertable_name,
       count(*) AS chunks,
       count(*) FILTER (WHERE is_compressed) AS compressed
  FROM timescaledb_information.chunks
 GROUP BY hypertable_name
 ORDER BY hypertable_name;
```

On the reference ZFS host, record live allocation separately from snapshot-held
blocks:

```sh
zfs get -Hp -o property,value referenced,used,available zroot/postgresql
zpool list -Hp -o size,alloc,free,cap zroot
```

Use a dry-run send estimate to measure wire churn between two retained backup
snapshots. The recursive estimate should reconcile with the puller's logged
estimate; individual datasets explain the total:

```sh
zfs send -nvP -c -L -e -s -R -X zroot/rpki-cache -I zroot@<older-tag> zroot@<newer-tag>
zfs send -nvP -c -L -e -I zroot/ROOT/default@<older-tag> zroot/ROOT/default@<newer-tag>
zfs send -nvP -c -L -e -I zroot/postgresql@<older-tag> zroot/postgresql@<newer-tag>
```

The recursive estimate logs an expected warning that the excluded cache's
target snapshot does not exist. Any other skipped dataset is a backup failure,
not another harmless warning.

Take these readings on a schedule. A difference between two `referenced` values
approximates live filesystem growth; `used - referenced` includes space retained
by snapshots and descendants. Neither is interchangeable with
`pg_database_size`, which reports PostgreSQL's logical relation footprint rather
than filesystem compression and snapshot retention.

Revisit the one-day interval if the active chunk's indexes approach 25% of main
memory, if ingest rate changes materially, or if compression can no longer keep
up. Revisit the lock budget before a longer retention horizon, a new hypertable
with more chunks, a lower connection limit, or a standby deployment. A standby's
lock setting must be at least the primary's.
