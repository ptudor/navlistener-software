# ESP32-S3 provisioning contract

The custom observer offers two setup paths whenever its complete operating
configuration is absent:

1. Bluetooth Low Energy through [ESP-IDF Unified Provisioning][unified], using
   Security 2 (SRP6a authentication and key exchange, then AES-GCM); and
2. the existing WPA2 SoftAP and browser form at `http://192.168.4.1/`.

Both are available at the same time and commit the same validated `netcfg`
record. BLE is the normal Station app path. SoftAP is the browser fallback.
The ESP32-C6 development build retains SoftAP without BLE.

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
boot then creates a different password, emits a new label payload once, and
requires a replacement physical label.

Firmware prints the secret only on the boot that creates it:

```text
NEW SETUP LABEL — print and attach before deployment: { ... }
```

Manufacturing must capture that line, print and verify the QR label, and attach
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
may already exist or the operating configuration may already be complete. Use
the attached label or display. A deliberate whole-NVS erase creates a new
secret and also destroys update keys and other NVS state, so it requires a new
physical label.

## Station app sequence

Use Espressif's [`ESPProvision` Swift package][ios] and its BLE transport with
Security 2. The app should:

1. scan the physical QR;
2. create the ESPProvision device using `name`, `username`, and `pop`;
3. establish the encrypted session and read `proto-ver`;
4. require the `navfeeder` app capability `nav-config-v1`;
5. send the collector settings to the custom `nav-config` endpoint;
6. use the standard provisioning API to scan for and submit the Wi-Fi SSID and
   password; and
7. wait for Wi-Fi verification and the observer reboot.

Firmware disables provisioning auto-stop, so steps 5 and 6 may arrive in either
order. Sending `nav-config` first gives the clearest error handling. Firmware
saves only after it has a valid custom request and ESP-IDF has successfully
connected with the submitted Wi-Fi credentials. Failed Wi-Fi authentication
leaves the encrypted session available for another attempt.

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

## Browser fallback

Join `navfeeder-XXYYZZ` with the password printed on the label, then open
`http://192.168.4.1/`. Submit Wi-Fi, collector host/port, station ID, and
enrollment token. The portal applies the same validation and atomic save used
by BLE, then reboots. Update-key pairing remains available only through this
protected setup AP.

The setup secret is visible on the physical label by design. Production flash
encryption, Secure Boot, and NVS encryption still protect collector, Wi-Fi,
update, and identity material against flash extraction and modification. Their
production transition is specified in [SOFTWARE-UPDATES.md](SOFTWARE-UPDATES.md#production-esp32-security);
this repository's current development/service profile does not burn those
irreversible eFuses.

[unified]: https://docs.espressif.com/projects/esp-idf/en/latest/esp32s3/api-reference/provisioning/provisioning.html
[ios]: https://github.com/espressif/esp-idf-provisioning-ios
