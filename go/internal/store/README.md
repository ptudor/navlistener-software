# `internal/store` — the TimescaleDB historian

**Headline:** the forensic system of record. Raw broadcast frame bytes stored side by side with a
decoded JSONB projection, so a decoder fix can re-derive every historical ephemeris **without
re-collecting**. Plus the durable integrity-event table and periodic feed snapshots. It runs off
the live hot path: a slow database degrades the historian, never live decoding.

---

## Index — what's in this folder

| File | What it is |
|---|---|
| `store.go` | `Store`, the batched `CopyFrom` writer goroutine, schema bootstrap, retention/compression policies, and the degraded-health probe. |
| `events.go` | `EventRow`, `StoredEvent`, `EventQuery`, event writes and the windowed read API. |
| `observer_history.go` | Private sensor samples filtered by receipt-time audience and collector, with bounded pages. |
| `evidence.go` | Station event evidence: `CaptureEventEvidence` copies an event's stored input window into retention-less tables; `QueryEventEvidence` pages it back under the same per-sample cap and page byte budget as the sensor history. |
| `schema.sql` | The complete DDL — three hypertables, the dedup ledger, indexes, compression settings, and the `pg_notify` trigger. Applied at startup. |
| `*_test.go` | Batching, dedup, policy application, event query bounds, and the degraded path. |
| `README.md` | This file. |

**TimescaleDB is required** — no plain-PostgreSQL fallback. Enabled only when `[store].dsn` is
set.

---

## Summary

```go
func New(ctx, cfg config.Store, log *slog.Logger) (*Store, error)
func (s *Store) Run(ctx context.Context)                         // the writer goroutine
func (s *Store) Enqueue(f *NavFrame)                             // bounded, drop-on-overflow
func (s *Store) WriteEvent(ctx, e EventRow) (int64, error)        // direct, idempotent
func (s *Store) WriteSnapshot(ctx, at, audience, endpoint string, data []byte) error
func (s *Store) QueryEvents(ctx, q EventQuery) ([]StoredEvent, int, error)
func (s *Store) QueryObserverSamples(ctx, q ObserverSampleQuery) (ObserverSamplePage, error)
func (s *Store) CaptureEventEvidence(ctx, collectorID string, now time.Time, p EvidencePolicy) (int, error)
func (s *Store) QueryEventEvidence(ctx, q EvidenceQuery) (EventEvidence, error)
func (s *Store) SummarizeEvents(ctx, since, until) (EventSummary, error)
func (s *Store) SetDurableNotify(fn func(source, session string, seq uint64))
func (s *Store) Degraded() string
```

**Two very different write paths, on purpose:**

- **Raw frames** — high rate, batched through a bounded queue with `pgx CopyFrom` (bulk load,
  never row-by-row INSERT).
- **Events and snapshots** — low rate, written directly and synchronously, because an integrity
  event losing a race to a queue overflow would be unacceptable.

---

## Details

### The bounded queue and the drop policy

`Enqueue` is non-blocking. When the queue is full — by frame count or by payload bytes, see
"The writer queue is bounded by frames and by bytes" below — the frame is **dropped and counted**
(`store_dropped_total`) — an explicit, metered policy rather than blocking ingest or growing
without bound. The first drop after a clean flush cycle logs one warning; the rest of that
streak is visible only through the counter and, from the second consecutive overflowing cycle,
through `Degraded()`.

This is the deliberate priority ordering: **live decoding and the integrity monitor matter more
than the forensic archive.** If the database stalls, the daemon keeps decoding, keeps detecting,
and keeps serving; what degrades is history. `Degraded()` surfaces that to `/healthz` as
`degraded` (HTTP 200, not 503) so a DB blip can't flap rc.d into a restart loop.

### The pool and the writer's connection

One `pgxpool` serves the batched writer, the direct event/snapshot/evidence writers and every
historian query the read API runs. Its size comes from `[store].max_conns` (or the DSN's
`pool_max_conns`, or the default of 8 — `config.Store.PoolConfig` is the one place it is
decided). The writer goroutine **holds one connection of that pool for its whole life** —
acquired when `Run` starts, handed back and re-acquired after a failed persist (a context cut
mid-statement closes the connection underneath it), released before the pool closes — so a
burst of API reads that occupies every other connection can never make the writer wait behind
them through its retry budget and drop the batch. The read side is bounded separately in
`serve` (query slots for the public event endpoints, history slots for the authenticated
reads), both sized below the pool.

One frame never reaches the queue at all: a `NavFrame` with a **nil `Raw`**. `raw` is
`BYTEA NOT NULL`, so a nil would map to SQL NULL and **poison the whole batch** — bisected,
quarantined, and dropped with only a generic log. It is rejected up front, counted as
`store_empty_raw_total`, **and resolved for the durability watermark**, because it is unfixable
by retransmit (the frame's own content is the problem) and leaving it outstanding would wedge the
feeder's ACK forever. A non-nil empty `[]byte{}` is a legitimate empty bytea and passes through.

### What a failing flush does

The writer's failure handling is layered, and each layer has a different accounting:

| Situation | Behavior | Metric |
|---|---|---|
| Retryable DB error | Bounded retry with backoff. | — |
| A poison row inside a batch (SQLSTATE class 22: a NUL in JSON, an out-of-range smallint) | The batch is **bisected** to isolate it; the bad row is quarantined — logged with its source, session, sequence, table, kind, satellite and SQLSTATE, and **acked**, since no retransmit can fix it — and the good ones commit. | `store_quarantined_total`, `store_quarantined_rows_total{table}` |
| A unique or check violation (23505, 23514) | Bisected like poison, but a single row is quarantined and acked **only once a sibling row of the same cycle has committed** under the same constraints. A cycle in which nothing commits is a constraint every row trips: nothing is acked, the batch stays replayable, the cycle counts as a give-up. | as poison, or `store_retry_dropped_total` |
| Any other class-23 violation (23502 not-null, 23503 foreign-key, …) | A constraint every row trips — a newer schema's column, an operator's constraint. No bisection, no ack: the cycle gives up at once, the batch stays replayable from the feeder's spool, and two such cycles degrade `/healthz`. | `store_retry_dropped_total` |
| Retries or the wall budget exhausted, parent still live | The batch is dropped unacked and counted — the writer must stay live and memory-bounded through a long outage; the feeder replays it. | `store_quarantined_total`, `store_retry_dropped_total` |
| Shutdown interrupts a flush | The batch is **retained** for the bounded shutdown drain rather than cleared. Nothing was committed, and the atomic claim+copy transaction rolled its replay-key claims back, so the re-flush cannot duplicate. | — |

The claim-and-copy being one transaction is what makes that last row safe: bisection, retry, and
retained re-flush all preserve the invariant that a dedup claim exists if and only if the row
committed.

`Degraded()` watches all three ways frames are discarded, each with the same anti-flap rule —
one bad flush cycle never degrades, two consecutive ones do:

| Streak | A cycle counts when | Cleared by |
|---|---|---|
| Flush failures | the flush exhausted its retries or wall budget | a successful persist, or the idle pool probe |
| Queue overflow | `Enqueue` dropped any frame since the previous cycle (a database that succeeds too slowly to keep up) | a cycle with no drops, or a full ingest-quiet window |
| Quarantine flood | more than a tenth of the rows the cycle flushed were quarantined | a cycle below that share, or a full ingest-quiet window |

A lone poison row in a healthy batch is what bisection is for and never degrades health; a
batch that is mostly poison is a systemic fault the operator must see.

### Startup schema checks

`schema.sql` is additive and idempotent, so an older binary runs against a newer database until
a change lands that an older writer trips on every row — a NOT NULL column without a default, a
tightened CHECK. Two startup checks catch that before the first batch instead of after it:

- **The schema marker.** `navlistener_schema` holds the schema generation (`schemaVersion` in
  `store.go`) the last build to apply the schema knew. `store.New` refuses to start when the
  stored version is higher than its own, and only ever moves the marker forward. Bump
  `schemaVersion` with any change an older writer cannot satisfy; never for a purely additive
  migration.
- **Unsupplied NOT NULL columns.** `verifyRequiredColumns` fails fast when any table this build
  writes has a NOT NULL column without a default (and neither identity nor generated) that the
  writer's column list does not supply, naming the table and the columns — the mirror image of
  its missing-column check.

### `nav_frames` — the forensic record

```sql
ts          TIMESTAMPTZ  -- INGEST time (the hypertable dimension)
received_at TIMESTAMPTZ  -- receiver/host reception time (separately indexed)
source_id   TEXT
organization_id, enrollment_id, collector_instance_id  TEXT
collection_ids  TEXT[]     -- receipt-time collection memberships
feed_grants, declared_capabilities  TEXT[]  -- receipt-time admission/capability evidence
provenance      TEXT       -- local or an inbound federation peer
credential_tier, credential_fingerprint, attestation_tier  TEXT
hardware_trust, commissioning_fingerprint  TEXT  -- verified session evidence; 'none' / ''
aggregate_use, station_metadata, event_visibility, raw_export, policy_revision  TEXT
federation_peers, publish_signals  TEXT[]
gnssid, svid, sigid, msg_type  SMALLINT
raw         BYTEA        -- the broadcast frame, untouched
decoded     JSONB        -- normalized projection, nullable
decoder_ver TEXT
```

**Why the hypertable dimension is ingest time, not event time:** radio and GNSS data arrives late
and out of order. A hypertable wants a near-monotonic dimension so chunks stay tight and
compression works; `received_at` is the semantically interesting clock and gets its own index.
Chunks are 1 hour.

**Why `raw` and `decoded` both:** `raw` means a decoder fix can re-derive every historical
ephemeris from stored bytes. `decoded` means the common queries don't have to. `decoder_ver`
records which version produced a projection, so a re-decode pass knows what to redo. This is the
same discipline as the radiolistener sibling, and it's why the daemon needs no cross-restart
state file — the historian *is* the durable record.

**Why the administrative columns live on every receipt:** organizations, group membership, and
publication consent change over time. Rejoining historical raw data to today's control-plane row
would silently rewrite the policy under which it was received. These columns are therefore an
immutable authorization snapshot. Legacy rows and omitted config default to
`local-unassigned/private`, never public.

### The dedup ledger — `nav_frames_seq_seen`

On feeder reconnect, `navfeeder` replays every DATA frame past the last ack it received. Decode
and live state tolerate the duplicate ("replay is harmless"), but the raw historian must not
 — otherwise a routine reconnect, or any collector restart, permanently duplicates rows
for one receiver and pollutes the dedup-on-read "N receivers saw this SV" integrity signal with
single-receiver replay artifacts.

`nav_frames` itself can't carry a UNIQUE constraint for this: Timescale requires a hypertable's
unique index to include its partition column, and `ts` legitimately differs between a frame and
its replay (ingest time, not broadcast time). So the dedup key lives in a small **side ledger
with a real, non-partitioned unique constraint**. The writer claims each push-path frame's
`(source_id, session_id, feeder_seq)` there **in the same transaction** that `CopyFrom`s the
newly seen rows — so a copy or commit failure rolls the claim back and leaves the edge replay
retryable.

**`session_id` is load-bearing.** Replay identity is *(canonical observer, session,
seq)*, not *(observer, seq)*. Without the session, a feeder that restarted without a recoverable
spool — or any ESP32 reboot, since its ring is RAM-only — reset its sequence to zero, and the
ledger's old rows then classified every fresh post-reboot frame as a replay until the new counter
passed the old high-water mark. That is **silent loss of fresh forensic frames after a routine
reboot**. A fresh session per sequence space makes the collision structurally impossible; the
feeder persists the session in its disk-spool header so a spool-recovering restart continues
session and sequence together.

**The key is normalised.** One row per sequenced frame at ~26 frames/s per receiver is ~16 M
rows per receiver-week, and two TEXT key columns (a 32-character session id and a free-form
observer id) stored in the heap and again in the primary key cost about 3 GB per
receiver-week — several times the compressed frames the ledger protects. So the
`(source_id, session_id)` pair lives once in `nav_frames_sessions` (`session_key BIGINT`
identity, `UNIQUE (source_id, session_id)`, `first_seen`) and the ledger is keyed
`(session_key, feeder_seq)`: 24 bytes of payload and a 16-byte index entry per claim. The
writer resolves a session's key once per process (`resolveSessionKeys`, one round trip that
creates missing sessions and returns existing ones) **inside the claim transaction**, so a
rolled-back commit rolls the new session back too, and the key reaches the in-memory cache
only after the commit. The claim keeps its `INSERT … ON CONFLICT DO NOTHING RETURNING` shape.
A pre-normalisation ledger (TEXT-keyed, with or without `session_id`) is dropped and recreated
by the guarded `DO $$` migration in `schema.sql`; the schema marker (version 2) keeps an
older binary, whose own migration block would drop the re-keyed table, from starting.
Version 3 re-keyed the two point-state tables below by collector, for the same reason: a
version-2 binary's checkpoint upsert names a constraint that no longer exists.

Dial-mode frames carry no feeder sequence and always pass through unfiltered — duplicates across
*different* receivers are intentional and untouched by this table.

**The prune is chunked and off the writer.** Every `pruneEvery` (1 h) a sweep on its own
goroutine deletes entries older than `raw_retention` in chunks of `pruneChunkRows` (10 000),
each its own statement under `pruneChunkTimeout` (10 s), until nothing is left or the
`pruneSweepBudget` (20 min) expires. Progress is durable per chunk: a backlog one sweep
cannot finish is continued by the next, where one whole-table `DELETE` under one timeout was
cancelled and rolled back entirely at fleet scale, deleting nothing while the table grew. The
writer loop keeps consuming the queue during a sweep; `Run` waits a sweep out before closing
the pool. Sessions with no entries left and a `first_seen` older than the window are retired
by the same sweep and evicted from the key cache.

### The writer queue is bounded by frames and by bytes

`Enqueue` never blocks: the queue holds at most `queueDepth` (16 384) frames **and** at most
`maxQueueBytes` (64 MiB) of frame payload (`raw`, `decoded`, board/RF data, the authority
evidence snapshot, plus a 512-byte allowance per frame for the struct and its provenance
strings). Whichever bound is reached first drops the newest frame, counted in
`navlistener_store_dropped_total` and the same overflow streak that feeds `Degraded()`; the
log line names the bound. The byte bound exists so the historian's memory no longer depends
on upstream validation of record sizes (ingest separately refuses navigation record bodies
over 4 KiB).

### `SetDurableNotify` — closing the ACK loop

The store notifies per committed (or deduped, or quarantined) batch entry, and that notification
is what advances `ingest.DurableTracker`'s watermark, which is what the collector sends in a GNF1
ACK, which is what lets the feeder prune its spool. See `../wire/README.md` for why that chain
has to be durable rather than receipt-based.

### `gnss_events` — retention-less, deduped, and notify-driven

The durable record of confirmed integrity transitions. It is a **superset of intsat's
`001_init.sql`**, so intsat's read path keeps working while it's still the serve front.

**No retention policy, ever.** Confirmed integrity transitions are the durable record. That is
precisely why compression matters here: a flapping detector over a multi-month run would
otherwise accumulate uncompressed row-oriented chunks without bound. Compression is *enabled* in
the DDL (segment by `audience,event_type`) and the 30-day *policy* is applied by the store at startup —
and note that enabling compression later requires the `ALTER` first; a policy alone cannot do it
.

**Idempotency.** `WriteEvent`'s INSERT can commit while the client observes a timeout
or connection error; a bounded retry would then store, notify, and serve the same transition
twice — two rows, two durable SSE ids. So every event carries a `dedupe_key` generated **once per
confirmed transition, before the first write attempt** (in `cmd`'s `prepareEvent`), and the retry
becomes an idempotent upsert. The unique index is `(time, dedupe_key)` because a hypertable's
unique index must include the partition column — safe, because retries reuse the identical
`EventRow`, `time` included. Legacy and intsat rows have NULL keys and never conflict under
PostgreSQL's NULLS DISTINCT.

**Audience-local cursors.** Every row carries `audience`, `audience_seq`, and
`redaction_class`. `WriteEvent` atomically increments `gnss_event_audience_cursors` and returns
`audience_seq` to SSE. Public ids therefore do not acquire gaps when operator-only events occur.
The idempotent retry first finds an already committed `dedupe_key`, so a commit-ack timeout does
not consume a second visible sequence. Queries and summaries require one audience predicate.
Legacy rows migrate to `legacy-operator` with their old internal id as a one-time private cursor.

**The notify contract.** An `AFTER INSERT` trigger fires `pg_notify('gnss_event_v2', …)` for
the `public` audience only, carrying the audience-local `id`, internal `row_id`, audience,
`sv`, `type`, and `severity`. `message` is intentionally omitted rather than truncated:
`pg_notify` has a hard ~8000-byte payload limit, and a long message would *raise in the trigger
and fail the whole INSERT in the same transaction*. Migrated consumers read the
`gnss_events_public` view or fetch by `(audience, audience_seq)`. The legacy unscoped channel is
not emitted; letting an old consumer interpret a public sequence as a global row id could fetch
private data.

`idx_gnss_events_id` remains for the internal `row_id` lookup : a hypertable PK must include the partition
column, so `id` has no index by default — and without an explicit one, every notify-driven
`SELECT ... WHERE id = $1` seq-scans **every chunk** of this retention-less table. That cost
lands entirely on the external consumer, since navlistener itself never queries by id. It would
have been invisible from inside this repo.

### Event evidence — `event_evidence` and `event_evidence_samples`

Raw samples expire with `raw_retention`; events never do. So the daemon's evidence sweeper
(`cmd/navlistener/evidence.go`, every 30 s) calls `CaptureEventEvidence`, which finds this
collector's station events (`spoofing_suspected`, `station_assurance`, `jamming_detected`,
`station_rf_degraded`, `antenna_fault`) that are at least the post-roll old, still within the
horizon raw retention covers, and have no `event_evidence` row. For each it copies the station's
`rf_samples` and `observer_samples` from ten minutes before to one minute after into
`event_evidence_samples`, in one transaction with the bundle row, scoped like the sensor-history
reads: an organization audience gets its organization's samples, a collection audience its
collection's, an operator audience the collector's. The bundle row is inserted first, so a
concurrent capture waits on its primary key and then skips. Capture state is entirely in the
database: a failed sweep, a restart or a crash just leaves the event for the next sweep. Each
origin is capped at the policy's sample bound, newest first, so a bundle that hits it keeps the
event instant and the post-roll and loses the oldest pre-roll samples; the copy and the
`truncated` decision are one statement over the same rows, so a capped bundle is always marked.

`gnss_events.collector_instance_id` is what lets each collector find its own events when several
share a database; rows written before the column existed are never captured.

### `gnss_snapshots`

Periodic dumps of each served feed's current body (`svs`, `global`, `observers`, `almanac`,
`sbas`) for replay and backfill. Active only when both `[serve].addr` and `[store].dsn` are set;
cadence is `[serve].snapshot_interval` (default 5m, `0s` disables). Every row is keyed and
compressed by `(audience, endpoint)`, so an operator payload cannot be replayed through a public
cache. The snapshot loop writes all materialized audience views, not only the listener's default
public/operator view.

### Retention and compression policies

Applied at startup from config:

| Setting | Default | Applies to |
|---|---|---|
| `raw_retention` | `"7 days"` | `nav_frames` drop policy (and the dedup ledger's prune) |
| `compress_after` | `"1 day"` | `nav_frames` compression policy |
| — | 30 days | `gnss_events` compression (fixed, not configurable) |

Both configurable values are PostgreSQL INTERVAL literals validated against
`config.IntervalRe` — which is simultaneously the format allowlist **and the injection guard**
for interpolating that string into policy DDL. There is exactly one definition of that
regex, in `config`, and it must never be relaxed.

Raw frames are the **short-window** forensic record. This setting does not retain a long-term
decoded ephemeris aggregate; events and snapshots live in their own tables with their own
policies.

### Event read API

```go
type EventQuery struct { /* window, filters, paging */ }
func (s *Store) QueryEvents(ctx, q) ([]StoredEvent, int, error)
func (s *Store) SummarizeEvents(ctx, since, until) (EventSummary, error)
```

Windowed reads over the historian, bounded per `docs/OUTPUT.md §2.1`, backing `serve`'s
`/gnss/api/events` and `/gnss/api/events/summary`. The page bounds are enforced here —
`EventQuery.Validate` refuses a limit outside 1..500, an offset outside 0..1 000 000 or an
inverted window before the database is touched, and `QueryEvents` applies it to every
query — and `serve` clamps request parameters into the same range before building the query,
so an unbounded page can't be constructed by any caller. The summary is one grouped
statement: its `last_critical` comes from the same snapshot as its counts.

---

## Local development note

Timescale's community edition is what production runs; a local Apache-licensed build does **not**
support the compression policies in `schema.sql`, so startup policy application will fail locally.
Use a plain-table workaround for local store-integration checks rather than editing the schema.

---

## Tests

`store_test.go` and `events_test.go` cover batch flushing on both size and interval, the dedup
ledger's transactional claim (including the rollback path), retention/compression policy
application, the interval allowlist rejection, event idempotency under retry, query bounds, and
`Degraded()` reporting.

```sh
go test ./internal/store/
```

---

## See also

- `schema.sql` — the authoritative DDL, heavily commented with the reasoning above.
- `../ingest/README.md` — `DurableTracker`, the other end of `SetDurableNotify`.
- `../serve/README.md` — the event read and SSE paths.
- `../../../docs/OUTPUT.md §4` — the persistence contract.

## Board measurements

Environmental and pulse-timing ObserverDetails records share the bounded batch
writer, transaction retry and GNF1 replay-dedup ledger with navigation, and are
inserted into the separate private `observer_samples` hypertable. Raw bytes,
decoded JSON, nullable sample UTC, source boot/sequence and receipt authority are
preserved. Compression and retention use the configured raw-evidence intervals.
See [the board telemetry contract](../../../docs/OBSERVER-TELEMETRY.md#persistent-environmental-and-clock-history)
for fields, acknowledgments, query examples and operator-only access.
