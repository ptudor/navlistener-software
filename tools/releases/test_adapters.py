import hashlib
import json
import os
from pathlib import Path
import subprocess
import sys
import tempfile
import unittest
from unittest.mock import patch
import urllib.error

import build_idf
from fixtures import PASSPHRASE_ENV, init_release_keys
import origin
import publisher
import release
import repository as repository_module
from firmware_signing import board_family, keys, sign_image, verify_image
from repository import Repository, Signers, digest, encoded, init_test_keys

class BuildAdapterTests(unittest.TestCase):
    def test_license_inventory_ignores_synthetic_component_and_keeps_public_paths(self):
        with tempfile.TemporaryDirectory() as temporary:
            base = Path(temporary).resolve()
            source, idf = base / "source", base / "idf"
            component = source / "esp32/components/example"
            component.mkdir(parents=True)
            idf.mkdir()
            (source / "LICENSE").write_text("Project license\n")
            (idf / "LICENSE").write_text("SDK license\n")
            (component / "NOTICE").write_text("Component notice\n")
            notices = build_idf.license_inventory(source, idf, {
                "build_component_paths": [str(component), ""]})
            self.assertEqual([n["path"] for n in notices], [
                "LICENSE", "esp-idf/LICENSE", "esp32/components/example/NOTICE"])
            self.assertNotIn(str(base), json.dumps(notices))

    def test_license_inventory_refuses_external_notice_symlink(self):
        with tempfile.TemporaryDirectory() as temporary:
            base = Path(temporary).resolve()
            source, idf = base / "source", base / "idf"
            source.mkdir()
            idf.mkdir()
            outside = base / "outside"
            outside.write_text("Unpinned notice\n")
            (source / "NOTICE").symlink_to(outside)
            with self.assertRaisesRegex(ValueError, "outside the pinned"):
                build_idf.license_inventory(source, idf, {"build_component_paths": []})

class AdapterTests(unittest.TestCase):
    @classmethod
    def setUpClass(cls):
        cls.workspace = tempfile.TemporaryDirectory(prefix="navlisten-adapters-")
        cls.base = Path(cls.workspace.name)
        cls.signers = Signers(init_test_keys(cls.base / "TEST-ONLY-keys"), profile="test")
        cls.open_keys, passphrase = init_release_keys(cls.base / "open-keys", "open")
        cls.environment = patch.dict(os.environ, {PASSPHRASE_ENV: passphrase})
        cls.environment.start()
        cls.open = Signers(cls.open_keys, profile="open")

    @classmethod
    def tearDownClass(cls):
        cls.environment.stop()
        cls.workspace.cleanup()

    def image(self, length=700):
        data = bytearray(length)
        data[0] = 0xe9; data[1] = 1; data[12] = 9
        data[288:304] = b"NVFOTA1\0\x02\x01\x01\x00\x03\x00\x00\x00"
        return bytes(data)

    def test_firmware_signature_binds_image_key_and_four_mib_limit(self):
        public, active = keys(self.signers.config, False)
        image, _ = sign_image(self.image(0x200001), self.signers.config, False)
        verify_image(image, public[active])
        for at in (0, -4096, -4050, -2900):
            corrupt = bytearray(image); corrupt[at] ^= 1
            with self.assertRaises(Exception): verify_image(bytes(corrupt), public[active])
        with self.assertRaises(Exception): verify_image(image, public[1])
        with self.assertRaises(ValueError): sign_image(self.image(0x400001), self.signers.config, False)
        old = bytearray(self.image()); old[296] = 1
        with self.assertRaises(ValueError): sign_image(bytes(old), self.signers.config, False)
        # Byte 297 names the board; a release follows it, and an unknown board is refused.
        for board, family in ((0, "gnss-color"), (1, "gnss-color-neo"), (2, "gnss-color-zed-x20"), (3, "gnss-color-max")):
            marked = bytearray(self.image()); marked[297] = board
            self.assertEqual(board_family(bytes(marked)), family)
        for board in (4, 255):
            marked = bytearray(self.image()); marked[297] = board
            with self.assertRaises(ValueError): sign_image(bytes(marked), self.signers.config, False)
        with self.assertRaises(ValueError): keys(self.signers.config, True)

    def test_file_adapter_refuses_accidental_production_test_key(self):
        private = self.signers.config["roles"]["timestamp"][0]["test_key"]
        adapter = str(Path(__file__).with_name("file_signer.py"))
        result = subprocess.run([sys.executable, adapter, "--key", private], input=b"payload", capture_output=True)
        self.assertNotEqual(result.returncode, 0)
        self.assertIn(b"encrypted external release key", result.stderr)
        result = subprocess.run([sys.executable, adapter, "--key", private, "--test-only"], input=b"payload", capture_output=True, check=True)
        from securesystemslib.signer import Signature
        signature = Signature.from_dict(json.loads(result.stdout))
        self.signers.keys["timestamp"][0].verify_signature(signature, b"payload")

    def header(self, path, data, previous=None, track="open"):
        return {"track": track, "path": path, "length": len(data), "sha256": digest(data), "previous_timestamp": previous}

    def test_origin_immutable_retries_timestamp_compare_and_swap(self):
        root = Path(tempfile.mkdtemp(dir=self.base))
        data = b"image"
        header = self.header("targets/image.bin", data)
        self.assertFalse(origin.write(root, "open", header, data)["already_present"])
        self.assertTrue(origin.write(root, "open", header, data)["already_present"])
        with self.assertRaisesRegex(ValueError, "immutable"):
            origin.write(root, "open", self.header("targets/image.bin", b"replacement"), b"replacement")
        old = encoded({"signed": {"version": 1}}); new = encoded({"signed": {"version": 2}})
        origin.write(root, "open", self.header("metadata/timestamp.json", old), old)
        with self.assertRaisesRegex(ValueError, "changed"):
            origin.write(root, "open", self.header("metadata/timestamp.json", new), new)
        origin.write(root, "open", self.header("metadata/timestamp.json", new, digest(old)), new)
        self.assertTrue(origin.write(root, "open", self.header("metadata/timestamp.json", new, digest(old)), new)["already_present"])
        with self.assertRaisesRegex(ValueError, "increase"):
            origin.write(root, "open", self.header("metadata/timestamp.json", old, digest(new)), old)

    def test_origin_refuses_another_tracks_upload_and_root_before_writing(self):
        root = Path(tempfile.mkdtemp(dir=self.base))
        data = b"image"
        for track, header in (("trusted", self.header("targets/image.bin", data)),
                              ("open", {k: v for k, v in self.header("targets/image.bin", data).items() if k != "track"}),
                              ("test", self.header("targets/image.bin", data, track="test"))):
            with self.assertRaisesRegex(ValueError, "different release track"):
                origin.write(root, track, header, data)
        repository = Repository(root / "candidate", self.open); repository.bootstrap()
        bootstrap = repository.files["metadata/1.root.json"]
        with self.assertRaisesRegex(ValueError, "different trust profile"):
            origin.write(root, "trusted", self.header("metadata/1.root.json", bootstrap, track="trusted"), bootstrap)
        self.assertEqual([p for p in root.iterdir() if p.name != "candidate"], [])
        self.assertFalse(origin.write(root, "open", self.header("metadata/1.root.json", bootstrap), bootstrap)["already_present"])

    def test_origin_rejects_traversal_and_symlinks_before_creating_directories(self):
        root = Path(tempfile.mkdtemp(dir=self.base)); outside = Path(tempfile.mkdtemp(dir=self.base))
        (root / "targets").symlink_to(outside, target_is_directory=True)
        for path in ("targets/new-directory/escape.bin", "targets/../../escape.bin", "/metadata/root.json"):
            with self.assertRaises(ValueError): origin.write(root, "open", self.header(path, b"x"), b"x")
        self.assertFalse((outside / "new-directory").exists())

    def test_publication_waits_for_both_origins_and_resumes_lost_timestamp_receipt(self):
        root = Path(tempfile.mkdtemp(dir=self.base))
        repository = Repository(root / "candidate", self.open); repository.bootstrap()
        image, key = sign_image(self.image(), self.open.config, True)
        repository.add_release(sequence=31, version="0.1.0", revision="a" * 40, image=image, board_family=board_family(image), boot_key_id=key,
            provenance=b"{}", licenses=b"[]", notes=b"Open release\n")
        remote = root / "origin"; remote.mkdir(); fetches=[]; origins=publisher.ORIGINS["open"]
        def upload(command, track, path, data, previous=None):
            origin.write(remote, "open", self.header(path, data, previous, track), data)
        def fetch(base, path, expected=None):
            fetches.append((base, path))
            if not (remote / path).exists(): raise urllib.error.HTTPError(base + path, 404, "absent", {}, None)
            data = (remote / path).read_bytes()
            if expected is not None: self.assertEqual(data, expected)
            return data
        bootstrap=repository.files["metadata/1.root.json"]
        with patch.object(publisher,"upload",side_effect=upload), patch.object(publisher,"fetch",side_effect=fetch):
            def lost_receipt(): raise RuntimeError("lost commit receipt")
            with self.assertRaisesRegex(RuntimeError,"lost commit"):
                publisher.publish([],"open",repository.files,None,bootstrap,["releases/31.json"],lost_receipt)
            self.assertTrue((remote / "metadata/timestamp.json").is_file())
            publisher.publish([],"open",repository.files,None,bootstrap,["releases/31.json"])
            publisher.verify_public(origins,bootstrap,["releases/31.json"],self.open.config)
        for base in origins:
            self.assertIn("/firmware/open/v1/", base)
            self.assertTrue(any(host==base and path.startswith("targets/artifacts/") for host,path in fetches))
        self.assertFalse(set(origins) & set(publisher.ORIGINS["trusted"]))
        remote2=root / "failed-origin";remote2.mkdir();remote=remote2
        def unavailable(base,path,expected=None):
            if base==origins[1]: raise OSError("backup origin unavailable")
            return fetch(base,path,expected)
        with patch.object(publisher,"upload",side_effect=upload),patch.object(publisher,"fetch",side_effect=unavailable):
            with self.assertRaisesRegex(OSError,"backup origin"):
                publisher.publish([],"open",repository.files,None,bootstrap,["releases/31.json"])
        self.assertFalse((remote / "metadata/timestamp.json").exists())
        # A publisher aimed at the wrong track's directory stops at its first object.
        remote3=root / "trusted-origin";remote3.mkdir()
        def misdirected(command, track, path, data, previous=None):
            origin.write(remote3, "trusted", self.header(path, data, previous, track), data)
        with patch.object(publisher,"upload",side_effect=misdirected),patch.object(publisher,"fetch",side_effect=fetch):
            with self.assertRaisesRegex(ValueError,"different release track"):
                publisher.publish([],"open",repository.files,None,bootstrap,["releases/31.json"])
        self.assertEqual(list(remote3.iterdir()), [])

    def test_metadata_preparation_resumes_its_frozen_signatures(self):
        state=Path(tempfile.mkdtemp(dir=self.base))
        repo=Repository(state/"repository",self.signers);repo.bootstrap()
        image,key=sign_image(self.image(),self.signers.config,False)
        repo.add_release(sequence=31,version="0.1.0",revision="a"*40,image=image,board_family=board_family(image),boot_key_id=key,provenance=b"{}",licenses=b"[]",notes=b"Notes\n")
        repo.publish_local();bootstrap=repo.files["metadata/1.root.json"]
        directory=state/"transactions/metadata-3";directory.mkdir(parents=True)
        tx={"phase":"preparing","previous_timestamp":digest(repo.files["metadata/timestamp.json"]),"action":"promote","channel":"canary","release":31,"percent":25}
        release.save_transaction(directory,tx,self.signers)
        original=repository_module.immutable
        calls=0
        def interrupted(path,data):
            nonlocal calls
            original(path,data);calls+=1
            if calls==2:raise RuntimeError("interrupted candidate write")
        with patch.object(repository_module,"immutable",side_effect=interrupted):
            with self.assertRaisesRegex(RuntimeError,"interrupted candidate"):
                release.prepare_metadata(directory,tx,{"state_dir":str(state)},self.signers,bootstrap)
        frozen=(directory/"candidate-bundle.json").read_bytes()
        restored=release.load_transaction(directory,self.signers)
        self.assertEqual(restored["phase"],"preparing")
        release.prepare_metadata(directory,restored,{"state_dir":str(state)},self.signers,bootstrap)
        self.assertEqual((directory/"candidate-bundle.json").read_bytes(),frozen)
        self.assertEqual(release.load_transaction(directory,self.signers)["phase"],"prepared")
        checked=Repository(directory/"candidate",self.signers);checked.load(bootstrap)
        self.assertEqual(checked.channel_value("canary")["percentage"],25)

    def release_config(self, **changes):
        directory = Path(tempfile.mkdtemp(dir=self.base))
        state = directory / "state"
        if not (self.base / "open-state").exists():
            repository = Repository(self.base / "open-state/repository", self.open); repository.bootstrap(); repository.publish_local()
            (self.base / "open-state/trust-root.json").write_bytes(repository.files["metadata/1.root.json"])
        value = {"track": "open", "open_approved": True, "state_dir": str(state), "signers": str(self.open_keys),
            "root": str(self.base / "open-state/trust-root.json"), "branch": "main", "remotes": ["origin", "github"],
            "build_command": [sys.executable], "publish_command": [sys.executable]}
        value.update(changes)
        path = directory / "release.json"
        path.write_bytes(encoded({k: v for k, v in value.items() if v is not None}))
        return path

    def test_release_configuration_names_one_approved_track_with_matching_keys_and_root(self):
        config, signers, bootstrap = release.configuration(self.release_config())
        self.assertEqual(config["origins"], publisher.ORIGINS["open"])
        self.assertEqual(signers.profile, "open")
        for changes, message in (({"track": None}, "must name its track"), ({"track": "production"}, "must name its track"),
                ({"track": "test"}, "must name its track"), ({"open_approved": False}, "open track is not approved"),
                ({"open_approved": None}, "open track is not approved"), ({"trusted_approved": True}, "another track's approval"),
                ({"track": "trusted", "open_approved": None, "trusted_approved": True}, "separate signer configuration"),
                ({"signers": str(self.signers.path)}, "separate signer configuration")):
            with self.assertRaisesRegex(ValueError, message):
                release.configuration(self.release_config(**changes))
        test_state = self.base / "test-state"
        if not test_state.exists():
            repository = Repository(test_state / "repository", self.signers); repository.bootstrap()
            test_state.mkdir(exist_ok=True); (test_state / "trust-root.json").write_bytes(repository.files["metadata/1.root.json"])
        with self.assertRaisesRegex(ValueError, "marked for another profile"):
            release.configuration(self.release_config(root=str(test_state / "trust-root.json")))

    def test_signer_entry_matches_the_generated_configuration_without_private_material(self):
        config = self.open.config
        spec = config["roles"]["timestamp"][0]
        public = Path(spec["command"][-1]).with_suffix(".pub")
        self.assertEqual(release.signer_entry(public, firmware=False), spec["public"])
        firmware = config["firmware"][0]
        self.assertEqual(release.signer_entry(firmware["public"], firmware=True), firmware["key_id"])
        with self.assertRaisesRegex(ValueError, "P-256"):
            release.signer_entry(firmware["public"], firmware=False)
        with self.assertRaises(ValueError):
            release.signer_entry(public, firmware=True)

    def test_build_adapter_selects_the_track_profile_and_refuses_others(self):
        self.assertEqual(build_idf.DEFAULTS, {"trusted": "sdkconfig.defaults.production", "open": "sdkconfig.defaults.open"})
        for name in build_idf.DEFAULTS.values():
            self.assertTrue((Path(release.SOURCE) / "esp32" / name).is_file())
        with patch.object(sys, "stdin", __import__("io").StringIO(json.dumps({"profile": "test"}))):
            with self.assertRaisesRegex(ValueError, "trusted or open"):
                build_idf.main()

if __name__ == "__main__": unittest.main()
