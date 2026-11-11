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

Revisit the one-day interval if the active chunk's indexes approach 25% of main
memory, if ingest rate changes materially, or if compression can no longer keep
up. Revisit the lock budget before a longer retention horizon, a new hypertable
with more chunks, a lower connection limit, or a standby deployment. A standby's
lock setting must be at least the primary's.
