# Persistent diagnostic journal

The S3 records a bounded history of boots, firmware identity and health in a
dedicated 512 KiB NVS partition. It works before network provisioning and without
the optional EEPROM. It does not store GNSS observations or make the PSRAM spool
durable.

## FIFO retention and failures

Two independent FIFO queues replace their **oldest** entries automatically:

| Queue | Capacity | Writes |
|---|---:|---|
| Lifecycle | 256 events | Boot, local startup confirmation, first usable time and first GNSS-qualified time, OTA selection/failure |
| Health | 1,024 checkpoints | After one minute of uptime, then hourly |

Hourly checkpoints retain about 42 days of continuous operation. Lifecycle
retention depends on the number of events, not elapsed days; health records
cannot evict boot history. A board switched off for years writes nothing while
off, so its last records remain until subsequent FIFO turnover or explicit flash
erasure. Flash retention over that duration is a property of the actual hardware,
not a lifetime guarantee from this software.

Each record is one atomic NVS blob, with a format version, sequence and CRC.
The sequence is stored inside the record, avoiding a separately committed queue
head. Startup scans the bounded key set. NVS handles garbage collection and wear
distribution; the application never writes a continuous console log.
[ESP-IDF NVS recovery and storage behavior](https://docs.espressif.com/projects/esp-idf/en/v5.5/esp32/api-reference/storage/nvs_flash.html)

A failed initialization, incompatible record, full store or write error disables
journaling for that boot and reports an error. **GNSS collection continues.**
There is no automatic partition erase, retry loop, growing RAM backlog, or
requirement for an operator to clear a full queue. A failed write may nevertheless
have persisted; restarting rescans the records before assigning another sequence.
The journal is diagnostic evidence, not tamper-resistant storage. It cannot
record failures before the application reaches its journal startup call.

## Interpreting the records

Every record includes the firmware version, application ELF SHA-256, running
partition, boot identity, monotonic uptime, reset reason and wall-clock source.
The ELF hash distinguishes binaries with the same version string; it is **not**
the download binary hash used to authorize OTA. Boot identity is the boot record's
lifecycle sequence, not a contiguous lifetime reboot count.

Wall time starts unknown. Time-anchor events pair UTC with uptime once a valid
running RTC is read or GNSS passes the existing three-sample UTC checks. GNSS is
preferred; an RTC timestamp remains labeled RTC. Neither is authenticated or
PPS-qualified. Build time and SNTP are not used. Unknown timestamps remain zero;
later anchors can estimate the corresponding boot time as UTC minus uptime,
with the source clock's uncertainty. Older records are never rewritten.

Compare successive boot identities, versions, reset reasons and uptimes to find
reset loops or upgrades. Compare the previous checkpoint with a later valid time
anchor to detect a long unobserved interval. Missing records do not establish the
exact outage duration or cause: the last ordinary checkpoint can precede power
loss by almost an hour, and logging itself may have failed. A board that remains
powered but disconnected continues to record checkpoints with the link flag off.

Health is recorded as evidence, not one blanket success flag:

| `flags` bit | Meaning |
|---:|---|
| 0 (`0x01`) | Fresh receiver satellite report (within 15 seconds) |
| 1 (`0x02`) | Fresh valid GNSS position |
| 2 (`0x04`) | Collector session connected; not a claim of durable ACK progress |
| 3 (`0x08`) | WiFi associated |
| 4 (`0x10`) | Spool allocated in PSRAM |
| 5 (`0x20`) | Board task supplied a health snapshot |

Records also contain queued/dropped spool counts, free internal heap,
environmental validity bits, RTC flags and the boot-time ATECC RNG/EEPROM status
codes from [ObserverDetails](../../docs/OBSERVER-TELEMETRY.md). ATECC results are
startup diagnostics, not continuous entropy monitoring. Local startup confirmation
does not require enrollment, an EEPROM, a satellite fix or collector connectivity.

## Installation and readout

Install the updated S3 partition table over USB once. It carves the journal out
of the previously unused end of the spool reservation: `journal` starts at
`0xf80000`; `spool` remains unmounted and is now 9.375 MiB. Existing application,
OTA and configuration offsets are unchanged. App-only OTA on an older table
continues normal collection with a journal-unavailable message. Subsequent
ordinary flashes, OTA and configuration resets preserve journal data; whole-chip
erase does not. The C6 has no journal partition and its journal hooks are no-ops.

Startup prints the most recent lifecycle event and checkpoint, then new events,
on the physically trusted serial console. For complete paged readout, pair the
existing management key and provision station mode as described in the
[OTA instructions](../README.md#on-demand-ota-updates-esp32-s3):

```sh
python3 tools/ota.py --device observer.example.invalid journal \
  --key-file /path/to/private/observer-ota.key
python3 tools/ota.py --device observer.example.invalid journal \
  --key-file /path/to/private/observer-ota.key --lane health --limit 1024 --json
```

Readout is newest-first. `--json` includes all fields; decimal strings preserve
64-bit integers. `/journal` accepts authenticated POSTs with `life\n<cursor>` or
`health\n<cursor>` bodies, using the `/ota` single-use nonce and a distinct
`navfeeder-journal-v1\n` HMAC domain. Cursor zero starts at the newest record;
subsequent cursors are exclusive sequence bounds, eight rows per page. Export
does not pause writes; entries overwritten during a long export can be absent.
The management connection is local HTTP, so authentication does not encrypt the
returned diagnostics. No network passwords, keys or location are journaled.

Host tests cover repeated FIFO wrap, preservation of the independent boot queue,
full/write/commit failures before and after persistence, restart recovery,
corruption refusal, long uptime and hourly cadence. Physical flash interruption,
long-term endurance and management transport still require hardware validation.
