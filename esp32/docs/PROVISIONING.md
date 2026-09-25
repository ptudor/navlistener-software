# ESP32-S3 provisioning contract

The custom observer offers two setup paths whenever its complete operating
configuration is absent:

1. Bluetooth Low Energy through [ESP-IDF Unified Provisioning][unified], using
   Security 2 (SRP6a authentication and key exchange, then AES-GCM); and
2. the existing WPA2 SoftAP and browser form at `http://192.168.4.1/`.

Both are available at the same time and commit the same validated `netcfg`
record. BLE is the normal Station app path. SoftAP is the browser fallback.
The ESP32-C6 development build retains SoftAP without BLE.

Both paths also accept an **optional WireGuard profile** on the S3, which carries
the GNF1/TLS session inside a tunnel to the collector whenever the peer is up and
falls back to the public collector endpoint otherwise. It is described under
[`nav-tunnel` endpoint](#nav-tunnel-endpoint) and
[Browser fallback](#browser-fallback). The C6 build has no tunnel support and
always uses the public endpoint.


## Device name and setup credential

The advertised BLE name and SoftAP SSID are both:

```text
navfeeder-XXYYZZ
```

`XXYYZZ` is the final three bytes of the Wi-Fi SoftAP MAC in uppercase hex. The
Security 2 username is the same string.

On the first setup boot, firmware creates a random 12-character password from
an alphabet that omits visually ambiguous characters. It stores the name,
username, and password as a versioned CRC-protected record at
`nvf_setup/credential_v1`. Later setup boots load that record. A malformed
record fails closed instead of silently creating a password that disagrees
with the label.

The password is deliberately random, not derived from the MAC address. It is
all of the following:

- the Security 2 password (`pop`) used by the app;
- the SoftAP WPA2 password; and
- the device's physical setup recovery secret.

The ordinary eight-second BOOT configuration reset clears the operating
configuration but preserves `nvf_setup`, so the label and stored app credential
continue to work. Erasing the complete NVS partition destroys both. The next
boot then creates a different password, emits a new label payload, and
requires a replacement physical label.

Development builds print the persistent setup credential on every setup boot,
including an ordinary reboot while unprovisioned and the eight-second
configuration reset. `NVF_SETUP_CONSOLE_PASSWORD` defaults on. A repeated label
is prefixed `DEVELOPMENT SETUP LABEL`; first creation uses:

```text
NEW SETUP LABEL — print and attach before deployment: { ... }
```

Disable `NVF_SETUP_CONSOLE_PASSWORD` for production manufacturing to print only
newly created labels. Secure Boot builds suppress both label log lines. This
switch does not enable chip security or change eFuses. It never prints the
password of the Wi-Fi network the observer joins.

Manufacturing must capture the first-creation line, print and verify the QR label, and attach
it inside the enclosure or another physically controlled recovery location
before the observer leaves the bench. The label should also print the device
name and password as text so browser recovery does not depend on a QR-capable
app. `tools/provisioning_label.py` validates a captured line and can invoke
`qrencode` to create the QR SVG.

The QR content follows Espressif's provisioning schema:

```json
{"ver":"v1","name":"navfeeder-A1B2C3","username":"navfeeder-A1B2C3","pop":"FakePass2345","transport":"ble"}
```

The real `pop` is secret. Do not put a real payload in source control, shared
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

## Station app sequence

Use Espressif's [`ESPProvision` Swift package][ios] and its BLE transport with
Security 2. The app should:

1. scan the physical QR;
2. create the ESPProvision device using `name`, `username`, and `pop`;
3. establish the encrypted session and read `proto-ver`;
4. require the `navfeeder` app capability `nav-config-v1` (and check for the
   optional `nav-tunnel-v1` capability before offering a WireGuard profile);
5. send the collector settings to the custom `nav-config` endpoint;
6. if the operator supplied a WireGuard profile and the device advertised
   `nav-tunnel-v1`, send it to the `nav-tunnel` endpoint;
7. use the standard provisioning API to scan for and submit the Wi-Fi SSID and
   password; and
8. wait for Wi-Fi verification and the observer reboot.

Firmware disables provisioning auto-stop, so the `nav-config`, `nav-tunnel` and
Wi-Fi steps may arrive in any order. Sending `nav-config` (then `nav-tunnel`)
before Wi-Fi gives the clearest error handling, because the record is saved only
once Wi-Fi has verified: a profile that arrives after that save is rejected with
status 5 rather than silently dropped. Firmware saves only after it has a valid
custom request and ESP-IDF has successfully connected with the submitted Wi-Fi
credentials. Failed Wi-Fi authentication leaves the encrypted session available
for another attempt.


Store a remembered setup password in the iOS Keychain with a
device-only accessibility class. The physical label remains the recovery
source after app removal, phone replacement, or Keychain loss. Treat QR
possession as physical setup access: BLE and SoftAP run only while the observer
is unprovisioned, and neither path removes the need for a valid collector
enrollment token.

## `nav-config` endpoint

Endpoint name: `nav-config`

Maximum request size: 233 bytes, below the Security 2 BLE transport limit.

The request is a byte string. Integer fields are unsigned and big-endian.
String fields contain printable ASCII bytes (`0x20` through `0x7e`) without a
terminating NUL.

| Offset | Size | Value |
| --- | ---: | --- |
| 0 | 4 | ASCII `NVF1` |
| 4 | 1 | Flags; must be zero |
| 5 | 2 | Collector TCP port, 1–65535 |
| 7 | 1 | Collector host length, 1–63 |
| 8 | 1 | Station ID length, 1–32 |
| 9 | 1 | Enrollment token length, 1–128 |
| 10 | variable | Collector host bytes |
| next | variable | Station ID bytes |
| next | variable | Enrollment token bytes |

The frame must end exactly after the token. Unknown flags, control bytes,
length mismatch, empty fields, and invalid ports are rejected. The endpoint
cannot set the development-only `insecure` flag.

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
from the BLE disconnect alone.

## `nav-tunnel` endpoint

Endpoint name: `nav-tunnel`. Present only on a build with `NVF_WIREGUARD`, which
advertises the `nav-tunnel-v1` capability. An app must check that capability
before sending; a build without it has no such endpoint.

The optional WireGuard profile carries the GNF1/TLS session inside a tunnel to
the collector whenever the peer session is up, and falls back to the public
collector endpoint otherwise, so the tunnel is an uplink upgrade and never a
single point of failure. The tunnel is IPv4-only. Only the collector's tunnel
address is routed through it; SNTP, OTA downloads and DNS keep the ordinary
uplink. The first handshake waits for a plausible wall clock, because a
WireGuard handshake carries a timestamp the peer rejects if it is not newer than
the last it accepted from that key.

Maximum request size: 178 bytes, below the Security 2 BLE transport limit.
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

### Generating a profile

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

The Station app and the browser portal parse exactly this format: one
`[Interface]` and one `[Peer]`, an IPv4 `Address`, an `Endpoint` of `host:port`
(IPv4 or DNS), and `AllowedIPs` naming the collector's single `/32`. wg-quick
host-side keys (`ListenPort`, `DNS`, `MTU`, `Table`, `PreUp`, and the like) are
accepted and ignored; any other key, a second section, or an IPv6 literal is
refused. Treat the profile as a secret: it contains the observer's private key.

## Browser fallback


Join `navfeeder-XXYYZZ` with the password printed on the label, then open
`http://192.168.4.1/`. Submit Wi-Fi, collector host/port, station ID, and
enrollment token. On an `NVF_WIREGUARD` build the form also has an optional
**WireGuard profile** box: paste the wg-quick `.conf` above to enable the
tunnel, leave it empty to connect over the public endpoint, or clear a stored
profile by submitting the box empty. The portal parses and validates the
profile with the same rules as the `nav-tunnel` endpoint, applies the same
atomic save used by BLE, then reboots. Update-key pairing remains available only
through this protected setup AP.

On the ZED/X20, whose Ethernet port is its uplink, the Wi-Fi SSID field is
optional: leave it empty for a wired-only observer. The Station app sequence
above always verifies a Wi-Fi network before it saves, so a wired-only
observer is set up through this form.


The setup secret is visible on the physical label by design. Production flash
encryption, Secure Boot, and NVS encryption still protect collector, Wi-Fi,
update, and identity material against flash extraction and modification. Their
production transition is specified in [SOFTWARE-UPDATES.md](SOFTWARE-UPDATES.md#production-esp32-security);
this repository's current development/service profile does not burn those
irreversible eFuses.

[unified]: https://docs.espressif.com/projects/esp-idf/en/latest/esp32s3/api-reference/provisioning/provisioning.html
[ios]: https://github.com/espressif/esp-idf-provisioning-ios
