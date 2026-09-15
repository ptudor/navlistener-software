# Operating software updates

The updater, signing adapters, local test repository, release publisher,
collector controls and app status are implemented. Development remains
unlocked and starts in **Manual** mode. Production releases require real
production keys and an explicit external release configuration; there is no
built-in private key or default signing-key pathname.

## During development

- Setup prints the persistent BLE/SoftAP password on the console each time
  setup starts, including after a configuration reset. This is the setup
  password, not the password of the Wi-Fi network the observer joins.
- Secure Boot builds and chips with Secure Boot already enabled suppress
  these console secrets. Ordinary development does not enable Secure Boot.
- S3 **layout 3** doubles the factory and both OTA slots to **4 MiB each**.
  It retains a 3.25 MiB unused spool reservation, 128 KiB update-state NVS and
  the 512 KiB journal. The partition table moves to `0x10000` to accommodate
  the secure bootloader. `partitions.csv` is retained for the unsupported C6's old 4 MB-flash layout;
  `partitions-s3.csv` describes the installed 16 MB S3 module.
- Moving an existing development board to this layout is a USB erase/reflash
  followed by fresh provisioning and a new setup label. Save the connection
  settings first; the erase also clears the journal. Use a fresh build directory
  as shown in the README. App-only OTA cannot change the partition table.
  Subsequent configuration resets reprint the same persistent setup password.

Build/flash instructions remain in the [ESP32 README](../README.md). The
attended `first-boot-flash` command refuses builds that enable chip locking.
No updater or release command runs eFuse programming.

## Store the test setup once

Run from the software repository root. These are persistent folders outside
Git, with private keys readable only by their owner:

```sh
python3 -m venv "$HOME/.local/share/navlisten/releases-venv"
"$HOME/.local/share/navlisten/releases-venv/bin/python" -m pip install \
  --require-hashes -r tools/releases/requirements.lock

make release-test-setup \
  RELEASE_PYTHON="$HOME/.local/share/navlisten/releases-venv/bin/python" \
  KEYS="$HOME/.config/navlisten/update-test-keys" \
  STATE="$HOME/.local/state/navlisten/update-test"
```

Run setup once; it refuses to overwrite existing keys or state. Keep the
whole key folder together. It contains `signers.json`, clearly named
`TEST-ONLY-…` keys, and a warning marker. `trust-root.json` in the state folder
contains public keys and may be embedded in an explicitly selected test build.
The production entry points reject this test configuration.

The routine host checks need no device, signing service or production key:

```sh
make release-tests \
  RELEASE_PYTHON="$HOME/.local/share/navlisten/releases-venv/bin/python"
make -C esp32/components/ota/test
```

To compile with test trust, activate the pinned ESP-IDF environment, then:

```sh
cd esp32
idf.py -B build/update-test \
  -D SDKCONFIG=build/update-test/sdkconfig \
  -D 'SDKCONFIG_DEFAULTS=sdkconfig.defaults;sdkconfig.defaults.s3;sdkconfig.defaults.update-test' \
  -D IDF_TARGET=esp32s3 \
  -D "NVF_TUF_ROOT_FILE=$HOME/.local/state/navlisten/update-test/trust-root.json" build
cd ..
```

Create a signed local fixture using that image's compiled build number:

```sh
make release-test \
  RELEASE_PYTHON="$HOME/.local/share/navlisten/releases-venv/bin/python" \
  KEYS="$HOME/.config/navlisten/update-test-keys/signers.json" \
  STATE="$HOME/.local/state/navlisten/update-test" \
  IMAGE=esp32/build/update-test/navfeeder-esp.bin RELEASE="$(cat BUILD_NUMBER)"
```

This signs locally and makes a TUF repository. It does not publish test
firmware to the live origin. The device's network updater still uses the
approved firmware origins below; compiling a test root does not upload the
local fixture. The host tests exercise repository verification and interrupted
publication locally. Physical power-loss, trial-boot rollback and observation
timing tests remain necessary before production approval.

## Hostnames and publishing

| Service | Primary | Secondary |
| --- | --- | --- |
| Collector and app API | `in.intsat.net` | `in.intsat.space` |
| Firmware | `firmware.intsat.net` | `firmware.intsat.space` |
| Collector machine alias | `klax1-navlistener.intsat.net` | `klax1-navlistener.intsat.space` |

Both firmware origins use `/firmware/v1/` on the existing collector server.
Its separate static directory is `/usr/local/www/navlistener-firmware`.
DNS aliases remain within their own domain, and TLS verifies the hostname
actually used. The second domain helps with domain-specific failures; both
names still share a server and network connection.

The C feeder, ESP pusher and Station app retry the approved collector alias
after connection/service failures. Authentication rejection is not a reason
to bypass credential checks. Firmware metadata and image downloads also try
the second origin and keep their TLS, digest and signature checks.

Other names were audited: the `traffic` pair has matching DNS records; apex
and `www` are separate websites and are not collector aliases. Custom receiver
addresses, explicit local DNS/NTP sources and tunnel endpoints remain their
own configured services. An arbitrary hostname is not rewritten by suffix.

### When production keys are ready

Keep a release configuration outside Git. Its fields are:

```json
{
  "production_approved": false,
  "state_dir": "/secure/navlisten/releases",
  "signers": "/secure/navlisten/signers.json",
  "root": "/secure/navlisten/releases/trust-root.json",
  "branch": "main",
  "remotes": ["origin", "github"],
  "build_command": ["python3", "/path/to/software/tools/releases/build_idf.py"],
  "publish_command": ["/secure/navlisten/publish-origin"],
  "hardware_min": 1,
  "hardware_max": 1
}
```

Replace the example paths. Leave `production_approved` false until keys,
hardware commissioning and rollback/soak testing have been reviewed. Initialize
production repository state with `release.py init-production --keys SIGNERS_JSON
--state STATE_DIRECTORY` after supplying the production adapters. The generated
public root is the one used by the firmware build and public verifier.

The publish wrapper uses the approved server transport to run:

```text
/usr/local/bin/python3.12 /usr/local/libexec/navlisten-release-origin.py --root /usr/local/www/navlistener-firmware
```

Install that program from `tools/releases/origin.py`. It receives a JSON header
line and file bytes on stdin and returns a digest receipt. It has no signing
keys. It refuses path escapes, replacement of immutable objects and a timestamp
commit based on outdated state. Production publication uploads immutable files,
checks identical bytes through both hostnames, and publishes timestamp last.

Activate the pinned ESP-IDF v5.5.4 environment and set these make variables:

```sh
export RELEASE_CONFIG=/secure/navlisten/release.json
export RELEASE_PYTHON="$HOME/.local/share/navlisten/releases-venv/bin/python"
make release-dry-run
make release NOTES=RELEASE-NOTES.md
```

`make release` reserves and commits `BUILD_NUMBER`, builds the exact commit in
two isolated directories, requires matching ELF/image bytes, signs the image
and TUF metadata, and pushes the source/tag to both software remotes. The
initial selection is Lab at 100%. A signed transaction records progress.

```sh
make release-resume RELEASE=42
make release-check RELEASE=42
make release-promote RELEASE=42 CHANNEL=canary PERCENT=1
make release-promote RELEASE=42 CHANNEL=stable PERCENT=100
make release-withdraw RELEASE=42 CHANNEL=stable
make release-refresh-online
```

Use the actual reserved number; metadata-only transactions print names such
as `metadata-7`, which `release-resume` also accepts. Do not edit or delete
published images or reduce metadata versions to undo an interrupted release.
Resume the transaction, or publish a higher-version correction. Withdrawal
prevents another install; it does not remotely downgrade running firmware.
The bounded release index retains channel-selected releases and recent releases;
older unselected entries leave the index when it would exceed 32 targets.
Their immutable files remain on disk for audit and retention.

Renew online metadata regularly: timestamp expires after 14 days, the other
online roles after 45 days. Offline targets/releases expire after one year
and root after two years; renewing or rotating those authorities requires the
offline signers. Preserve the old-to-new root chain. Freshness checks deliberately
stop updates when metadata expires, while normal collection continues.

## Signing adapters and the dangerous parts

There are two independent kinds of signing key:

- **Firmware keys** authorize code that can boot. Three separate RSA-3072
  public-key digests reserve the active and recovery slots.
- **Metadata keys** authorize releases and channel choices. Root, targets and
  releases each require two of three separate P-256 keys. Stable, Canary, Lab,
  snapshot and timestamp each use their own online key.

`signers.json` lists each metadata role under `roles`, with `public` containing
the TUF key (`keyid`, `keytype`, `scheme`, `keyval.public`) and `command` containing
an argument array. A command reads canonical metadata bytes and returns a TUF
signature JSON object. `firmware` lists three entries with `public` PEM path,
`key_id` and optional `command`; `firmware_active` selects the active index.
That command reads padded image bytes and returns a 384-byte RSA-PSS signature.
Set `test_only` to false only in a separately created production configuration.
The unused third offline signer may be disconnected; two valid signatures are
still required. Adapter output is verified against its configured public key.

The supplied `file_signer.py` supports encrypted private PEM files outside Git,
with mode `0600`. It prompts for the passphrase on the terminal, or accepts the
*name* of a private environment variable through `--passphrase-env`. Add
`--firmware` for the RSA adapter. A vault or hardware signer can implement the
same stdin/stdout interface. No private-key path enters the firmware build.

**Do not flash a production bootloader onto an unlocked board yet.** Its first
boot can permanently enable Secure Boot and flash encryption and restrict USB
recovery. Normal update commands do not do this. The production profile is
provided for build verification; chip locking is a separate attended operation.

Before locking a chip, retain encrypted backups of the production signing
keys and their passphrases, verify the three intended public-key digests and
recovery slots, and test a signed factory recovery image. Losing every trusted
signer can prevent future firmware updates. Disclosing signing keys can let
someone authorize firmware. Keep private keys off Git and the firmware server;
public roots, public key digests and signed images are safe to distribute.

## Device and collector controls

The local tool uses the separately paired per-device HMAC key:

```sh
python3 esp32/tools/ota.py --device DEVICE status
python3 esp32/tools/ota.py --device DEVICE check --key-file PRIVATE_PAIRING_KEY
python3 esp32/tools/ota.py --device DEVICE update --key-file PRIVATE_PAIRING_KEY
python3 esp32/tools/ota.py --device DEVICE set-policy --key-file PRIVATE_PAIRING_KEY --mode download --channel lab
```

Download, install and cancel are also separate commands. `service-recovery`
is the explicit attended image/URL path. An install that discards backlog
requires the explicit `--discard-backlog` option; the response reports records
present when producers paused. Use normal safe install for unattended devices.

Collector controls are opt-in. `[updates]` requires an absolute `state_file`,
an absolute `repository_path` pointing to the published static tree, and
`[[updates.principal]]` entries with `id`, `token_sha256` and explicit
`[[updates.principal.device]]` grants containing `observer_id`, `enrollment_id`,
`organization_id` and `collector_instance_id`. A read credential gains no update
permission unless it is also explicitly granted here. Changes take effect when
the collector restarts. Do not expose its private request-state file.

The app shows controls only when its current private-session credential has
that grant. Requested/accepted is a command receipt; downloaded, rebooting,
confirmed and rolled-back come from device telemetry. The collector retains
32 recent requests and 32 state transitions per device. UUID retries within
that retained history reuse the original command. Device command numbers are
monotonic and persist before side effects. Old connections cannot replace the
latest admitted session's updater state.

The app can check, download, safely install, update now or cancel. Mode/channel
changes use the paired local tool. Automatic installation requires a collector
configured for durable acknowledgments, an empty backlog, a record-boundary
producer pause and a final durable acknowledgment. A live-only collector makes
the device wait; the updater does not silently discard observations.

Metrics include update checks, errors, rollbacks, release adoption, security
profile drift, last report time, staging time and time waiting for safe reboot.
Use report freshness alongside staging/waiting timestamps when alerting.
