"""The label tool against a fake qrencode: the setup password never reaches argv and the
SVG is private from the moment it exists."""
import os
from pathlib import Path
import stat
import subprocess
import tempfile
import unittest
from unittest.mock import patch

import provisioning_label

PAYLOAD = '{"ver":"v1","name":"navfeeder-0A1B2C","username":"navfeeder-0A1B2C","pop":"abcdefghjkmn","transport":"ble"}'

# Stands in for qrencode: records its argument vector, reads the payload from stdin and
# writes an SVG that embeds it, so the test can see what reached the process and how. Shell
# builtins only: PATH holds nothing but this script while the test runs.
FAKE_QRENCODE = """#!/bin/sh
printf '%s\\n' "$@" > "$FAKE_QRENCODE_ARGS"
out=
while [ $# -gt 0 ]; do
    if [ "$1" = -o ]; then out=$2; fi
    shift
done
IFS= read -r payload || true
printf '<svg><!-- %s --></svg>\\n' "$payload" > "$out"
"""


class LabelTests(unittest.TestCase):
    def setUp(self):
        self.temporary = tempfile.TemporaryDirectory()
        self.directory = Path(self.temporary.name)
        self.addCleanup(self.temporary.cleanup)
        tools = self.directory / "bin"
        tools.mkdir()
        fake = tools / "qrencode"
        fake.write_text(FAKE_QRENCODE)
        fake.chmod(0o700)
        self.args = self.directory / "argv"
        self.environment = patch.dict(os.environ, {"PATH": str(tools), "FAKE_QRENCODE_ARGS": str(self.args)})
        self.environment.start()
        self.addCleanup(self.environment.stop)

    def test_payload_goes_on_stdin_and_the_svg_is_private_at_creation(self):
        output = self.directory / "label" / "setup.svg"
        old_umask = os.umask(0o022)  # a permissive umask must not widen the file
        try:
            provisioning_label.render_qr(PAYLOAD, output)
        finally:
            os.umask(old_umask)
        argv = self.args.read_text().splitlines()
        self.assertEqual(argv, ["-t", "SVG", "-m", "4", "-o", str(output)])
        self.assertNotIn("abcdefghjkmn", " ".join(argv))
        self.assertIn(PAYLOAD, output.read_text())
        self.assertEqual(stat.S_IMODE(output.stat().st_mode), 0o600)

    def test_an_existing_label_file_is_not_overwritten(self):
        output = self.directory / "setup.svg"
        output.write_text("earlier label")
        with self.assertRaisesRegex(RuntimeError, "refusing to overwrite"):
            provisioning_label.render_qr(PAYLOAD, output)
        self.assertEqual(output.read_text(), "earlier label")

    def test_a_failed_render_leaves_no_empty_private_file(self):
        output = self.directory / "setup.svg"
        failure = subprocess.CalledProcessError(1, "qrencode")
        with patch.object(provisioning_label.subprocess, "run", side_effect=failure):
            with self.assertRaises(subprocess.CalledProcessError):
                provisioning_label.render_qr(PAYLOAD, output)
        self.assertFalse(output.exists())

    def test_payload_validation(self):
        value = provisioning_label.extract_payload("NEW SETUP LABEL: " + PAYLOAD + "\n")
        self.assertEqual(provisioning_label.canonical(value), PAYLOAD)
        for broken in (PAYLOAD.replace("abcdefghjkmn", "abcdefghjkm"), PAYLOAD.replace('"ble"', '"softap"'),
                       PAYLOAD.replace("navfeeder-0A1B2C", "navfeeder-0a1b2c", 1), "no payload here"):
            with self.assertRaises(ValueError):
                provisioning_label.extract_payload(broken)


if __name__ == "__main__":
    unittest.main()
