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
func (s *Store) WriteSnapshot(ctx, at, endpoint string, data []byte) error
func (s *Store) QueryEvents(ctx, q EventQuery) ([]StoredEvent, int, error)
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

`Enqueue` is non-blocking. When the queue is full, the frame is **dropped and counted**
(`store_dropped_total`) — an explicit, metered policy rather than blocking ingest or growing
without bound.

This is the deliberate priority ordering: **live decoding and the integrity monitor matter more
than the forensic archive.** If the database stalls, the daemon keeps decoding, keeps detecting,
and keeps serving; what degrades is history. `Degraded()` surfaces that to `/healthz` as
`degraded` (HTTP 200, not 503) so a DB blip can't flap rc.d into a restart loop.

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
| A poison row inside a batch | The batch is **bisected** to isolate it; the bad rows are quarantined and the good ones commit. | `store_quarantined_total` |
| Wall budget expires, parent still live | The batch is dropped and counted — the writer must stay live and memory-bounded through a long outage. | `store_quarantined_total` |
| Shutdown interrupts a flush | The batch is **retained** for the bounded shutdown drain rather than cleared. Nothing was committed, and the atomic claim+copy transaction rolled its replay-key claims back, so the re-flush cannot duplicate. | — |

The claim-and-copy being one transaction is what makes that last row safe: bisection, retry, and
retained re-flush all preserve the invariant that a dedup claim exists if and only if the row
committed.

### `nav_frames` — the forensic record

```sql
ts          TIMESTAMPTZ  -- INGEST time (the hypertable dimension)
received_at TIMESTAMPTZ  -- receiver/host reception time (separately indexed)
source_id   TEXT
organization_id, enrollment_id, collector_instance_id  TEXT
collection_ids  TEXT[]     -- receipt-time collection memberships
provenance      TEXT       -- local or an inbound federation peer
credential_tier, attestation_tier  TEXT
aggregate_use, station_metadata, policy_revision  TEXT
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
state file  — the historian *is* the durable record.

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

Dial-mode frames carry no feeder sequence and always pass through unfiltered — duplicates across
*different* receivers are intentional and untouched by this table. The ledger is pruned on the
same interval as `raw_retention`.

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
the DDL (segment by `event_type`) and the 30-day *policy* is applied by the store at startup —
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

**The notify contract.** An `AFTER INSERT` trigger fires `pg_notify('gnss_event', …)` carrying
**only** `id`, `sv`, `type`, and `severity`. `message` is intentionally omitted rather than
truncated: `pg_notify` has a hard ~8000-byte payload limit, and a long message would *raise in
the trigger and fail the whole INSERT in the same transaction*. LISTENers fetch the full
row by `id`.

Which is why `idx_gnss_events_id` exists : a hypertable PK must include the partition
column, so `id` has no index by default — and without an explicit one, every notify-driven
`SELECT ... WHERE id = $1` seq-scans **every chunk** of this retention-less table. That cost
lands entirely on the external consumer, since navlistener itself never queries by id. It would
have been invisible from inside this repo.

### `gnss_snapshots`

Periodic dumps of each served feed's current body (`svs`, `global`, `observers`, `almanac`,
`sbas`) for replay and backfill. Active only when both `[serve].addr` and `[store].dsn` are set;
cadence is `[serve].snapshot_interval` (default 5m, `0s` disables). Compressed, segmented by
endpoint.

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
`/gnss/api/events` and `/gnss/api/events/summary`. Bounds are enforced here rather than in the
HTTP layer so an unbounded query can't be constructed at all.

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
