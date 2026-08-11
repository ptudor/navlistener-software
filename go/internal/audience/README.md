# `internal/audience` — pre-aggregation privacy projection

This package enforces the central privacy rule from `docs/GROUPS-AND-FEDERATION.md`: an
unauthorized observation never enters an audience's state. Filtering fields after one global
aggregation would still leak private receivers through confidence, selected ephemerides,
timestamps, counters, detector transitions, and event timing.

The public projection has three outcomes:

- `private`: reject the frame from public state;
- `public_anonymous`: admit satellite data under one non-enumerable bucket, stripping
  observables, RF telemetry, and station identity;
- `public_attributed`: admit under the canonical public observer id, including per-SV
  observations; both `coarse` and `full` are locatable product tiers.

RF security telemetry is operator-only until a separate explicit public grant exists.

Public detector state is a further projection. `event_visibility=private` contributes no
public detector input; `public_redacted` uses the anonymous bucket; `public` retains only the
attribution already allowed by `aggregate_use`. This ensures an event grant cannot widen the
underlying feed grant.

`Registry` owns physically separate organization/collection stores created only by trusted
receipt scope. On an authorization transition it resets every view touched by the old context;
merged state cannot soundly subtract one receiver after freshest-value and confidence selection.
`PolicyEpochs` supplies the matching current-policy boundary for pending events, historical
queries, and SSE replay. Its startup boundary is intentionally conservative: aggregate event
rows from an earlier process are forensic records until explicitly republished, not implicitly
visible under a possibly changed current policy.
