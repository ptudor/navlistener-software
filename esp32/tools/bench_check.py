#!/usr/bin/env python3
"""Bench-test one ESP32-S3 observer and write its bench report (docs/FLEET-REPORTS.md).

The tool resets the board through its USB port, reads the console for a window and
judges only what the firmware printed. With --flash it first writes the build's
images, the bootloader, partition table and application, without erasing NVS. Run it
in the ESP-IDF Python environment, which provides esptool and pyserial.

Before a board is provisioned its console prints the setup label, which holds the setup
network password. The capture therefore stays on the bench: it and the report are
written mode 0600 into a new private directory, and only the capture's SHA-256 is in the
report.
"""

from __future__ import annotations

import argparse
from dataclasses import dataclass, field
from datetime import datetime, timedelta, timezone
import hashlib
import json
import os
from pathlib import Path
import re
import subprocess
import sys
import time

FORMAT = "navlistener-bench-report-v1"
TOOL = "bench_check.py 1"
DEFAULT_SECONDS = 30.0

_ANSI = re.compile(r"\x1b\[[0-9;]*m")
_BOOT = re.compile(r"rst:0x[0-9a-f]+ \(")
_LOG = re.compile(r"[IWE] \(\d+\) ([A-Za-z0-9_]+): (.*)")
_PANICS = (
    "Guru Meditation Error",
    "abort() was called",
    "Brownout detector was triggered",
    "***ERROR*** A stack overflow",
    "Stack smashing protect failure",
    "assert failed:",
)
_PART = re.compile(r"([A-Za-z][A-Za-z0-9 _/-]{0,30}) at 0x([0-9a-f]{2}): (\S+)")
# manifest_board_name() values that name no supported board.
_NO_BOARD = {"no board", "an unsupported board or revision", "more than one board"}
_ATECC = re.compile(r"ATECC Info revision=([0-9a-f]{1,16}); (\S{1,32}) \(CRC verified\)")
_MAC = re.compile(r"MAC:\s+((?:[0-9a-f]{2}:){5}[0-9a-f]{2})")

OBSERVER_ID = re.compile(r"board-000[34]-([0-9a-f]{32})")
_STATION = re.compile(r"[A-Za-z0-9][A-Za-z0-9._-]{0,63}")
_CHECK_NAME = re.compile(r"[a-z][a-z0-9_.:@-]{0,63}")
_HEX64 = re.compile(r"[0-9a-f]{64}")
_REVISION = re.compile(r"[0-9a-f]{40}")
_TARGET = re.compile(r"[a-z0-9]{1,16}")
_MAC_HEX = re.compile(r"[0-9a-f]{12}")
_TIME = re.compile(r"\d{4}-\d{2}-\d{2}T\d{2}:\d{2}:\d{2}Z")
_PRINTABLE = re.compile(r"[\x20-\x7e]*")


@dataclass
class ConsoleFacts:
    """What the firmware printed, read from one capture."""

    boots: int = 0
    panic: str | None = None
    app_version: str | None = None
    elf_prefix: str | None = None
    observer_id: str | None = None
    manifest: tuple[str, str] | None = None  # (boot action, board)
    atecc: tuple[str, str] | None = None  # (revision, model)
    parts: dict[tuple[str, str], list[str]] = field(default_factory=dict)  # (name, address) → results
    receiver_baud: str | None = None
    receiver_silent: bool = False
    unprovisioned: bool = False
    setup_network: bool = False
    app_setup: bool = False


def parse_console(text: str) -> ConsoleFacts:
    facts = ConsoleFacts()
    for raw in text.splitlines():
        line = _ANSI.sub("", raw).strip()
        if _BOOT.match(line):
            facts.boots += 1
            continue
        if facts.panic is None and any(marker in line for marker in _PANICS):
            facts.panic = line
            continue
        log = _LOG.fullmatch(line)
        if not log:
            continue
        tag, message = log.groups()
        if tag == "app_init":
            if message.startswith("App version:") and facts.app_version is None:
                facts.app_version = message.partition(":")[2].strip() or None
            elif message.startswith("ELF file SHA256:") and facts.elf_prefix is None:
                prefix = re.match(r"[0-9a-f]+", message.partition(":")[2].strip())
                facts.elf_prefix = prefix.group(0) if prefix else None
        elif tag == "hw_manifest" and message.startswith("manifest EEPROM factory serial: "):
            candidate = message.rpartition(" ")[2]
            if valid_observer_id(candidate) and facts.observer_id is None:
                facts.observer_id = candidate
        elif tag == "hw_manifest" and message.startswith("manifest boot action: ") and facts.manifest is None:
            action, _, board = message.removeprefix("manifest boot action: ").partition("; board: ")
            facts.manifest = (action, board)
        elif tag == "observer_board":
            atecc = _ATECC.fullmatch(message)
            part = _PART.fullmatch(message)
            if atecc and facts.atecc is None:
                facts.atecc = (atecc.group(1), atecc.group(2))
            elif part:
                name = re.sub(r"[^a-z0-9]+", "_", part.group(1).lower())
                facts.parts.setdefault((name, part.group(2)), []).append(part.group(3))
        elif tag == "receiver":
            baud = re.match(r"valid receiver traffic at (\d+) baud", message)
            if baud:
                facts.receiver_baud = baud.group(1)
            elif message.startswith("no UART bytes observed"):
                facts.receiver_silent = True
        elif tag == "navfeeder" and message.startswith("unprovisioned ("):
            facts.unprovisioned = True
        elif tag == "netcfg" and message.startswith("browser provisioning ready:"):
            facts.setup_network = True
        elif (tag == "netcfg_prov" and message.startswith("app provisioning ready")) or (
            tag == "netcfg_ble" and message.startswith("BLE provisioning ready")
        ):
            facts.app_setup = True
    return facts


def valid_observer_id(value: object) -> bool:
    if not isinstance(value, str):
        return False
    match = OBSERVER_ID.fullmatch(value)
    return bool(match) and match.group(1) != "0" * 32


def _detail(text: str) -> str:
    """Printable ASCII, at most 200 characters: what a report may carry."""
    clean = "".join(ch if " " <= ch <= "~" else "?" for ch in text)
    return clean if len(clean) <= 200 else clean[:197] + "..."


def _check(name: str, result: str, detail: str) -> dict:
    return {"name": name, "result": result, "detail": _detail(detail)}


def evaluate(facts: ConsoleFacts, *, elf_sha256: str, mac: str | None, seconds: float) -> list[dict]:
    """Judge each check from what the firmware printed, the build's ELF digest and the
    MAC the ROM bootloader gave."""
    checks = []
    if facts.elf_prefix is None:
        checks.append(_check("firmware.image", "fail", "no ELF SHA-256 printed"))
    elif len(facts.elf_prefix) >= 8 and elf_sha256.startswith(facts.elf_prefix):
        checks.append(_check("firmware.image", "pass", f"ELF SHA-256 {facts.elf_prefix} matches the build"))
    else:
        build_prefix = elf_sha256[: len(facts.elf_prefix)]
        checks.append(_check("firmware.image", "fail", f"booted ELF {facts.elf_prefix}, build ELF {build_prefix}"))

    if facts.panic is not None:
        checks.append(_check("boot", "fail", facts.panic))
    elif facts.boots == 1:
        checks.append(_check("boot", "pass", "one boot, no panic"))
    elif facts.boots == 0:
        checks.append(_check("boot", "fail", "no boot seen in the window"))
    else:
        checks.append(_check("boot", "fail", f"{facts.boots} boots in the window"))

    if facts.observer_id is not None:
        checks.append(_check("board_uid", "pass", facts.observer_id))
    else:
        checks.append(_check("board_uid", "fail", "no board identity read"))

    if mac is not None:
        checks.append(_check("mcu_mac", "pass", ":".join(mac[i : i + 2] for i in range(0, 12, 2))))
    else:
        checks.append(_check("mcu_mac", "fail", "the ROM bootloader did not answer"))

    # The firmware runs board-specific code, the secure element and part probes among it,
    # only once the manifest names a supported board.
    named = facts.manifest is not None and facts.manifest[0] == "use manifest" and facts.manifest[1] not in _NO_BOARD
    if facts.manifest is None:
        checks.append(_check("manifest", "fail", "no manifest boot action printed"))
    elif named:
        checks.append(_check("manifest", "pass", facts.manifest[1]))
    else:
        checks.append(_check("manifest", "fail", f"{facts.manifest[0]}; board: {facts.manifest[1]}"))

    if facts.atecc is not None:
        revision, model = facts.atecc
        checks.append(_check("atecc", "pass", f"{model} revision {revision}, CRC verified"))
    elif not named:
        checks.append(_check("atecc", "skip", "the manifest names no board, so board checks did not run"))
    else:
        checks.append(_check("atecc", "fail", "no CRC-verified info read"))

    for (name, address), results in sorted(facts.parts.items()):
        result = "pass" if all(r == "ESP_OK" for r in results) else "fail"
        checks.append(_check(f"i2c:{name}@0x{address}", result, ", then ".join(results)))

    if facts.receiver_baud is not None:
        checks.append(_check("receiver", "pass", f"valid traffic at {facts.receiver_baud} baud"))
    elif facts.receiver_silent:
        checks.append(_check("receiver", "fail", "no UART bytes from the receiver"))
    else:
        checks.append(_check("receiver", "skip", f"no receiver verdict within {seconds:g} s"))

    if not facts.unprovisioned:
        checks.append(_check("setup", "skip", "the board did not report itself unprovisioned"))
    elif facts.setup_network and facts.app_setup:
        checks.append(_check("setup", "pass", "setup network and app provisioning ready"))
    else:
        missing = [what for what, ok in (("setup network", facts.setup_network), ("app provisioning", facts.app_setup)) if not ok]
        checks.append(_check("setup", "fail", " and ".join(missing) + " not ready"))
    return checks


def overall(checks: list[dict]) -> str:
    results = {c["result"] for c in checks}
    return "pass" if "fail" not in results and "pass" in results else "fail"


def _utc(moment: datetime) -> str:
    return moment.astimezone(timezone.utc).strftime("%Y-%m-%dT%H:%M:%SZ")


def build_report(
    *,
    station: str,
    started: datetime,
    finished: datetime,
    facts: ConsoleFacts,
    mac: str | None,
    build: "Build",
    checks: list[dict],
    console: bytes,
) -> dict:
    report = {
        "format": FORMAT,
        "tool": TOOL,
        "station": station,
        "started_at": _utc(started),
        "finished_at": _utc(finished),
        "board": {"observer_id": facts.observer_id, "mcu_mac": mac},
        "firmware": {
            "image_sha256": build.image_sha256,
            "app_version": facts.app_version if facts.app_version and len(facts.app_version) <= 32 else None,
            "project_revision": build.project_revision,
            "project_dirty": build.project_dirty,
            "idf_revision": build.idf_revision,
            "target": build.target,
        },
        "checks": checks,
        "result": overall(checks),
        "console_sha256": hashlib.sha256(console).hexdigest(),
    }
    validate_report(report)
    return report


def _members(value: object, names: set[str], where: str) -> dict:
    if not isinstance(value, dict):
        raise ValueError(f"{where} must be an object")
    if set(value) != names:
        extra, missing = sorted(set(value) - names), sorted(names - set(value))
        raise ValueError(f"{where}: unknown {extra} / missing {missing}" if extra else f"{where}: missing {missing}")
    return value


def _text(value: object, where: str, low: int, high: int, *, nullable: bool = False) -> None:
    if value is None and nullable:
        return
    if not isinstance(value, str) or not low <= len(value) <= high or not _PRINTABLE.fullmatch(value):
        raise ValueError(f"{where} must be {low}-{high} printable ASCII characters")


def _pattern(value: object, pattern: re.Pattern, where: str, *, nullable: bool = False) -> None:
    if value is None and nullable:
        return
    if not isinstance(value, str) or not pattern.fullmatch(value):
        raise ValueError(f"{where} is malformed")


def _time(value: object, where: str) -> datetime:
    _pattern(value, _TIME, where)
    try:
        return datetime.strptime(value, "%Y-%m-%dT%H:%M:%SZ").replace(tzinfo=timezone.utc)
    except ValueError as exc:
        raise ValueError(f"{where} is not a valid time") from exc


def validate_report(report: object) -> None:
    """Refuse anything docs/FLEET-REPORTS.md does not allow; raises ValueError."""
    doc = _members(
        report,
        {"format", "tool", "station", "started_at", "finished_at", "board", "firmware", "checks", "result", "console_sha256"},
        "report",
    )
    if doc["format"] != FORMAT:
        raise ValueError("unknown format")
    _text(doc["tool"], "tool", 1, 64)
    _pattern(doc["station"], _STATION, "station")
    started, finished = _time(doc["started_at"], "started_at"), _time(doc["finished_at"], "finished_at")
    if not started <= finished <= started + timedelta(hours=1):
        raise ValueError("finished_at must be within an hour after started_at")
    board = _members(doc["board"], {"observer_id", "mcu_mac"}, "board")
    if board["observer_id"] is not None and not valid_observer_id(board["observer_id"]):
        raise ValueError("board.observer_id is malformed")
    _pattern(board["mcu_mac"], _MAC_HEX, "board.mcu_mac", nullable=True)
    firmware = _members(
        doc["firmware"],
        {"image_sha256", "app_version", "project_revision", "project_dirty", "idf_revision", "target"},
        "firmware",
    )
    _pattern(firmware["image_sha256"], _HEX64, "firmware.image_sha256")
    _text(firmware["app_version"], "firmware.app_version", 1, 32, nullable=True)
    _pattern(firmware["project_revision"], _REVISION, "firmware.project_revision", nullable=True)
    if firmware["project_dirty"] is not None and not isinstance(firmware["project_dirty"], bool):
        raise ValueError("firmware.project_dirty must be true, false or null")
    _text(firmware["idf_revision"], "firmware.idf_revision", 1, 64, nullable=True)
    _pattern(firmware["target"], _TARGET, "firmware.target")
    checks = doc["checks"]
    if not isinstance(checks, list) or not 1 <= len(checks) <= 64:
        raise ValueError("checks must list 1-64 checks")
    names = set()
    for index, check in enumerate(checks):
        check = _members(check, {"name", "result", "detail"}, f"checks[{index}]")
        _pattern(check["name"], _CHECK_NAME, f"checks[{index}].name")
        if check["name"] in names:
            raise ValueError(f"check {check['name']} is listed twice")
        names.add(check["name"])
        if check["result"] not in ("pass", "fail", "skip"):
            raise ValueError(f"checks[{index}].result must be pass, fail or skip")
        _text(check["detail"], f"checks[{index}].detail", 0, 200)
    if doc["result"] not in ("pass", "fail") or doc["result"] != overall(checks):
        raise ValueError("result does not follow from the checks")
    _pattern(doc["console_sha256"], _HEX64, "console_sha256")


@dataclass(frozen=True)
class Build:
    path: Path
    image_sha256: str
    elf_sha256: str
    project_revision: str | None
    project_dirty: bool | None
    idf_revision: str | None
    target: str


def load_build(path: Path) -> Build:
    """The build the board should be running, from its recorded provenance. Refuses a
    provenance record that does not describe the image now in the directory."""
    try:
        project = json.loads((path / "project_description.json").read_text())
        provenance = json.loads((path / "firmware-provenance.json").read_text())
        image = (path / project["app_bin"]).read_bytes()
        elf = (path / project["app_elf"]).read_bytes()
    except (OSError, ValueError, KeyError) as exc:
        raise ValueError(f"{path} is not a recorded build: {exc}") from exc
    image_sha256 = hashlib.sha256(image).hexdigest()
    if provenance.get("firmware_sha256") != image_sha256:
        raise ValueError(f"{path}: firmware-provenance.json describes another image; rebuild to record this one")
    revision = provenance.get("project_revision")
    dirty = provenance.get("project_dirty")
    idf = provenance.get("idf_revision")
    return Build(
        path=path,
        image_sha256=image_sha256,
        elf_sha256=hashlib.sha256(elf).hexdigest(),
        project_revision=revision if isinstance(revision, str) and _REVISION.fullmatch(revision) else None,
        project_dirty=dirty if isinstance(dirty, bool) else None,
        idf_revision=idf if isinstance(idf, str) and 1 <= len(idf) <= 64 and _PRINTABLE.fullmatch(idf) else None,
        target=provenance.get("target") or project["target"],
    )


def _esptool(args: list[str], *, cwd: Path | None = None, timeout: float) -> subprocess.CompletedProcess:
    return subprocess.run(
        [sys.executable, "-m", "esptool", *args], cwd=cwd, capture_output=True, text=True, timeout=timeout
    )


def flash(port: str, build: Build) -> None:
    """Write the build's flash_args regions (never an erase). Raises RuntimeError."""
    result = _esptool(
        ["--chip", build.target, "--port", port, "--baud", "460800", "--before", "default_reset",
         "--after", "hard_reset", "write_flash", "@flash_args"],
        cwd=build.path,
        timeout=300,
    )  # fmt: skip
    if result.returncode != 0:
        raise RuntimeError(f"flashing failed:\n{(result.stdout + result.stderr)[-2000:]}")


def read_mac(port: str, target: str) -> str | None:
    """The MAC from the ROM bootloader, as 12 hex digits; a freshly plugged board may
    still be enumerating, so try three times."""
    for attempt in range(3):
        try:
            result = _esptool(["--chip", target, "--port", port, "read_mac"], timeout=60)
        except subprocess.TimeoutExpired:
            result = None
        match = _MAC.search(result.stdout) if result is not None else None
        if match:
            return match.group(1).replace(":", "")
        time.sleep(2)
    return None


def capture(port: str, seconds: float) -> bytes:
    """Reset the board into its application and read its console for `seconds`."""
    import serial  # the ESP-IDF Python environment provides pyserial

    data = bytearray()
    with serial.Serial() as ser:
        ser.port, ser.baudrate, ser.timeout = port, 115200, 0.2
        ser.dtr = ser.rts = False
        # The USB-Serial/JTAG port re-enumerates after esptool's reset; wait for it.
        for _ in range(50):
            try:
                ser.open()
                break
            except serial.SerialException:
                time.sleep(0.2)
        else:
            raise RuntimeError(f"{port} did not reappear")
        ser.reset_input_buffer()
        ser.rts = True  # EN low with DTR released: a chip reset into the application
        time.sleep(0.2)
        ser.rts = False
        deadline = time.monotonic() + seconds
        while time.monotonic() < deadline:
            try:
                data += ser.read(4096)
            except serial.SerialException:
                time.sleep(0.3)  # the port drops briefly while the USB device re-enumerates
                try:
                    ser.close()
                    ser.open()
                except serial.SerialException:
                    pass
    return bytes(data)


def write_private(directory: Path, report: dict, console: bytes) -> tuple[Path, Path]:
    """Create `directory` (it must be new) and write both files mode 0600."""
    directory.parent.mkdir(parents=True, exist_ok=True)
    directory.mkdir(mode=0o700)
    paths = (directory / "report.json", directory / "console.log")
    for path, data in zip(paths, (json.dumps(report, indent=2).encode() + b"\n", console)):
        fd = os.open(path, os.O_WRONLY | os.O_CREAT | os.O_EXCL, 0o600)
        with os.fdopen(fd, "wb") as handle:
            handle.write(data)
    return paths


def main(argv: list[str] | None = None) -> int:
    parser = argparse.ArgumentParser(description=__doc__.split("\n\n")[0])
    parser.add_argument("--port", required=True, help="the board's USB serial device")
    parser.add_argument("--build-dir", required=True, type=Path, help="the build the board runs, or will after --flash")
    parser.add_argument("--station", required=True, help="this bench's name, recorded in the report")
    parser.add_argument("--flash", action="store_true", help="write the build's images first (never erases NVS)")
    parser.add_argument("--seconds", type=float, default=DEFAULT_SECONDS, help="console window (default 30)")
    parser.add_argument("--out", type=Path, help="new directory for the report and capture")
    args = parser.parse_args(argv)
    if not _STATION.fullmatch(args.station):
        parser.error("--station must be 1-64 of A-Z a-z 0-9 . _ - starting with a letter or digit")
    if not 5 <= args.seconds <= 600:
        parser.error("--seconds must be between 5 and 600")
    try:
        build = load_build(args.build_dir)
    except ValueError as exc:
        print(f"bench_check: {exc}", file=sys.stderr)
        return 2
    started = datetime.now(timezone.utc)
    out = args.out or build.path / "bench" / started.strftime("%Y%m%dT%H%M%SZ")
    if out.exists():
        print(f"bench_check: {out} already exists", file=sys.stderr)
        return 2
    try:
        if args.flash:
            flash(args.port, build)
        mac = read_mac(args.port, build.target)
        console = capture(args.port, args.seconds)
    except (RuntimeError, subprocess.TimeoutExpired) as exc:
        print(f"bench_check: {exc}", file=sys.stderr)
        return 2
    finished = datetime.now(timezone.utc)
    facts = parse_console(console.decode("utf-8", "replace"))
    checks = evaluate(facts, elf_sha256=build.elf_sha256, mac=mac, seconds=args.seconds)
    report = build_report(
        station=args.station, started=started, finished=finished, facts=facts, mac=mac, build=build,
        checks=checks, console=console,
    )  # fmt: skip
    report_path, console_path = write_private(out, report, console)
    width = max(len(c["name"]) for c in checks)
    for check in checks:
        print(f"{check['result']:4}  {check['name']:{width}}  {check['detail']}")
    print(f"\n{report['result'].upper()}  {facts.observer_id or 'board identity not read'}")
    print(f"report:  {report_path}\nconsole: {console_path} (private: it can hold the setup password)")
    return 0 if report["result"] == "pass" else 1


if __name__ == "__main__":
    sys.exit(main())
