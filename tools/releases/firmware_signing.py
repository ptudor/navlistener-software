"""Secure Boot v2 RSA-3072 adapters; private keys never enter a firmware build."""
import hashlib
import json
from pathlib import Path
import struct
import subprocess
import zlib

from cryptography.hazmat.primitives import hashes, serialization
from cryptography.hazmat.primitives.asymmetric import padding, rsa, utils

from repository import atomic

def public_bytes(key):
    return key.public_bytes(serialization.Encoding.PEM, serialization.PublicFormat.SubjectPublicKeyInfo)

def public_block(key):
    if not isinstance(key, rsa.RSAPublicKey) or key.key_size != 3072:
        raise ValueError("firmware signing requires RSA-3072 public keys")
    numbers = key.public_numbers()
    if numbers.e != 65537:
        raise ValueError("unsupported RSA exponent")
    return struct.pack("<384sI384sI", numbers.n.to_bytes(384, "little"), numbers.e,
        pow(2, 6144, numbers.n).to_bytes(384, "little"), (-pow(numbers.n, -1, 2**32)) % 2**32)

def key_id(key):
    return hashlib.sha256(public_block(key)).hexdigest()

def init_test_firmware_keys(directory, config):
    config["firmware_active"] = 0
    config["firmware"] = []
    for index in range(3):
        key = rsa.generate_private_key(65537, 3072)
        private = directory / f"TEST-ONLY-firmware-{index}.key"
        public = directory / f"TEST-ONLY-firmware-{index}.pub"
        atomic(private, key.private_bytes(serialization.Encoding.PEM, serialization.PrivateFormat.PKCS8,
            serialization.NoEncryption()), private=True)
        atomic(public, public_bytes(key.public_key()))
        config["firmware"].append({"public": str(public), "key_id": key_id(key.public_key()), "test_key": str(private)})

def keys(config, production):
    entries = config["firmware"]
    if len(entries) != 3 or config.get("test_only") is not (not production):
        raise ValueError("three independent firmware public keys and an explicit key purpose are required")
    result = []
    for entry in entries:
        key = serialization.load_pem_public_key(Path(entry["public"]).read_bytes())
        if key_id(key) != entry["key_id"]:
            raise ValueError("firmware public key identity differs from configuration")
        if production and ("test_key" in entry or "TEST-ONLY" in entry["public"]):
            raise ValueError("production refuses test firmware keys")
        result.append(key)
    if len({key_id(key) for key in result}) != 3:
        raise ValueError("firmware recovery keys must be independent")
    active = config["firmware_active"]
    if type(active) is not int or not 0 <= active < 3:
        raise ValueError("invalid active firmware signing key")
    if production and not entries[active].get("command"):
        raise ValueError("active production firmware key needs a signing adapter")
    return result, active

def sign_image(image, config, production):
    public, active = keys(config, production)
    if not 304 <= len(image) <= 0x200000 - 4096 or image[0] != 0xe9 or image[12:14] != b"\x09\x00":
        raise ValueError("input is not an ESP32-S3 application fitting the OTA slot")
    if image[288:300] != b"NVFOTA1\0\x01\x01\x01\x00":
        raise ValueError("input lacks the approved board/rollback marker or enables manufacturing writes")
    padded = image + b"\xff" * (-len(image) % 4096)
    digest = hashlib.sha256(padded).digest()
    spec = config["firmware"][active]
    if production:
        # Adapter reads the secure-padded image on stdin and returns exactly a
        # 384-byte RSA-PSS signature (SHA-256, MGF1-SHA256, 32-byte salt).
        signature = subprocess.run(spec["command"], input=padded, stdout=subprocess.PIPE,
            check=True, timeout=300).stdout
    else:
        private = Path(spec["test_key"])
        if not private.is_absolute() or private.stat().st_mode & 0o077:
            raise ValueError("test firmware key must be an absolute private file")
        key = serialization.load_pem_private_key(private.read_bytes(), password=None)
        signature = key.sign(digest, padding.PSS(mgf=padding.MGF1(hashes.SHA256()), salt_length=32), utils.Prehashed(hashes.SHA256()))
    if len(signature) != 384:
        raise ValueError("firmware adapter did not return one RSA-3072 signature")
    public[active].verify(signature, digest, padding.PSS(mgf=padding.MGF1(hashes.SHA256()), salt_length=32), utils.Prehashed(hashes.SHA256()))
    block = b"\xe7\x02\0\0" + digest + public_block(public[active]) + signature[::-1]
    block += struct.pack("<I", zlib.crc32(block)) + bytes(16)
    assert len(block) == 1216
    result = padded + block + b"\xff" * (4096 - len(block))
    verify_image(result, public[active])
    return result, key_id(public[active])

def verify_image(image, key):
    if len(image) % 4096 or not 8192 <= len(image) <= 0x200000:
        raise ValueError("invalid signed image length")
    block = image[-4096:-4096 + 1216]
    digest = hashlib.sha256(image[:-4096]).digest()
    if block[:4] != b"\xe7\x02\0\0" or block[4:36] != digest or block[36:812] != public_block(key) or \
       struct.unpack("<I", block[1196:1200])[0] != zlib.crc32(block[:1196]):
        raise ValueError("invalid Secure Boot signature block")
    key.verify(block[812:1196][::-1], digest, padding.PSS(mgf=padding.MGF1(hashes.SHA256()), salt_length=32), utils.Prehashed(hashes.SHA256()))
