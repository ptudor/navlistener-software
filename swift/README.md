# Integrity Station

SwiftUI client for iOS 18 and macOS 15. It reads the collector's v2 API;
configure the server address and credentials in the app.

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
