"""Disposable release-track signer configurations for the host tests.

The production and open tracks refuse test keys, so these fixtures create
encrypted private keys and sign through file_signer.py exactly as an operator's
offline file adapter would. Nothing here is a default or a reusable key.
"""
import os
from pathlib import Path
import sys

from cryptography.hazmat.primitives import serialization
from cryptography.hazmat.primitives.asymmetric import ec, rsa

from firmware_signing import public_bytes
from release import signer_entry
from repository import COUNTS, atomic, encoded

PASSPHRASE_ENV = "NAVLISTEN_FIXTURE_PASSPHRASE"
ADAPTER = str(Path(__file__).with_name("file_signer.py"))


def init_release_keys(directory: Path, profile: str):
    """Return a signers.json for one release track; the caller exports the passphrase."""
    directory.mkdir(mode=0o700, parents=True)
    passphrase = os.urandom(24).hex()
    encryption = serialization.BestAvailableEncryption(passphrase.encode())
    adapter = [sys.executable, ADAPTER, "--passphrase-env", PASSPHRASE_ENV, "--key"]

    def store(name, key):
        private, public = directory / f"{name}.key", directory / f"{name}.pub"
        atomic(private, key.private_bytes(serialization.Encoding.PEM, serialization.PrivateFormat.PKCS8, encryption), private=True)
        atomic(public, public_bytes(key.public_key()))
        return private, public

    config = {"profile": profile, "roles": {}, "firmware": [], "firmware_active": 0}
    for role, count in COUNTS.items():
        config["roles"][role] = []
        for index in range(count):
            private, public = store(f"{profile}-{role}-{index}", ec.generate_private_key(ec.SECP256R1()))
            config["roles"][role].append({"public": signer_entry(public, firmware=False), "command": [*adapter, str(private)]})
    for index in range(3):
        private, public = store(f"{profile}-firmware-{index}", rsa.generate_private_key(65537, 3072))
        config["firmware"].append({"public": str(public), "key_id": signer_entry(public, firmware=True),
            "command": [*adapter, str(private), "--firmware"]})
    atomic(directory / "signers.json", encoded(config), private=True)
    return directory / "signers.json", passphrase
