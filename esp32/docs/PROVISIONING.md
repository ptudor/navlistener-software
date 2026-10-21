# ESP32-S3 provisioning contract

Whenever its complete operating configuration is absent, the custom observer
raises a WPA2 SoftAP named for the device. It can be set up over that network in
two ways, each with the password on the setup label:

1. the Station app, which joins the network and provisions the observer through
   [ESP-IDF Unified Provisioning][unified] with Security 2 (SRP6a
   authentication and key exchange, then AES-GCM); or
2. the browser form at `http://192.168.4.1/`.

Both are available at the same time, on the same web server, and commit the
same validated `netcfg` record before the observer reboots into station mode.
Bluetooth is off in the S3 image ([BLE transport](#ble-transport)).

Both also accept an **optional WireGuard profile**, which carries the GNF1/TLS
session inside a tunnel to the collector whenever the peer is up and falls back
to the public collector endpoint otherwise
([WireGuard profile](#wireguard-profile)).

## Device name and setup credential

The setup SoftAP's SSID is:

```text
navfeeder-XXYYZZ
```

`XXYYZZ` is the final three bytes of the Wi-Fi SoftAP MAC in uppercase hex. The
Security 2 username is the same string, and a BLE build advertises it too.

On the first setup boot, firmware creates a random 12-character password from
an alphabet that omits visually ambiguous characters. It stores the name,
username, and password as a versioned CRC-protected record at
`nvf_setup/credential_v1`. Later setup boots load that record. A malformed
record fails closed instead of silently creating a password that disagrees
with the label.

The password is deliberately random, not derived from the MAC address. It is
all of the following:

- the SoftAP WPA2 password;
- the Security 2 password (`pop`) used by the app; and
- the device's physical setup recovery secret.

The ordinary eight-second BOOT configuration reset clears the operating
configuration but preserves `nvf_setup`, so the label and any stored app credential
continue to work. Erasing the complete NVS partition destroys both. The next
boot then creates a different password, emits a new label payload, and
requires a replacement physical label.

Development builds print the persistent setup credential on every setup boot,
including an ordinary reboot while unprovisioned and the eight-second
configuration reset. `NVF_SETUP_CONSOLE_PASSWORD` defaults on. A repeated label
is prefixed `DEVELOPMENT SETUP LABEL`; first creation uses:

```text
NEW SETUP LABEL — print and attach before deployment: WIFI:T:WPA;S:...;;
```

Disable `NVF_SETUP_CONSOLE_PASSWORD` for production manufacturing to print only
newly created labels. Secure Boot builds suppress both label log lines. This
switch does not enable chip security or change eFuses. It never prints the
password of the Wi-Fi network the observer joins.

Manufacturing must capture the first-creation line, print and verify the QR label, and attach
it inside the enclosure or another physically controlled recovery location
before the observer leaves the bench. The label should also print the device
name and password as text so setup does not depend on a phone that reads QR
codes. `tools/provisioning_label.py` validates a captured line and can invoke
`qrencode` to create the QR SVG.

The QR content is a Wi-Fi network QR. The Station app reads the device name and
password from it, and a phone camera offers to join the network:

```text
WIFI:T:WPA;S:navfeeder-A1B2C3;P:FakePass2345;;
```

Neither the device name nor the password alphabet contains a character that
form escapes. A BLE build prints Espressif's provisioning schema instead:

```json
{"ver":"v1","name":"navfeeder-A1B2C3","username":"navfeeder-A1B2C3","pop":"FakePass2345","transport":"ble"}
```

The real password is secret. Do not put a real payload in source control, shared
CI/build logs, issue trackers, or release artifacts. The attended workflow below
keeps its local capture in private files ignored by Git.

## Factory cable workflow

From the `esp32` directory, keep the S3 connected through its native USB console
and run:

```sh
export IDF_PATH=/path/to/esp-idf
PORT=/dev/cu.usbmodemXXXX make first-boot-flash
```

The target builds, flashes, and monitors the S3. Each run creates a unique
mode-0700 capture directory under `build/s3`, with mode-0600 files inside, so a
later board cannot overwrite an earlier credential. On a blank NVS partition,
keep monitoring until `NEW SETUP LABEL` appears, then leave the monitor with
**Ctrl-]**. The target extracts the line and validates it with
`tools/provisioning_label.py`. If `qrencode` is installed, it also creates a
private QR SVG. Print the QR and text device name/password before deployment.
Set `FIRST_BOOT_DIR=/new/private/path` to select a new capture directory; an
existing path is rejected.

`first-boot-flash` never erases NVS. If no label line appears, the credential
may be hidden by the production profile or the operating configuration may
already be complete. In development, use the configuration-reset gesture to
reopen setup and reprint the same credential. A normal reset while already in
setup also reprints it. The attached label and display remain available. A deliberate whole-NVS erase creates a new
secret and also destroys update keys and other NVS state, so it requires a new
physical label.

## Browser setup

Join `navfeeder-XXYYZZ` with the password printed on the label (or scan its QR),
then open `http://192.168.4.1/`. Submit Wi-Fi, collector host/port, station ID,
and enrollment token. The station ID depends on the observer:

- An observer **without** a commissioning record needs one: the name its
  enrollment token was issued for.
- A **commissioned** observer names itself. It connects as the observer ID its
  record names, `board-<kind>-<serial>`
  ([board identity](../../docs/BOARD-IDENTITY.md)), because a collector accepts
  its hardware evidence only under that name. The form shows that ID in place of
  the station field and refuses a submitted one, so a typed name can never
  displace the record's. Saving also drops a station stored before the board was
  commissioned.

A commissioned observer that still holds a station provisioned before it was
commissioned ignores it and logs a warning at boot. That name's enrollment token
is for a different observer, so provision the unit again with the token enrolled
for its own name.

On an `NVF_WIREGUARD` build the form also has an optional **WireGuard profile**
box: paste the wg-quick `.conf` described under [WireGuard profile](#wireguard-profile)
to enable the tunnel, leave it empty to connect over the public endpoint, or
clear a stored profile by submitting the box empty. The portal validates the
complete record, saves it atomically, then reboots. Update-key pairing remains
available only through this protected setup AP.

On the ZED/X20, whose Ethernet port is its uplink, the Wi-Fi SSID field is
optional: leave it empty for a wired-only observer. On the MAX the form also has
two per-unit sensor settings, shown with their stored values: the mains frequency
where the thermocouple is used (60 or 50 Hz) and the motion profile (surface or
aerial). They are saved to the `nvf_sensor` namespace before the network record,
survive the configuration-reset gesture, and are set only through this form; the
app's endpoints do not carry them. The app always verifies a Wi-Fi network
before it saves, so a wired-only observer is set up through this form.

Setup runs only while the observer is unprovisioned. Treat the label as
physical setup access; setup never removes the need for a valid collector
enrollment token.

## WireGuard profile

The optional WireGuard profile carries the GNF1/TLS session inside a tunnel to
the collector whenever the peer session is up, and falls back to the public
collector endpoint otherwise, so the tunnel is an uplink upgrade and never a
single point of failure. The tunnel is IPv4-only. Only the collector's tunnel
address is routed through it; SNTP, OTA downloads and DNS keep the ordinary
uplink. The first handshake waits for a plausible wall clock, because a
WireGuard handshake carries a timestamp the peer rejects if it is not newer than
the last it accepted from that key.

The collector operator runs a WireGuard peer and issues one profile per
observer. Generate the observer's keypair, add the observer as a peer on the
server with its tunnel address as a `/32`, and hand the operator a wg-quick(8)
`.conf` whose `[Peer]` `AllowedIPs` is the collector's tunnel address as a
single `/32`:

```ini
[Interface]
PrivateKey = <observer private key>
Address = 10.77.0.12/24

[Peer]
PublicKey = <collector public key>
Endpoint = wg.collector.invalid:51820
AllowedIPs = 10.77.0.1/32
PersistentKeepalive = 25
```

The browser form and the Station app parse exactly this format: one
`[Interface]` and one `[Peer]`, an IPv4 `Address`, an `Endpoint` of `host:port`
(IPv4 or DNS), and `AllowedIPs` naming the collector's single `/32`. wg-quick
host-side keys (`ListenPort`, `DNS`, `MTU`, `Table`, `PreUp`, and the like) are
accepted and ignored; any other key, a second section, or an IPv6 literal is
refused. Treat the profile as a secret: it contains the observer's private key.

## Station app setup

The app uses Espressif's [`ESPProvision` Swift package][ios] with its SoftAP
transport and Security 2. The observer serves Unified Provisioning's endpoints
(`proto-ver`, `prov-session`, `prov-config`, `prov-scan`, `prov-ctrl`) and the
custom ones below as HTTP POSTs on the browser form's server at
`http://192.168.4.1/`. The app should:

1. scan the physical QR (or take the device name and password as typed);
2. create the ESPProvision SoftAP device with the device name and the label
   password as the network passphrase. It joins the setup network through
   `NEHotspotConfiguration`, which iOS asks the operator to allow, and so needs
   the Hotspot Configuration entitlement and local-network access;
3. read `proto-ver`, then establish the Security 2 session with the device name
   as username and the label password as `pop`;
4. require the `navfeeder` app capability `nav-config-v1` (and check for the
   optional `nav-tunnel-v1` capability before offering a WireGuard profile);
5. if the device advertises `nav-identity-v1`, ask the `nav-identity` endpoint
   whether the observer is commissioned, and if it is, show its name instead of
   asking for a station ID;
6. send the collector settings to the custom `nav-config` endpoint;
7. if the operator supplied a WireGuard profile and the device advertised
   `nav-tunnel-v1`, send it to the `nav-tunnel` endpoint;
8. use the standard provisioning API to scan for and submit the Wi-Fi SSID and
   password; and
9. wait for Wi-Fi verification and the observer reboot.

Firmware disables provisioning auto-stop, so the `nav-config`, `nav-tunnel` and
Wi-Fi steps may arrive in any order. Sending `nav-config` (then `nav-tunnel`)
before Wi-Fi gives the clearest error handling, because the record is saved only
once Wi-Fi has verified: a profile that arrives after that save is rejected with
status 5 rather than silently dropped. Firmware saves only after it has a valid
custom request and ESP-IDF has successfully connected with the submitted Wi-Fi
credentials. Failed Wi-Fi authentication leaves the encrypted session available
for another attempt.

To verify Wi-Fi the observer joins the submitted network as a station, a second
after replying, and its SoftAP moves to that network's channel. The phone may
drop off the setup network for a moment. The session survives a new connection,
because the observer identifies it by a cookie that ESPProvision returns.

Store a remembered setup password in the iOS Keychain with a
device-only accessibility class. The physical label remains the recovery
source after app removal, phone replacement, or Keychain loss. Treat QR
possession as physical setup access. Setup runs only while the observer is
unprovisioned, and neither path removes the need for a valid collector
enrollment token.

A label with the ESP Provisioning BLE payload (JSON, `"transport":"ble"`) comes
from a [BLE build](#ble-transport); the app then uses ESPProvision's BLE
transport with the same sequence.

## `nav-config` endpoint

Endpoint name: `nav-config`

Maximum request size: 233 bytes, which also fits the Security 2 BLE transport limit.

The request is a byte string. Integer fields are unsigned and big-endian.
String fields contain printable ASCII bytes (`0x20` through `0x7e`) without a
terminating NUL.

| Offset | Size | Value |
| --- | ---: | --- |
| 0 | 4 | ASCII `NVF1` |
| 4 | 1 | Flags; must be zero |
| 5 | 2 | Collector TCP port, 1–65535 |
| 7 | 1 | Collector host length, 1–63 |
| 8 | 1 | Station ID length, 0–32; see below |
| 9 | 1 | Enrollment token length, 1–128 |
| 10 | variable | Collector host bytes |
| next | variable | Station ID bytes |
| next | variable | Enrollment token bytes |

The frame must end exactly after the token. Unknown flags, control bytes,
length mismatch, an empty host or token, and invalid ports are rejected. The
endpoint cannot set the development-only `insecure` flag.

The station ID follows the rule under [Browser setup](#browser-setup): an
observer without a commissioning record needs one, and an empty station ID is
rejected; a commissioned observer names itself, and a non-empty station ID is
rejected.

The five-byte response is ASCII `NVR1` followed by one status byte:

| Status | Meaning |
| ---: | --- |
| 0 | Custom settings accepted; waiting for verified Wi-Fi |
| 1 | Complete configuration saved; reboot scheduled |
| 2 | Invalid request |
| 3 | NVS save failed; retry is allowed |
| 4 | Configuration saved, but task creation failed; power-cycle the observer |

The endpoint response may be status 0 even though the later standard Wi-Fi
operation completes the save and reboots the observer. A disconnect shortly
after Wi-Fi success is therefore expected. On reconnect, the app should confirm
that the observer appears as an enrolled station rather than assuming success
from the disconnect alone.

## `nav-identity` endpoint

Endpoint name: `nav-identity`. Advertised by the `nav-identity-v1` capability.
Like every custom endpoint it answers only inside the Security 2 session, so the
observer's name reaches a holder of the device label, not anyone in radio range
or on the setup network.

The request is exactly the four ASCII bytes `NVI1`; anything else gets an error
rather than an answer. The response is `NVI1`, one length byte, and that many
bytes of printable ASCII:

| Length | Meaning |
| ---: | --- |
| 0 | No commissioning record: send a station ID in `nav-config` |
| 1–75 | The commissioned observer ID: send an empty station ID, and confirm the observer at the collector under this name |

The app accepts only a canonical `board-<four hex>-<hex>` name and treats
anything else as an invalid response. A board reads its commissioning record at
boot, so a record installed at the bench names the observer from its next boot.

## `nav-tunnel` endpoint

Endpoint name: `nav-tunnel`. Present only on a build with `NVF_WIREGUARD`, which
advertises the `nav-tunnel-v1` capability. An app must check that capability
before sending; a build without it has no such endpoint.

The profile is described under [WireGuard profile](#wireguard-profile).

Maximum request size: 178 bytes, which also fits the Security 2 BLE transport limit.
Integer fields are unsigned and big-endian. The keys are the raw 32-byte values,
not base64. An all-zero preshared key means none. The endpoint host is printable
ASCII (`0x20`–`0x7e`, restricted to a DNS name or IPv4 literal) with no NUL.

| Offset | Size | Value |
| --- | ---: | --- |
| 0 | 4 | ASCII `NVT1` |
| 4 | 1 | Flags; must be zero |
| 5 | 32 | Interface private key |
| 37 | 32 | Peer public key |
| 69 | 32 | Peer preshared key (all zero = none) |
| 101 | 4 | This observer's tunnel address (IPv4) |
| 105 | 1 | Address prefix length, 1–32 |
| 106 | 4 | Collector's tunnel address (IPv4), the sole AllowedIPs /32 |
| 110 | 2 | Endpoint UDP port, 1–65535 |
| 112 | 2 | Persistent keepalive seconds, 0 = off |
| 114 | 1 | Endpoint host length, 1–63 |
| 115 | variable | Endpoint host bytes |

The frame must end exactly after the host. Unknown flags, a zero key, an
address equal to the collector, an out-of-range prefix or port, a non-printable
host, and any length mismatch are rejected. The five-byte response is ASCII
`NVR1` followed by the same status byte as `nav-config`, plus one value specific
to this endpoint:

| Status | Meaning |
| ---: | --- |
| 0 | Profile accepted; waiting for verified Wi-Fi |
| 2 | Invalid request |
| 3 | NVS save failed; retry is allowed |
| 5 | Configuration already saved; the profile was not applied |

Status 5 means Wi-Fi verification completed the save before the profile
arrived. The app must send the profile before Wi-Fi; on status 5 it should
reset the device and provision again, tunnel first.

## BLE transport

An image built with `NVF_BLE_PROVISIONING` runs the Station app's provisioning
over Bluetooth Low Energy instead of the setup network, with the same Security 2
session and endpoints. The SoftAP and browser form still run, and the setup
label is the ESP Provisioning BLE payload. The S3 image leaves Bluetooth off,
because the BLE stack does not fit beside the SoftAP in internal RAM.

Measured on the universal image in October 2026: with stock settings, Wi-Fi
initialization left 72 KB of internal heap, starting BLE provisioning exhausted
it, the SoftAP failed to allocate its beacon, and the board reset in a loop.
With NimBLE trimmed to one peripheral connection and two controller activities,
and Wi-Fi's IRAM optimizations turned off to free 18 KB more, BLE provisioning
started and took 67 KB, leaving 12 KB free at the setup low point and 28 KB
once setup was idle, before any app session. Without Bluetooth, and with app
provisioning on the SoftAP's web server, the image keeps 65 KB at the low point
and 79 KB idle.

Two ways to make room for BLE were set aside. Placing Wi-Fi and lwIP packet
buffers in PSRAM would put the setup form's plaintext submissions there, while
this board keeps secrets and ordinary allocations in internal RAM. Running the
Bluetooth controller from flash disturbs Bluetooth during flash erases, which
provisioning performs when it saves. BLE would therefore need internal RAM freed
elsewhere, or setup that does not run BLE and the SoftAP together.

`NVF_BLE_PROVISIONING` requires NimBLE with `WIFI_PROV_BLE_SEC_CONN`. The build
refuses Bluetooth enabled without it: that is an `sdkconfig` generated before
the S3 image turned Bluetooth off. Start such a build directory afresh.

## Physical label and flash protection

The setup secret is visible on the physical label by design. Production flash
encryption, Secure Boot, and NVS encryption still protect collector, Wi-Fi,
update, and identity material against flash extraction and modification. Their
production transition is specified in [SOFTWARE-UPDATES.md](SOFTWARE-UPDATES.md#production-esp32-security);
this repository's current development/service profile does not burn those
irreversible eFuses.

[unified]: https://docs.espressif.com/projects/esp-idf/en/latest/esp32s3/api-reference/provisioning/provisioning.html
[ios]: https://github.com/espressif/esp-idf-provisioning-ios
