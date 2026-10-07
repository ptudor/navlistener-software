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

    def ota_build_body(self):
        # What timing_poll logs on a CONFIG_NVF_OTA build: the timing body with the tag-9
        # update status (3-byte header, 140-byte value) appended by append_update.
        return self.golden() + bytes([9, 0, 140]) + bytes(140)

    def test_update_status_element_is_ignored(self):
        self.assertEqual(timing_plot.decode(self.ota_build_body()), timing_plot.decode(self.golden()))
        # An unknown bounded tag in front of the timing element is skipped the same way.
        reordered = self.golden()[:24] + bytes([9, 0, 140]) + bytes(140) + self.golden()[24:]
        self.assertEqual(timing_plot.decode(reordered), timing_plot.decode(self.golden()))

    def test_truncated_or_missing_elements_are_rejected(self):
        b = self.ota_build_body()
        for n in range(len(self.golden()) + 1, len(b)):  # a partial tag-9 header or value
            with self.assertRaises(ValueError):
                timing_plot.decode(b[:n])
        with self.assertRaises(ValueError):
            timing_plot.decode(self.golden()[:24] + bytes([9, 0, 140]) + bytes(140))  # no timing element
        with self.assertRaises(ValueError):
            timing_plot.decode(self.golden() + self.golden()[24:])  # two timing elements

    def test_ota_build_log_exports(self):
        with tempfile.TemporaryDirectory() as tmp:
            p = Path(tmp)/"timing.log"
            p.write_text("I (10) pulse_timing: sample=" + self.ota_build_body().hex() + "\n")
            rows = timing_plot.read_log(p)
        self.assertEqual(len(rows), 1)
        self.assertEqual(rows[0]["elapsed_s"], 999)


if __name__ == "__main__":
    unittest.main()
