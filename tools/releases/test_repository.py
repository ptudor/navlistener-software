import copy
import hashlib
from datetime import datetime, timezone, timedelta
import json
import os
from pathlib import Path
import shutil
import subprocess
import tempfile
import unittest
from unittest.mock import patch

from fixtures import PASSPHRASE_ENV, init_release_keys
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
        cls.signers = Signers(cls.keys, profile="test")

    @classmethod
    def tearDownClass(cls):
        cls.workspace.cleanup()

    def setUp(self):
        self.directory = Path(tempfile.mkdtemp(dir=self.base)) / "repo"
        self.repo = Repository(self.directory, self.signers, now=NOW)
        self.repo.bootstrap()
        self.repo.add_release(board_family="gnss-color-neo", sequence=31, version="0.1.0", revision="a" * 40,
            image=b"firmware fixture" * 50, boot_key_id="ab" * 32, provenance=b"{}", licenses=b"[]", notes=b"Test release\n")

    def client(self, expected=0, second=None, profile=None):
        self.repo.publish_local()
        command = [str(CLIENT), str(self.directory), str(expected)]
        if second:
            command += [str(second[0]), str(second[1])]
        if profile:
            command.append(f"--profile={profile}")
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

    def test_a_release_names_a_known_board_family(self):
        for family in ("", "gnss-color", "gnss-color-neo2"):
            with self.assertRaises(ValueError):
                self.repo.add_release(board_family=family, sequence=40, version="0.1.0", revision="a" * 40,
                    image=b"fixture", boot_key_id="ab" * 32, provenance=b"{}", licenses=b"[]", notes=b"Notes")

    def test_release_tracks_refuse_test_signers(self):
        for track in ("trusted", "open"):
            with self.assertRaisesRegex(ValueError, "separate signer configuration"):
                Signers(self.keys, profile=track)
        with self.assertRaisesRegex(ValueError, "unknown trust profile"):
            Signers(self.keys, profile="production")

    def test_device_accepts_only_its_own_profile(self):
        # The fixture root and manifest are marked test; a trusted or open
        # build must reject the root itself, before any network metadata.
        self.assertIn("sequence=31", self.client(profile="test"))
        for profile in ("trusted", "open"):
            self.assertIn("initialize=2001", self.client(2001, profile=profile))

    def test_manifest_profile_must_match_even_under_a_valid_root(self):
        path = "releases/31.json"
        manifest = json.loads(self.repo.target_bytes("releases", path))
        self.assertEqual(manifest["profile"], "test")
        manifest["profile"] = "open"
        targets = self.repo.metadata["releases"].signed
        self.repo.add_target(targets, path, encoded(manifest))
        self.repo.metadata_for("releases", targets)
        self.repo.online()
        self.client(2001)

    def test_unmarked_root_belongs_to_no_profile(self):
        value = json.loads(self.repo.files["metadata/1.root.json"])
        del value["signed"]["x_navlisten_profile"]
        self.repo.files["metadata/1.root.json"] = self.signers.sign("root", Metadata.from_dict(value))
        for profile in ("trusted", "open", "test"):
            self.assertIn("initialize=2001", self.client(2001, profile=profile))

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

    def test_key_id_must_bind_public_material(self):
        value = json.loads(self.repo.files["metadata/1.root.json"])
        ids = value["signed"]["roles"]
        old = ids["timestamp"]["keyids"][0]
        value["signed"]["keys"][old] = copy.deepcopy(value["signed"]["keys"][ids["snapshot"]["keyids"][0]])
        self.repo.files["metadata/1.root.json"] = self.signers.sign("root", Metadata.from_dict(value))
        self.client(2001)

    def test_roles_cannot_share_authority(self):
        value = json.loads(self.repo.files["metadata/1.root.json"])
        value["signed"]["roles"]["timestamp"]["keyids"] = value["signed"]["roles"]["snapshot"]["keyids"]
        self.repo.files["metadata/1.root.json"] = self.signers.sign("root", Metadata.from_dict(value))
        self.client(2001)

    def test_renewing_root_does_not_reset_rollback_floors(self):
        self.repo.publish_local()
        old = self.directory.parent / "old"
        shutil.copytree(self.directory, old)
        self.repo.online()
        self.repo.publish_local()
        root = Metadata.from_bytes(self.repo.files["metadata/1.root.json"])
        root.signed.version = 2
        (old / "metadata/2.root.json").write_bytes(self.signers.sign("root", root))
        self.client(0, second=(old, 2004))

    def test_repository_paths_cannot_escape_before_writing(self):
        self.repo.files["targets/../../escape.bin"] = b"escaped"
        with self.assertRaisesRegex(ValueError, "path"):
            self.repo.publish_local()
        self.assertFalse((self.directory.parent / "escape.bin").exists())

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
        self.repo.add_release(board_family="gnss-color-neo", sequence=32, version="0.1.1", revision="b" * 40, image=b"image", boot_key_id="ab" * 32,
            provenance=b"{}", licenses=b"[]", notes=b"notes", layout=99)
        self.client(3002)

    def test_rollout_zero_and_withdrawal(self):
        self.repo.set_channel("lab", 31, "releases/31.json", percentage=0)
        self.repo.online()
        self.client(3003)

    def test_rollout_uses_complete_typed_128_bit_identity(self):
        salt = bytes(range(32))
        uid = bytes.fromhex("000310000102030405060708090a0b0c0d0e0f") + bytes(16)
        digest = hashlib.sha256(salt + uid + (31).to_bytes(8, "big")).digest()
        bucket = int.from_bytes(digest[:2], "big") % 100
        self.repo.set_channel("lab", 31, "releases/31.json", percentage=bucket, salt=salt.hex())
        self.repo.online()
        self.client(3003)
        self.repo.set_channel("lab", 31, "releases/31.json", percentage=bucket + 1, salt=salt.hex())
        self.repo.online()
        self.client()

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

    def test_failed_refresh_retains_timestamp_after_restart(self):
        self.repo.publish_local()
        old = self.directory.parent / "old"
        shutil.copytree(self.directory, old)
        self.repo.online()
        path = f"metadata/{self.repo.versions['snapshot']}.snapshot.json"
        self.repo.files.pop(path)
        self.client(1002, second=(old, 2004))

    def test_failed_refresh_retains_snapshot_after_restart(self):
        self.repo.publish_local()
        old = self.directory.parent / "old"
        shutil.copytree(self.directory, old)
        self.repo.set_channel("lab", 31, "releases/31.json", percentage=100)
        self.repo.online()
        # Let an authorized newer timestamp point at the old snapshot. Its
        # signature is valid; the persisted snapshot version must reject it.
        timestamp = Metadata.from_bytes((old / "metadata/timestamp.json").read_bytes())
        timestamp.signed.version = self.repo.versions['timestamp'] + 1
        (old / "metadata/timestamp.json").write_bytes(self.signers.sign("timestamp", timestamp))
        path = f"metadata/{self.repo.versions['lab']}.lab.json"
        self.repo.files.pop(path)
        self.client(1002, second=(old, 2004))

    def test_withdrawal_does_not_depend_on_manifest_download(self):
        self.repo.set_channel("lab", 31, "releases/31.json", withdrawn=[31])
        self.repo.online()
        for path in list(self.repo.files):
            if path.startswith("targets/releases/"):
                self.repo.files.pop(path)
        self.client(3002)

    def test_immutable_content_cannot_be_replaced(self):
        self.repo.publish_local()
        self.repo.files["metadata/1.root.json"] += b" "
        with self.assertRaisesRegex(ValueError, "immutable"):
            self.repo.publish_local()

    def test_release_numbers_are_immutable_and_index_preserves_selected_channels(self):
        self.repo.set_channel("stable",31,"releases/31.json")
        for sequence in range(32,42):
            self.repo.add_release(board_family="gnss-color-neo", sequence=sequence,version="0.1.0",revision="a"*40,image=b"fixture",boot_key_id="ab"*32,provenance=b"{}",licenses=b"[]",notes=b"Notes")
        targets=self.repo.metadata["releases"].signed.targets
        self.assertLessEqual(len(targets),32)
        self.assertIn("releases/31.json",targets)
        self.assertIn("releases/41.json",targets)
        with self.assertRaisesRegex(ValueError,"already exists"):
            self.repo.add_release(board_family="gnss-color-neo", sequence=41,version="0.1.0",revision="a"*40,image=b"other",boot_key_id="ab"*32,provenance=b"{}",licenses=b"[]",notes=b"Notes")


class OpenTrackTests(unittest.TestCase):
    """The open track signs with real encrypted keys through the file adapter."""

    @classmethod
    def setUpClass(cls):
        cls.workspace = tempfile.TemporaryDirectory(prefix="navlisten-open-")
        cls.base = Path(cls.workspace.name)
        cls.keys, passphrase = init_release_keys(cls.base / "open-keys", "open")
        cls.environment = patch.dict(os.environ, {PASSPHRASE_ENV: passphrase})
        cls.environment.start()
        cls.signers = Signers(cls.keys, profile="open")
        cls.directory = cls.base / "repo"
        cls.repo = Repository(cls.directory, cls.signers, now=NOW)
        cls.repo.bootstrap()
        cls.repo.add_release(board_family="gnss-color-neo", sequence=31, version="0.1.0", revision="a" * 40,
            image=b"firmware fixture" * 50, boot_key_id="ab" * 32, provenance=b"{}", licenses=b"[]", notes=b"Open release\n")
        cls.repo.publish_local()

    @classmethod
    def tearDownClass(cls):
        cls.environment.stop()
        cls.workspace.cleanup()

    def client(self, expected, profile):
        result = subprocess.run([str(CLIENT), str(self.directory), str(expected), f"--profile={profile}"], capture_output=True, text=True)
        self.assertEqual(result.returncode, 0, result.stdout + result.stderr)
        return result.stdout

    def test_open_repository_is_marked_and_followed_only_by_open_builds(self):
        root = json.loads((self.directory / "metadata/1.root.json").read_bytes())
        self.assertEqual(root["signed"]["x_navlisten_profile"], "open")
        self.assertEqual(json.loads(self.repo.target_bytes("releases", "releases/31.json"))["profile"], "open")
        self.assertIn("sequence=31", self.client(0, "open"))
        for profile in ("trusted", "test"):
            self.assertIn("initialize=2001", self.client(2001, profile))

    def test_open_signers_cannot_extend_another_track(self):
        for profile in ("trusted", "test"):
            with self.assertRaisesRegex(ValueError, "separate signer configuration"):
                Signers(self.keys, profile=profile)

    def test_stored_open_repository_refuses_a_root_marked_for_another_track(self):
        value = json.loads((self.directory / "metadata/1.root.json").read_bytes())
        value["signed"]["x_navlisten_profile"] = "trusted"
        forged = self.signers.sign("root", Metadata.from_dict(value))
        with self.assertRaisesRegex(ValueError, "wrong trust profile"):
            Repository(self.directory, self.signers, now=NOW).load(forged)


if __name__ == "__main__":
    unittest.main()
