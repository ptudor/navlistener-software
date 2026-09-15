#!/usr/bin/env python3
"""A file signing adapter. Production PEM files must be encrypted and private.

Use a vault/HSM adapter for shared production roles. This implementation is
useful for an offline operator-owned encrypted key file and adapter testing.
"""
import argparse
import getpass
import hashlib
import json
import os
from pathlib import Path
import sys

from cryptography.hazmat.primitives import hashes, serialization
from cryptography.hazmat.primitives.asymmetric import padding, rsa, utils
from securesystemslib.signer import CryptoSigner

def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--key", required=True, type=Path)
    parser.add_argument("--firmware", action="store_true")
    parser.add_argument("--test-only", action="store_true")
    parser.add_argument("--passphrase-env", help="name of a private process environment variable; never pass its value as an argument")
    args = parser.parse_args()
    path = args.key.expanduser().resolve(strict=True)
    if path.stat().st_mode & 0o077:
        raise ValueError("signing key must have mode 0600")
    pem = path.read_bytes()
    if not args.test_only and (b"ENCRYPTED PRIVATE KEY" not in pem or "TEST-ONLY" in path.name or path.is_relative_to(Path(__file__).resolve().parents[2])):
        raise ValueError("production file adapter requires an encrypted external production key")
    password = None
    if b"ENCRYPTED PRIVATE KEY" in pem:
        password = (os.environ[args.passphrase_env] if args.passphrase_env else getpass.getpass("Unlock offline signing key: ")).encode()
    key = serialization.load_pem_private_key(pem, password=password)
    data = sys.stdin.buffer.read(0x400001 if args.firmware else 24577)
    if len(data) > (0x400000 if args.firmware else 24576):
        raise ValueError("signing input exceeds the bounded profile")
    if args.firmware:
        if not isinstance(key, rsa.RSAPrivateKey) or key.key_size != 3072:
            raise ValueError("firmware adapter requires RSA-3072")
        signature = key.sign(hashlib.sha256(data).digest(), padding.PSS(mgf=padding.MGF1(hashes.SHA256()), salt_length=32), utils.Prehashed(hashes.SHA256()))
        sys.stdout.buffer.write(signature)
    else:
        signer = CryptoSigner(key)
        if signer.public_key.scheme != "ecdsa-sha2-nistp256":
            raise ValueError("metadata adapter requires ECDSA P-256")
        print(json.dumps(signer.sign(data).to_dict()))

if __name__ == "__main__":
    try:
        main()
    except (OSError, ValueError, KeyError) as error:
        raise SystemExit(str(error)) from None
