# Station presentation contract

Integrity Station and the Django owner portal use the same collector data, while
retaining native and browser controls. The native application's name remains
Integrity Station. IntSat/NavListen supplies the common palette and constellation
colors; native system fonts and accessibility behavior remain platform-specific.

## Independent status dimensions

Receiver connectivity uses observer `last_seen_s`, or `last_seen` with the
collector's response time. Contact within 300 seconds is online. A receiver can
be online without an environmental board report. Missing liveness remains unknown;
an absent inventory receiver without a recorded contact has no contact recorded.
Inventory enablement, collector connectivity and board freshness are separate.

Condition health uses the complete current-condition endpoint, never a recent
event-history tail. Offline takes precedence over alarms; an online receiver has
unknown health until conditions are known. Critical conditions take precedence
over warnings. Station assurance transitions (`station_assurance` other than
`assured`) are conditions like RF alarms, at the severity the collector assigns.
Either edge or collector reception alarms contribute a warning,
including held alarms during stale or unknown coverage. A healthy communication
path does not establish correct navigation or accurate UTC.

## Sample freshness

Board, timing and reception thresholds are 660, 5 and 15 seconds respectively.
Freshness uses collector time plus monotonic elapsed time rather than the user's
wall clock. Whole-second collector timestamps use the end of the second to avoid
mistaking a fractional receipt within it for a future receipt.

| State | Meaning |
| --- | --- |
| `current` | Both receipt and valid source UTC are within the threshold. |
| `receipt_only` | Receipt is recent; source UTC cannot establish replay age. |
| `stale` | Collector marks stale, cached data is displayed, or a known age exceeds the threshold. |
| `unknown` | Receipt, reference clock or collector freshness state is unavailable or inconsistent. |

Collector `hardware_trust` is shown for each sample independently of firmware's
reported track, manifest contents and recorded inventory. Missing verification
means not reported. Unknown future wire values remain visible without claiming
trusted evidence.

## Historical readings

Both clients use the [private sensor-history API](SENSOR-HISTORY.md) for board
temperature, humidity, pressure and RTC/GNSS phase. Charts use receipt time and
break across missing measurements, receiver restarts and sampling gaps. Zero is
a measurement; missing values are not zero. Source UTC, boot session and collector
verification remain available alongside the chart.

Native requests recheck the selected private grant before and after each bounded
page. Results remain in memory and clear when access, selection or app activity
changes. Both clients disclose the collector's policy boundary and historian
availability; neither promises uninterrupted history across collector restarts.

The portal owns accounts, group membership and approved inventory. Direct native
collector credentials remain separate from portal login, and local favorites do
not establish ownership. The native **My fleets** mode uses the same portal
account, approved inventory
and read authorization through its own revocable app session. Collector service
tokens stay on the server. It reloads current inventory, conditions and authorized
history after reconnecting instead of synthesizing values for missing readings.

## Shared regression cases

The canonical fixture is
[station-presentation-v1.json](../swift/IntegrityStationTests/Fixtures/station-presentation-v1.json).
The portal carries an identical copy under `django/tests/fixtures/` so each
repository can build independently. Native tests, Python tests and browser-module
tests evaluate the same cases. Update both copies together when changing the
contract, and review changed expectations against the collector protocol.

These cases cover boundary ages, missing source UTC, old replay, cached samples,
unknown conditions, condition severity and held reception alarms. They complement
authorization, clock, browser and collector tests; they do not replace them.
