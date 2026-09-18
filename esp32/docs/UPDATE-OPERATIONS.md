# Operating software updates

The updater, signing adapters, local test repository, release publisher,
collector controls and app status are implemented. Development remains
unlocked and starts in **Manual** mode. Releases travel on two separately
rooted tracks: **trusted** for locked hardware and **open** for boards that are
never locked. Either requires real keys of its own and an explicit external
release configuration; there is no built-in private key or default signing-key
pathname. The open track does not depend on the trusted track's keys or on
locking any board, so it can publish while trusted commissioning is pending.

| Track | Build profile | Runs on | Origin path |
| --- | --- | --- | --- |
| `trusted` | `sdkconfig.defaults.production` | Locked hardware only | `/firmware/trusted/v1/` |
| `open` | `sdkconfig.defaults.open` | Unlocked hardware only | `/firmware/open/v1/` |
| `test` (bench, never published) | `sdkconfig.defaults.update-test` | Unlocked bench boards | `/firmware/test/v1/` |

A build, its embedded root, its signer configuration and every release manifest
name one profile, and none is accepted in place of another. The
[design contract](SOFTWARE-UPDATES.md#release-tracks) states what each track
does and does not protect.

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
- A board keeps the root it first trusted in `update_meta`. Moving a board to a
  different profile or a recreated repository means erasing that partition (or
  the whole flash) over USB; an app update never changes a board's track.

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
The trusted and open entry points reject this test configuration. A test setup
whose root lacks the `x_navlisten_profile` marker cannot be loaded; create
fresh key and state folders.

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
firmware to any origin. A test build asks only for `/firmware/test/v1/`, which
no tool publishes, so it never polls a release track. The host tests exercise
repository verification and interrupted publication locally. Network delivery
to real boards is exercised on the open track's Lab channel. Physical
power-loss, trial-boot rollback and observation timing tests remain necessary
before trusted approval.

## Hostnames and publishing

| Service | Primary | Secondary |
| --- | --- | --- |
| Collector and app API | `in.intsat.net` | `in.intsat.space` |
| Firmware | `firmware.intsat.net` | `firmware.intsat.space` |
| Collector machine alias | `klax1-navlistener.intsat.net` | `klax1-navlistener.intsat.space` |

Both firmware origins serve each track below its own path on the existing
collector server, from one static directory per track:

| Track | Path | Static directory |
| --- | --- | --- |
| Trusted | `/firmware/trusted/v1/` | `/usr/local/www/navlistener-firmware/trusted` |
| Open | `/firmware/open/v1/` | `/usr/local/www/navlistener-firmware/open` |

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

### Release configuration

Keep one release configuration per track outside Git. The trusted track's
fields are:

```json
{
  "track": "trusted",
  "trusted_approved": false,
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

Replace the example paths. Leave `trusted_approved` false until keys,
hardware commissioning and rollback/soak testing have been reviewed. Initialize
trusted repository state with `release.py init-trusted --keys SIGNERS_JSON
--state STATE_DIRECTORY` after supplying the trusted adapters. The generated
public root is the one used by the firmware build and public verifier.

An open configuration names `"track": "open"` and `"open_approved"` in place
of those two fields, with its own `state_dir`, `signers`, `root` and
`publish_command`. A configuration serves one track: it is refused if it
carries the other track's approval, points at signers or a root marked for
another profile, or omits `track`.

Each track's publish wrapper uses the approved server transport to run the
origin adapter against that track's directory:

```text
/usr/local/bin/python3.12 /usr/local/libexec/navlisten-release-origin.py --track trusted --root /usr/local/www/navlistener-firmware/trusted
/usr/local/bin/python3.12 /usr/local/libexec/navlisten-release-origin.py --track open --root /usr/local/www/navlistener-firmware/open
```

Install that program from `tools/releases/origin.py`. It receives a JSON header
line and file bytes on stdin and returns a digest receipt. It has no signing
keys. It refuses path escapes, replacement of immutable objects, a timestamp
commit based on outdated state, an upload prepared for a different track and a
root marked for another profile. A wrapper pointed at the wrong directory
therefore stops at its first object and writes nothing. Publication uploads
immutable files, checks identical bytes through both hostnames, and publishes
timestamp last.

Activate the pinned ESP-IDF v5.5.4 environment and set these make variables:

```sh
export RELEASE_CONFIG=/secure/navlisten/release.json
export RELEASE_PYTHON="$HOME/.local/share/navlisten/releases-venv/bin/python"
make release-dry-run
make release NOTES=RELEASE-NOTES.md
```

`make release` reserves and commits `BUILD_NUMBER`, builds the exact commit in
two isolated directories with the track's build profile, requires matching
ELF/image bytes, signs the image and TUF metadata, and pushes the source/tag to
both software remotes. Trusted releases are tagged `firmware-N` and open
releases `firmware-open-N`; the tracks share one number sequence, so a number
names one commit on one track. The initial selection is Lab at 100%. A signed
transaction records progress. Every command below acts on the track named by
`RELEASE_CONFIG`.

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

### Bringing up the open track

The open track needs its own complete key set: fourteen P-256 metadata keys
(three each for root, targets and releases; one each for Stable, Canary, Lab,
snapshot and timestamp) and three RSA-3072 firmware keys. Never reuse a trusted
key here. Generate each as an encrypted PKCS#8 file, mode `0600`, outside Git,
with a generated passphrase that is backed up with the key:

```sh
umask 077
openssl genpkey -algorithm EC -pkeyopt ec_paramgen_curve:P-256 -aes-256-cbc -out open-root-0.key
openssl pkey -in open-root-0.key -pubout -out open-root-0.pub
openssl genpkey -algorithm RSA -pkeyopt rsa_keygen_bits:3072 -aes-256-cbc -out open-firmware-0.key
openssl pkey -in open-firmware-0.key -pubout -out open-firmware-0.pub
```

`release.py signer-entry --public-key FILE.pub` prints the `public` object for
a metadata key, and with `--firmware` the `key_id` for a firmware key. It reads
no private material. Assemble them into a `signers.json` whose `profile` is
`open`, as described under [signing adapters](#signing-adapters-and-the-dangerous-parts),
then:

```sh
"$RELEASE_PYTHON" tools/releases/release.py init-open \
  --keys /secure/navlisten-open/signers.json --state /secure/navlisten-open/releases
```

Create the open static directory on the server, install its publish wrapper,
and confirm both hostnames serve `/firmware/open/v1/` under the
[static origin requirements](SOFTWARE-UPDATES.md#static-origin-requirements).
A board joins the track by one attended USB flash of an open build carrying
the generated root; every later build arrives over the network:

```sh
cd esp32
idf.py -B build/open \
  -D SDKCONFIG=build/open/sdkconfig \
  -D 'SDKCONFIG_DEFAULTS=sdkconfig.defaults;sdkconfig.defaults.s3;sdkconfig.defaults.open' \
  -D IDF_TARGET=esp32s3 \
  -D NVF_TUF_ROOT_FILE=/secure/navlisten-open/releases/trust-root.json build
python tools/production_profile.py --profile open build/open/sdkconfig
cd ..
```

Set `open_approved` once the keys are backed up and the origin serves, then
release with `RELEASE_CONFIG` naming the open configuration. New releases start
in Lab; put development boards there with `ota.py set-policy --channel lab`
and promote to Canary and Stable when they have held up. The updater needs a
readable hardware manifest: a board whose manifest EEPROM is unpopulated
reports `ELIGIBILITY_HARDWARE_UNKNOWN` and stays on its running app.

An open build never enables Secure Boot or flash encryption, and an open board
does not become a trusted one by updating. Open signing keys deserve the same
custody as trusted ones: they are the only thing standing between the network
and every unlocked board that follows the track.

## Signing adapters and the dangerous parts

There are two independent kinds of signing key:

- **Firmware keys** authorize code that can boot. Three separate RSA-3072
  public-key digests reserve the active and recovery slots.
- **Metadata keys** authorize releases and channel choices. Root, targets and
  releases each require two of three separate P-256 keys. Stable, Canary, Lab,
  snapshot and timestamp each use their own online key.

`signers.json` names its `profile` (`trusted`, `open` or `test`) and lists each
metadata role under `roles`, with `public` containing the TUF key (`keyid`,
`keytype`, `scheme`, `keyval.public`) and `command` containing an argument
array. A command reads canonical metadata bytes and returns a TUF signature
JSON object. `firmware` lists three entries with `public` PEM path, `key_id`
and optional `command`; `firmware_active` selects the active index. That
command reads padded image bytes and returns a 384-byte RSA-PSS signature.
Each track has its own separately created configuration; one marked for
another profile is refused, and only `test` may name unencrypted key files.
The unused third offline signer may be disconnected; two valid signatures are
still required. Adapter output is verified against its configured public key.

The supplied `file_signer.py` supports encrypted private PEM files outside Git,
with mode `0600`, for either release track. It prompts for the passphrase on
the terminal, or accepts the *name* of a private environment variable through
`--passphrase-env`. Add `--firmware` for the RSA adapter. A vault or hardware
signer can implement the same stdin/stdout interface. No private-key path
enters the firmware build.

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
at least one published static tree (`repository_path` for the trusted track,
`open_repository_path` for the open track; they must differ), and
`[[updates.principal]]` entries with `id`, `token_sha256` and explicit
`[[updates.principal.device]]` grants containing `observer_id`, `enrollment_id`,
`organization_id` and `collector_instance_id`. A read credential gains no update
permission unless it is also explicitly granted here. Changes take effect when
the collector restarts. Do not expose its private request-state file.

Each device reports the track it was built to follow as `trust_profile`
(`trusted`, `open`, `test`, or `unreported` from firmware that predates the
field) in `ota.py status`, the collector API and the app. The collector offers
a device only its own track's releases. Treat the report as a label for sorting
the fleet, not as proof: firmware on unlocked hardware can report anything, so
a device counts as trusted because it was commissioned and locked, never
because it says so.

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

Metrics include update checks, errors, rollbacks, release adoption, the
reported track (`navlistener_update_trust_profile_info`), security profile
drift, last report time, staging time and time waiting for safe reboot. Drift
is raised when security flags contradict the reported track: a trusted build
that is not fully locked, or an open or test build on a locked chip. An
unlocked open device is expected and does not raise it. Use report freshness
alongside staging/waiting timestamps when alerting.
