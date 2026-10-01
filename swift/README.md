# Integrity Station

SwiftUI client for iOS 18 and macOS 15. It reads the collector's v2 API;
configure the server address and credentials in the app.

## Observer hardware

Authorized private station details show ESP32-S3 board environment, RAM buffer
usage and losses, firmware identity, RTC flags, security-chip boot diagnostics,
manifest EEPROM identity, and GNSS/RTC pulse timing. Temperatures remain separate;
pressure is local absolute pressure. Pulse periods use the ESP capture clock and
wrapped RTC/GNSS phase does not measure UTC accuracy.

The detail screen refreshes an available private timing or reception stream once per second
while active. Other observer polling retains its 30-second cadence. Sensor and
timing reports have separate freshness indicators, and cached samples remain
marked stale. A recent receipt with no sample UTC cannot establish replay age.
Board diagnostics do not change the collector's GNSS integrity verdict. Public
feeds do not contain board telemetry.

Expected reception shows observed/expected counts and separate edge and collector
verdicts for each constellation. An alarm from either contributes a station
warning. Unknown or stale coverage keeps a reported alarm visible as held, and
disagreement remains explicit. See [reception rules](../docs/RECEPTION.md).

The latest receiver interference context shows its transition report and the
preceding report from the same boot. A separate **Sensor history** card reads the
private historian API for temperature, humidity, absolute pressure and RTC/GNSS
phase over one hour through seven days. Charts break at missing readings, long
receipt gaps and reboots; individual samples retain source UTC and verification.
History is bounded by collector startup/current audience policy, even when older
rows remain stored. It does not read the device's local journal.

History stays in memory, is limited to 5,000 displayed samples, and is discarded
when its selection retires or access fails. Each page rechecks the collector's
grant and validates its receiver, audience, bounds and revision. The sample table
remains available alongside the native chart. Missing historian support appears
as an explicit unavailable response.

Live and historical samples show collector `hardware_trust` independently of
the firmware-reported track. Shared status/freshness rules and palette decisions
are recorded in the [station presentation contract](../docs/STATION-PRESENTATION.md).
Integrity Station keeps its name and native typography. The **My fleets** tab
signs in to the Django owner portal with the same account
as the website. It reads approved inventory, specifications, live conditions and
history through revocable portal sessions. Direct collector mode remains available
with its separate credentials.

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

## Owner portal sign-in

Open **My fleets**, enter the portal HTTPS URL including its application path,
and choose **Sign in through browser**. Approve the connection after normal
portal login. The same personal and group stations appear with their approved
labels/specifications; no collector service credential is copied to the app.
The default public installation address is `https://stations.intsat.net/stations/`;
customer origins and prefixes are supported without rebuilding the app.

The native session is stored in an installation-scoped Keychain service. Fleets,
readings and history remain in memory. After a Mac restart, the app validates the
saved account/session and reloads current server inventory. Opening a receiver
fetches its conditions and history window again, recovering stored readings from
while the Mac was away. Missing measurements and receiver reboot gaps remain
explicit, as do the collector's historical visibility limits.

Sign-out deletes the local credential and requests server revocation. Users can
also revoke connections from the website's account page. Browser approval uses
an exact reverse-DNS callback, random state and S256 PKCE; API requests reject
redirects and never use the browser's session cookies. The portal connection is
read-only; device controls and enrollment retain their existing authority.
