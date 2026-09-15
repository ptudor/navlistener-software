#!/usr/bin/env python3
"""Export PPS/RTC samples from an ESP serial log to CSV and an optional chart."""
import argparse
import csv
from pathlib import Path
import re
import struct


def decode(body):
    if len(body) != 223 or body[:2] != b"\x01\x04" or body[24:27] != b"\x08\x00\xc4":
        raise ValueError("expected ObserverDetails v1 timing snapshot")
    b = body[27:]
    if b[0] != 1 or b[1] != 1:
        raise ValueError("unsupported timing version/clock")
    u32 = lambda p: struct.unpack_from(">I", b, p)[0]
    u64 = lambda p: struct.unpack_from(">Q", b, p)[0]
    uptime = struct.unpack_from(">Q", body, 2)[0]
    hz, started = u32(8), u64(16)
    if started > uptime or hz > 200000000:
        raise ValueError("invalid timing clock/start")
    row = {"uptime_ms": uptime, "elapsed_s": (uptime-started)/1000,
           "resolution_hz": hz, "capture_queue_dropped": u32(12),
           "rtc_state": b[2], "rtc_trim_raw": b[4],
           "next_timepulse_flags": b[6] if b[5] & 2 else None,
           "rtc_minus_gnss_phase_ns": struct.unpack_from(">i", b, 24)[0]*1e9/hz if b[5] & 1 and hz else None}
    for i, name in enumerate(("gnss", "rtc")):
        p = 36+80*i
        flags, span, intervals = u32(p), u64(p+48), u64(p+56)
        row.update({f"{name}_flags": flags, f"{name}_hardware_pulses": u64(p+40),
                    f"{name}_captured_rising_edges": u64(p+32),
                    f"{name}_missing_estimate": u64(p+24), f"{name}_discontinuities": u32(p+20),
                    f"{name}_counter_discontinuities": u32(p+72),
                    f"{name}_period_ns": u32(p+4)*1e9/hz if flags & 8 and hz else None,
                    f"{name}_width_ns": u32(p+8)*1e9/hz if flags & 16 and hz else None,
                    f"{name}_span_intervals": intervals,
                    f"{name}_period_error_ppm": (span/intervals/hz-1)*1e6 if flags & 32 and hz and intervals else None})
    return row


def read_log(path):
    rows, boot, previous = [], 0, None
    for line in Path(path).read_text(errors="replace").splitlines():
        match = re.search(r"pulse_timing: sample=([0-9a-f]+)", line)
        if not match:
            continue
        row = decode(bytes.fromhex(match[1]))
        if previous is not None and row["uptime_ms"] < previous:
            boot += 1
        previous = row["uptime_ms"]
        row = {"capture_boot": boot, **row}
        rows.append(row)
    return rows


def plot(rows, path):
    # Optional dependency; CSV export uses only the standard library.
    import matplotlib
    matplotlib.use("Agg")
    import matplotlib.pyplot as plt
    fig, axes = plt.subplots(3, 1, figsize=(11, 9), sharex=True, constrained_layout=True)
    colors = {"gnss": "#146e96", "rtc": "#d66c20"}
    for boot in sorted({r["capture_boot"] for r in rows}):
        run = [r for r in rows if r["capture_boot"] == boot]
        x = [r["elapsed_s"] for r in run]
        for name, color in colors.items():
            label = f"{name.upper()} (boot {boot})"
            # Plot interval error per pulse. Span-average ppm is retained in CSV.
            y = [(r[f"{name}_period_ns"]-1e9)/1000 if r[f"{name}_period_ns"] is not None else float("nan") for r in run]
            axes[0].plot(x, y, color=color, linewidth=1, label=label)
            y = [r[f"{name}_hardware_pulses"]-r["elapsed_s"] if r[f"{name}_flags"] & 2 else float("nan") for r in run]
            axes[2].plot(x, y, color=color, linewidth=1, label=label)
        y = [r["rtc_minus_gnss_phase_ns"]/1e6 if r["rtc_minus_gnss_phase_ns"] is not None else float("nan") for r in run]
        # A modulo-one-second phase wraps naturally; break the plotted line.
        for i in range(len(y)-1, 0, -1):
            if abs(y[i]-y[i-1]) > 500:
                y[i] = float("nan")
        axes[1].plot(x, y, color="#6651a7", linewidth=1, label=f"boot {boot}")
    axes[0].set_ylabel("Period − 1 ESP second (µs)")
    axes[1].set_ylabel("RTC − GNSS phase (ms)\nmodulo one second")
    axes[2].set_ylabel("Hardware pulses − elapsed seconds")
    axes[2].set_xlabel("Elapsed ESP time since capture start (s)")
    for ax in axes:
        ax.grid(alpha=.25)
        ax.legend(loc="best", fontsize="small")
    fig.suptitle("GNSS PPS and RTC 1 Hz — relative to ESP APB\nPulse phase/count diagnostics; not absolute UTC accuracy", fontsize=12)
    fig.savefig(path)
    plt.close(fig)


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("log", help="serial log containing pulse_timing sample lines")
    parser.add_argument("--csv", required=True)
    parser.add_argument("--plot", help="optional SVG, PNG or PDF; requires matplotlib")
    args = parser.parse_args()
    rows = read_log(args.log)
    if not rows:
        raise SystemExit("No timing samples found")
    with Path(args.csv).open("w", newline="") as out:
        writer = csv.DictWriter(out, fieldnames=list(rows[0]))
        writer.writeheader()
        writer.writerows(rows)
    if args.plot:
        plot(rows, args.plot)
    print(f"Exported {len(rows)} samples; latest: elapsed={rows[-1]['elapsed_s']:.3f}s, "
          f"GNSS={rows[-1]['gnss_hardware_pulses']}, RTC={rows[-1]['rtc_hardware_pulses']}, "
          f"capture queue losses={rows[-1]['capture_queue_dropped']}")


if __name__ == "__main__":
    main()
