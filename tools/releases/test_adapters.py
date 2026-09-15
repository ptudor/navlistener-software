import hashlib
import json
from pathlib import Path
import subprocess
import sys
import tempfile
import unittest
from unittest.mock import patch
import urllib.error

import origin
import publisher
import release
import repository as repository_module
from firmware_signing import keys, sign_image, verify_image
from repository import Repository, Signers, digest, encoded, init_test_keys

class AdapterTests(unittest.TestCase):
    @classmethod
    def setUpClass(cls):
        cls.workspace = tempfile.TemporaryDirectory(prefix="navlisten-adapters-")
        cls.base = Path(cls.workspace.name)
        cls.signers = Signers(init_test_keys(cls.base / "TEST-ONLY-keys"), production=False)

    @classmethod
    def tearDownClass(cls):
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
        with self.assertRaises(ValueError): keys(self.signers.config, True)

    def test_file_adapter_refuses_accidental_production_test_key(self):
        private = self.signers.config["roles"]["timestamp"][0]["test_key"]
        adapter = str(Path(__file__).with_name("file_signer.py"))
        result = subprocess.run([sys.executable, adapter, "--key", private], input=b"payload", capture_output=True)
        self.assertNotEqual(result.returncode, 0)
        self.assertIn(b"encrypted external production key", result.stderr)
        result = subprocess.run([sys.executable, adapter, "--key", private, "--test-only"], input=b"payload", capture_output=True, check=True)
        from securesystemslib.signer import Signature
        signature = Signature.from_dict(json.loads(result.stdout))
        self.signers.keys["timestamp"][0].verify_signature(signature, b"payload")

    def header(self, path, data, previous=None):
        return {"path": path, "length": len(data), "sha256": digest(data), "previous_timestamp": previous}

    def test_origin_immutable_retries_timestamp_compare_and_swap(self):
        root = Path(tempfile.mkdtemp(dir=self.base))
        data = b"image"
        header = self.header("targets/image.bin", data)
        self.assertFalse(origin.write(root, header, data)["already_present"])
        self.assertTrue(origin.write(root, header, data)["already_present"])
        with self.assertRaisesRegex(ValueError, "immutable"):
            origin.write(root, self.header("targets/image.bin", b"replacement"), b"replacement")
        old = encoded({"signed": {"version": 1}}); new = encoded({"signed": {"version": 2}})
        origin.write(root, self.header("metadata/timestamp.json", old), old)
        with self.assertRaisesRegex(ValueError, "changed"):
            origin.write(root, self.header("metadata/timestamp.json", new), new)
        origin.write(root, self.header("metadata/timestamp.json", new, digest(old)), new)
        self.assertTrue(origin.write(root, self.header("metadata/timestamp.json", new, digest(old)), new)["already_present"])
        with self.assertRaisesRegex(ValueError, "increase"):
            origin.write(root, self.header("metadata/timestamp.json", old, digest(new)), old)

    def test_origin_rejects_traversal_and_symlinks_before_creating_directories(self):
        root = Path(tempfile.mkdtemp(dir=self.base)); outside = Path(tempfile.mkdtemp(dir=self.base))
        (root / "targets").symlink_to(outside, target_is_directory=True)
        for path in ("targets/new-directory/escape.bin", "targets/../../escape.bin", "/metadata/root.json"):
            with self.assertRaises(ValueError): origin.write(root, self.header(path, b"x"), b"x")
        self.assertFalse((outside / "new-directory").exists())

    def test_publication_waits_for_both_origins_and_resumes_lost_timestamp_receipt(self):
        root = Path(tempfile.mkdtemp(dir=self.base))
        repository = Repository(root / "candidate", self.signers); repository.bootstrap()
        image, key = sign_image(self.image(), self.signers.config, False)
        repository.add_release(sequence=31, version="0.1.0", revision="a" * 40, image=image, boot_key_id=key,
            provenance=b"{}", licenses=b"[]", notes=b"Test release\n")
        remote = root / "origin"; remote.mkdir(); fetches=[]
        def upload(command, path, data, previous=None):
            origin.write(remote, self.header(path, data, previous), data)
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
                publisher.publish([],repository.files,None,bootstrap,["releases/31.json"],lost_receipt)
            self.assertTrue((remote / "metadata/timestamp.json").is_file())
            publisher.publish([],repository.files,None,bootstrap,["releases/31.json"])
            publisher.verify_public(bootstrap,["releases/31.json"],self.signers.config)
        for base in publisher.ORIGINS:
            self.assertTrue(any(host==base and path.startswith("targets/artifacts/") for host,path in fetches))
        remote2=root / "failed-origin";remote2.mkdir();remote=remote2
        def unavailable(base,path,expected=None):
            if base==publisher.ORIGINS[1]: raise OSError("backup origin unavailable")
            return fetch(base,path,expected)
        with patch.object(publisher,"upload",side_effect=upload),patch.object(publisher,"fetch",side_effect=unavailable):
            with self.assertRaisesRegex(OSError,"backup origin"):
                publisher.publish([],repository.files,None,bootstrap,["releases/31.json"])
        self.assertFalse((remote / "metadata/timestamp.json").exists())

    def test_metadata_preparation_resumes_its_frozen_signatures(self):
        state=Path(tempfile.mkdtemp(dir=self.base))
        repo=Repository(state/"repository",self.signers);repo.bootstrap()
        image,key=sign_image(self.image(),self.signers.config,False)
        repo.add_release(sequence=31,version="0.1.0",revision="a"*40,image=image,boot_key_id=key,provenance=b"{}",licenses=b"[]",notes=b"Notes\n")
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

if __name__ == "__main__": unittest.main()
