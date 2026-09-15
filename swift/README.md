# Integrity Station

SwiftUI client for iOS 18 and macOS 15. It reads the collector's v2 API;
configure the server address and credentials in the app.

## Observer hardware

Authorized private station details show ESP32-S3 board environment, RAM buffer
usage and losses, firmware identity, RTC flags, security-chip boot diagnostics,
manifest EEPROM identity, and GNSS/RTC pulse timing. Temperatures remain separate;
pressure is local absolute pressure. Pulse periods use the ESP capture clock and
wrapped RTC/GNSS phase does not measure UTC accuracy.

The detail screen refreshes an available private timing stream once per second
while active. Other observer polling retains its 30-second cadence. Sensor and
timing reports have separate freshness indicators, and cached samples remain
marked stale. A recent receipt with no sample UTC cannot establish replay age.
Board diagnostics do not change the collector's GNSS integrity verdict. Public
feeds do not contain board telemetry.

The latest receiver interference context shows its transition report and the
preceding report from the same boot. This is bounded live context; the app does
not query the collector's database sensor history or the device's local journal.

## Bluetooth setup (iPhone and iPad)

Choose **Set up an ESP32-S3 observer** in onboarding or Settings. Scan the physical
setup label, or enter its device name and password. The app requires Security 2
and the firmware's `nav-config-v1` capability. It then sends the collector's
feeder hostname, GNF1 TLS port, assigned station ID and enrollment token before
submitting Wi-Fi credentials. The collector read token is a separate credential.

Remembered setup passwords use a separate device-only Keychain service. The app
does not persist Wi-Fi passwords or observer enrollment tokens. The physical
label remains the recovery source; library credential logging is disabled.
The iOS target pins [Espressif ESPProvision](https://github.com/espressif/esp-idf-provisioning-ios)
to version 3.1.0; Xcode resolves it through Swift Package Manager.

With an authorized private collector connection available before configuration,
**Check collector** confirms a new boot for the enrolled station after setup
began, then adds the station to the app. Wi-Fi success or a Bluetooth disconnect
alone leaves confirmation pending. If private read access is unavailable, confirm
enrollment directly with the collector. Wi-Fi failures can be retried; reconnect
from the setup form after a lost session.

The protected browser portal remains available at `http://192.168.4.1/` after
joining the observer's setup network with its label password. See the
[firmware provisioning contract](../esp32/docs/PROVISIONING.md) for reset behavior.
BLE setup needs a physical observer and an iOS device; simulator tests cover the
protocol, setup state transitions and UI, but cannot verify radio interaction.

## Build

Install Xcode and XcodeGen, then generate the project from this directory:

```sh
xcodegen generate
open IntegrityStation.xcodeproj
```

Choose the `IntegrityStation` scheme for iOS or `IntegrityStationMac` for
macOS. Set your own development team and a suitable bundle identifier in
Xcode's Signing & Capabilities settings when signing for a device or distribution.
No developer team is included in `project.yml`; regeneration replaces generated
project settings, so keep recurring local signing overrides outside version control.

To run the macOS unit tests without distribution signing:

```sh
xcodebuild -project IntegrityStation.xcodeproj -scheme IntegrityStationMac \
  -destination 'platform=macOS' CODE_SIGNING_ALLOWED=NO test
```
