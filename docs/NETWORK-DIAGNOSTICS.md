# Troubleshooting a remote observer's network

ESP32 observers collect passive diagnostics alongside GPS traffic. Deploy the
collector first so it understands network ObserverDetails and upgrades the
historian's sample-kind constraint, then deploy the firmware and app. An older
collector discards the unfamiliar telemetry. This instrumentation covers the
ESP32 uplinks and collector session; the portable C feeder does not yet produce
these board reports.

## Retrieve the evidence

In Integrity Station's direct collector mode, open an authorized private station
view and its **Network diagnostics** card. The card
shows the collector's actual socket path separately from the current default
route. Expand events, counters or addresses, and export the snapshot as JSON.
The export includes observer identity, boot session, sample UTC, uptime and
collector receipt time. **Network history** charts Wi-Fi signal, Wi-Fi disconnect
counts or collector connections; expand individual samples for the complete report.
The owner portal uses a separate API and does not yet expose this network card
or the network history metrics.

The private observer API exposes `board.network.details.network`, with
`board.network_stale`. Query `/gnss/api/v2/observer-samples` with `kind=network`
for delivered history, using the authorization and paging described in
[sensor history](SENSOR-HISTORY.md). Network samples also accompany retained
event evidence. Public station feeds exclude board telemetry and addresses.

If the observer has rebooted or cannot reach its collector, read its persistent
network summaries over the local management connection, from the `esp32` directory:

```sh
python3 tools/ota.py --device observer.example.invalid journal \
  --key-file /path/to/private/observer-ota.key --lane network --limit 128 --json
```

The [journal](../esp32/docs/JOURNAL.md) has a separate 128-entry network lane.
Changed events trigger a summary at most once a minute; stable state is saved
hourly. Summaries retain link values, counters and the last native error. They
are smaller than complete reports and do not preserve every transition or
address. An interrupted write or failure before a checkpoint can leave a gap.

## Work from the link toward the collector

| Evidence | What to investigate |
|---|---|
| Low current/minimum RSSI, rising weak-sample count, Wi-Fi disconnect reasons | AP reachability, antenna placement, association failures. The weak bucket is ≤ −75 dBm; it is context, not an automatic fault verdict. |
| Ethernet carrier transitions, unexpected speed or half duplex | Cable, switch port and negotiation. The W5500 is a 10/100 Mbps interface; SPI clock speed is unrelated. |
| Address losses/changes, gateway or DNS changes | DHCP and local routing. Compare both interfaces and the collector socket path; an existing connection may remain on Wi-Fi after Ethernet becomes the default. |
| DNS, TCP, TLS or application-handshake failures | Follow the recorded stage and native code/detail. Authentication rejection is distinct from a transport failure. Check the collector and clock evidence for TLS failures. |
| ACK stalls, growing maximum ACK gap, queue high-water or dropped records | Collector reachability and persistence delays. ACK progress reflects durable ingestion, so the gap is not a ping RTT. Increasing dropped records establishes actual spool loss. |
| New boot session or interrupted uptime | Compare lifecycle reset reason and firmware identity. A reboot clears RAM counters and unsent spool records; persistent summaries can preserve earlier clues. |

The collector's `navlistener_push_disconnects_total{observer,reason}` adds the
receiving side's view: `idle_timeout`, `eof` or `read_error`. It counts read-side
stream termination, not every possible shutdown. Compare it with observer
connection counters and historian health rather than assuming a cable fault.

## Interpret time, gaps and unavailable measurements

Counters saturate and belong to one boot/session. Compare deltas only within
that session. The eight-event window overlaps successive reports; deduplicate
by session and event sequence. `events_overwritten` makes window turnover
explicit. `last_error` remains available after recovery and is not a current
alarm. Current link attributes are sampled every ten seconds. Queue depth and
its reported high-water mark are sampled every two seconds and can miss shorter
peaks; the spool's dropped-record counter counts evictions and rejected records.

Reports carry sample UTC separately from collector receipt time. Replayed
reports can arrive much later. Event times are monotonic uptime; the app only
estimates wall time when sample UTC is available. Missing sample UTC cannot
establish replay age. History charts use receipt time and break at null readings,
reboots and long gaps. The latest report becomes stale after eleven minutes.

Wi-Fi retries and Ethernet frame/CRC error counters are `null`: the current
drivers do not expose them. Unknown does not mean zero. RSSI does not measure
channel utilization, and TLS byte counters measure application plaintext, not
wire traffic or retransmissions. Use switch/AP counters when those details are
needed. IPv4 addresses are included; SSIDs, credentials and packet contents are
not collected here.

The observer opens no diagnostic connections and performs no scans, pings or
speed tests. Stable telemetry runs every five minutes; events or a 10 dB RSSI
change can request a report, limited to once a minute. Reports use a nonblocking
append with a quarter of spool capacity reserved for navigation and defer under
pressure. A missing report can therefore reflect queue pressure as well as a
disconnected observer. [Wire format and field semantics](OBSERVER-TELEMETRY.md#network-diagnostics-tag-22-version-1).
