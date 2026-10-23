# Fleet reports

Two JSON documents carry a board's history into its manufacturer's own records,
alongside the commissioning chain ([COMMISSIONING.md](COMMISSIONING.md)):

- A **bench report** is one bench test of one board. The bench writes it with
  `esp32/tools/bench_check.py`, and the manufacturer records it against the unit.
- A **field report** is what one collector saw of the boards enrolled under one
  manufacturer authority. A collector sends it only when its operator configures
  `[fleet_report]`; nothing is sent by default, and a board never sends one itself.

Neither document is evidence that a board is genuine, and neither changes what a
collector concludes about one. A bench report is the bench's account. In a field
report, `hardware_trust` and `commissioning_fingerprint` are what the collector
verified for the board's session; `firmware` and `update` are the board's own words.

Shared vectors are in `common/fixtures/bench-report-v1.json` and
`common/fixtures/field-report-v1.json`. Each holds `valid` documents and `invalid`
cases, each with the reason it must be refused. A consumer accepts every valid
document and refuses every invalid one.

## Common rules

- A document is a UTF-8 JSON object. A duplicate member, an unknown member, a
  missing member and a non-finite number are refusals.
- Times are UTC in the form `2026-10-23T14:02:11Z`: whole seconds, no offset.
- Hex is lowercase.
- Text fields are printable ASCII (0x20–0x7e) unless a field says otherwise.
- A 64-bit release number is a decimal string, as in the collector's API, because
  a JSON number loses precision above 2^53.

## Bench report v1

```json
{
  "format": "navlistener-bench-report-v1",
  "tool": "bench_check.py 1",
  "station": "bench-1",
  "started_at": "2026-10-23T14:02:11Z",
  "finished_at": "2026-10-23T14:02:49Z",
  "board": {
    "observer_id": "board-0003-00112233445566778899aabbccddeeff",
    "mcu_mac": "02a1b2c3d4e5"
  },
  "firmware": {
    "image_sha256": "<64 hex>",
    "app_version": "0.1.0+1.b8e037c",
    "project_revision": "<40 hex>",
    "project_dirty": false,
    "idf_revision": "v5.5.4",
    "target": "esp32s3"
  },
  "checks": [
    {"name": "boot", "result": "pass", "detail": "one boot, no panic"}
  ],
  "result": "pass",
  "console_sha256": "<64 hex>"
}
```

| Member | Rule |
|---|---|
| `tool` | The program and its version, 1–64 characters |
| `station` | The bench's own name: 1–64 of `A–Z a–z 0–9 . _ -`, starting with a letter or digit |
| `started_at`, `finished_at` | `finished_at` is not before `started_at`, and at most one hour after it |
| `board.observer_id` | The board identity the firmware read from its identity EEPROM ([BOARD-IDENTITY.md](BOARD-IDENTITY.md)), or `null` when it read none |
| `board.mcu_mac` | The microcontroller's factory MAC as 12 hex digits, read from its ROM bootloader, or `null` |
| `firmware.image_sha256` | SHA-256 of the application image the bench wrote: the build's `firmware_sha256` provenance |
| `firmware.app_version` | The version the booted image printed, 1–32 characters, or `null` when none was printed |
| `firmware.project_revision` | The 40-digit source revision of the build, or `null` outside a Git checkout |
| `firmware.project_dirty` | Whether the build's tree had changes; `null` when unknown |
| `firmware.idf_revision` | The ESP-IDF revision, 1–64 characters, or `null` |
| `firmware.target` | The chip target: 1–16 of `a–z 0–9` |
| `checks` | 1–64 checks with distinct names |
| `checks[].name` | A lowercase letter, then up to 63 of `a–z 0–9 _ . : @ -` |
| `checks[].result` | `pass`, `fail` or `skip` (did not apply to this board) |
| `checks[].detail` | What was seen, 0–200 characters |
| `result` | `fail` when any check failed; otherwise `pass`, which needs at least one passing check |
| `console_sha256` | SHA-256 of the console capture the checks were read from |

The console capture stays on the bench. Before a board is provisioned, its console
prints the setup label, which holds the setup network password, so the capture is
written mode 0600 and only its digest leaves the bench. No detail carries the label.

### Checks from `bench_check.py`

The tool resets the board through its USB port, reads the console for the
capture window and judges only lines the firmware printed:

| Check | Pass | Fail | Skip |
|---|---|---|---|
| `firmware.image` | The booted image's ELF SHA-256 prefix matches the build's ELF | Another ELF, or none printed | — |
| `boot` | One boot in the window, no panic, abort, brownout or stack overflow | A second boot or any of those | — |
| `board_uid` | A board identity read from the identity EEPROM | None read | — |
| `mcu_mac` | The ROM bootloader answered with a MAC | It did not | — |
| `manifest` | The hardware manifest names a supported board | It names none, or no boot action was printed | — |
| `atecc` | The secure element answered with a CRC-verified info read | It did not, on a named board | The manifest names no board |
| `i2c:<part>@0x<addr>` | The firmware probed the part and it answered | It probed the part and it did not | — |
| `receiver` | Valid receiver traffic | No UART bytes from the receiver | No verdict within the window |
| `setup` | An unprovisioned board raised its setup network and the app's provisioning | It did not | The board is provisioned |

The firmware runs board-specific code, including the secure element read and the
part probes, only once the manifest names a supported board, and probes only the
parts the manifest lists. A part it never probes is not tested by this tool. A
part's check name is the firmware's name for it in lower case, with each run of
other characters made `_`: `I/O expander at 0x20` becomes `i2c:i_o_expander@0x20`.
The window defaults to 30 seconds.

## Field report v1

```json
{
  "format": "navlistener-field-report-v1",
  "report_id": "6c5b1f0e-8d1a-4c4e-9f7a-2b3c4d5e6f70",
  "manufacturer_authority_id": "example-manufacturing",
  "collector_instance_id": "collector-1",
  "generated_at": "2026-10-23T14:00:05Z",
  "boards": [{
    "observer_id": "board-0003-00112233445566778899aabbccddeeff",
    "hardware_trust": "trusted",
    "commissioning_fingerprint": "<64 hex>",
    "last_seen": "2026-10-23T13:00:00Z",
    "firmware": "0.1.0+1.b8e037c",
    "update": {
      "channel": "stable",
      "mode": "install",
      "state": "idle",
      "trust_profile": "trusted",
      "running_release": "17",
      "failed_release": "0",
      "error": "OK"
    }
  }]
}
```

| Member | Rule |
|---|---|
| `report_id` | A UUID in lowercase 8-4-4-4-12 form. A retry resends the same report with the same ID; a new report gets a new one |
| `manufacturer_authority_id` | The manufacturer authority every listed board is enrolled under: 1–253 of `A–Z a–z 0–9 . _ : -`, starting with a letter or digit |
| `collector_instance_id` | The collector's `[collector] instance_id`, under the same rule, or `null` when it has none |
| `generated_at` | When the collector built the report |
| `boards` | 0–10,000 boards with distinct `observer_id`s. An empty list says the collector is reporting and has none of this manufacturer's boards |
| `observer_id` | A board identity, `board-0003-…` or `board-0004-…` with 32 hex digits, not all zero |
| `hardware_trust` | `none`, `open`, `test` or `trusted`: the collector's verdict for the board's latest session |
| `commissioning_fingerprint` | SHA-256 of the commissioning record the collector verified, or `null` exactly when `hardware_trust` is `none` |
| `last_seen` | The hour of the collector's latest receipt from the board, with minutes and seconds zero |
| `firmware` | The application version the board reported, 1–32 characters, or `null` when it reported none |
| `update` | The board's latest update status, or `null` when it has reported none |
| `update.channel` | `stable`, `canary` or `lab` |
| `update.mode` | `manual`, `download` or `install` |
| `update.state` | One of the update states in [SOFTWARE-UPDATES.md](../esp32/docs/SOFTWARE-UPDATES.md#device-state-machine) |
| `update.trust_profile` | The release track the board says it follows: `unreported`, `trusted`, `open` or `test` |
| `update.running_release`, `update.failed_release` | Release sequence numbers; `0` when none |
| `update.error` | The latest update error's name, `OK` when none: 1–64 of `A–Z 0–9 _` |

### What a report leaves out

The report carries no position, observation, receiver or sensor data, no network
name, address or diagnostic, no organization, enrollment or credential, and no
time finer than an hour. It lists only boards with a board identity that are
enrolled under the named manufacturer authority; software stations and other
manufacturers' boards are never listed.

The collector reports from what it holds in memory: each board it has received
from since it started, with that board's latest report. A board that stops
reporting stays listed, with its old `last_seen`, until the collector restarts.
A first-seen time is the manufacturer's to derive from the reports it keeps.

### Sending

When `[fleet_report] url` is set, the collector builds a report a few minutes
after it starts, then once per `interval`, and POSTs it:

```http
POST /intake/field-reports/ HTTP/1.1
Authorization: Bearer <token>
Content-Type: application/json
```

| Response | Collector |
|---|---|
| 201 | Accepted |
| 200 | Accepted earlier under the same `report_id`: done |
| 400, 401, 403, 404, 413 | Refused. Logged with the status; the next interval sends a new report |
| 429, 5xx, timeout, connection failure | Retried with the same `report_id`, backing off from one minute up to an hour, until the next interval replaces it |

The collector follows no redirect, so the token is sent only to the configured
URL, and only over HTTPS.

```toml
[fleet_report]
url = "https://records.example-manufacturer.invalid/intake/field-reports/"
# One line, the token the manufacturer issued; mode 0600.
token_file = "/usr/local/etc/navlistener/fleet-report.token"
manufacturer_authority_id = "example-manufacturing"
interval = "24h"  # the default; at least 1h
```

`manufacturer_authority_id` must name a configured `[[manufacturer_authority]]`.
Removing `url` stops reporting at the next restart. The manufacturer can refuse a
token at any time; the collector then logs each refusal.

### One board, several collectors

A board enrolled with one collector is reported by that collector. A manufacturer
that also runs collectors of its own receives their reports the same way. A
manufacturer that receives reports for one board from more than one collector
keeps each collector's report separately; the reports are not merged and a
disagreement between them is shown, not resolved.
