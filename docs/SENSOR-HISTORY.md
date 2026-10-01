# Private sensor-history API

`GET /gnss/api/v2/observer-samples` returns persisted, decoded board samples for
one receiver and one private audience. `HEAD` accepts the same parameters and
authorization, performs the same checks, and omits the response body.

The endpoint is implemented in the collector. The Django owner portal and
Integrity Station both provide charts using this contract. Django authorizes
human accounts against approved fleet inventory; the native application uses
its selected private collector audience. These remain separate sign-in modes.
Live values remain available through the private `/gnss/api/v2/observers` feed.

## Authorization and availability

Requests require native read authorization, a valid `Authorization: Bearer`
credential, and a granted private audience. Use `X-GNSS-Audience` to select
`organization:<id>`, `collection:<id>`, or `operator:<collector-id>`. The default
listener audience applies when the header is absent. A public audience cannot
read sensor history, including samples from stations that publish GNSS data.
A fixed operator listener protected only by an external login still needs a
native read principal for this endpoint.

The collector enables the reader when its existing historian is configured.
No schema migration, second database, or additional retention policy is needed.
The selected audience must have materialized collector state, as with other
private feeds. A receiver need not currently be online or have live board data.

The SQL query checks the collector instance and immutable receipt-time
organization or collection membership. An operator grant covers only its
collector. Organization `local-unassigned` is excluded from owner access;
those receipts require the matching operator grant. The receiver identifier
is an opaque, case-sensitive string, preserved exactly and URL-encoded by the
client. Query parameters cannot grant access to an organization or collection.

Read credentials are checked before the query and again after reading and
encoding the response. A grant/revision change discards pending results. An
audience policy reset invalidates active delivery using the existing collector
transport guard. Bytes already sent before revocation cannot be recalled.
Authorization changes become visible according to the configured authorizer's
refresh/recheck behavior.

Every response, including errors, uses `Cache-Control: private, no-store` and
`Vary: Authorization, X-GNSS-Audience`. Portal clients should keep collector
credentials on the server and expose only session-authorized results.

## History boundary

Receipt-time scope alone does not prove that a past sharing grant still applies.
This first implementation also uses the collector's existing conservative
audience policy epoch. It serves receipts at or after the **current collector
process start or most recent reset of that audience**, whichever is later.
An ownership change or collection withdrawal cannot confer access to the
previous owner's private samples. A reset can hide history for other stations
in that audience as well.

This restriction does not delete persisted data. It does mean that restarting
the collector temporarily empties HTTP sensor history. Serving older receipts
across restarts or policy changes requires a durable authorization-history
contract; the current endpoint does not provide one.

The response exposes `visible_since`, `effective_since`, and `history_limited`
so clients can label this boundary. If the requested window ends before the
boundary, the response is successful with an empty sample array;
`effective_since` can then be later than `until`. This is distinguishable from
an ordinary empty window through `history_limited`.

## Query parameters

| Parameter | Meaning and bounds |
| --- | --- |
| `observer` | Required opaque receiver ID; nonempty UTF-8, no NUL, maximum 4096 bytes. |
| `kind` | `environment` (default) or `timing`. |
| `since` | Inclusive collector receipt-time lower bound, RFC3339 with optional fractional seconds; defaults to one hour before `until`. |
| `until` | Inclusive receipt-time upper bound, RFC3339; defaults to now, future values clamp to now. |
| `limit` | Integer 1–500; defaults to 100. |
| `offset` | Integer 0–100000; defaults to zero. |
| `revision` | Optional response revision from the previous page; mismatch returns 409. |

Windows exceeding 30 days or with `since > until` are rejected before applying
the policy boundary. Empty, duplicate, unknown, malformed, or excessive
parameters return 400. The complete encoded query is limited to 16 KiB.
Database timestamps have microsecond precision; lower bounds round up and
upper bounds round down when necessary.

```sh
curl --get 'https://collector.example.invalid/gnss/api/v2/observer-samples' \
  --header "Authorization: Bearer ${NAVLISTENER_READ_TOKEN}" \
  --header 'X-GNSS-Audience: organization:example-team' \
  --data-urlencode 'observer=receiver:001/roof' \
  --data-urlencode 'kind=environment' \
  --data-urlencode 'limit=100'
```

An example response, with the revision abbreviated for readability:

```json
{
  "ok": true,
  "time": "2026-09-30T12:00:00Z",
  "data": {
    "schema": "2.0",
    "audience": "organization:example-team",
    "observer": "receiver:001/roof",
    "kind": "environment",
    "since": "2026-09-30T11:00:00Z",
    "until": "2026-09-30T12:00:00Z",
    "visible_since": "2026-09-30T11:30:00Z",
    "effective_since": "2026-09-30T11:30:00Z",
    "history_limited": true,
    "revision": "opaque-revision",
    "limit": 100,
    "offset": 0,
    "next_offset": null,
    "has_more": false,
    "pagination_limited": false,
    "samples": [{
      "received_at": "2026-09-30T11:45:00Z",
      "sample_time": null,
      "session": "boot-session",
      "sequence": "18446744073709551615",
      "hardware_trust": "trusted",
      "details": {"environment": {"mcp9808_c": 24.5, "humidity_percent": null}}
    }]
  }
}
```

## Sample semantics and pagination

`details` is the persisted decoded [ObserverDetails](OBSERVER-TELEMETRY.md)
object, with its original units, validity flags, missing fields, and nulls.
`environment` is the historian's general non-timing board-report category;
it can also contain resource, reception, or update reports. It does not promise
that every sample has an `environment` object. Timing has its own category.

`received_at` is the collector receipt time and drives the query window.
`sample_time` is nullable feeder UTC, not a substitute receipt timestamp.
`session` and `sequence` are nullable for records without source metadata.
Unlike live samples, history emits `sequence` as a **decimal string** to retain
all 64 bits in browser clients. Session and sequence identify reboot/replay
context; they do not prove uninterrupted sampling.

`hardware_trust` is the collector's receipt-time verification result beside
the device-reported details. Raw wire bytes, credential fingerprints, private
authority evidence, and commissioning records are not returned. Receiver
strings inside details remain untrusted display content; render them as text.

Samples are oldest first, with deterministic ties resolved by stored time,
session, sequence, sample UTC, hardware trust, and decoded data. There is no
global count. A page returns at most `limit` rows and about 4 MiB of encoded
samples. An individual stored JSON object over 64 KiB, or invalid stored
metadata, fails the query rather than silently dropping a sample.

When `has_more` is true, request `next_offset` using the returned `since`,
`until`, and `revision`, plus the same observer, kind, limit, and audience.
The revision is scoped to this history response and is not interchangeable
with the revision from audience discovery. On a revision conflict, clear the
old series and restart from offset zero with current authorization.
At the offset ceiling, `pagination_limited` is true and `next_offset` is null;
narrow the time window to continue.

Pagination is deterministic for an unchanged dataset, but it is **not a database
snapshot**. Late writer commits and retention can shift offsets even within a
fixed window. For charts, periodically reload a recent overlapping window;
do not use this route as a lossless export protocol. No totals, downsampling,
interpolation, or guarantees of continuous coverage are supplied.

Existing historian retention applies (normally seven days of raw records,
compression after one day). The 30-day query ceiling does not extend retention.
Writer queue loss, retention, device outages, and absent measurements can all
produce gaps; a missing row is not a zero reading.

## Errors and resource limits

| Status | Meaning |
| --- | --- |
| 200 | Valid query; unknown, out-of-scope, and sample-free observers all return empty arrays within the granted audience. |
| 400 | Invalid selector or query parameters. |
| 401 | Missing or invalid read credential for a private audience. |
| 403 | Public/ungranted audience, or credentials changed during the request. |
| 404 | Granted audience has no materialized collector state. |
| 405 | Method other than GET or HEAD. |
| 409 | Supplied history revision no longer matches. |
| 500 | Query/data/encoding failure; database details are omitted from the response. |
| 503 | Historian or native authorizer unavailable, request concurrency exhausted, or audience reset during delivery. |
| 504 | Query timeout or cancellation. |

The request's authorization and database work share a five-second context.
At most eight history reads run concurrently per API server; excess requests
receive 503 with `Retry-After: 1`. Response writes have a ten-second deadline
without changing the long-lived SSE server timeout.

## Verification

HTTP regression tests cover grant checks, query bounds, policy/reset boundaries,
revocation during a query, unavailable dependencies, pagination, and HEAD.
The store tests execute the real SQL against PostgreSQL in an isolated temporary
schema derived from the production table definition:

```sh
cd go
go test ./internal/serve ./internal/store
NAVLISTENER_OBSERVER_TEST_DSN='host=localhost dbname=navlistener_test sslmode=disable' \
  go test ./internal/store -run TestObserverHistory -count=1
```

The SQL tests cover owner/group/collector isolation, nullable evidence, opaque
IDs, unsigned sequences, tied ordering, byte limits, and cancellation. They
need schema-creation privileges in a dedicated test database. They do not test
Timescale compression or change the production writer/retention configuration.
