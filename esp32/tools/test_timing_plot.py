from pathlib import Path
import struct
import tempfile
import unittest
import timing_plot


class TimingPlotTests(unittest.TestCase):
    def golden(self):
        return bytes.fromhex((Path(__file__).resolve().parents[2] / "testdata/observer_timing_v1.hex").read_text())

    def test_shared_fixture_units_and_counts(self):
        row = timing_plot.decode(self.golden())
        self.assertEqual(row["elapsed_s"], 999)
        self.assertEqual(row["gnss_hardware_pulses"], 999)
        self.assertEqual(row["gnss_period_ns"], 1000001000)
        self.assertEqual(row["rtc_minus_gnss_phase_ns"], -1000)
        self.assertAlmostEqual(row["rtc_period_error_ppm"], 2)

    def test_stale_and_unavailable_are_not_zero_error(self):
        b = bytearray(self.golden())
        b[32] = 0
        for p in (27+36, 27+116):
            struct.pack_into(">I", b, p, 3)
            b[p+4:p+12] = bytes(8)
        row = timing_plot.decode(b)
        self.assertIsNone(row["gnss_period_ns"])
        self.assertIsNone(row["rtc_period_error_ppm"])
        self.assertIsNone(row["rtc_minus_gnss_phase_ns"])
        self.assertEqual(row["gnss_hardware_pulses"], 999)

    def test_log_filter_and_boot_boundaries(self):
        b = bytearray(self.golden())
        struct.pack_into(">Q", b, 2, 2000000)
        with tempfile.TemporaryDirectory() as tmp:
            p = Path(tmp)/"timing.log"
            p.write_text("unrelated serial message\nI (10) pulse_timing: sample=" + b.hex() +
                         "\nI (20) pulse_timing: sample=" + self.golden().hex() + "\n")
            rows = timing_plot.read_log(p)
        self.assertEqual([r["capture_boot"] for r in rows], [0, 1])

    def test_truncation(self):
        b = self.golden()
        for n in range(len(b)):
            with self.assertRaises(ValueError):
                timing_plot.decode(b[:n])


if __name__ == "__main__":
    unittest.main()
