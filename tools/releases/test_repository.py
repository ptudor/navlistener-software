import copy
from datetime import datetime, timezone, timedelta
import json
from pathlib import Path
import shutil
import subprocess
import tempfile
import unittest

from repository import Repository, Signers, init_test_keys, encoded
from tuf.api.metadata import Metadata
from tuf.ngclient import Updater
from tuf.ngclient.fetcher import FetcherInterface
from tuf.api.exceptions import DownloadHTTPError

ROOT = Path(__file__).resolve().parents[2]
CLIENT = ROOT / "esp32/components/ota/test/build/update_tuf_test"
NOW = datetime.fromtimestamp(1800000000, timezone.utc)


class LocalFetcher(FetcherInterface):
    def __init__(self, directory):
        self.directory = directory

    def _fetch(self, url):
        from urllib.parse import urlparse
        path = self.directory / urlparse(url).path.lstrip("/")
        if not path.is_file():
            raise DownloadHTTPError("absent fixture", 404)
        yield path.read_bytes()


class RepositoryTests(unittest.TestCase):
    @classmethod
    def setUpClass(cls):
        cls.workspace = tempfile.TemporaryDirectory(prefix="navlisten-tuf-")
        cls.base = Path(cls.workspace.name)
        cls.keys = init_test_keys(cls.base / "TEST-ONLY-keys")
        cls.signers = Signers(cls.keys, production=False)

    @classmethod
    def tearDownClass(cls):
        cls.workspace.cleanup()

    def setUp(self):
        self.directory = Path(tempfile.mkdtemp(dir=self.base)) / "repo"
        self.repo = Repository(self.directory, self.signers, now=NOW)
        self.repo.bootstrap()
        self.repo.add_release(sequence=31, version="0.1.0", revision="a" * 40,
            image=b"firmware fixture" * 50, boot_key_id="ab" * 32, provenance=b"{}", licenses=b"[]", notes=b"Test release\n")

    def client(self, expected=0, second=None):
        self.repo.publish_local()
        command = [str(CLIENT), str(self.directory), str(expected)]
        if second:
            command += [str(second[0]), str(second[1])]
        result = subprocess.run(command, capture_output=True, text=True)
        self.assertEqual(result.returncode, 0, result.stdout + result.stderr)
        return result.stdout

    def test_reference_and_device_accept_same_repository(self):
        self.assertIn("sequence=31", self.client())
        client = self.directory.parent / "reference-client"
        client.mkdir()
        shutil.copy(self.directory / "metadata/1.root.json", client / "root.json")
        updater = Updater(str(client), "https://fixture.invalid/metadata/", target_base_url="https://fixture.invalid/targets/",
                          fetcher=LocalFetcher(self.directory), bootstrap=(client / "root.json").read_bytes())
        updater.refresh()
        target = updater.get_targetinfo("releases/31.json")
        self.assertIsNotNone(target)
        updater.download_target(target, str(client / "manifest.json"))
        self.assertEqual(json.loads((client / "manifest.json").read_bytes())["release_sequence"], 31)

    def test_production_refuses_test_signers(self):
        with self.assertRaisesRegex(ValueError, "test-only"):
            Signers(self.keys, production=True)

    def test_json_exact_integers_and_unicode(self):
        for value in ["18446744073709551615", '{"z":9007199254740993,"a":"café\\nPEM"}', '{"a":"\\uD83D\\uDE80"}']:
            subprocess.run([str(CLIENT), "--json", value, "1"], check=True, capture_output=True)
        for value in ['{"a":1,"a":2}', '{"a":1,"\\u0061":2}', '18446744073709551616', '1.5', '1e3',
                      '{"a":"\\u0000"}', '{"a":"\\uD800"}', '01', '{"a":1,}', '[' * 17 + '0' + ']' * 17]:
            subprocess.run([str(CLIENT), "--json", value, "0"], check=True, capture_output=True)

    def test_bad_timestamp_signature(self):
        path = "metadata/timestamp.json"
        value = json.loads(self.repo.files[path])
        value["signatures"][0]["sig"] = "00" * 70
        self.repo.files[path] = encoded(value)
        self.client(2002)

    def test_duplicate_root_signatures_cannot_meet_threshold(self):
        path = "metadata/1.root.json"
        value = json.loads(self.repo.files[path])
        value["signatures"] = [value["signatures"][0]] * 3
        self.repo.files[path] = encoded(value)
        self.client(2002)

    def test_signed_expired_timestamp(self):
        md = self.repo.metadata["timestamp"]
        md.signed.expires = NOW - timedelta(seconds=1)
        self.repo.files["metadata/timestamp.json"] = self.signers.sign("timestamp", md)
        self.client(2003)

    def test_snapshot_hash_mismatch(self):
        path = f"metadata/{self.repo.versions['snapshot']}.snapshot.json"
        self.repo.files[path] += b" "
        self.client(4001)

    def test_wrong_board_layout(self):
        self.repo.add_release(sequence=32, version="0.1.1", revision="b" * 40, image=b"image", boot_key_id="ab" * 32,
            provenance=b"{}", licenses=b"[]", notes=b"notes", layout=99)
        self.client(3002)

    def test_rollout_zero_and_withdrawal(self):
        self.repo.set_channel("lab", 31, "releases/31.json", percentage=0)
        self.repo.online()
        self.client(3003)

    def test_withdrawal(self):
        self.repo.set_channel("lab", 31, "releases/31.json", withdrawn=[31])
        self.repo.online()
        self.client(3002)

    def test_downgrade_metadata_rejected(self):
        self.repo.publish_local()
        old = self.directory.parent / "old"
        shutil.copytree(self.directory, old)
        self.repo.set_channel("lab", 31, "releases/31.json", percentage=25)
        self.repo.online()
        # Cohort eligibility is independent of rollback protection; use 100%.
        self.repo.set_channel("lab", 31, "releases/31.json", percentage=100)
        self.repo.online()
        self.client(0, second=(old, 2004))

    def test_immutable_content_cannot_be_replaced(self):
        self.repo.publish_local()
        self.repo.files["metadata/1.root.json"] += b" "
        with self.assertRaisesRegex(ValueError, "immutable"):
            self.repo.publish_local()


if __name__ == "__main__":
    unittest.main()
